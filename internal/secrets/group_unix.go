//go:build !windows && !js

package secrets

import (
	"os/exec"
	"syscall"
)

// setGroup starts the command in a process group of its own, so that everything it starts can be
// killed with it.
func setGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup kills the command's process group: what it left running in the background still holds
// the secrets in its environment.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
