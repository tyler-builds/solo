//go:build !windows

package claudesdk

import (
	"os/exec"
	"syscall"
)

var lookPath = exec.LookPath

// setProcessGroup starts the CLI in a process group of its own, so it and everything it starts can be
// stopped together.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
