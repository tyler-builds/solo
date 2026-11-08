package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func must(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a repository on main with one commit.
func newRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, dir, "init", "-q", "-b", "main")
	must(t, dir, "config", "user.email", "test@example.com")
	must(t, dir, "config", "user.name", "Test")
	must(t, dir, "config", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, "a.txt"), "one\ntwo\n")
	must(t, dir, "add", "-A")
	must(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	newRepo(t, filepath.Join(root, "alpha"))
	newRepo(t, filepath.Join(root, "group", "beta"))
	newRepo(t, filepath.Join(root, "group", "beta", "nested")) // inside a repo: not listed
	newRepo(t, filepath.Join(root, "node_modules", "dep"))
	newRepo(t, filepath.Join(root, ".hidden", "x"))
	newRepo(t, filepath.Join(root, "a", "b", "c", "d", "deep"))
	got := Scan([]string{root, root}, 3, "")
	want := []string{filepath.Join(root, "alpha"), filepath.Join(root, "group", "beta")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scan:\n got %v\nwant %v", got, want)
	}
}

func TestWorktreeChangesMerge(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, filepath.Join(t.TempDir(), "repo"))
	wtPath := filepath.Join(t.TempDir(), "wt", "fix")
	wt, err := AddWorktree(ctx, repo, wtPath, "solo/fix", "")
	if err != nil {
		t.Fatal(err)
	}
	if wt.BaseBranch != "main" || wt.BaseCommit == "" {
		t.Fatalf("worktree %+v", wt)
	}

	write(t, filepath.Join(wtPath, "a.txt"), "one\n2\nthree\n")
	write(t, filepath.Join(wtPath, "new.txt"), "hello\nworld\n")
	changes, err := Changes(ctx, wtPath, wt.BaseCommit)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Path != "a.txt" || changes[0].Added != 2 || changes[0].Removed != 1 ||
		changes[1].Path != "new.txt" || changes[1].Status != "?" || changes[1].Added != 2 {
		t.Fatalf("changes %+v", changes)
	}
	diff, err := Diff(ctx, wtPath, wt.BaseCommit)
	if err != nil || !strings.Contains(diff, "+three") || !strings.Contains(diff, "+hello") {
		t.Fatalf("diff %q %v", diff, err)
	}

	must(t, wtPath, "config", "user.email", "test@example.com")
	must(t, wtPath, "config", "user.name", "Test")
	if made, err := CommitAll(ctx, wtPath, "fix: things"); err != nil || !made {
		t.Fatalf("commit %v %v", made, err)
	}
	// Committed changes still count against the base.
	if changes, _ := Changes(ctx, wtPath, wt.BaseCommit); len(changes) != 2 {
		t.Fatalf("after commit %+v", changes)
	}

	if _, err := Merge(ctx, repo, wt.Branch, "main", "merge: fix"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "new.txt")); string(b) != "hello\nworld\n" {
		t.Fatalf("merged file %q", b)
	}
	if err := RemoveWorktree(ctx, repo, wtPath, wt.Branch, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatal("worktree still exists")
	}
	if out := must(t, repo, "branch", "--list", "solo/fix"); out != "" {
		t.Fatalf("branch not deleted: %q", out)
	}
}

func TestMergeConflictIsAborted(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, filepath.Join(t.TempDir(), "repo"))
	wtPath := filepath.Join(t.TempDir(), "wt")
	wt, err := AddWorktree(ctx, repo, wtPath, "solo/x", "main")
	if err != nil {
		t.Fatal(err)
	}
	must(t, wtPath, "config", "user.email", "test@example.com")
	must(t, wtPath, "config", "user.name", "Test")
	write(t, filepath.Join(wtPath, "a.txt"), "branch\n")
	CommitAll(ctx, wtPath, "branch change")
	write(t, filepath.Join(repo, "a.txt"), "main\n")
	must(t, repo, "commit", "-qam", "main change")

	_, err = Merge(ctx, repo, wt.Branch, "main", "merge")
	var me *MergeError
	if !errors.As(err, &me) || len(me.Conflicts) != 1 || me.Conflicts[0] != "a.txt" {
		t.Fatalf("want conflict on a.txt, got %v", err)
	}
	if st := must(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("repo left mid-merge: %q", st)
	}
}

