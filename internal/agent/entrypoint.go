//go:build !js

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// sessionEntrypoint is the CLAUDE_CODE_ENTRYPOINT of Solo's claude processes: Solo's, unless the user's
// or managed Claude settings set one in their env. Claude Code reads its entrypoint from the process's
// environment, not from its settings' env, so Solo applies a setting itself (as t3code's
// mergeClaudeProcessEnvironment does).
func sessionEntrypoint() string {
	if e := settingsEntrypoint(); e != "" {
		return e
	}
	return entrypoint
}

// settingsEntrypoint is the CLAUDE_CODE_ENTRYPOINT that Claude's user and managed settings set in env;
// later files win.
func settingsEntrypoint() string {
	found := ""
	for _, file := range settingsFiles() {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var s struct {
			Env map[string]any `json:"env"`
		}
		if json.Unmarshal(lenientJSON(b), &s) != nil {
			continue
		}
		if v, ok := s.Env["CLAUDE_CODE_ENTRYPOINT"].(string); ok {
			found = v
		}
	}
	return found
}

// settingsFiles are Claude Code's user settings, then its managed settings.
func settingsFiles() []string {
	dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	files := []string{filepath.Join(dir, "settings.json")}
	switch runtime.GOOS {
	case "windows":
		for _, root := range []string{os.Getenv("ProgramData"), os.Getenv("ProgramFiles")} {
			if root != "" {
				files = append(files, filepath.Join(root, "ClaudeCode", "managed-settings.json"))
			}
		}
	case "darwin":
		files = append(files, "/Library/Application Support/ClaudeCode/managed-settings.json")
	default:
		files = append(files, "/etc/claude-code/managed-settings.json")
	}
	return files
}

var (
	jsonComments       = regexp.MustCompile(`("(?:[^"\\]|\\.)*")|//[^\r\n]*|/\*[\s\S]*?\*/`)
	jsonTrailingCommas = regexp.MustCompile(`("(?:[^"\\]|\\.)*")|,(\s*[}\]])`)
)

// lenientJSON strips the comments and trailing commas settings files may hold.
func lenientJSON(b []byte) []byte {
	b = jsonComments.ReplaceAllFunc(b, func(m []byte) []byte {
		if m[0] == '"' {
			return m
		}
		return []byte(" ")
	})
	return jsonTrailingCommas.ReplaceAllFunc(b, func(m []byte) []byte {
		if m[0] == '"' {
			return m
		}
		return m[1:]
	})
}
