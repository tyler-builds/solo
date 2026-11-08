// Package launch opens a folder in the user's code editor or a terminal.
package launch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// editors are tried in order when Settings names none: the command, and how it opens a folder.
var editors = []string{"code", "cursor", "zed", "codium", "subl", "idea", "gnome-text-editor", "kate"}

// terminals are tried in order when Settings names none, each with how it starts in a folder
// ({path} is replaced; without it the terminal starts in the folder as its working directory).
var terminals = map[string][]string{
	"linux": {
		"ptyxis --new-window --working-directory={path}",
		"kgx --working-directory={path}",
		"gnome-terminal --working-directory={path}",
		"konsole --workdir {path}",
		"kitty --directory {path}",
		"wezterm start --cwd {path}",
		"alacritty --working-directory {path}",
		"foot --working-directory={path}",
		"ghostty --working-directory={path}",
		"xfce4-terminal --working-directory={path}",
		"tilix --working-directory={path}",
		"x-terminal-emulator",
		"xterm",
	},
	"windows": {
		"wt.exe -d {path}",
		"pwsh.exe -NoExit",
		"powershell.exe -NoExit",
		"cmd.exe /K",
	},
	"darwin": {
		"open -a Terminal {path}",
	},
}

// Editor opens dir in the editor command (such as "code" or "zed {path}"), or in the first editor
// found when command is empty.
func Editor(command, dir string) error {
	if strings.TrimSpace(command) == "" {
		command = Detect("editor")
		if command == "" {
			return errors.New("no code editor found: set one in Settings › General")
		}
	}
	return start(command, dir)
}

// Terminal opens a terminal in dir with the terminal command, or the first one found.
func Terminal(command, dir string) error {
	if strings.TrimSpace(command) == "" {
		command = Detect("terminal")
		if command == "" {
			return errors.New("no terminal found: set one in Settings › General")
		}
	}
	return start(command, dir)
}

// Detect returns the editor or terminal command Solo would use: the first one installed.
func Detect(kind string) string {
	if kind == "editor" {
		for _, e := range editors {
			if _, err := exec.LookPath(e); err == nil {
				return e
			}
		}
		return ""
	}
	for _, t := range terminals[runtime.GOOS] {
		name := strings.Fields(t)[0]
		if _, err := exec.LookPath(name); err == nil {
			return t
		}
	}
	return ""
}

// start runs command in dir without waiting for it. {path} in the command is replaced by dir; an
// editor command without it gets dir as its last argument.
func start(command, dir string) error {
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("%s isn't there: %w", dir, err)
	}
	args := strings.Fields(command)
	if len(args) == 0 {
		return errors.New("empty command")
	}
	hasPath := false
	for i, a := range args {
		if strings.Contains(a, "{path}") {
			args[i], hasPath = strings.ReplaceAll(a, "{path}", dir), true
		}
	}
	if !hasPath && isEditor(args[0]) {
		args = append(args, dir)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", args[0], err)
	}
	go cmd.Wait() // reap it; it outlives nothing of ours
	return nil
}

func isEditor(name string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(name)), ".exe")
	base = strings.TrimSuffix(base, ".cmd")
	for _, e := range editors {
		if base == e {
			return true
		}
	}
	return false
}

// cleanEnv is Solo's environment without what an AppImage sets for itself, which would leak into the
// programs it starts.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "APPIMAGE", "APPDIR", "ARGV0", "OWD":
			continue
		}
		out = append(out, kv)
	}
	return out
}
