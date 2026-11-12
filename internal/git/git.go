//go:build !js

package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/proc"
)

// Run runs git in dir and returns its trimmed stdout. A failure's error includes git's stderr.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := proc.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Never prompt: a credential or editor prompt would hang a background call.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_OPTIONAL_LOCKS=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// skipDirs are never searched for repositories.
var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "target": true, "dist": true, "build": true}

// Scan finds the git repositories in folders, searching up to depth levels below each. It doesn't look
// inside a repository it found, nor in hidden directories or exclude.
func Scan(folders []string, depth int, exclude string) []string {
	repos, _ := ScanAll(folders, depth, exclude)
	return repos
}

// ScanAll is Scan that also returns the plain folders: a folder Solo searches, or one directly inside
// it, that isn't a repository and has none inside it, such as a project that was never put in git.
func ScanAll(folders []string, depth int, exclude string) (repos, plain []string) {
	seen := map[string]bool{}
	for _, root := range folders {
		root = filepath.Clean(ExpandHome(root))
		// walk reports whether dir is or holds a repository.
		var walk func(dir string, level int) bool
		walk = func(dir string, level int) bool {
			if exclude != "" && dir == exclude {
				return false
			}
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				if !seen[dir] {
					seen[dir] = true
					repos = append(repos, dir)
				}
				return true
			}
			if level >= depth {
				return false
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return false
			}
			found := false
			for _, e := range entries {
				name := e.Name()
				if !e.IsDir() || strings.HasPrefix(name, ".") || skipDirs[name] {
					continue
				}
				if walk(filepath.Join(dir, name), level+1) {
					found = true
				}
			}
			if !found && level <= 1 && len(entries) > 0 && !seen[dir] {
				seen[dir] = true
				plain = append(plain, dir)
			}
			return found
		}
		walk(root, 0)
	}
	sort.Strings(repos)
	sort.Strings(plain)
	return repos, plain
}

// defaultIgnore is the .gitignore Init writes when a folder has none: what shouldn't be committed in
// most projects.
const defaultIgnore = `# Written by Solo when it put this folder in git. Edit as you like.
node_modules/
vendor/
dist/
build/
target/
out/
.venv/
venv/
__pycache__/
*.pyc
.DS_Store
Thumbs.db
*.log
.env
.env.*
`

// maxInitFiles is the most files Init commits; more usually means build output or dependencies a
// .gitignore should leave out.
var maxInitFiles = 20000

// Init puts a plain folder in git: a repository on branch main, a .gitignore if it has none, and a
// first commit of everything else, so worktrees have something to start from. When the folder holds
// too many files it undoes all of that and says so.
func Init(ctx context.Context, dir string) (err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s isn't a folder", dir)
	}
	if top, err := Run(ctx, dir, "rev-parse", "--show-toplevel"); err == nil {
		return fmt.Errorf("%s is already inside the repository %s", dir, top)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(dir) == filepath.Clean(home) {
		return errors.New("Solo won't put your whole home folder in git")
	}
	ignore := filepath.Join(dir, ".gitignore")
	wroteIgnore := false
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ignore, []byte(defaultIgnore), 0o644); err != nil {
			return err
		}
		wroteIgnore = true
	}
	defer func() {
		if err != nil { // leave the folder as it was
			os.RemoveAll(filepath.Join(dir, ".git"))
			if wroteIgnore {
				os.Remove(ignore)
			}
		}
	}()
	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		return err
	}
	if _, err := Run(ctx, dir, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	if _, err := Run(ctx, dir, "add", "--all"); err != nil {
		return err
	}
	staged, err := Run(ctx, dir, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return err
	}
	if n := strings.Count(staged, "\x00"); n > maxInitFiles {
		return fmt.Errorf("%s has %d files, which usually means build output or dependencies; add a .gitignore that leaves them out, then try again", filepath.Base(dir), n)
	}
	args := []string{"commit", "--quiet", "--allow-empty", "--no-verify", "-m", "chore: initial commit"}
	if email, _ := Run(ctx, dir, "config", "user.email"); email == "" { // git refuses to commit without one
		args = append([]string{"-c", "user.name=Solo", "-c", "user.email=solo@localhost"}, args...)
	}
	_, err = Run(ctx, dir, args...)
	return err
}

