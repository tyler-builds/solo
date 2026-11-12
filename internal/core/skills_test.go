package core

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tyler-builds/solo/internal/git"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeClaudeDir makes a Claude Code folder with a skill or command from each place Solo reads, and
// points CLAUDE_CONFIG_DIR at it.
func fakeClaudeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFile(t, filepath.Join(dir, "skills", "pr-style", "SKILL.md"),
		"---\nname: pr-style\ndescription: >\n  How we write\n  pull requests.\n---\n# PR style\n\nTitles start with the ticket, like OW-123.\n")
	writeFile(t, filepath.Join(dir, "skills", "folder-name", "SKILL.md"), "---\nname: named-in-file\ndescription: \"Quoted.\"\n---\nBody.\n")
	writeFile(t, filepath.Join(dir, "skills", "no-skill-here", "README.md"), "not a skill")
	writeFile(t, filepath.Join(dir, "commands", "deploy.md"), "# Deploy the app\n\nRun the deploy.\n")
	writeFile(t, filepath.Join(dir, "commands", "git", "sync.md"), "---\ndescription: Sync with main\n---\nSync.\n")
	writeFile(t, filepath.Join(dir, "skills", "synced", "bucket-1", "pdf", "SKILL.md"), "---\nname: pdf\ndescription: PDFs.\n---\nPDF body.\n")
	writeFile(t, filepath.Join(dir, "skills", "synced", "bucket-1", ".staging", "x", "SKILL.md"), "---\nname: x\n---\n")
	writeFile(t, filepath.Join(dir, "plugins", "synced", "bucket-1", "cowork", ".claude-plugin", "plugin.json"), `{"name":"cowork-tools"}`)
	writeFile(t, filepath.Join(dir, "plugins", "synced", "bucket-1", "cowork", "skills", "make", "SKILL.md"), "---\nname: make\n---\nMake.\n")
	// Installed from a marketplace: one for the user, one turned off, one for another project.
	inst := filepath.Join(dir, "plugins", "cache", "mkt", "fmt", "1.0")
	off := filepath.Join(dir, "plugins", "cache", "mkt", "off", "1.0")
	other := filepath.Join(dir, "plugins", "cache", "mkt", "other", "1.0")
	writeFile(t, filepath.Join(inst, "skills", "lint", "SKILL.md"), "---\nname: lint\ndescription: Lint.\n---\nLint.\n")
	writeFile(t, filepath.Join(inst, "commands", "fix.md"), "Fix it.\n")
	writeFile(t, filepath.Join(off, "skills", "nope", "SKILL.md"), "---\nname: nope\n---\n")
	writeFile(t, filepath.Join(other, "skills", "elsewhere", "SKILL.md"), "---\nname: elsewhere\n---\n")
	writeFile(t, filepath.Join(dir, "plugins", "installed_plugins.json"), `{"version":2,"plugins":{
		"fmt@mkt":[{"scope":"user","installPath":`+quote(inst)+`}],
		"off@mkt":[{"scope":"user","installPath":`+quote(off)+`}],
		"other@mkt":[{"scope":"project","projectPath":"/somewhere/else","installPath":`+quote(other)+`}]}}`)
	writeFile(t, filepath.Join(dir, "settings.json"), `{"enabledPlugins":{"off@mkt":false,"fmt@mkt":true}}`)
	return dir
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

