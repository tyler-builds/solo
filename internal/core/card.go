//go:build !js

package core

// Board cards. A card the user adds is a builder (a worker, on the threads' model) that changes the code
// in the card's worktree, and two leads (KindLead) that never do, each a session of its own on its own
// model (Settings.CardModel):
//
//   - the briefer reads the code and turns the user's words into the builder's brief. It runs once (or
//     until the first build, if it asks the user something first), so its context, often the largest,
//     is never read again.
//   - the reviewer starts fresh at the builder's first finished turn, with the user's ask, the brief and
//     the builder's report, and reviews the diff itself: it sends the builder back to fix things (at
//     most cardMaxRounds times) or writes the user a short review. From then on the user's replies and
//     the builder's questions go to it.
//
// Separate sessions keep each one's prompt cache on one model and effort: a session that switched model
// would write its whole context again at the new model's price.
//
// Go drives the loop: each lead ends its turn with one call to its card tools (brief_builder, send_back,
// answer_builder, report; cardTools). A call is only queued (leadTool), and cardSettled acts on it once
// the lead's turn is over, so the builder never works while a lead is still running things in the same
// worktree. The user reads only reviews, and never talks to the builder directly. Solo's own
// conversation hears nothing about cards.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/store"
)

// cardMaxRounds is how many times the lead may send the builder back before the user sees its review.
const cardMaxRounds = 2

// card is the thread's board card, or nil. Call with c.mu held.
func (t *thread) card() *CardState {
	if t.row.Card == "" {
		return nil
	}
	var cs CardState
	if json.Unmarshal([]byte(t.row.Card), &cs) != nil {
		return nil
	}
	return &cs
}

// setCardLocked saves the thread's card. Call with c.mu held.
func (c *Core) setCardLocked(t *thread, cs CardState) {
	raw, _ := json.Marshal(cs)
	t.row.Card = string(raw)
	t.row.UpdatedAt = now()
	if err := c.saveLocked(t); err != nil {
		c.log.Warn("card: save", "thread", t.row.ID, "err", err)
	}
}

// newLeadThreadLocked makes a lead of builder b's card in the role, on the role's model. Register and
// save it after. Call with c.mu held.
func (c *Core) newLeadThreadLocked(b *thread, role string) *thread {
	ts := now()
	model, effort := c.settings.CardModel(role)
	lead := &thread{row: store.Thread{
		ID:        agent.NewUUID(),
		Kind:      store.KindLead,
		SessionID: agent.NewUUID(),
		Title:     b.row.Title,
		RepoPath:  b.row.RepoPath,
		Model:     model,
		Effort:    effort,
		Status:    StatusIdle,
		CreatedAt: ts,
		UpdatedAt: ts,
	}}
	lc, _ := json.Marshal(CardState{Builder: b.row.ID, Role: role})
	lead.row.Card = string(lc)
	return lead
}

// newLeadLocked makes the briefer of a new card whose builder is b, and links the two. Call with c.mu
// held.
func (c *Core) newLeadLocked(b *thread, ask string, files []string) *thread {
	lead := c.newLeadThreadLocked(b, CardBriefer)
	bc, _ := json.Marshal(CardState{Lead: lead.row.ID, Phase: CardPlanning, Ask: ask, AskFiles: files, AskAt: now()})
	b.row.Card = string(bc)
	return lead
}

// reviewerFor returns the card's reviewer, making it when the card has none yet; made says it is new.
func (c *Core) reviewerFor(builderID string) (id string, made bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.threads[builderID]
	if b == nil {
		return "", false, fmt.Errorf("no card %s", builderID)
	}
	cs := b.card()
	if cs == nil {
		return "", false, fmt.Errorf("no card %s", builderID)
	}
	if r := c.threads[cs.Reviewer]; r != nil {
		return r.row.ID, false, nil
	}
	r := c.newLeadThreadLocked(b, CardReviewer)
	if err := c.saveLocked(r); err != nil {
		return "", false, err
	}
	c.threads[r.row.ID] = r
	c.bySession[r.row.SessionID] = r
	cs.Reviewer = r.row.ID
	c.setCardLocked(b, *cs)
	return r.row.ID, true, nil
}

