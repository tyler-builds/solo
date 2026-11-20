//go:build !js

package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/proc"
)

// Limits of Run.
const (
	DefaultTimeout = 2 * time.Minute
	MaxTimeout     = 10 * time.Minute
	// maxCapture is how much output Run keeps; the rest is counted and dropped.
	maxCapture = 2 << 20
	// scanBudget bounds the look for files a command wrote a secret into.
	scanBudget  = 5 * time.Second
	scanMaxFile = 32 << 20
)

// RunSpec is one command to run with secrets.
type RunSpec struct {
	Command string
	Dir     string
	// Secrets are the values to put in the command's environment, by variable name.
	Secrets map[string]string
	// Redactor takes values out of the output and of files the command wrote: every secret the app
	// keeps, not only the ones the command was given.
	Redactor *Redactor
	Timeout  time.Duration
	// Roots are the folders to look in for files the command wrote a secret into (an agent's worktree),
	// besides Dir and the user's well-known credential files.
	Roots []string
	// Skip are folders under the roots not to look through (too large, and not the agent's).
	Skip []string
	// Shell overrides the shell the command runs in: the program, then its arguments before the
	// command. Tests set it.
	Shell []string
}

// RunResult is what a command run with secrets did, with the secrets taken out.
type RunResult struct {
	Output   string // stdout and stderr together, redacted
	ExitCode int
	TimedOut bool
	Dropped  int  // bytes of output past maxCapture, not kept
	Redacted bool // the output held a secret, now taken out
	// Scrubbed are files the command wrote a secret into, which now hold its Placeholder instead.
	Scrubbed []string
}

// Run runs spec.Command in a shell of its own, with the secrets in its environment and nowhere else:
// not in Solo's environment or any agent's, and not on its command line. Its output comes back
// redacted. It gets a temporary folder of its own (TMPDIR, TMP, TEMP), removed after, and once it
// ends, everything left in its process group is killed (on POSIX) and the files it wrote that hold a
// secret are scrubbed.
func Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	var res RunResult
	if spec.Timeout <= 0 {
		spec.Timeout = DefaultTimeout
	}
	spec.Timeout = min(spec.Timeout, MaxTimeout)
	shell := spec.Shell
	if len(shell) == 0 {
		shell = defaultShell()
	}
	tmp, err := os.MkdirTemp("", "solo-run-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	start := time.Now()

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	cmd := proc.CommandContext(runCtx, shell[0], append(shell[1:], spec.Command)...)
	cmd.Dir = spec.Dir
	cmd.Env = environ(spec.Secrets, tmp)
	cmd.Stdin = nil // /dev/null
	out := &capture{}
	cmd.Stdout, cmd.Stderr = out, out
	setGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second // a child that keeps the pipes open doesn't hold Run
	err = cmd.Run()
	_ = killGroup(cmd) // background children it left would hold the secrets
	res.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
	case errors.Is(err, exec.ErrWaitDelay):
	case res.TimedOut || ctx.Err() != nil:
		res.ExitCode = -1
	default:
		return res, fmt.Errorf("running the command: %w", err)
	}

	raw, dropped := out.result()
	res.Dropped = dropped
	res.Output = spec.Redactor.Redact(raw)
	res.Redacted = res.Output != raw
	roots := append([]string{spec.Dir}, spec.Roots...)
	res.Scrubbed = scrub(roots, spec.Skip, start, spec.Redactor)
	return res, nil
}

// environ is Solo's environment with the secrets, and a temporary folder of the command's own.
func environ(secrets map[string]string, tmp string) []string {
	drop := map[string]bool{"TMPDIR": true, "TMP": true, "TEMP": true}
	for k := range secrets {
		drop[envKey(k)] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[envKey(k)] {
			env = append(env, kv)
		}
	}
	env = append(env, "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
	for k, v := range secrets {
		env = append(env, k+"="+v)
	}
	return env
}

func envKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

// defaultShell is bash where there is one (Git Bash on Windows, which Claude Code's own shell is), as an
// agent's Bash tool runs it; else sh, or cmd on Windows.
func defaultShell() []string {
	if p, err := exec.LookPath("bash"); err == nil {
		return []string{p, "-c"}
	}
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/C"}
	}
	return []string{"/bin/sh", "-c"}
}

// capture keeps the first maxCapture bytes written to it and counts the rest.
type capture struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	dropped int
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := maxCapture - c.buf.Len()
	if room >= len(p) {
		c.buf.Write(p)
	} else {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		c.dropped += len(p) - max(room, 0)
	}
	return len(p), nil
}

func (c *capture) result() (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String(), c.dropped
}

// credentialFiles are where common command-line tools keep a login, under the home folder: a command
// that logs one in with a secret leaves the secret there.
var credentialFiles = []string{
	".npmrc", ".yarnrc.yml", ".netrc", "_netrc", ".git-credentials", ".config/git/credentials",
	".docker/config.json", ".config/gh/hosts.yml", ".config/glab-cli/config.yml", ".pypirc",
	".aws/credentials", ".kube/config", ".cargo/credentials", ".cargo/credentials.toml",
	".gem/credentials", ".wrangler/config/default.toml", ".config/.wrangler/config/default.toml",
	".config/hub", ".terraform.d/credentials.tfrc.json", ".config/configstore/firebase-tools.json",
}

// scrub replaces each secret in the files under roots (but not under skip) changed since start, and in the user's
// credential files and the roots' git config changed since then, with its Placeholder. It returns the
// files it changed. It looks for at most scanBudget, skipping git's object store and files over
// scanMaxFile.
func scrub(roots, skip []string, start time.Time, r *Redactor) []string {
	if r.Empty() {
		return nil
	}
	since := start.Add(-2 * time.Second) // file systems keep coarse times
	deadline := time.Now().Add(scanBudget)
	seen := map[string]bool{}
	var changed []string
	check := func(path string, info fs.FileInfo) {
		if seen[path] || !info.Mode().IsRegular() || info.ModTime().Before(since) || info.Size() > scanMaxFile {
			return
		}
		seen[path] = true
		data, err := os.ReadFile(path)
		if err != nil || !r.Contains(string(data)) {
			return
		}
		if os.WriteFile(path, []byte(r.RedactFull(string(data))), info.Mode().Perm()) == nil {
			changed = append(changed, path)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, f := range credentialFiles {
			p := filepath.Join(home, filepath.FromSlash(f))
			if info, err := os.Stat(p); err == nil {
				check(p, info)
			}
		}
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		if cfg := gitConfig(root); cfg != "" {
			if info, err := os.Stat(cfg); err == nil {
				check(cfg, info)
			}
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if time.Now().After(deadline) {
				return filepath.SkipAll
			}
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if slices.Contains(skip, path) {
					return filepath.SkipDir
				}
				if d.Name() == "objects" && filepath.Base(filepath.Dir(path)) == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if info, err := d.Info(); err == nil {
				check(path, info)
			}
			return nil
		})
	}
	return changed
}

// gitConfig is the config file of the repository root is in (for a worktree, the main repository's,
// which `git remote set-url` writes), or "".
func gitConfig(root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := proc.CommandContext(ctx, "git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	return filepath.Join(strings.TrimSpace(string(out)), "config")
}
