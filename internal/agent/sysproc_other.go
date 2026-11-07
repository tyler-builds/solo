//go:build !windows

package agent

import "os/exec"

// setSysProcAttr does nothing here: the sidecar exits when its stdin closes, which happens when this
// process exits for any reason.
func setSysProcAttr(*exec.Cmd) {}
