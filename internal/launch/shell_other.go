//go:build !windows && !js

package launch

import "errors"

// shellOpen is Windows only; Open uses open or xdg-open elsewhere.
func shellOpen(string) error { return errors.New("not on Windows") }

// policyBlocks is false: only Windows has the policies it reads.
func policyBlocks(string) bool { return false }
