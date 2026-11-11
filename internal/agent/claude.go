package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/proc"
)

// Claude is where the claude executable was found.
type Claude struct {
	Path   string // "" when none was found
	Source string // "setting", "PATH", "default location" or "bundled"
}

// FindClaude finds the claude executable: the override from Settings, else `claude` on PATH, else a
// usual install location (apps started from a desktop launcher often have a shorter PATH), else one
// shipped next to Solo. Path is "" when there is none.
func FindClaude(override string) Claude {
	if override = strings.TrimSpace(override); override != "" {
		return Claude{Path: expandHome(override), Source: "setting"}
	}
	if p, err := exec.LookPath(claudeName()); err == nil {
		if p = sdkEntry(p); p != "" {
			return Claude{Path: p, Source: "PATH"}
		}
	}
	for _, p := range claudeLocations() {
		if isFile(p) {
			return Claude{Path: p, Source: "default location"}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), claudeName()); isFile(p) {
			return Claude{Path: p, Source: "bundled"}
		}
	}
	return Claude{}
}

func claudeName() string {
	if runtime.GOOS == "windows" {
		return "claude.exe"
	}
	return "claude"
}

// claudeLocations are where the Claude Code installers put claude.
func claudeLocations() []string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		return []string{filepath.Join(home, ".local", "bin", "claude.exe")}
	}
	return []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
		"/usr/bin/claude",
	}
}

// sdkEntry returns what the SDK can spawn for a claude found on PATH. On Windows, npm installs a .cmd
// launcher the SDK can't run, so it uses the package's cli.js beside it (as t3code does).
func sdkEntry(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".cmd", ".bat", ".ps1":
		cli := filepath.Join(filepath.Dir(p), "node_modules", "@anthropic-ai", "claude-code", "cli.js")
		if isFile(cli) {
			return cli
		}
		return ""
	}
	return p
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

var versionRE = regexp.MustCompile(`\d+\.\d+\.\d+[\w.-]*`)

// ClaudeVersion runs `claude --version` and returns the version it prints.
func ClaudeVersion(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", errors.New("Claude Code not found")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	name, args := path, []string{"--version"}
	if strings.HasSuffix(path, ".js") {
		name, args = "node", []string{path, "--version"}
	}
	cmd := proc.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s doesn't exist", path)
		}
		if ctx.Err() != nil {
			return "", errors.New("claude --version timed out")
		}
		return "", fmt.Errorf("claude --version failed: %v", err)
	}
	v := versionRE.FindString(string(out))
	if v == "" {
		return "", fmt.Errorf("%s doesn't look like Claude Code", path)
	}
	return v, nil
}
