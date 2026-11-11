// Package proc starts Solo's background programs (git, gh, glab, claude, the agent sidecar) without
// console windows.
//
// Solo is a GUI program on Windows, so it has no console. Each console program it starts with plain
// os/exec gets a console window of its own, which flashes on screen while the program runs. Command
// and CommandContext start it with a console but no window, which the programs it starts in turn
// share. Elsewhere they are exec.Command and exec.CommandContext.
//
// It mirrors github.com/tyler-builds/jamb/proc, which Solo can use instead once it requires a jamb
// release that has it. A program the user should see, such as the terminal Settings names, is
// started by package launch with flags of its own.
package proc

import (
	"context"
	"os/exec"
)

// Command is exec.Command for a background console program: on Windows the program starts without
// a console window.
func Command(name string, arg ...string) *exec.Cmd {
	cmd := exec.Command(name, arg...)
	Hide(cmd)
	return cmd
}

// CommandContext is exec.CommandContext for a background console program: on Windows the program
// starts without a console window.
func CommandContext(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, arg...)
	Hide(cmd)
	return cmd
}

// Hide makes cmd start without a console window on Windows, and asks the program to keep its first
// window hidden. It keeps cmd's other SysProcAttr settings, but drops flags that would give the
// program a visible console or none at all. It does nothing elsewhere. Call it before cmd.Start.
func Hide(cmd *exec.Cmd) { hide(cmd) }