// leadParams are the session parameters of a card's lead: in its builder's worktree, with tools that
// only read.
func (c *Core) leadParams(row store.Thread) agent.OpenParams {
	c.mu.Lock()
	dir := ""
	var cs CardState
	_ = json.Unmarshal([]byte(row.Card), &cs)
	if b := c.threads[cs.Builder]; b != nil {
		dir = c.sessionDir(b.row)
	}
	s := c.settings
	c.mu.Unlock()
	return agent.OpenParams{
		SessionID:    row.SessionID,
		Resume:       row.HasTranscript,
		Cwd:          dir,
		Model:        row.Model,
		Effort:       row.Effort,
		Title:        "Lead: " + row.Title,
		SystemAppend: leadInstructions(s, cs.Role),
		// It runs tests, builds and linters with Bash, as a worker does, but has no tools that edit;
		// its prompt asks it to leave the worktree as it found it.
		PermissionMode: agent.PermissionBypass,
		ToolPolicy:     agent.ToolPolicyAllow,
		Tools:          []string{"Read", "Grep", "Glob", "Bash", "WebFetch", "WebSearch"},
		MCPServerName:  "card",
		MCPTools:       cardTools(cs.Role),
		ClaudePath:     c.claudePath(s),
	}
}

// leadInstructions are a card lead's instructions: its prompt (Settings.CardPrompt). What moves the card
// is in its tools' descriptions (cardTools), so a prompt the user rewrote can't break it.
func leadInstructions(s Settings, role string) string { return s.CardPrompt(role) }

// leadReply is what a lead's turn ended with: the action of its card tool call.
type leadReply struct {
	kind, need, body string // kind: brief, fix, answer or review
}

// leadFallback is the action of a lead turn that called no card tool: its reply, for the user.
func leadFallback(text string) leadReply {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "Solo ended its turn without a word. Reply to tell it what to do next."
	}
	return leadReply{kind: "review", need: BriefNeedInput, body: text}
}

// The card tools, as the leads see them (mcp__card__<name>).
const (
	toolBrief  = "brief_builder"
	toolFix    = "send_back"
	toolAnswer = "answer_builder"
	toolReport = "report"
)

const toolsRule = " Each turn ends with exactly one call to brief_builder, send_back, answer_builder or report; it takes effect when you end your turn, so end it right after."

// cardTools are the tools a lead in the role gets.
func cardTools(role string) []agent.ToolSpec {
	text := func(name, desc string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"type": "object", "required": []string{name},
			"properties": map[string]any{name: map[string]any{"type": "string", "description": desc}}})
		return b
	}
	brief := agent.ToolSpec{Name: toolBrief,
		Description: "Give the builder its instructions: what to change and why, where (files, functions), the conventions to follow, how to verify it, and what done means. The builder sees only this, never the user's words." + toolsRule,
		InputSchema: text("instructions", "The builder's instructions, complete on their own.")}
	answer := agent.ToolSpec{Name: toolAnswer,
		Description: "Answer the question the builder is waiting on." + toolsRule,
		InputSchema: text("answer", "The answer, as the builder should read it.")}
	reportSchema, _ := json.Marshal(map[string]any{"type": "object", "required": []string{"message", "need"},
		"properties": map[string]any{
			"message": map[string]any{"type": "string", "description": "What the user reads."},
			"need": map[string]any{"type": "string", "enum": []string{BriefNeedReview, BriefNeedInput, BriefNeedBlocked},
				"description": "review: the work is done for the user to look at. input: you need the user's answer or decision first. blocked: it can't be done (missing access, a failing step the builder couldn't fix)."},
		}})
	report := agent.ToolSpec{Name: toolReport,
		Description: "Your message to the user: the only thing they read of this card." + toolsRule,
		InputSchema: reportSchema}
	if role == CardReviewer {
		fix := agent.ToolSpec{Name: toolFix,
			Description: fmt.Sprintf("Send the builder back to fix its work before the user sees it: what is wrong and what to do, as instructions it can act on. At most %d times since the user last spoke; after that, report instead.", cardMaxRounds) + toolsRule,
			InputSchema: text("what_to_fix", "What is wrong and what to do about it.")}
		return []agent.ToolSpec{fix, report, brief, answer}
	}
	return []agent.ToolSpec{brief, report, answer}
}

