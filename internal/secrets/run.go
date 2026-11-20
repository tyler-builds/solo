//go:build !js

package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
	// Own is the agent's own folder (a thread's worktree): files the command writes a secret into there
	// are scrubbed. No other folder is looked through; the user's credential files and the git config
	// of Dir and Own are put back as they were instead (see guardFiles).
	Own string
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
	// Scrubbed are files in Own the command wrote a secret into, which now hold its Placeholder (or,
	// in a binary file, as many '*') instead.
	Scrubbed []string
	// Restored are credential and git config files the command wrote a secret into, put back as they
	// were before it ran (or removed, when it made them).
	Restored []string
	// Unchecked: Own was too large to look through in time, before or after the command, so files there
	// may hold a secret it wrote.
	Unchecked bool
}

// Run runs spec.Command in a shell of its own, with the secrets in its environment and nowhere else:
// not in Solo's environment or any agent's, and not on its command line. Its output comes back
// redacted. It gets a temporary folder of its own (TMPDIR, TMP, TEMP), removed after; while it runs, its
// process group is in Running (for Guard); once it ends, everything left in that group is killed (on
// POSIX), and files it wrote a secret into are scrubbed or put back (guardFiles).
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
	files := guardFiles(spec.Redactor, spec.Own, []string{spec.Dir, spec.Own})

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
	if err = cmd.Start(); err == nil {
		running.add(cmd.Process.Pid)
		err = cmd.Wait()
		_ = killGroup(cmd) // background children it left would hold the secrets
		running.remove(cmd.Process.Pid)
	}
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
	res.Scrubbed, res.Restored, res.Unchecked = files.settle()
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
