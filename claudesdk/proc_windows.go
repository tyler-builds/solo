package claudesdk

import (
	"os/exec"
	"syscall"
)

var lookPath = exec.LookPath

const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000 // a console without a window, so a GUI host flashes none
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewProcessGroup | createNoWindow}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
