package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
)

// gitInit makes a repository at dir, with origin at remote when it isn't "".
func gitInit(t *testing.T, dir, remote string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	ctx := context.Background()
	args := [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}}
	if remote != "" {
		args = append(args, []string{"remote", "add", "origin", remote})
	}
	for _, a := range args {
		if _, err := git.Run(ctx, dir, a...); err != nil {
			t.Fatal(err)
		}
	}
}

// The setup suggests the usual project folders that hold repositories, and not ones already searched
// or ones without any.
func TestSuggestFolders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the home folder is set with HOME")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	gitInit(t, filepath.Join(home, "code", "web"), "")
	gitInit(t, filepath.Join(home, "code", "group", "api"), "")
	gitInit(t, filepath.Join(home, "src", "tool"), "")
	os.MkdirAll(filepath.Join(home, "Projects", "notes"), 0o755) // no repositories
	e := newEnv(t)

	got := e.c.SuggestFolders(context.Background())
	if len(got) != 2 || got[0].Path != filepath.Join(home, "code") || got[1].Path != filepath.Join(home, "src") {
		t.Fatalf("suggested %+v, want ~/code and ~/src", got)
	}
	if len(got[0].Repos) != 2 {
		t.Errorf("~/code holds %v, want web and api", got[0].Repos)
	}
	if s := e.c.Settings(); len(s.Folders) != 1 {
		t.Errorf("suggesting changed the folders: %v", s.Folders)
	}

	if err := e.c.UpdateSettings(func(s *Settings) { s.Folders = append(s.Folders, "~/code") }); err != nil {
		t.Fatal(err)
	}
	got = e.c.SuggestFolders(context.Background())
	if len(got) != 1 || got[0].Path != filepath.Join(home, "src") {
		t.Errorf("with ~/code searched, suggested %+v, want ~/src alone", got)
	}
}

// fakeCLI puts a gh first on PATH that prints out and exits with code.
func fakeCLI(t *testing.T, name, out string, code int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake CLI is a shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + out + "' >&2\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The forges the repositories push to are found, and each one's CLI checked; repositories without a
// remote are left out.
func TestCheckForges(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		code      int
		signedIn  bool
		account   string
	}{
		{"signed in", "github.com\n  ✓ Logged in to github.com account octocat (keyring)", 0, true, "octocat on github.com"},
		{"older gh", "github.com\n  ✓ Logged in to github.com as octocat (oauth_token)", 0, true, "octocat on github.com"},
		{"signed out", "You are not logged into any GitHub hosts. To log in, run: gh auth login", 1, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCLI(t, "gh", tc.out, tc.code)
			e := newEnv(t)
			gitInit(t, filepath.Join(e.root, "web"), "git@github.com:acme/web.git")
			e.c.RescanRepos()
			got := e.c.CheckForges(context.Background())
			if len(got) != 1 {
				t.Fatalf("forges %+v, want GitHub alone", got)
			}
			st := got[0]
			if st.Kind != forge.GitHub || st.CLI != "gh" || len(st.Repos) != 1 || filepath.Base(st.Repos[0]) != "web" {
				t.Errorf("forge %+v, want GitHub for web", st)
			}
			if st.SignedIn != tc.signedIn || st.Account != tc.account || st.Ready() != tc.signedIn || st.Path == "" {
				t.Errorf("signed in %v as %q (ready %v, path %q); want %v as %q", st.SignedIn, st.Account, st.Ready(), st.Path, tc.signedIn, tc.account)
			}
			if st.Login != "gh auth login" || st.InstallURL == "" {
				t.Errorf("fixes: login %q, install page %q", st.Login, st.InstallURL)
			}
		})
	}
}