// leadTool takes a lead's call to a card tool: it checks it and queues its action for the end of the
// turn (cardSettled).
func (c *Core) leadTool(t *thread, name string, args json.RawMessage) (string, bool) {
	var in struct {
		Instructions string `json:"instructions"`
		WhatToFix    string `json:"what_to_fix"`
		Answer       string `json:"answer"`
		Message      string `json:"message"`
		Need         string `json:"need"`
	}
	if json.Unmarshal(args, &in) != nil {
		return "The arguments aren't valid JSON.", true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	lc := t.card()
	b, _, cs := c.cardOfLocked(t.row.ID)
	if lc == nil || b == nil || cs == nil {
		return "This card is gone.", true
	}
	if t.leadAction != nil {
		return "You already called a card tool this turn; it takes effect when you end your turn. End it now.", true
	}
	var r leadReply
	queued := "Queued: the builder gets this when you end your turn. End your turn now."
	switch name {
	case toolBrief:
		r = leadReply{kind: "brief", body: in.Instructions}
	case toolFix:
		if lc.Role != CardReviewer {
			return "Only the reviewer sends the builder back.", true
		}
		if cs.Rounds >= cardMaxRounds {
			return fmt.Sprintf("You have sent it back %d times since the user last spoke. Call report instead, saying what is still open.", cs.Rounds), true
		}
		r = leadReply{kind: "fix", body: in.WhatToFix}
	case toolAnswer:
		waiting := false
		for _, qs := range b.askQs {
			waiting = waiting || SecretOf(qs) == nil
		}
		if !waiting {
			return "The builder isn't waiting on a question. Use brief_builder to give it new instructions.", true
		}
		r = leadReply{kind: "answer", body: in.Answer}
	case toolReport:
		switch in.Need {
		case BriefNeedReview, BriefNeedInput, BriefNeedBlocked:
		default:
			return `need is "review", "input" or "blocked".`, true
		}
		r = leadReply{kind: "review", need: in.Need, body: in.Message}
		queued = "Queued: the user sees this when you end your turn. End your turn now."
	default:
		return "No such tool.", true
	}
	if r.body = strings.TrimSpace(r.body); r.body == "" {
		return "That's empty.", true
	}
	t.leadAction = &r
	return queued, false
}

// cardOf returns the card a thread is on, with its builder and lead. Call with c.mu held.
func (c *Core) cardOfLocked(id string) (builder, lead *thread, cs *CardState) {
	t := c.threads[id]
	if t == nil {
		return nil, nil, nil
	}
	tc := t.card()
	if tc == nil {
		return nil, nil, nil
	}
	if t.row.Kind == store.KindLead {
		b := c.threads[tc.Builder]
		if b == nil {
			return nil, nil, nil
		}
		return b, t, b.card()
	}
	return t, activeLeadLocked(c, tc), tc
}

// activeLeadLocked is the lead that takes a card's replies and questions: its reviewer once it has one,
// else its briefer. Call with c.mu held.
func activeLeadLocked(c *Core, cs *CardState) *thread {
	if r := c.threads[cs.Reviewer]; r != nil {
		return r
	}
	return c.threads[cs.Lead]
}

// updateCard changes a card (by its builder) and announces it. ready says the lead wrote a review.
func (c *Core) updateCard(builderID string, f func(*CardState)) {
	c.mu.Lock()
	b := c.threads[builderID]
	if b == nil {
		c.mu.Unlock()
		return
	}
	cs := b.card()
	if cs == nil {
		c.mu.Unlock()
		return
	}
	before := cs.Phase
	f(cs)
	c.setCardLocked(b, *cs)
	v := c.viewLocked(b)
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeThread, ThreadID: builderID, PrevStatus: v.Status, Status: v.Status,
		CardReady: cs.Phase == CardReview && before != CardReview})
}

// cardFail ends a card's work with a review that says what went wrong.
func (c *Core) cardFail(builderID, why string) {
	c.log.Warn("card", "thread", builderID, "err", why)
	c.updateCard(builderID, func(cs *CardState) {
		cs.Phase, cs.Need, cs.Review, cs.ReviewAt = CardReview, BriefNeedBlocked, "⚠ "+why, now()
	})
}

// toLead sends the card's active lead (its reviewer once it has one, else its briefer) a message from
// Solo.
func (c *Core) toLead(builderID, phase, text string, files []string) {
	c.mu.Lock()
	_, lead, _ := c.cardOfLocked(builderID)
	c.mu.Unlock()
	if lead == nil {
		c.cardFail(builderID, "This card lost its lead.")
		return
	}
	c.toLeadID(builderID, lead.row.ID, phase, text, files)
}

// toLeadID sends one of the card's leads a message from Solo.
func (c *Core) toLeadID(builderID, leadID, phase, text string, files []string) {
	c.updateCard(builderID, func(cs *CardState) { cs.Phase = phase })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.send(ctx, leadID, text, files, true, FromSolo, ""); err != nil {
		c.cardFail(builderID, "Solo couldn't reach the card's lead: "+err.Error())
	}
}

