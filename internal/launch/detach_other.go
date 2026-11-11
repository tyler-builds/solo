//go:build !windows

package launch

import (
	"os/exec"
	"syscall"
)

// detach starts the program in its own session, so it outlives Solo and doesn't get its signals.
// Only Windows gives a program a console window, so console changes nothing here.
func detach(cmd *exec.Cmd, console bool) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
