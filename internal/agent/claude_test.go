package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFindClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as claude")
	}
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)

	if got := FindClaude("  ~/tools/claude "); got.Path != filepath.Join(home, "tools", "claude") || got.Source != "setting" {
		t.Errorf("override: %+v", got)
	}
	if got := FindClaude(""); got.Path != "" && got.Source != "bundled" && got.Source != "default location" {
		t.Errorf("nothing installed: %+v", got)
	}

	local := filepath.Join(home, ".local", "bin", "claude")
	writeScript(t, local, "echo '2.1.300 (Claude Code)'")
	if got := FindClaude(""); got.Path != local || got.Source != "default location" {
		t.Errorf("default location: %+v", got)
	}

	onPath := filepath.Join(bin, "claude")
	writeScript(t, onPath, "echo '2.1.319 (Claude Code)'")
	got := FindClaude("")
	if got.Path != onPath || got.Source != "PATH" {
		t.Fatalf("PATH: %+v", got)
	}
	if v, err := ClaudeVersion(context.Background(), got.Path); err != nil || v != "2.1.319" {
		t.Errorf("version = %q, %v", v, err)
	}
	if _, err := ClaudeVersion(context.Background(), filepath.Join(home, "missing")); err == nil {
		t.Error("missing claude: no error")
	}
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
