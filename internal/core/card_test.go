package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/store"
)

// waitCard waits until the card (by its builder) satisfies ok.
func waitCard(t *testing.T, c *Core, id string, ok func(ThreadView) bool) ThreadView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := c.Thread(id)
		if v.Card != nil && ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("card %+v, status %s", v.Card, v.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func reviewed(v ThreadView) bool { return v.Card.Phase == CardReview && !v.InTurn }

func TestCardLeadBriefsAndReviews(t *testing.T) {
	e := cardEnv(t)
	ctx := context.Background()
	e.c.UpdateSettings(func(s *Settings) {
		s.CardBriefModel, s.CardBriefEffort, s.CardReviewModel, s.CardReviewEffort = "opus", "medium", "fable", "high"
	})
	v, err := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Hello", Prompt: "write hello.txt hi there", FromUser: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Card == nil || v.Card.Lead == "" || v.Card.Phase != CardPlanning || v.Card.Ask != "write hello.txt hi there" {
		t.Fatalf("new card %+v", v.Card)
	}
	lead, ok := e.c.Thread(v.Card.Lead)
	if !ok || lead.Kind != store.KindLead || lead.IsWorker || lead.Model != "opus" || lead.Effort != "medium" ||
		lead.Card.Builder != v.ID || lead.Card.Role != CardBriefer {
		t.Fatalf("briefer %+v", lead)
	}

	v = waitCard(t, e.c, v.ID, reviewed)
	if v.Card.Review != "✓ Reviewed: wrote hello.txt" || v.Card.Need != BriefNeedReview || v.Card.Rounds != 0 ||
		v.Card.Brief != "write hello.txt hi there" {
		t.Fatalf("card %+v", v.Card)
	}
	// The review is a session of its own, on the review model, told what was asked and briefed.
	reviewer, ok := e.c.Thread(v.Card.Reviewer)
	if !ok || reviewer.ID == lead.ID || reviewer.Model != "fable" || reviewer.Effort != "high" || reviewer.Card.Role != CardReviewer {
		t.Fatalf("reviewer %+v", reviewer)
	}
	ritems, _ := e.c.Items(ctx, reviewer.ID, 0)
	var rfirst UserData
	for _, it := range ritems {
		if it.Kind == ItemUser {
			it.Decode(&rfirst)
			break
		}
	}
	if !strings.HasPrefix(rfirst.Text, "The user asked:\n\nwrite hello.txt hi there\n\nThe builder was told:") {
		t.Fatalf("reviewer's first message %q", rfirst.Text)
	}
	if _, err := os.Stat(filepath.Join(v.WorktreePath, "hello.txt")); err != nil {
		t.Fatal("the builder didn't write the file:", err)
	}
	// The builder heard the lead's brief, from Solo; the lead worked in the builder's worktree.
	items, _ := e.c.Items(ctx, v.ID, 0)
	var first UserData
	for _, it := range items {
		if it.Kind == ItemUser {
			it.Decode(&first)
			break
		}
	}
	if first.Text != "write hello.txt hi there" || first.From != FromSolo {
		t.Fatalf("builder's first message %+v", first)
	}

	// The user's reply goes to the lead, which briefs the builder again.
	if err := e.c.Send(ctx, v.ID, "write more.txt again"); err != nil {
		t.Fatal(err)
	}
	v = waitCard(t, e.c, v.ID, func(v ThreadView) bool {
		return reviewed(v) && v.Card.Ask == "write more.txt again" && strings.Contains(v.Card.Review, "more.txt")
	})
	if _, err := os.Stat(filepath.Join(v.WorktreePath, "more.txt")); err != nil {
		t.Fatal("the reply didn't reach the builder:", err)
	}
	heard := func(id string) bool {
		items, _ := e.c.Items(ctx, id, 0)
		for _, it := range items {
			var d UserData
			if it.Kind == ItemUser && it.Decode(&d) == nil && strings.HasPrefix(d.Text, "The user replied:") {
				return true
			}
		}
		return false
	}
	if !heard(reviewer.ID) || heard(lead.ID) {
		t.Fatalf("the reply went to the reviewer: %v, the briefer: %v", heard(reviewer.ID), heard(lead.ID))
	}
	if v.Card.Reviewer != reviewer.ID {
		t.Fatal("a second review made a second reviewer")
	}
}

func TestCardLeadAnswersBuilder(t *testing.T) {
	e := cardEnv(t)
	v, err := e.c.CreateWorker(context.Background(), WorkerSpec{Repo: "proj", Prompt: "ask", FromUser: true})
	if err != nil {
		t.Fatal(err)
	}
	v = waitCard(t, e.c, v.ID, reviewed)
	if v.Card.Review != "✓ Reviewed: Tabs" {
		t.Fatalf("card %+v", v.Card)
	}
}