// ExpandHome replaces a leading ~ in p with the home folder.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// RepoInfo describes a repository's main checkout.
type RepoInfo struct {
	Path          string    `json:"path"`
	Name          string    `json:"name"`
	Branch        string    `json:"branch"`
	DefaultBranch string    `json:"defaultBranch,omitempty"`
	Dirty         bool      `json:"dirty"`
	LastCommit    string    `json:"lastCommit,omitempty"`
	LastCommitAt  time.Time `json:"lastCommitAt,omitzero"`
	Remote        string    `json:"remote,omitempty"`
	Err           string    `json:"error,omitempty"`
}

// Info reads a repository's state.
func Info(ctx context.Context, path string) RepoInfo {
	info := RepoInfo{Path: path, Name: filepath.Base(path)}
	branch, err := Run(ctx, path, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		branch, _ = Run(ctx, path, "rev-parse", "--short", "HEAD")
		if branch != "" {
			branch = "(detached " + branch + ")"
		}
	}
	info.Branch = branch
	if st, err := Run(ctx, path, "status", "--porcelain", "--untracked-files=no"); err == nil {
		info.Dirty = st != ""
	} else {
		info.Err = err.Error()
	}
	if log, err := Run(ctx, path, "log", "-1", "--format=%ct%x00%s"); err == nil && log != "" {
		ts, subject, _ := strings.Cut(log, "\x00")
		if sec, err := strconv.ParseInt(ts, 10, 64); err == nil {
			info.LastCommitAt = time.Unix(sec, 0)
		}
		info.LastCommit = subject
	}
	info.Remote, _ = Run(ctx, path, "remote", "get-url", "origin")
	if ref, err := Run(ctx, path, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); err == nil {
		info.DefaultBranch = strings.TrimPrefix(ref, "origin/")
	}
	return info
}

// InfoAll reads many repositories concurrently.
func InfoAll(ctx context.Context, paths []string) []RepoInfo {
	out := make([]RepoInfo, len(paths))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = Info(ctx, p)
		}()
	}
	wg.Wait()
	return out
}

// Worktree is a worker thread's checkout.
type Worktree struct {
	Path       string
	Branch     string
	BaseBranch string
	BaseCommit string
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a title into a branch-safe name of at most 40 characters.
func Slug(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "thread"
	}
	return s
}

// AddWorktree creates a worktree at path on a new branch from base (the repository's current branch
// when base is empty).
func AddWorktree(ctx context.Context, repo, path, branch, base string) (Worktree, error) {
	if base == "" {
		b, err := Run(ctx, repo, "symbolic-ref", "--short", "-q", "HEAD")
		if err != nil {
			return Worktree{}, fmt.Errorf("%s has no current branch to start from; name a base branch", filepath.Base(repo))
		}
		base = b
	}
	commit, err := Run(ctx, repo, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return Worktree{}, fmt.Errorf("base %q not found in %s", base, filepath.Base(repo))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Worktree{}, err
	}
	if _, err := Run(ctx, repo, "worktree", "add", "-b", branch, path, commit); err != nil {
		return Worktree{}, err
	}
	return Worktree{Path: path, Branch: branch, BaseBranch: base, BaseCommit: commit}, nil
}

// RestoreWorktree recreates a removed worktree at path: on branch when it still exists, or on a new
// branch of that name from base's current commit. BaseCommit is where a new branch starts, or "" when
// the branch was kept.
func RestoreWorktree(ctx context.Context, repo, path, branch, base string) (Worktree, error) {
	if _, err := Run(ctx, repo, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Worktree{}, err
		}
		_, _ = Run(ctx, repo, "worktree", "prune")
		if _, err := Run(ctx, repo, "worktree", "add", path, branch); err != nil {
			return Worktree{}, err
		}
		return Worktree{Path: path, Branch: branch, BaseBranch: base}, nil
	}
	_, _ = Run(ctx, repo, "worktree", "prune")
	return AddWorktree(ctx, repo, path, branch, base)
}

