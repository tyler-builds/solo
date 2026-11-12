package core

import (
	"context"
	"strings"
	"testing"
)

// The single PR rules setting from before titles and descriptions were split carries on as both, both
// when it's loaded and when a device still on it writes it.
func TestPRRulesMigrate(t *testing.T) {
	e := newEnv(t)
	legacy := map[string]any{"folders": []string{e.root}, "scanDepth": 3, "orchestratorModel": "opus", "workerModel": "sonnet",
		"prRules": " Conventional Commit title. Description: Summary, Changes, Testing. "}
	if err := e.c.db.SetSetting("settings", legacy); err != nil {
		t.Fatal(err)
	}
	e.c.Close()

	c := e.open(t)
	want := "Conventional Commit title. Description: Summary, Changes, Testing."
	s := c.Settings()
	if s.PRTitleRules != want || s.PRDescriptionRules != want || s.PRRules != "" {
		t.Fatalf("loaded title %q, description %q, legacy %q", s.PRTitleRules, s.PRDescriptionRules, s.PRRules)
	}
	// Saved migrated: the next load reads the new fields, with no legacy value left.
	if err := c.UpdateSettings(func(s *Settings) { s.PRDescriptionRules = "Summary and Testing." }); err != nil {
		t.Fatal(err)
	}
	var saved Settings
	if _, err := c.db.GetSetting("settings", &saved); err != nil {
		t.Fatal(err)
	}
	if saved.PRTitleRules != want || saved.PRDescriptionRules != "Summary and Testing." || saved.PRRules != "" {
		t.Fatalf("saved %+v", saved)
	}
	// An older device writing the old setting changes both.
	if err := c.UpdateSettings(func(s *Settings) { s.PRRules = "Short." }); err != nil {
		t.Fatal(err)
	}
	if s := c.Settings(); s.PRTitleRules != "Short." || s.PRDescriptionRules != "Short." || s.PRRules != "" {
		t.Fatalf("after a legacy write %+v", s)
	}
}

func TestPRRulesPrompt(t *testing.T) {
	for _, c := range []struct {
		name string
		s    Settings
		want string
	}{
		{"defaults", Settings{}, "Rules for the title:\n" + defaultPRTitleRules + "\n\nRules for the description:\n" + defaultPRDescriptionRules},
		{"both", Settings{PRTitleRules: " feat(auth): OW-123 summary ", PRDescriptionRules: "Summary, Changes, Testing."},
			"Rules for the title:\nfeat(auth): OW-123 summary\n\nRules for the description:\nSummary, Changes, Testing."},
		{"title only set", Settings{PRTitleRules: "OW-123: summary"},
			"Rules for the title:\nOW-123: summary\n\nRules for the description:\n" + defaultPRDescriptionRules},
		{"same rules once", Settings{PRTitleRules: "X", PRDescriptionRules: "X"}, "Rules for the title and description:\nX"},
		{"no description", Settings{PRTitleRules: "T", PRDescriptionRules: "D", SkipPRDescription: true}, "Rules for the title:\nT"},
	} {
		if got := prRules(c.s); got != c.want {
			t.Errorf("%s: prRules = %q, want %q", c.name, got, c.want)
		}
	}
}

// Commit rules reach a thread's instructions in place of the Conventional Commits default.
func TestCommitRulesInWorkerInstructions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Rules", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	instructions := func() string {
		e.c.mu.Lock()
		row := e.c.threads[v.ID].row
		e.c.mu.Unlock()
		return e.c.openParams(row).SystemAppend
	}

	got := instructions()
	if !strings.Contains(got, "with a Conventional Commit message") || strings.Contains(got, "Commit message rules") {
		t.Fatalf("without rules:\n%s", got)
	}
	rules := "Start with the ticket, like OW-123: Refresh tokens. Imperative mood."
	if err := e.c.UpdateSettings(func(s *Settings) { s.CommitRules = "  " + rules + "\n" }); err != nil {
		t.Fatal(err)
	}
	got = instructions()
	if strings.Contains(got, "Conventional Commit") || !strings.Contains(got, "follows the commit message rules below") ||
		!strings.Contains(got, "apply to every commit you make in this thread") || !strings.HasSuffix(got, "\n"+rules) {
		t.Fatalf("with rules:\n%s", got)
	}
}