// toBuilder sends the card's builder a message from its lead.
func (c *Core) toBuilder(builderID, text string, files []string) {
	c.updateCard(builderID, func(cs *CardState) { cs.Phase, cs.Brief = CardBuilding, text })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.send(ctx, builderID, text, files, false, FromSolo, ""); err != nil {
		c.cardFail(builderID, "Solo couldn't hand the work to the builder: "+err.Error())
	}
}

// cardPlan starts a new card: its worktree is ready, and the lead briefs the builder.
func (c *Core) cardPlan(builderID string) {
	c.mu.Lock()
	b, _, cs := c.cardOfLocked(builderID)
	if b == nil || cs == nil {
		c.mu.Unlock()
		return
	}
	ask, files := cs.Ask, cs.AskFiles
	c.mu.Unlock()
	c.toLead(builderID, CardPlanning, "New card from the user:\n\n"+ask+
		"\n\nRead what you need, then call brief_builder, or report with need \"input\" to ask the user first.", files)
}

// cardReply takes the user's reply on a card to its lead.
func (c *Core) cardReply(builderID, text string, attachments []string) error {
	c.mu.Lock()
	b, lead, cs := c.cardOfLocked(builderID)
	if b == nil || lead == nil || cs == nil {
		c.mu.Unlock()
		return fmt.Errorf("no card %s", builderID)
	}
	asking := len(b.askQs) > 0
	building := b.inTurn
	c.mu.Unlock()
	c.updateCard(builderID, func(cs *CardState) {
		cs.Ask, cs.AskFiles, cs.AskAt, cs.Rounds = text, attachments, now(), 0
	})
	var note string
	switch {
	case asking:
		note = "\n\nThe builder is waiting on the question it asked: answer it with answer_builder."
	case building:
		note = "\n\nThe builder is still working: brief_builder reaches it when you end your turn, and changes what it is doing."
	}
	go c.toLead(builderID, CardReplying, "The user replied:\n\n"+text+note+
		"\n\nCall brief_builder with instructions for the builder, or report if you can answer the user without changing code.", attachments)
	return nil
}

// cardSettled acts on the end of a card thread's turn: the lead's block, or the builder's finished
// work, which goes to the lead for review.
func (c *Core) cardSettled(id string) {
	c.mu.Lock()
	t := c.threads[id]
	b, _, cs := c.cardOfLocked(id)
	if t == nil || b == nil || cs == nil || c.closed {
		c.mu.Unlock()
		return
	}
	status := t.row.Status
	if Active(status) {
		c.mu.Unlock()
		return // background work, or a question: not done yet
	}
	builderID := b.row.ID
	if t.row.Kind == store.KindLead {
		text, errText, phase, rounds := t.row.Summary, t.row.Error, cs.Phase, cs.Rounds
		action := t.leadAction
		t.leadAction = nil
		var pending *PendingQuestion
		for key, qs := range b.askQs {
			if SecretOf(qs) == nil {
				pending = &PendingQuestion{ToolUseID: key, Questions: qs}
			}
		}
		c.mu.Unlock()
		switch status {
		case StatusFailed:
			c.cardFail(builderID, "Solo couldn't finish: "+firstLine(errText, 200))
			return
		case StatusInterrupted:
			c.updateCard(builderID, func(cs *CardState) {
				cs.Phase, cs.Need, cs.Review, cs.ReviewAt = CardReview, BriefNeedInput, "Stopped.", now()
			})
			return
		}
		if phase == CardReview || phase == CardBuilding {
			return // a turn that wasn't Solo's to act on (a restart's carrying on, say)
		}
		r := leadFallback(text)
		if action != nil {
			r = *action
		}
		c.leadDone(builderID, r, rounds, pending)
		return
	}
	// The builder.
	phase := cs.Phase
	report, errText, base := t.row.Summary, t.row.Error, t.row.BaseCommit
	rounds, ask, brief, files := cs.Rounds, cs.Ask, cs.Brief, cs.AskFiles
	c.mu.Unlock()
	if phase != CardBuilding {
		return
	}
	if status == StatusInterrupted {
		c.updateCard(builderID, func(cs *CardState) {
			cs.Phase, cs.Need, cs.Review, cs.ReviewAt = CardReview, BriefNeedInput, "You stopped the builder before it finished.", now()
		})
		return
	}
	state := "finished"
	if status == StatusFailed {
		state, report = "failed", errText
	}
	reviewer, made, err := c.reviewerFor(builderID)
	if err != nil {
		c.cardFail(builderID, "Solo couldn't start the review: "+err.Error())
		return
	}
	var b2 strings.Builder
	var attach []string
	if made {
		// A new reviewer knows nothing yet: what was asked, and what the builder was told.
		fmt.Fprintf(&b2, "The user asked:\n\n%s\n\nThe builder was told:\n\n%s\n\n", ask, clipText(strings.TrimSpace(brief), 8000))
		attach = files
	} else if brief != "" {
		fmt.Fprintf(&b2, "The builder was last told:\n\n%s\n\n", clipText(strings.TrimSpace(brief), 4000))
	}
	fmt.Fprintf(&b2, "The builder %s. Its report:\n\n%s\n\n", state, clipText(strings.TrimSpace(report), 6000))
	if base != "" {
		fmt.Fprintf(&b2, "Its branch started from commit %s. Review the work: git status, git log %s..HEAD, git diff %s.\n", base, base, base)
	}
	if rounds < cardMaxRounds {
		b2.WriteString("If something must change before the user sees it, call send_back; otherwise report.")
	} else {
		fmt.Fprintf(&b2, "You have sent it back %d times already: call report now, saying what is still open.", rounds)
	}
	c.toLeadID(builderID, reviewer, CardReviewing, b2.String(), attach)
}

