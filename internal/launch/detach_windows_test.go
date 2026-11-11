package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConsoleExe(t *testing.T) {
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		t.Skip("no SystemRoot")
	}
	for path, want := range map[string]bool{
		filepath.Join(sys, "System32", "cmd.exe"):     true,
		filepath.Join(sys, "System32", "notepad.exe"): false,
		filepath.Join(sys, "explorer.exe"):            false,
		filepath.Join(t.TempDir(), "missing.exe"):     false,
	} {
		if got := consoleExe(path); got != want {
			t.Errorf("consoleExe(%s) = %v, want %v", path, got, want)
		}
	}
}
