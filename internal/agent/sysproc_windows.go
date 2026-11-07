package agent

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr keeps the sidecar from opening a console window.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
