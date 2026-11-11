package launch

import (
	"debug/pe"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Process creation flags (see CreateProcess).
const (
	createNewConsole      = 0x00000010
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// detach keeps the program apart from Solo. A console program gets a console window of its own when
// console is set (a terminal, the window the user asked for) or when it is a console .exe, such as an
// editor that runs in a terminal; a .cmd or .bat launcher, such as VS Code's code.cmd, gets a console
// without a window, so it doesn't flash one. A GUI program, which has no console, shows its windows
// either way: no HideWindow here.
func detach(cmd *exec.Cmd, console bool) {
	flags := uint32(createNewProcessGroup | createNoWindow)
	if console || consoleExe(cmd.Path) {
		flags = createNewProcessGroup | createNewConsole
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
}

// consoleExe reports whether path is an executable built for the console subsystem.
func consoleExe(path string) bool {
	if !strings.EqualFold(filepath.Ext(path), ".exe") {
		return false
	}
	f, err := pe.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	const consoleSubsystem = 3 // IMAGE_SUBSYSTEM_WINDOWS_CUI
	switch h := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		return h.Subsystem == consoleSubsystem
	case *pe.OptionalHeader32:
		return h.Subsystem == consoleSubsystem
	}
	return false
}