func TestFindSkills(t *testing.T) {
	fakeClaudeDir(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".claude", "skills", "house", "SKILL.md"), "---\nname: house\ndescription: House rules.\n---\nHouse.\n")
	writeFile(t, filepath.Join(project, ".claude", "skills", "pr-style", "SKILL.md"), "---\nname: pr-style\n---\nThe project's own.\n")
	writeFile(t, filepath.Join(project, ".claude", "commands", "ship.md"), "Ship it.\n")

	got := map[string]skillFile{}
	for _, f := range findSkills(project) {
		key := f.Name
		if f.Command {
			key = "/" + key
		}
		got[key] = f
	}
	want := map[string]string{
		"pr-style": "Personal", "named-in-file": "Personal", "/deploy": "Personal", "/git:sync": "Personal",
		"anthropic-skills:pdf": "Organization", "cowork-tools:make": "Plugin cowork-tools",
		"fmt:lint": "Plugin fmt", "/fmt:fix": "Plugin fmt", "house": "Project", "/ship": "Project",
	}
	for name, source := range want {
		if f, ok := got[name]; !ok || f.Source != source {
			t.Errorf("%s: found %v, source %q, want %q", name, ok, f.Source, source)
		}
	}
	if len(got) != len(want) {
		t.Errorf("found %d, want %d: %v", len(got), len(want), slices.Collect(func(yield func(string) bool) {
			for k := range got {
				yield(k)
			}
		}))
	}
	if d := got["pr-style"].Description; d != "How we write pull requests." {
		t.Errorf("folded description %q", d)
	}
	if d := got["named-in-file"].Description; d != "Quoted." {
		t.Errorf("quoted description %q", d)
	}
	if d := got["/deploy"].Description; d != "Deploy the app" {
		t.Errorf("a command's first line %q", d)
	}
	if !strings.HasPrefix(got["pr-style"].path, os.Getenv("CLAUDE_CONFIG_DIR")) {
		t.Errorf("the user's skill comes first, not the project's: %s", got["pr-style"].path)
	}
}

func TestMatchScore(t *testing.T) {
	order := []string{"review", "review-pr", "code-review", "frontend:review", "revise-docs-very-long-name"}
	var scores []int
	for _, c := range order {
		s, ok := MatchScore("rev", c)
		if !ok {
			t.Fatalf("%q doesn't match", c)
		}
		scores = append(scores, s)
	}
	if !(scores[0] > scores[1] && scores[1] > scores[2] || scores[0] > scores[2]) {
		t.Errorf("scores %v", scores)
	}
	if _, ok := MatchScore("xyz", "review"); ok {
		t.Error("xyz matches review")
	}
	if _, ok := MatchScore("itok", "internal/auth/token.go"); !ok {
		t.Error("fuzzy itok doesn't match internal/auth/token.go")
	}
	exact, _ := MatchScore("token.go", "internal/auth/token.go")
	sub, _ := MatchScore("token.go", "internal/auth/token.go.orig")
	if exact <= sub {
		t.Errorf("exact base name %d not above %d", exact, sub)
	}
}

// The skills chosen for pull request text reach the PR text model's prompt, without their frontmatter,
// and one that is gone is skipped with a notice rather than failing.
func TestPRPromptIncludesSkills(t *testing.T) {
	fakeClaudeDir(t)
	e := newEnv(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(e.repo, ".claude", "skills", "house-pr", "SKILL.md"),
		"---\nname: house-pr\ndescription: Our PR format.\n---\nEnd every description with a Testing section.\n")
	if _, err := git.Run(ctx, e.repo, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, e.repo, "commit", "-q", "-m", "add the house skill"); err != nil {
		t.Fatal(err)
	}
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Skills", Prompt: "write x.txt hello"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if err := e.c.UpdateSettings(func(s *Settings) {
		s.PRTitleRules = "Ticket first."
		s.PRSkills = []string{"pr-style", " house-pr ", "gone-skill", "pr-style"}
	}); err != nil {
		t.Fatal(err)
	}
	s := e.c.Settings()
	if !slices.Equal(s.PRSkills, []string{"pr-style", "house-pr", "gone-skill"}) {
		t.Fatalf("saved skills %q", s.PRSkills)
	}
	r := prPrompt(ctx, v, v.Parts[0], s)
	for _, want := range []string{"Rules for the title:\nTicket first.", "<skill name=\"pr-style\">\n# PR style\n\nTitles start with the ticket, like OW-123.\n</skill>",
		"<skill name=\"house-pr\">\nEnd every description with a Testing section.\n</skill>"} {
		if !strings.Contains(r.prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, r.prompt)
		}
	}
	if strings.Contains(r.prompt, "description: >") || strings.Contains(r.prompt, "gone-skill") {
		t.Errorf("the prompt has frontmatter or the missing skill:\n%s", r.prompt)
	}
	if !slices.Equal(r.missing, []string{"gone-skill"}) {
		t.Errorf("missing %q", r.missing)
	}

	title, _ := e.c.prText(ctx, v, v.Parts[0])
	if title != "Skills" {
		t.Errorf("title %q", title)
	}
	items, _ := e.c.Items(ctx, v.ID, 0)
	noticed := false
	for _, it := range items {
		var n NoticeData
		if it.Kind == ItemNotice && it.Decode(&n) == nil && strings.Contains(n.Text, "skips the skill /gone-skill") {
			noticed = true
		}
	}
	if !noticed {
		t.Error("no notice about the missing skill")
	}

	// Without skills, the prompt has no skill section.
	s.PRSkills = nil
	if r := prPrompt(ctx, v, v.Parts[0], s); strings.Contains(r.prompt, "<skill") || len(r.missing) > 0 {
		t.Errorf("without skills:\n%s", r.prompt)
	}
}

