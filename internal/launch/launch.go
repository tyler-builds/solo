// Package launch opens a folder in the user's code editor or a terminal, and files in their apps.
package launch

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/godbus/dbus/v5"
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
	command, err := editorCommand(command)
	if err != nil {
		return err
	}
	return start(command, dir, dir)
}

// EditFile opens the file at path in the editor command, or the first editor found, at line when
// it is above 0 and the editor takes one.
func EditFile(command, path string, line int) error {
	command, err := editorCommand(command)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s isn't there: %w", path, err)
	}
	args := strings.Fields(command)
	if line > 0 && !strings.Contains(command, "{path}") {
		at := fmt.Sprintf("%s:%d", path, line)
		switch editorName(args[0]) {
		case "code", "cursor", "codium":
			return run(append(args, "-g", at), filepath.Dir(path))
		case "zed", "subl":
			return run(append(args, at), filepath.Dir(path))
		case "idea":
			return run(append(args, "--line", fmt.Sprint(line), path), filepath.Dir(path))
		}
	}
	return start(command, path, filepath.Dir(path))
}

// ErrNoEditor is EditFile's and Editor's error when Settings names no editor and none is installed.
var ErrNoEditor = errors.New("no code editor found: set one in Settings › General")

func editorCommand(command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		command = Detect("editor")
		if command == "" {
			return "", ErrNoEditor
		}
	}
	return command, nil
}

// Open opens path, a file or a folder, in the system's default app for it.
func Open(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s isn't there: %w", path, err)
	}
	switch runtime.GOOS {
	case "windows":
		return run([]string{"rundll32", "url.dll,FileProtocolHandler", path}, "")
	case "darwin":
		return run([]string{"open", path}, "")
	}
	return run([]string{"xdg-open", path}, "")
}

// Reveal shows path in the file manager with it selected, or opens the folder that holds it when the
// file manager can't select it.
func Reveal(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s isn't there: %w", path, err)
	}
	switch runtime.GOOS {
	case "windows":
		return run([]string{"explorer", "/select," + path}, "")
	case "darwin":
		return run([]string{"open", "-R", path}, "")
	}
	if conn, err := dbus.SessionBus(); err == nil {
		uri := (&url.URL{Scheme: "file", Path: path}).String()
		fm := conn.Object("org.freedesktop.FileManager1", "/org/freedesktop/FileManager1")
		if fm.Call("org.freedesktop.FileManager1.ShowItems", 0, []string{uri}, "").Err == nil {
			return nil
		}
	}
	return Open(filepath.Dir(path))
}

// Terminal opens a terminal in dir with the terminal command, or the first one found.
func Terminal(command, dir string) error {
	if strings.TrimSpace(command) == "" {
		command = Detect("terminal")
		if command == "" {
			return errors.New("no terminal found: set one in Settings › General")
		}
	}
	return start(command, dir, dir)
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

// start runs command in dir without waiting for it. {path} in the command is replaced by target; an
// editor command without it gets target as its last argument.
func start(command, target, dir string) error {
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("%s isn't there: %w", target, err)
	}
	args := strings.Fields(command)
	if len(args) == 0 {
		return errors.New("empty command")
	}
	hasPath := false
	for i, a := range args {
		if strings.Contains(a, "{path}") {
			args[i], hasPath = strings.ReplaceAll(a, "{path}", target), true
		}
	}
	if !hasPath && isEditor(args[0]) {
		args = append(args, target)
	}
	return run(args, dir)
}

// run starts args in dir (or Solo's own folder when "") without waiting for it.
func run(args []string, dir string) error {
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

func editorName(name string) string {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(name)), ".exe")
	return strings.TrimSuffix(base, ".cmd")
}

func isEditor(name string) bool {
	base := editorName(name)
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
