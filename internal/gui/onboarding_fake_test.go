package gui

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// setupFake is a backend on this machine, as the desktop's is (api.Setup): the fake, with the
// first-run setup's checks. What a search of a folder finds is set per folder, by its full path.
type setupFake struct {
	*fakeBackend
	smu       sync.Mutex
	claude    *core.ClaudeStatus // what CheckClaude finds without a path set; the fake's own when nil
	checked   []string           // the paths CheckClaude was asked about, in order
	forges    []core.ForgeStatus
	forgeRuns int
	suggest   []core.FolderSuggestion
	found     map[string][2][]string // by folder: the repositories and the folders outside git in it
}

func newSetupFake() *setupFake {
	return &setupFake{fakeBackend: &fakeBackend{items: map[string][]core.Item{}, settings: core.DefaultSettings()},
		found: map[string][2][]string{}}
}

// claudeModels are the models Claude Code 2.1.325 reported, with its words for each.
var claudeModels = func() []agent.ModelInfo {
	all := []string{"low", "medium", "high", "xhigh", "max"}
	return []agent.ModelInfo{
		{Value: "default", DisplayName: "Default (recommended)", Description: "Opus 5.5 · Best for everyday, complex tasks", EffortLevels: all},
		{Value: "opus", DisplayName: "Opus 5.5", Description: "For complex work and everyday tasks", EffortLevels: all},
		{Value: "fable", DisplayName: "Fable 5.2", Description: "For your toughest challenges", EffortLevels: all},
		{Value: "sonnet", DisplayName: "Sonnet 5.5", Description: "Most efficient for simpler tasks", EffortLevels: all},
		{Value: "haiku", DisplayName: "Haiku 5.5", Description: "Fastest for quick answers", EffortLevels: all},
		{Value: "claude-fable-5-1", DisplayName: "Fable 5.1", Description: "Most capable for your hardest and longest-running tasks", EffortLevels: all},
		{Value: "claude-sonnet-5", DisplayName: "Sonnet 5", Description: "Efficient for routine tasks", EffortLevels: all},
		{Value: "claude-opus-5", DisplayName: "Opus 5", Description: "Best for everyday, complex tasks", EffortLevels: all},
		{Value: "claude-sonnet-4-6", DisplayName: "Sonnet 4.6", Description: "Efficient for routine tasks", EffortLevels: []string{"low", "medium", "high", "max"}},
		{Value: "claude-haiku-4-5-20251001", DisplayName: "Haiku 4.5", Description: "Fastest for quick answers"},
	}
}()

func (s *setupFake) CheckClaude(ctx context.Context, override string) core.ClaudeStatus {
	s.smu.Lock()
	s.checked = append(s.checked, override)
	st := s.claude
	s.smu.Unlock()
	if st == nil || override != "" {
		out := s.fakeBackend.CheckClaude(ctx, override)
		if out.Err == "" {
			out.Models = claudeModels
		}
		return out
	}
	return *st
}

func (s *setupFake) SuggestFolders(context.Context) []core.FolderSuggestion {
	s.smu.Lock()
	defer s.smu.Unlock()
	var out []core.FolderSuggestion
	for _, f := range s.suggest {
		if !slices.Contains(s.Settings().Folders, tildePath(f.Path)) {
			out = append(out, f)
		}
	}
	return out
}

func (s *setupFake) CheckForges(context.Context) []core.ForgeStatus {
	s.smu.Lock()
	defer s.smu.Unlock()
	s.forgeRuns++
	return slices.Clone(s.forges)
}

// UpdateSettings searches the folders again, as the engine does when they change.
func (s *setupFake) UpdateSettings(fn func(*core.Settings)) error {
	if err := s.fakeBackend.UpdateSettings(fn); err != nil {
		return err
	}
	s.smu.Lock()
	var repos, plain []string
	for _, f := range s.Settings().Folders {
		got := s.found[expandHome(f)]
		repos, plain = append(repos, got[0]...), append(plain, got[1]...)
	}
	s.smu.Unlock()
	s.mu.Lock()
	s.repos, s.plain = repos, plain
	s.mu.Unlock()
	return nil
}

func (s *setupFake) RepoPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.repos)
}

func (s *setupFake) PlainFolders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.plain)
}

// homeDir is a folder under the home folder, as the setup shows it and Settings keeps it.
func homeDir(rel string) string { return expandHome("~/" + rel) }

// projects fills the fake's folders: ~/code holds six repositories (web and api push to GitHub, infra
// to GitLab, and the rest have no remote), ~/notes is outside git, and ~/src holds two more.
func (s *setupFake) projects() *setupFake {
	code := func(names ...string) []string {
		var out []string
		for _, n := range names {
			out = append(out, filepath.Join(homeDir("code"), n))
		}
		return out
	}
	s.found[homeDir("code")] = [2][]string{code("web", "api", "docs", "infra", "solo", "dotfiles"), {filepath.Join(homeDir("code"), "sketches")}}
	s.found[homeDir("notes")] = [2][]string{nil, {homeDir("notes")}}
	s.found[homeDir("src")] = [2][]string{{filepath.Join(homeDir("src"), "kernel-tools"), filepath.Join(homeDir("src"), "dash")}, nil}
	s.found[homeDir("empty")] = [2][]string{}
	s.suggest = []core.FolderSuggestion{
		{Path: homeDir("code"), Repos: s.found[homeDir("code")][0]},
		{Path: homeDir("src"), Repos: s.found[homeDir("src")][0]},
	}
	s.forges = []core.ForgeStatus{
		{Kind: forge.GitHub, CLI: "gh", Repos: code("web", "api"), Path: "/usr/bin/gh", SignedIn: true,
			Account: "octocat on github.com", Login: "gh auth login", InstallURL: "https://cli.github.com"},
		{Kind: forge.GitLab, CLI: "glab", Repos: code("infra"), Path: "/usr/bin/glab",
			Login: "glab auth login", Install: "sudo dnf install glab", InstallURL: "https://gitlab.com/gitlab-org/cli#installation"},
	}
	return s
}

// withFolders sets the folders searched, and what the search finds.
func (s *setupFake) withFolders(folders ...string) *setupFake {
	_ = s.UpdateSettings(func(c *core.Settings) { c.Folders = folders })
	return s
}

// signedOut and missing are Claude Code signed out, and not found.
var (
	claudeSignedOut = core.ClaudeStatus{Path: homeDir(".local/bin/claude"), Source: "PATH", Version: "2.1.319",
		Err: "Not signed in. Run claude in a terminal and sign in with /login.", SignedOut: true}
	claudeMissing = core.ClaudeStatus{Err: "Claude Code isn't installed, or isn't where Solo looks. Install it, or set its path."}
	claudeBroken  = core.ClaudeStatus{Path: homeDir(".local/bin/claude"), Source: "PATH", Version: "2.1.319",
		Err: "Claude Code didn't start: the probe timed out"}
)

// joined is the text of every node, for a test to look in.
func hasText(text string, in []string) bool {
	return slices.ContainsFunc(in, func(s string) bool { return strings.Contains(s, text) })
}
