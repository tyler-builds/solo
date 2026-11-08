package launch

import (
	"os"
	"path/filepath"
	"runtime"
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
