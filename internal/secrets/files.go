//go:build !js

package secrets

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tyler-builds/solo/internal/proc"
)

// credentialFiles are where common command-line tools keep a login, under the home folder: a command
// that logs one in with a secret leaves the secret there.
var credentialFiles = []string{
	".npmrc", ".yarnrc.yml", ".netrc", "_netrc", ".git-credentials", ".config/git/credentials",
	".docker/config.json", ".config/gh/hosts.yml", ".config/glab-cli/config.yml", ".pypirc",
	".aws/credentials", ".kube/config", ".cargo/credentials", ".cargo/credentials.toml",
	".gem/credentials", ".wrangler/config/default.toml", ".config/.wrangler/config/default.toml",
	".config/hub", ".terraform.d/credentials.tfrc.json", ".config/configstore/firebase-tools.json",
}

// fileGuard keeps a command run with secrets from leaving a secret in a file the agent can read, without
// leaving any file worse than it was:
//
//   - The user's credential files and the git config of the command's folders are read before it runs.
//     One it changed to hold a secret it didn't hold before is put back as it was (or removed, if the
//     command made it): a live config never gets a placeholder, and a login that held the value before
//     the command is left alone.
//   - In the agent's own folder, files are read before it runs, to know which held a secret already;
//     those are left alone. A file the command changed to hold one gets its Placeholder, or as many
//     '*' in a binary file (Redactor.Mask), so that its layout doesn't change.
//   - No other folder is looked through, or changed.
type fileGuard struct {
	r        *Redactor
	start    time.Time
	own      string
	saved    map[string]snapshot // credential and git config files, as they were
	held     map[string]bool     // files in own that held a secret before
	seen     map[string]bool     // files in own read before
	complete bool                // every file in own was read before
}

type snapshot struct {
	exists bool
	data   []byte
	mode   fs.FileMode
	held   bool
}

// guardFiles reads what the command might change, before it runs: the credential files, the git config
// of each of dirs, and the files in own.
func guardFiles(r *Redactor, own string, dirs []string) *fileGuard {
	g := &fileGuard{r: r, start: time.Now(), own: own, saved: map[string]snapshot{}, held: map[string]bool{}, seen: map[string]bool{}}
	if r.Empty() {
		return g
	}
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		for _, f := range credentialFiles {
			files = append(files, filepath.Join(home, filepath.FromSlash(f)))
		}
	}
	for _, d := range dirs {
		if d != "" {
			if cfg := gitConfig(d); cfg != "" {
				files = append(files, cfg)
			}
		}
	}
	for _, f := range files {
		if _, ok := g.saved[f]; ok {
			continue
		}
		info, err := os.Stat(f)
		switch {
		case err != nil:
			g.saved[f] = snapshot{}
		case info.Mode().IsRegular() && info.Size() <= scanMaxFile:
			if data, err := os.ReadFile(f); err == nil {
				g.saved[f] = snapshot{exists: true, data: data, mode: info.Mode().Perm(), held: r.Contains(string(data))}
			}
		}
	}
	if own != "" {
		g.complete = walk(own, time.Now().Add(scanBudget), func(path string, info fs.FileInfo) {
			if data, err := os.ReadFile(path); err == nil {
				g.seen[path] = true
				if r.Contains(string(data)) {
					g.held[path] = true
				}
			}
		})
	}
	return g
}

// settle puts back the files the command left a secret in, after it ends.
func (g *fileGuard) settle() (scrubbed, restored []string, unchecked bool) {
	if g.r.Empty() {
		return nil, nil, false
	}
	for f, before := range g.saved {
		data, err := os.ReadFile(f)
		if err != nil || before.held || !g.r.Contains(string(data)) {
			continue
		}
		if before.exists {
			err = os.WriteFile(f, before.data, before.mode)
		} else {
			err = os.Remove(f)
		}
		if err == nil {
			restored = append(restored, f)
		}
	}
	if g.own == "" {
		return nil, restored, false
	}
	since := g.start.Add(-2 * time.Second) // file systems keep coarse times
	complete := walk(g.own, time.Now().Add(scanBudget), func(path string, info fs.FileInfo) {
		if info.ModTime().Before(since) || g.held[path] {
			return
		}
		if _, ok := g.saved[path]; ok {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil || !g.r.Contains(string(data)) {
			return
		}
		if !g.seen[path] && !g.complete {
			// The look before didn't reach it: it may have held the secret already.
			unchecked = true
			return
		}
		if isBinary(data) {
			data = g.r.Mask(data)
		} else {
			data = []byte(g.r.RedactFull(string(data)))
		}
		if os.WriteFile(path, data, info.Mode().Perm()) == nil {
			scrubbed = append(scrubbed, path)
		}
	})
	return scrubbed, restored, unchecked || !complete || !g.complete
}

// walk calls f for each regular file under root of at most scanMaxFile, skipping git's object store,
// until deadline. It reports whether it got through.
func walk(root string, deadline time.Time, f func(path string, info fs.FileInfo)) bool {
	done := true
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if time.Now().After(deadline) {
			done = false
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "objects" && filepath.Base(filepath.Dir(path)) == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() && info.Size() <= scanMaxFile {
			f(path, info)
		}
		return nil
	})
	return done
}

// isBinary reports whether data isn't text: it has a NUL byte, or isn't UTF-8.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 || !utf8.Valid(data)
}

// gitConfig is the config file of the repository dir is in (for a worktree, the main repository's,
// which `git remote set-url` writes), or "".
func gitConfig(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := proc.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	return filepath.Join(strings.TrimSpace(string(out)), "config")
}