// RemoveWorktree removes the worktree, discarding its changes, and deletes its branch when
// deleteBranch is set.
func RemoveWorktree(ctx context.Context, repo, path, branch string, deleteBranch bool) error {
	var errs []error
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if _, err := Run(ctx, repo, "worktree", "remove", "--force", path); err != nil {
				errs = append(errs, err)
			}
		}
		_, _ = Run(ctx, repo, "worktree", "prune")
	}
	if deleteBranch && branch != "" {
		if _, err := Run(ctx, repo, "branch", "-D", branch); err != nil && !strings.Contains(err.Error(), "not found") {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Changes lists what the worktree changed since base: commits on its branch and uncommitted edits,
// including new untracked files.
func Changes(ctx context.Context, worktree, base string) ([]FileChange, error) {
	numstat, err := Run(ctx, worktree, "diff", "--numstat", "-z", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	names, err := Run(ctx, worktree, "diff", "--name-status", "-z", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	byPath := map[string]*FileChange{}
	var order []string
	fields := strings.Split(names, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		p := fields[i+1]
		byPath[p] = &FileChange{Path: p, Status: fields[i]}
		order = append(order, p)
	}
	for _, rec := range strings.Split(numstat, "\x00") {
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		if fc := byPath[parts[2]]; fc != nil {
			fc.Added, _ = strconv.Atoi(parts[0])
			fc.Removed, _ = strconv.Atoi(parts[1])
		}
	}
	untracked, err := Run(ctx, worktree, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(untracked, "\x00") {
		if p == "" || byPath[p] != nil {
			continue
		}
		fc := &FileChange{Path: p, Status: "?"}
		if b, err := os.ReadFile(filepath.Join(worktree, p)); err == nil && !bytes.Contains(b, []byte{0}) {
			fc.Added = bytes.Count(b, []byte{'\n'})
			if len(b) > 0 && b[len(b)-1] != '\n' {
				fc.Added++
			}
		}
		byPath[p] = fc
		order = append(order, p)
	}
	out := make([]FileChange, 0, len(order))
	for _, p := range order {
		out = append(out, *byPath[p])
	}
	return out, nil
}

// Diff returns the unified diff of the worktree against base, including untracked files.
func Diff(ctx context.Context, worktree, base string) (string, error) {
	return DiffPrefixed(ctx, worktree, base, "")
}

// DiffPrefixed is Diff with every path under prefix ("api" makes a/api/x.go), for showing several
// repositories' diffs as one.
func DiffPrefixed(ctx context.Context, worktree, base, prefix string) (string, error) {
	var px []string
	if prefix != "" {
		px = []string{"--src-prefix=a/" + prefix + "/", "--dst-prefix=b/" + prefix + "/"}
	}
	d, err := Run(ctx, worktree, append(append([]string{"diff", "--no-color", "--no-ext-diff"}, px...), base)...)
	if err != nil {
		return "", err
	}
	untracked, err := Run(ctx, worktree, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(d)
	for _, p := range strings.Split(untracked, "\x00") {
		if p == "" {
			continue
		}
		// --no-index exits 1 when files differ, which they always do here.
		cmd := proc.CommandContext(ctx, "git", append(append([]string{"diff", "--no-color", "--no-index"}, px...), "--", os.DevNull, p)...)
		cmd.Dir = worktree
		out, _ := cmd.Output()
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		b.Write(out)
	}
	return b.String(), nil
}

// CommitAll commits every change in the worktree, including untracked files, if there are any. It
// reports whether it made a commit.
func CommitAll(ctx context.Context, worktree, message string) (bool, error) {
	st, err := Run(ctx, worktree, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if st == "" {
		return false, nil
	}
	if _, err := Run(ctx, worktree, "add", "-A"); err != nil {
		return false, err
	}
	if _, err := Run(ctx, worktree, "commit", "-q", "--no-verify", "-m", message); err != nil {
		return false, err
	}
	return true, nil
}

// MergeError is a merge that could not be done; the repository is left as it was.
type MergeError struct {
	Reason    string
	Conflicts []string
	// Local is set when the conflicts are with uncommitted changes in the checkout, not with the base
	// branch: the branch merges cleanly, but not under the user's edits.
	Local bool
}

func (e *MergeError) Error() string {
	if len(e.Conflicts) > 0 {
		return fmt.Sprintf("%s: %s", e.Reason, strings.Join(e.Conflicts, ", "))
	}
	return e.Reason
}

// Merge merges branch into base with a merge commit and returns it. The merge is made without touching
// any checkout, so it works whatever the checkout is on and whatever is uncommitted in it. When base is
// checked out, the checkout then moves to the merge, and uncommitted changes there are kept (set aside
// and put back if they touch the files the merge changes). On a conflict nothing changes and a
// *MergeError lists the conflicting files.
func Merge(ctx context.Context, repo, branch, base, message string) (string, error) {
	ref := "refs/heads/" + base
	old, err := Run(ctx, repo, "rev-parse", "--verify", "-q", ref)
	if err != nil {
		return "", &MergeError{Reason: fmt.Sprintf("%s has no branch %q", filepath.Base(repo), base)}
	}
	tree, conflicts, err := mergeTree(ctx, repo, "", old, branch)
	if err != nil {
		return "", err
	}
	if len(conflicts) > 0 {
		return "", &MergeError{Reason: "the branch conflicts with " + base, Conflicts: conflicts}
	}
	commit, err := Run(ctx, repo, "commit-tree", tree, "-p", old, "-p", branch, "-m", message)
	if err != nil {
		return "", err
	}
	dir, err := checkedOutAt(ctx, repo, ref)
	if err != nil {
		return "", err
	}
	if dir == "" { // no checkout to update: move the branch
		if _, err := Run(ctx, repo, "update-ref", "-m", "merge "+branch, ref, commit, old); err != nil {
			return "", err
		}
		return commit, nil
	}
	return commit, fastForward(ctx, dir, old, commit, branch)
}

// fastForward moves the checkout in dir from old to the merge commit, keeping its uncommitted changes.
func fastForward(ctx context.Context, dir, old, commit, branch string) error {
	if _, err := Run(ctx, dir, "merge", "--ff-only", "-q", commit); err == nil {
		return nil // git keeps uncommitted changes to files the merge doesn't touch
	}
	// The uncommitted changes touch files the merge changes. Put them aside, move, and put them back,
	// once it's clear they go back cleanly.
	edits, err := Run(ctx, dir, "stash", "create")
	if err != nil {
		return err
	}
	if edits == "" {
		_, err := Run(ctx, dir, "merge", "--ff-only", "-q", commit)
		return err
	}
	if _, conflicts, err := mergeTree(ctx, dir, old, commit, edits); err != nil {
		return err
	} else if len(conflicts) > 0 {
		return &MergeError{Reason: fmt.Sprintf("uncommitted changes in %s overlap the merge", filepath.Base(dir)),
			Conflicts: conflicts, Local: true}
	}
	if _, err := Run(ctx, dir, "stash", "push", "-q", "-m", "Solo: uncommitted changes, set aside to merge "+branch); err != nil {
		return err
	}
	_, ffErr := Run(ctx, dir, "merge", "--ff-only", "-q", commit)
	if _, err := Run(ctx, dir, "stash", "pop", "-q"); err != nil {
		return fmt.Errorf("your uncommitted changes in %s are kept in git stash: %w", filepath.Base(dir), err)
	}
	return ffErr
}

// mergeTree merges b into a without a checkout (from base when set, else their merge base), and
// returns the merged tree, or the files that conflict.
func mergeTree(ctx context.Context, dir, base, a, b string) (string, []string, error) {
	args := []string{"merge-tree", "--write-tree", "--name-only", "--no-messages"}
	if base != "" {
		args = append(args, "--merge-base="+base)
	}
	cmd := proc.CommandContext(ctx, "git", append(args, a, b)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var ee *exec.ExitError
	switch {
	case err == nil:
		return lines[0], nil, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1 && len(lines) > 1: // conflicts
		var files []string
		for _, f := range lines[1:] {
			if f = strings.TrimSpace(f); f != "" && !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
		return "", files, nil
	}
	return "", nil, fmt.Errorf("git merge-tree: %w: %s", err, strings.TrimSpace(errb.String()))
}

// checkedOutAt returns the checkout (the main one or a worktree) that has ref checked out, or "".
func checkedOutAt(ctx context.Context, repo, ref string) (string, error) {
	out, err := Run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	dir := ""
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "worktree "):
			dir = strings.TrimPrefix(l, "worktree ")
		case l == "branch "+ref:
			return dir, nil
		}
	}
	return "", nil
}

// InBase reports whether a worktree's work is already in base: nothing uncommitted, and its commit
// is in the local base branch or origin's copy of it (the thread pushed or merged it there itself), or
// its changes are, as after a squash merge, a rebase or a cherry-pick (see ContentIn).
func InBase(ctx context.Context, worktree, base string) bool {
	return InRemoteBase(ctx, worktree, base) || (clean(ctx, worktree) && inRef(ctx, worktree, "refs/heads/"+base))
}

// InRemoteBase is InBase for origin's copy of base alone, as last fetched.
func InRemoteBase(ctx context.Context, worktree, base string) bool {
	return clean(ctx, worktree) && InRemoteBranch(ctx, worktree, base)
}

// InRemoteBranch reports whether a worktree's commit is in origin's copy of branch, by ancestry or by
// content, whatever it has uncommitted.
func InRemoteBranch(ctx context.Context, worktree, branch string) bool {
	return inRef(ctx, worktree, "refs/remotes/origin/"+branch)
}

// clean reports whether a worktree has nothing uncommitted.
func clean(ctx context.Context, worktree string) bool {
	st, err := Run(ctx, worktree, "status", "--porcelain")
	return err == nil && st == ""
}

// inRef reports whether a worktree's commit is in ref, by ancestry or by content.
func inRef(ctx context.Context, worktree, ref string) bool {
	if _, err := Run(ctx, worktree, "rev-parse", "--verify", "-q", ref+"^{commit}"); err != nil {
		return false
	}
	if _, err := Run(ctx, worktree, "merge-base", "--is-ancestor", "HEAD", ref); err == nil {
		return true
	}
	return ContentIn(ctx, worktree, "HEAD", ref)
}

// ContentIn reports whether the changes commit makes since it forked from ref are already in ref,
// whatever their commits: merging commit into ref would change nothing. This sees work that landed
// as a squash merge, a rebase or a cherry-pick, which ancestry doesn't. A later change in ref to the
// same lines makes the merge conflict, so that reads as not in, never wrongly as in.
func ContentIn(ctx context.Context, dir, commit, ref string) bool {
	want, err := Run(ctx, dir, "rev-parse", "--verify", "-q", ref+"^{tree}")
	if err != nil {
		return false
	}
	tree, conflicts, err := mergeTree(ctx, dir, "", ref, commit)
	return err == nil && len(conflicts) == 0 && tree == want
}

// Fetch updates origin's copy of branch.
func Fetch(ctx context.Context, repo, branch string) error {
	_, err := Run(ctx, repo, "fetch", "--quiet", "origin", branch)
	return err
}

// CurrentBranch is the branch checked out in dir, or "" when its HEAD is detached.
func CurrentBranch(ctx context.Context, dir string) string {
	b, err := Run(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return b
}

// Pushed reports whether branch was pushed to origin: origin has a copy of it, or it tracks one.
func Pushed(ctx context.Context, dir, branch string) bool {
	if branch == "" {
		return false
	}
	if _, err := Run(ctx, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch); err == nil {
		return true
	}
	_, err := Run(ctx, dir, "config", "--get", "branch."+branch+".merge")
	return err == nil
}

// IsRepo reports whether dir is inside a git work tree.
func IsRepo(dir string) bool {
	_, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	out, err := Run(context.Background(), dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// Base is where a new worktree starts.
type Base struct {
	Ref    string // what to start from: "origin/main" after a fetch, or a local branch
	Branch string // the branch name to open pull requests against and merge into: "main"
	Remote bool   // Ref is the remote's copy
	// FetchErr is set when the fetch failed (offline, say); Ref is then the last fetched copy.
	FetchErr error
}

// DefaultBranch is the repository's default branch: origin's HEAD, else main or master, else "".
func DefaultBranch(ctx context.Context, repo string) string {
	if head, err := Run(ctx, repo, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(head, "origin/")
	}
	for _, c := range []string{"main", "master"} {
		for _, ref := range []string{"refs/remotes/origin/" + c, "refs/heads/" + c} {
			if _, err := Run(ctx, repo, "rev-parse", "--verify", "-q", ref+"^{commit}"); err == nil {
				return c
			}
		}
	}
	return ""
}

// LatestBase finds the branch a new worktree starts from, and fetches it so the worktree starts from
// its latest commit. want names the branch; empty means the default branch: origin's HEAD, else main
// or master, else the checkout's current branch.
func LatestBase(ctx context.Context, repo, want string) (Base, error) {
	_, err := Run(ctx, repo, "remote", "get-url", "origin")
	hasOrigin := err == nil
	exists := func(ref string) bool {
		_, err := Run(ctx, repo, "rev-parse", "--verify", "-q", ref+"^{commit}")
		return err == nil
	}
	branch := want
	if branch == "" {
		branch = DefaultBranch(ctx, repo)
	}
	if branch == "" {
		cur, err := Run(ctx, repo, "symbolic-ref", "--short", "-q", "HEAD")
		if err != nil {
			return Base{}, fmt.Errorf("%s has no main or master branch to start from; name a base branch", filepath.Base(repo))
		}
		branch = cur
	}
	b := Base{Ref: branch, Branch: branch}
	if hasOrigin {
		fctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		_, b.FetchErr = Run(fctx, repo, "fetch", "--quiet", "origin", branch)
		cancel()
		if exists("refs/remotes/origin/" + branch) {
			b.Ref, b.Remote = "origin/"+branch, true
		}
	}
	if !exists(b.Ref) {
		return Base{}, fmt.Errorf("branch %q not found in %s", branch, filepath.Base(repo))
	}
	return b, nil
}

// BranchExists reports whether the repository has the branch, locally or on origin.
func BranchExists(ctx context.Context, repo, name string) bool {
	for _, ref := range []string{"refs/heads/" + name, "refs/remotes/origin/" + name} {
		if _, err := Run(ctx, repo, "rev-parse", "--verify", "-q", ref); err == nil {
			return true
		}
	}
	return false
}

// ValidBranchName reports whether git accepts name as a branch name.
func ValidBranchName(ctx context.Context, repo, name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	_, err := Run(ctx, repo, "check-ref-format", "--branch", name)
	return err == nil
}

// Push pushes the worktree's branch to origin and sets it as the upstream.
func Push(ctx context.Context, worktree, branch string) error {
	_, err := Run(ctx, worktree, "push", "--quiet", "-u", "origin", branch)
	return err
}

// PushTo pushes the worktree's commit to origin's branch, which must be behind it (no force).
func PushTo(ctx context.Context, worktree, branch string) error {
	_, err := Run(ctx, worktree, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return err
}

// RemoteURL returns origin's URL, or "" when there is none.
func RemoteURL(ctx context.Context, repo string) string {
	u, err := Run(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return u
}
