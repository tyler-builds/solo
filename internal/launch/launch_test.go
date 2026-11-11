package launch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStartReplacesPathAndAppendsForEditors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell scripts")
	}
	bin, dir := t.TempDir(), t.TempDir()
	out := filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\necho \"$PWD|$@\" > " + out + "\n"
	for _, name := range []string{"code", "myterm"} {
		os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	wait := func() string {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
				os.Remove(out)
				return strings.TrimSpace(string(b))
			}
		}
		t.Fatal("the program didn't run")
		return ""
	}
	real, _ := filepath.EvalSymlinks(dir)

	if err := Editor("", dir); err != nil { // detected: code, given the folder
		t.Fatal(err)
	}
	if got := wait(); !strings.HasSuffix(got, "|"+dir) {
		t.Errorf("editor got %q", got)
	}
	if err := Terminal("myterm --cwd={path} -x", dir); err != nil {
		t.Fatal(err)
	}
	if got := wait(); got != real+"|--cwd="+dir+" -x" && got != dir+"|--cwd="+dir+" -x" {
		t.Errorf("terminal got %q", got)
	}
	if err := Terminal("myterm", filepath.Join(dir, "missing")); err == nil {
		t.Error("opened a folder that isn't there")
	}
}

func TestTerminalsToTryFallsBackFromBlockedPowerShell(t *testing.T) {
	all := func(string) bool { return true }
	only := func(names ...string) func(string) bool {
		return func(n string) bool { return slices.Contains(names, n) }
	}
	none := func(string) string { return "" }
	psBlocked := func(n string) string {
		if isPowerShell(n) {
			return n + " is blocked by group policy"
		}
		return ""
	}
	cases := []struct {
		name, command string
		installed     func(string) bool
		blocked       func(string) string
		want          []string
	}{
		{"windows, all usable", "", all, none, []string{"wt.exe -d {path}", "pwsh.exe -NoExit", "powershell.exe -NoExit", "cmd.exe /K"}},
		{"windows, PowerShell blocked", "", all, psBlocked, []string{"wt.exe -d {path} cmd.exe", "cmd.exe /K"}},
		{"windows, no wt, PowerShell blocked", "", only("powershell.exe", "cmd.exe"), psBlocked, []string{"cmd.exe /K"}},
		{"windows, set PowerShell blocked", "powershell.exe -NoExit -Command x", only("powershell.exe", "cmd.exe"), psBlocked, []string{"cmd.exe /K"}},
		{"windows, set PowerShell usable", "pwsh -NoExit", only("cmd.exe"), none, []string{"pwsh -NoExit", "cmd.exe /K"}},
		{"windows, set other", "alacritty.exe", all, psBlocked, []string{"alacritty.exe"}},
		{"linux, first installed", "", only("kitty", "xterm"), psBlocked, []string{"kitty --directory {path}"}},
		{"linux, set", "foo {path}", all, none, []string{"foo {path}"}},
		{"linux, none", "", only(), none, nil},
	}
	for _, c := range cases {
		goos := "windows"
		if strings.HasPrefix(c.name, "linux") {
			goos = "linux"
		}
		if got := terminalsToTry(goos, c.command, c.installed, c.blocked); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIsPowerShell(t *testing.T) {
	for name, want := range map[string]bool{
		"powershell.exe": true, "PowerShell": true, "pwsh": true, `C:\Program Files\PowerShell\7\pwsh.exe`: true,
		"cmd.exe": false, "wt.exe": false, "powershell_ise.exe": false,
	} {
		if got := isPowerShell(name); got != want {
			t.Errorf("isPowerShell(%q) = %v", name, got)
		}
	}
}

func TestProbeFailure(t *testing.T) {
	exitErr := exec.Command("sh", "-c", "exit 1").Run()
	if exitErr == nil {
		t.Skip("needs sh")
	}
	cases := []struct {
		err    error
		out    string
		ctxErr error
		want   string // a substring; "" means usable
	}{
		{nil, "", nil, ""},
		{exitErr, "Program 'powershell.exe' failed to run: This program is blocked by group policy.", nil, "is blocked: Program"},
		{exitErr, "", nil, "failed: exit status 1"},
		{errors.New("This program is blocked by group policy. For more information, contact your system administrator."), "", nil, "can't start"},
		{nil, "", context.DeadlineExceeded, "didn't answer"},
		{nil, "PowerShell has been disabled by your administrator", nil, "is blocked"},
	}
	for _, c := range cases {
		got := probeFailure("powershell.exe", c.err, c.out, c.ctxErr)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("probeFailure(%v, %q) = %q, want %q", c.err, c.out, got, c.want)
		}
	}
}

func TestPowerShellBlockedChecksOnce(t *testing.T) {
	runs := 0
	old := probePowerShell
	probePowerShell = func(string) string { runs++; return "blocked" }
	t.Cleanup(func() { probePowerShell = old; psChecks.Clear() })
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "pwsh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "pwsh.exe"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)
	for range 3 {
		if got := powerShellBlocked("pwsh"); got != "blocked" {
			t.Fatalf("got %q", got)
		}
	}
	if runs != 1 {
		t.Errorf("probed %d times, want 1", runs)
	}
	if got := powerShellBlocked("powershell.exe"); got != "powershell.exe isn't installed" {
		t.Errorf("missing PowerShell: got %q", got)
	}
}
