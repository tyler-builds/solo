package launch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/proc"
)

// psChecks holds what powerShellBlocked found for each PowerShell, by lowercased file name.
var psChecks sync.Map // string → string

// powerShellBlocked returns why the PowerShell called name ("powershell.exe", "pwsh") can't be used
// to open a terminal, or "" when it can. It checks once per name and remembers the answer: first the
// registry policies that disallow programs, then that it is installed, then it runs it once, hidden,
// to see that it starts and exits cleanly. That run is what catches AppLocker, Software Restriction
// Policies and App Control, which refuse to start it ("This program is blocked by group policy").
func powerShellBlocked(name string) string {
	exe := strings.ToLower(filepath.Base(name))
	if !strings.HasSuffix(exe, ".exe") {
		exe += ".exe"
	}
	if why, ok := psChecks.Load(exe); ok {
		return why.(string)
	}
	why := checkPowerShell(name, exe)
	psChecks.Store(exe, why)
	return why
}

func checkPowerShell(name, exe string) string {
	if policyBlocks(exe) {
		return exe + " is blocked by group policy"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return exe + " isn't installed"
	}
	return probePowerShell(path)
}

// probeTimeout bounds the hidden run; a first run after boot can take a few seconds.
const probeTimeout = 15 * time.Second

// probePowerShell runs PowerShell at path with a command that does nothing, and returns why it
// failed, or "".
var probePowerShell = func(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := proc.CommandContext(ctx, path, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "exit 0")
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return probeFailure(filepath.Base(path), err, string(out), ctx.Err())
}

// probeFailure says why a PowerShell probe that ended with err, printing out, failed, or "" when it
// didn't.
func probeFailure(exe string, err error, out string, ctxErr error) string {
	text := strings.ToLower(out)
	policy := strings.Contains(text, "group policy") || strings.Contains(text, "blocked") || strings.Contains(text, "administrator has") || strings.Contains(text, "disabled")
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return fmt.Sprintf("%s didn't answer within %v", exe, probeTimeout)
	case err != nil && policy:
		return fmt.Sprintf("%s is blocked: %s", exe, firstLine(out))
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if line := firstLine(out); line != "" {
				return fmt.Sprintf("%s failed: %s", exe, line)
			}
			return fmt.Sprintf("%s failed: %v", exe, err)
		}
		return fmt.Sprintf("%s can't start: %v", exe, err) // such as ERROR_ACCESS_DISABLED_BY_POLICY
	case policy:
		return fmt.Sprintf("%s is blocked: %s", exe, firstLine(out))
	}
	return ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