func TestMergeRefusesWrongBranchOrDirty(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, filepath.Join(t.TempDir(), "repo"))
	must(t, repo, "branch", "solo/y")
	write(t, filepath.Join(repo, "a.txt"), "dirty\n")
	if _, err := Merge(ctx, repo, "solo/y", "main", "m"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty: %v", err)
	}
	must(t, repo, "checkout", "-q", "--", "a.txt")
	must(t, repo, "checkout", "-q", "-b", "other")
	if _, err := Merge(ctx, repo, "solo/y", "main", "m"); err == nil || !strings.Contains(err.Error(), "not \"main\"") {
		t.Fatalf("wrong branch: %v", err)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Fix OAuth login!":        "fix-oauth-login",
		"  ":                      "thread",
		"Ünïcode & stuff":         "n-code-stuff",
		strings.Repeat("ab ", 30): "ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-a",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// ScanAll also finds the folders that aren't repositories and hold none: the searched folder itself
// when it has no repositories, and the ones directly inside it.
func TestScanAllPlain(t *testing.T) {
	root := t.TempDir()
	newRepo(t, filepath.Join(root, "alpha"))
	write(t, filepath.Join(root, "vfl", "index.html"), "<p>hi</p>\n")
	write(t, filepath.Join(root, "group", "notes", "a.txt"), "a\n") // too deep to offer
	newRepo(t, filepath.Join(root, "group", "beta"))
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, ".hidden", "x.txt"), "x\n")
	lone := filepath.Join(t.TempDir(), "lone")
	write(t, filepath.Join(lone, "main.go"), "package main\n")

	repos, plain := ScanAll([]string{root, lone}, 3, "")
	if want := []string{filepath.Join(root, "alpha"), filepath.Join(root, "group", "beta")}; !slices.Equal(repos, want) {
		t.Fatalf("repos %v, want %v", repos, want)
	}
	want := []string{lone, filepath.Join(root, "vfl")}
	slices.Sort(want)
	if !slices.Equal(plain, want) {
		t.Fatalf("plain %v, want %v", plain, want)
	}
}

// Init makes a plain folder a repository with a first commit that leaves out what .gitignore says, so
// a worktree can start from main.
func TestInit(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "vfl")
	write(t, filepath.Join(dir, "index.html"), "<p>hi</p>\n")
	write(t, filepath.Join(dir, "node_modules", "dep", "x.js"), "x\n")
	write(t, filepath.Join(dir, ".env"), "SECRET=1\n")
	if err := Init(ctx, dir); err != nil {
		t.Fatal(err)
	}
	files, err := Run(ctx, dir, "ls-files")
	if err != nil {
		t.Fatal(err)
	}
	if files != ".gitignore\nindex.html" {
		t.Fatalf("committed %q", files)
	}
	if err := Init(ctx, dir); err == nil {
		t.Fatal("a second Init worked")
	}
	base, err := LatestBase(ctx, dir, "")
	if err != nil || base.Branch != "main" {
		t.Fatalf("base %+v, %v", base, err)
	}
	if _, err := AddWorktree(ctx, dir, filepath.Join(t.TempDir(), "wt"), "solo/x", base.Ref); err != nil {
		t.Fatal(err)
	}
}

// Init with too many files leaves the folder as it was.
func TestInitTooMany(t *testing.T) {
	defer func(n int) { maxInitFiles = n }(maxInitFiles)
	maxInitFiles = 2
	dir := filepath.Join(t.TempDir(), "big")
	for _, f := range []string{"a", "b", "c"} {
		write(t, filepath.Join(dir, f), f+"\n")
	}
	err := Init(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "4 files") {
		t.Fatalf("err %v", err)
	}
	for _, f := range []string{".git", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind: %v", f, err)
		}
	}
}
