//go:build !js

package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
)

// projectFolders are the folders under the home folder where people commonly keep their projects,
// which the first-run setup offers when they hold repositories.
var projectFolders = []string{"code", "Code", "src", "Projects", "projects", "dev", "Developer", "repos",
	"git", "workspace", "source/repos"}

// SuggestFolders finds the folders where projects are commonly kept (~/code, ~/src, ~/Projects, ~/dev,
// …) that exist and hold git repositories, for the first-run setup to offer. Folders already searched,
// and folders inside them, are left out. It only reads folders; nothing is added.
func (c *Core) SuggestFolders(ctx context.Context) []FolderSuggestion {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	s := c.Settings()
	var searched []string
	for _, f := range s.Folders {
		searched = append(searched, filepath.Clean(git.ExpandHome(f)))
	}
	var out []FolderSuggestion
	var seen []os.FileInfo // "code" and "Code" are one folder where names ignore case
	for _, name := range projectFolders {
		if ctx.Err() != nil {
			break
		}
		dir := filepath.Join(home, filepath.FromSlash(name))
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() || slices.ContainsFunc(seen, func(o os.FileInfo) bool { return os.SameFile(o, st) }) {
			continue
		}
		seen = append(seen, st)
		if slices.ContainsFunc(searched, func(f string) bool { return within(dir, f) }) {
			continue
		}
		if repos, _ := git.ScanAll([]string{dir}, s.ScanDepth, c.worktreesDir()); len(repos) > 0 {
			out = append(out, FolderSuggestion{Path: dir, Repos: repos})
		}
	}
	return out
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// CheckForges finds the forges the known repositories push to (GitHub, GitLab) and checks, for each,
// that its command-line tool is installed and signed in, with what to run when it isn't. Repositories
// without such a remote are left out: their work merges locally.
func (c *Core) CheckForges(ctx context.Context) []ForgeStatus {
	byKind := map[forge.Kind][]string{}
	for _, r := range c.RepoPaths() {
		if k := c.forgeFor(r); k != forge.None {
			byKind[k] = append(byKind[k], r)
		}
	}
	var out []ForgeStatus
	for _, k := range []forge.Kind{forge.GitHub, forge.GitLab} {
		repos := byKind[k]
		if len(repos) == 0 {
			continue
		}
		st := ForgeStatus{Kind: k, CLI: forge.CLIName(k), Repos: repos, Login: forge.CLIName(k) + " auth login"}
		st.Install, st.InstallURL = forge.InstallHint(k)
		path, account, err := forge.AuthStatus(ctx, k)
		st.Path = path
		switch {
		case err == nil:
			st.SignedIn, st.Account = true, account
		case errors.Is(err, forge.ErrNoCLI), errors.Is(err, forge.ErrSignedOut):
		default:
			st.Err = err.Error()
		}
		out = append(out, st)
	}
	return out
}
