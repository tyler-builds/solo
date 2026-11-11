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
	"slices"
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
	return start(command, dir, dir, false)
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
			return run(append(args, "-g", at), filepath.Dir(path), false)
		case "zed", "subl":
			return run(append(args, at), filepath.Dir(path), false)
		case "idea":
			return run(append(args, "--line", fmt.Sprint(line), path), filepath.Dir(path), false)
		}
	}
	return start(command, path, filepath.Dir(path), false)
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
		// The shell opens it in this process: no program to start.
		if err := shellOpen(path); err == nil {
			return nil
		}
		return run([]string{"rundll32", "url.dll,FileProtocolHandler", path}, "", false)
	case "darwin":
		return run([]string{"open", path}, "", false)
	}
	return run([]string{"xdg-open", path}, "", false)
}

// Reveal shows path in the file manager with it selected, or opens the folder that holds it when the
// file manager can't select it.
func Reveal(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s isn't there: %w", path, err)
	}
	switch runtime.GOOS {
	case "windows":
		return run([]string{"explorer", "/select," + path}, "", false)
	case "darwin":
		return run([]string{"open", "-R", path}, "", false)
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

// Terminal opens a terminal in dir with the terminal command, or the first one found. On Windows it
// can take a moment: the first time it would use PowerShell, it checks that PowerShell may run (see
// powerShellBlocked), so call it off the UI thread.
//
// On Windows, when the command is PowerShell (or empty) and PowerShell is blocked or fails to start,
// it falls back to the next terminal that starts: Windows Terminal with cmd.exe, then cmd.exe.
func Terminal(command, dir string) error {
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("%s isn't there: %w", dir, err)
	}
	try := terminalsToTry(runtime.GOOS, command, installed, powerShellBlocked)
	if len(try) == 0 {
		msg := "no terminal found: set one in Settings › General"
		if runtime.GOOS == "windows" {
			if why := powerShellBlocked("powershell.exe"); why != "" {
				msg = "no terminal found (" + why + "): set one in Settings › General"
			}
		}
		return errors.New(msg)
	}
	var errs []string
	for _, t := range try {
		err := start(t, dir, dir, true)
		if err == nil {
			return nil
		}
		errs = append(errs, err.Error())
	}
	return errors.New(strings.Join(errs, "; "))
}

// terminalsToTry is the terminal commands Terminal tries in order on goos: the command when it is
// set, else the ones installed (only the first, except on Windows). On Windows a PowerShell that
// blocked reports a reason for (by name, such as "pwsh.exe") is left out, a PowerShell command that
// is set falls back to the others when it is blocked or fails to start, and Windows Terminal opens
// cmd.exe instead of its default profile, which is usually PowerShell, when PowerShell is blocked.
func terminalsToTry(goos, command string, installed func(string) bool, blocked func(string) string) []string {
	var out []string
	if f := strings.Fields(command); len(f) > 0 {
		if goos != "windows" || !isPowerShell(f[0]) {
			return []string{command}
		}
		if blocked(f[0]) == "" {
			out = append(out, command)
		}
	}
	for _, t := range terminals[goos] {
		name := strings.Fields(t)[0]
		if !installed(name) {
			continue
		}
		if goos != "windows" {
			if len(out) == 0 {
				out = append(out, t)
			}
			break
		}
		switch {
		case isPowerShell(name):
			if blocked(name) != "" {
				continue
			}
		case name == "wt.exe":
			if blocked("powershell.exe") != "" {
				t = "wt.exe -d {path} cmd.exe"
			}
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// isPowerShell reports whether the program name is Windows PowerShell or PowerShell 7.
func isPowerShell(name string) bool {
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(name, `\`, "/"))), ".exe") {
	case "powershell", "pwsh":
		return true
	}
	return false
}

func installed(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// Detect returns the editor or terminal command Solo would use: the first one installed. It runs
// nothing: a PowerShell that Windows policy disallows is skipped, but one blocked another way is
// found only when Terminal opens it.
func Detect(kind string) string {
	if kind == "editor" {
		for _, e := range editors {
			if installed(e) {
				return e
			}
		}
		return ""
	}
	try := terminalsToTry(runtime.GOOS, "", installed, func(name string) string {
		if policyBlocks(filepath.Base(name)) {
			return "blocked by policy"
		}
		return ""
	})
	if len(try) == 0 {
		return ""
	}
	return try[0]
}

// start runs command in dir without waiting for it. {path} in the command is replaced by target; an
// editor command without it gets target as its last argument. A console program gets a console
// window when console is set (a terminal), and none otherwise.
func start(command, target, dir string, console bool) error {
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
	return run(args, dir, console)
}

// run starts args in dir (or Solo's own folder when "") without waiting for it. On Windows a console
// program gets a console window only when console is set: an editor's .cmd launcher would otherwise
// flash one. A GUI program shows its windows either way.
func run(args []string, dir string, console bool) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	detach(cmd, console)
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
