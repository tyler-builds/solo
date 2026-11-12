package gui

import (
	"os"
	"os/exec"
	"testing"
)

// The UI builds for a web page (GOOS=js), which is how Solo is used from a browser. There, the
// packages it shares with the desktop app hold only their types: the files that run git, Claude,
// the database or the user's apps are marked !js, and SQLite does not build at all. Code that the
// UI reaches must stay in the files that build everywhere (core/view.go and items.go, forge/pr.go,
// git/change.go, store/thread.go, agent/protocol.go, launch/launch_js.go); this test fails when it
// does not, with the compiler's own account of what is missing.
func TestBuildsForWeb(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the package for another platform")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool to build with")
	}
	cmd := exec.Command(goTool, "build", ".")
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("internal/gui does not build for the web (GOOS=js GOARCH=wasm go build ./internal/gui): %v\n%s", err, out)
	}
}
