//go:build !js

package forge

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"runtime"
	"time"
)

// CLIName is the command-line tool that drives a forge: gh for GitHub, glab for GitLab.
func CLIName(k Kind) string { return map[Kind]string{GitHub: "gh", GitLab: "glab"}[k] }

// accountRE finds who a CLI is signed in as in its `auth status`: gh's "Logged in to github.com
// account octocat (keyring)", or glab's and older gh's "Logged in to gitlab.com as octocat".
var accountRE = regexp.MustCompile(`Logged in to (\S+) (?:account|as) (\S+)`)

// ErrSignedOut is returned when a forge's CLI is installed but not signed in.
var ErrSignedOut = errors.New("not signed in")

// AuthStatus finds the forge's CLI and asks it who is signed in. It fails with ErrNoCLI when the CLI
// isn't installed and ErrSignedOut when it isn't signed in; account names the user and host when the
// CLI says them, such as "octocat on github.com".
func AuthStatus(ctx context.Context, k Kind) (path, account string, err error) {
	name := CLIName(k)
	if name == "" {
		return "", "", errors.New("not a forge Solo opens pull requests on")
	}
	path = FindTool(name)
	if path == "" {
		return "", "", ErrNoCLI
	}
	c := &Client{Kind: k, Path: path}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, runErr := c.run(ctx, "", "auth", "status")
	switch {
	case ctx.Err() != nil:
		return path, "", errors.New(name + " auth status timed out")
	case runErr != nil:
		// Signed out, or signed in with a token that no longer works: either way, auth login fixes it.
		return path, "", ErrSignedOut
	}
	if m := accountRE.FindStringSubmatch(out); m != nil {
		return path, m[2] + " on " + m[1], nil
	}
	return path, "", nil // signed in, as someone it didn't name
}

// InstallHint is how to install a forge's CLI here: a command when one package manager is the
// obvious way (winget, Homebrew, dnf), else "", and the install page.
func InstallHint(k Kind) (command, url string) {
	name := CLIName(k)
	url = map[Kind]string{GitHub: "https://cli.github.com", GitLab: "https://gitlab.com/gitlab-org/cli#installation"}[k]
	has := func(tool string) bool { _, err := exec.LookPath(tool); return err == nil }
	switch {
	case runtime.GOOS == "windows":
		return "winget install --id " + map[Kind]string{GitHub: "GitHub.cli", GitLab: "glab.glab"}[k], url
	case runtime.GOOS == "darwin" || has("brew"):
		return "brew install " + name, url
	case has("dnf"):
		return "sudo dnf install " + name, url
	}
	return "", url
}