func TestCardLeadAsksUser(t *testing.T) {
	e := cardEnv(t)
	v, err := e.c.CreateWorker(context.Background(), WorkerSpec{Repo: "proj", Prompt: "unclear change", FromUser: true})
	if err != nil {
		t.Fatal(err)
	}
	v = waitCard(t, e.c, v.ID, reviewed)
	if v.Card.Need != BriefNeedInput || v.Card.Review != "Needs you: which page?" || v.HasTranscript {
		t.Fatalf("card %+v, builder started %v", v.Card, v.HasTranscript)
	}
}

func TestCardModel(t *testing.T) {
	s := Settings{OrchestratorModel: "opus", OrchestratorEffort: "high"}
	if m, e := s.CardModel(CardBriefer); m != "opus" || e != "high" {
		t.Fatalf("default briefer %s/%s", m, e)
	}
	s.CardReviewModel = "fable"
	if m, e := s.CardModel(CardReviewer); m != "fable" || e != "" {
		t.Fatalf("reviewer %s/%s", m, e)
	}
	s.CardBriefEffort = "low"
	if m, e := s.CardModel(CardBriefer); m != "opus" || e != "low" {
		t.Fatalf("briefer with effort %s/%s", m, e)
	}
}

func TestLeadInstructions(t *testing.T) {
	s := Settings{}
	if got := leadInstructions(s, CardReviewer); got != DefaultCardReviewerPrompt {
		t.Fatalf("default reviewer instructions:\n%s", got)
	}
	s.CardBriefPrompt = "Brief tersely."
	if got := leadInstructions(s, CardBriefer); got != "Brief tersely." {
		t.Fatalf("custom briefer instructions:\n%s", got)
	}
	// What moves the card is in the tools, whatever the prompt says.
	var names []string
	for _, tool := range cardTools(CardReviewer) {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "send_back,report,brief_builder,answer_builder" {
		t.Fatalf("reviewer tools %v", names)
	}
	for _, tool := range cardTools(CardBriefer) {
		if tool.Name == toolFix {
			t.Fatal("the briefer can send the builder back")
		}
	}
}

func TestLeadToolChecks(t *testing.T) {
	e := cardEnv(t)
	v, err := e.c.CreateWorker(context.Background(), WorkerSpec{Repo: "proj", Prompt: "unclear change", FromUser: true})
	if err != nil {
		t.Fatal(err)
	}
	waitCard(t, e.c, v.ID, reviewed)
	rid, _, err := e.c.reviewerFor(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.c.mu.Lock()
	r := e.c.threads[rid]
	e.c.mu.Unlock()
	call := func(name, args string) (string, bool) { return e.c.leadTool(r, name, json.RawMessage(args)) }
	if text, isErr := call(toolAnswer, `{"answer":"x"}`); !isErr || !strings.Contains(text, "isn't waiting") {
		t.Errorf("answer with no question: %q", text)
	}
	if text, isErr := call(toolReport, `{"message":"hi","need":"maybe"}`); !isErr || !strings.Contains(text, "need is") {
		t.Errorf("bad need: %q", text)
	}
	e.c.updateCard(v.ID, func(cs *CardState) { cs.Rounds = cardMaxRounds })
	if text, isErr := call(toolFix, `{"what_to_fix":"x"}`); !isErr || !strings.Contains(text, "report instead") {
		t.Errorf("fix past the limit: %q", text)
	}
	if text, isErr := call(toolReport, `{"message":"All done.","need":"review"}`); isErr || !strings.HasPrefix(text, "Queued") {
		t.Errorf("report: %q", text)
	}
	if text, isErr := call(toolBrief, `{"instructions":"more"}`); !isErr || !strings.Contains(text, "already called") {
		t.Errorf("second call: %q", text)
	}
	b, _ := e.c.Thread(v.Card.Lead)
	e.c.mu.Lock()
	briefer := e.c.threads[b.ID]
	e.c.mu.Unlock()
	if text, isErr := e.c.leadTool(briefer, toolFix, json.RawMessage(`{"what_to_fix":"x"}`)); !isErr || !strings.Contains(text, "Only the reviewer") {
		t.Errorf("briefer sending back: %q", text)
	}
}

// cardEnv is newEnv with the fake claude as the claude that the card's leads (on claudesdk) run.
func cardEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	exe, _ := os.Executable()
	if err := e.c.UpdateSettings(func(s *Settings) { s.ClaudePath = exe }); err != nil {
		t.Fatal(err)
	}
	return e
}
