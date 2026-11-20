package secrets

import "regexp"

// readers match commands that read a secret store or another process's environment directly. An agent
// runs as the user, so nothing stops it reading the keyring, or a running command's environment, with
// a program of its own; this only keeps the obvious ways out of commands Solo runs with secrets (and,
// in sidecar/src/guard.ts, which mirrors this list, out of agents' own shell commands).
var readers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsecret-tool\b`),
	regexp.MustCompile(`(?i)org\.freedesktop\.secrets`),
	regexp.MustCompile(`(?i)\bgnome-keyring\b`),
	regexp.MustCompile(`(?i)\bkwallet`),
	regexp.MustCompile(`(?i)\bsolo-secrets\b`),
	regexp.MustCompile(`(?i)\bkeyring\s+get\b`),
	regexp.MustCompile(`(?i)\bkeyring\.get_password\b`),
	regexp.MustCompile(`(?i)\bfind-(generic|internet)-password\b`),
	regexp.MustCompile(`(?i)\bcmdkey\b`),
	regexp.MustCompile(`(?i)\bCredRead`),
	regexp.MustCompile(`(?i)\bGet-StoredCredential\b`),
	regexp.MustCompile(`(?i)\bPasswordVault\b`),
	regexp.MustCompile(`(?i)/proc/[^\s/]+/environ\b`),
	regexp.MustCompile(`(?i)\benviron\b.*/proc|/proc\b.*\benviron\b`),
}

// Forbidden says why command may not run with secrets, or "" when it may: it reads a secret store or
// another process's environment.
func Forbidden(command string) string {
	for _, re := range readers {
		if re.MatchString(command) {
			return "Solo doesn't run commands that read secret stores or other processes' environments " +
				"(matched " + re.FindString(command) + "). Use run_with_secrets with the secret's name: the app puts the value in " +
				"that command's environment for you."
		}
	}
	return ""
}