// A skill's instructions are cut at maxSkillRules.
func TestSkillRulesCap(t *testing.T) {
	dir := fakeClaudeDir(t)
	writeFile(t, filepath.Join(dir, "skills", "huge", "SKILL.md"), "---\nname: huge\n---\n"+strings.Repeat("x", 3*maxSkillRules))
	rules, missing := skillRules("", []string{"huge"})
	if len(missing) != 0 || len(rules) != 1 || len(rules[0].text) > maxSkillRules+100 || !strings.HasSuffix(rules[0].text, "cut.)") {
		t.Fatalf("missing %v, %d rules, %d bytes", missing, len(rules), len(rules[0].text))
	}
}

func TestSkillsForSettingsAndThreads(t *testing.T) {
	fakeClaudeDir(t)
	e := newEnv(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(e.repo, ".claude", "skills", "house-pr", "SKILL.md"), "---\nname: house-pr\n---\nHouse.\n")
	writeFile(t, filepath.Join(e.repo, ".claude", "commands", "ship.md"), "Ship.\n")
	e.c.RescanRepos()

	names := func(ss []Skill) map[string]Skill {
		m := map[string]Skill{}
		for _, s := range ss {
			m[s.Name] = s
		}
		return m
	}
	all, err := e.c.Skills(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	m := names(all)
	if m["house-pr"].Source != "Project proj" || m["pr-style"].Source != "Personal" {
		t.Errorf("settings' skills %+v", all)
	}
	for _, s := range all {
		if s.Command {
			t.Errorf("settings lists a command: %+v", s)
		}
	}
	orch, err := e.c.Skills(ctx, OrchestratorID)
	if err != nil {
		t.Fatal(err)
	}
	if m := names(orch); m["house-pr"].Name != "" || !m["deploy"].Command {
		t.Errorf("Solo's skills %+v", orch)
	}
	if _, err := e.c.Skills(ctx, "nope"); err == nil {
		t.Error("an unknown thread has skills")
	}
}

func TestSearchPaths(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(e.repo, ".gitignore"), "build/\n*.log\n")
	writeFile(t, filepath.Join(e.repo, "internal", "auth", "token.go"), "package auth\n")
	writeFile(t, filepath.Join(e.repo, "README.md"), "hi\n")
	if _, err := git.Run(ctx, e.repo, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, e.repo, "commit", "-q", "-m", "files"); err != nil {
		t.Fatal(err)
	}
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Paths", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	wt := v.Parts[0].Worktree
	writeFile(t, filepath.Join(wt, "build", "out.bin"), "x")
	writeFile(t, filepath.Join(wt, "debug.log"), "x")
	writeFile(t, filepath.Join(wt, "notes", "new-token.txt"), "untracked, not ignored")

	hits, err := e.c.SearchPaths(ctx, v.ID, "token", 10)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, h := range hits {
		paths = append(paths, h.Path)
	}
	if len(paths) < 2 || paths[0] != "internal/auth/token.go" || !slices.Contains(paths, "notes/new-token.txt") {
		t.Errorf("token: %q", paths)
	}
	top, err := e.c.SearchPaths(ctx, v.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var tops []string
	for _, h := range top {
		tops = append(tops, h.Path)
		if h.Path == "build" || h.Path == "debug.log" {
			t.Errorf("an ignored path is listed: %s", h.Path)
		}
	}
	if len(top) == 0 || !top[0].Dir || !slices.Contains(tops, "README.md") || !slices.Contains(tops, "internal") {
		t.Errorf("top level %+v", top)
	}
	if _, err := e.c.SearchPaths(ctx, OrchestratorID, "x", 5); err == nil {
		t.Error("Solo's conversation has a folder to search")
	}
}
