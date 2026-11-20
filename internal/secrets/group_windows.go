package secrets

import "os/exec"

// setGroup does nothing on Windows: the command's children aren't tracked (see killGroup).
func setGroup(*exec.Cmd) {}

// killGroup kills the command itself. Programs it started that outlive it aren't found on Windows.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