// leadDone acts on a lead's block.
func (c *Core) leadDone(builderID string, r leadReply, rounds int, pending *PendingQuestion) {
	if r.body == "" {
		r = leadReply{kind: "review", need: BriefNeedInput, body: "Solo had nothing to say. Reply to tell it what to do next."}
	}
	switch r.kind {
	case "answer", "brief":
		if pending != nil {
			answers := map[string]string{}
			for _, q := range pending.Questions {
				answers[q.Question] = r.body
			}
			c.updateCard(builderID, func(cs *CardState) { cs.Phase = CardBuilding })
			if err := c.AnswerFromOrchestrator(builderID, pending.ToolUseID, answers); err != nil {
				c.cardFail(builderID, "Solo couldn't answer the builder: "+err.Error())
			}
			return
		}
		if r.kind == "answer" { // the question is gone (the user answered it): nothing to send
			c.updateCard(builderID, func(cs *CardState) { cs.Phase = CardBuilding })
			return
		}
		c.mu.Lock()
		var files []string
		if b, _, cs := c.cardOfLocked(builderID); b != nil && cs != nil && (cs.Phase == CardPlanning || cs.Phase == CardReplying) {
			files = cs.AskFiles // the builder sees what the user attached
		}
		c.mu.Unlock()
		c.toBuilder(builderID, r.body, files)
	case "fix":
		if rounds >= cardMaxRounds {
			c.updateCard(builderID, func(cs *CardState) {
				cs.Phase, cs.Need, cs.ReviewAt = CardReview, BriefNeedReview, now()
				cs.Review = "⚠ Still open after two rounds of fixes:\n" + r.body
			})
			return
		}
		c.updateCard(builderID, func(cs *CardState) { cs.Rounds++ })
		c.toBuilder(builderID, r.body, nil)
	default:
		c.updateCard(builderID, func(cs *CardState) {
			cs.Phase, cs.Need, cs.Review, cs.ReviewAt = CardReview, r.need, r.body, now()
		})
	}
}

// cardBuilderAsked hands a question the builder asked to the lead, which answers it if it can.
func (c *Core) cardBuilderAsked(builderID string, qs []agent.Question) {
	c.mu.Lock()
	b, _, cs := c.cardOfLocked(builderID)
	ok := b != nil && cs != nil && cs.Phase == CardBuilding
	c.mu.Unlock()
	if !ok || SecretOf(qs) != nil {
		return // a secret is the user's to give
	}
	var q strings.Builder
	for _, x := range qs {
		fmt.Fprintf(&q, "- %s", x.Question)
		for _, o := range x.Options {
			fmt.Fprintf(&q, "\n  · %s: %s", o.Label, o.Description)
		}
		q.WriteString("\n")
	}
	c.toLead(builderID, CardAnswering, "The builder asks:\n\n"+q.String()+
		"\nAnswer it with answer_builder if the request and the code settle it. If only the user can decide, call report with need \"input\", asking them in plain words.", nil)
}
