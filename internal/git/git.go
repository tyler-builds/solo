// Package git finds repositories and manages the worktrees worker threads run in, through the git CLI.
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Run runs git in dir and returns its trimmed stdout. A failure's error includes git's stderr.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
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
	seen := map[string]bool{}
	var repos []string
	for _, root := range folders {
		root = filepath.Clean(expandHome(root))
		var walk func(dir string, level int)
		walk = func(dir string, level int) {
			if exclude != "" && dir == exclude {
				return
			}
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				if !seen[dir] {
					seen[dir] = true
					repos = append(repos, dir)
				}
				return
			}
			if level >= depth {
				return
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return
			}
			for _, e := range entries {
				name := e.Name()
				if !e.IsDir() || strings.HasPrefix(name, ".") || skipDirs[name] {
					continue
				}
				walk(filepath.Join(dir, name), level+1)
			}
		}
		walk(root, 0)
	}
	sort.Strings(repos)
	return repos
}

func expandHome(p string) string {
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

// FileChange is one changed file.
type FileChange struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // A, M, D, R, ?
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
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
	d, err := Run(ctx, worktree, "diff", "--no-color", "--no-ext-diff", base)
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
		cmd := exec.CommandContext(ctx, "git", "diff", "--no-color", "--no-index", "--", os.DevNull, p)
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
}

func (e *MergeError) Error() string {
	if len(e.Conflicts) > 0 {
		return fmt.Sprintf("%s: %s", e.Reason, strings.Join(e.Conflicts, ", "))
	}
	return e.Reason
}

// Merge merges branch into base in the repository's main checkout, with a merge commit. The checkout
// must be on base with no uncommitted changes to tracked files. On a conflict the merge is aborted and a
// *MergeError lists the conflicting files.
func Merge(ctx context.Context, repo, branch, base, message string) (string, error) {
	cur, err := Run(ctx, repo, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || cur != base {
		return "", &MergeError{Reason: fmt.Sprintf("%s is on %q, not %q; switch it to %s to merge", filepath.Base(repo), cur, base, base)}
	}
	if st, err := Run(ctx, repo, "status", "--porcelain", "--untracked-files=no"); err != nil {
		return "", err
	} else if st != "" {
		return "", &MergeError{Reason: fmt.Sprintf("%s has uncommitted changes; commit or stash them to merge", filepath.Base(repo))}
	}
	if _, err := Run(ctx, repo, "merge", "--no-ff", "--no-edit", "-m", message, branch); err != nil {
		conflicts, _ := Run(ctx, repo, "diff", "--name-only", "--diff-filter=U")
		_, _ = Run(ctx, repo, "merge", "--abort")
		if conflicts != "" {
			return "", &MergeError{Reason: "the branch conflicts with " + base, Conflicts: strings.Split(conflicts, "\n")}
		}
		return "", err
	}
	return Run(ctx, repo, "rev-parse", "HEAD")
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
