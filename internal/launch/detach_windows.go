package launch

import (
	"os/exec"
	"syscall"
)

// detach gives a console program its own window (a terminal) and keeps it apart from Solo.
func detach(cmd *exec.Cmd) {
	const createNewConsole, createNewProcessGroup = 0x00000010, 0x00000200
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole | createNewProcessGroup}
}
