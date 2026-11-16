//go:build !js

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/store"
)

// resumeText tells a thread that Solo restarted under it.
const resumeText = "Solo was closed while you were working and has just restarted, which stopped anything you had running " +
	"(commands, subagents). Carry on with your task from where you left off. A step you started may or may not have " +
	"taken effect: before you repeat one that changes something, check whether it already did; if you can't tell, say so " +
	"instead of repeating it."

// Restarts that keep landing on the same thread (a crash it causes, say) stop carrying it on: at most
// resumeMax continuations within resumeWindow.
const (
	resumeMax    = 3
	resumeWindow = 5 * time.Minute
)

// resumeLogKey is the setting that records when each thread was last carried on after a restart.
const resumeLogKey = "resume-log"

// ResumeInterrupted carries on the threads that were working when Solo last closed, each from where it
// stopped: its session resumes with a note that Solo restarted and what that stopped. A worker that
// hadn't got going yet starts again. Settings can turn this off, and a thread restarted too often in a
// short time stays stopped. Call it once, after the orchestrator is set up.
// Messages the user queued for Solo that it hadn't read go to it then: after its own carrying on, if it
// was working.
func (c *Core) ResumeInterrupted() {
	if !c.resumeInterrupted()[OrchestratorID] {
		c.mu.Lock()
		o := c.threads[OrchestratorID]
		// A question Solo asked before the restart still waits; a queued message would move past it.
		waiting := o != nil && o.row.Status == StatusWaiting
		c.mu.Unlock()
		if !waiting {
			go c.sendQueued(context.Background(), OrchestratorID)
		}
	}
}

// resumeInterrupted does ResumeInterrupted's carrying on, and returns the threads it carries on.
func (c *Core) resumeInterrupted() map[string]bool {
	c.mu.Lock()
	todo := c.resume
	c.resume = nil
	off := c.settings.NoResumeAfterRestart
	c.mu.Unlock()
	resumed := map[string]bool{}
	if off || len(todo) == 0 {
		return resumed
	}
	log := map[string][]int64{} // thread id → unix ms of each continuation
	_, _ = c.db.GetSetting(resumeLogKey, &log)
	cutoff := time.Now().Add(-resumeWindow).UnixMilli()
	for id, times := range log {
		times = slices.DeleteFunc(times, func(ms int64) bool { return ms < cutoff })
		if len(times) == 0 {
			delete(log, id)
		} else {
			log[id] = times
		}
	}
	for _, r := range todo {
		if len(log[r.id]) >= resumeMax {
			c.addNotice(r.id, "warning", fmt.Sprintf("Solo restarted %d times in a few minutes while this thread worked, so it stays stopped. Message it to carry on.", len(log[r.id])))
			continue
		}
		log[r.id] = append(log[r.id], time.Now().UnixMilli())
		resumed[r.id] = true
		go c.resumeOne(r)
	}
	if err := c.db.SetSetting(resumeLogKey, log); err != nil {
		c.log.Warn("core: saving the resume log", "err", err)
	}
	return resumed
}

func (c *Core) resumeOne(r resumeInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Messages the user queued for Solo go after its carrying on, or now if it didn't carry on.
	defer c.sendQueued(context.Background(), r.id)
	c.mu.Lock()
	t := c.threads[r.id]
	if t == nil || t.row.Status != StatusInterrupted || !t.row.DoneAt.IsZero() {
		c.mu.Unlock()
		return // the user moved on already
	}
	if t.row.Kind == store.KindWorker && !t.row.HasTranscript {
		// It never got as far as Claude: start it over (its worktree, if made, is reused).
		ch := c.setStatusLocked(t, StatusQueued, "")
		c.mu.Unlock()
		c.notify(ch)
		c.schedule()
		return
	}
	c.mu.Unlock()
	c.addNotice(r.id, "info", "Solo restarted; carrying on where this thread left off.")
	if err := c.SendSynthetic(ctx, r.id, resumeText+stoppedNote(r.stopped)); err != nil {
		c.log.Warn("core: resuming a thread", "thread", r.id, "err", err)
	}
}

// stoppedNote lists what the restart stopped, for the thread to start again if it still needs it.
func stoppedNote(stopped []string) string {
	if len(stopped) == 0 {
		return ""
	}
	const max = 8
	var b strings.Builder
	b.WriteString("\n\nThe restart stopped these before they finished; they won't report back, so start again what you still need and wait for its result:")
	for i, s := range stopped {
		if i == max {
			fmt.Fprintf(&b, "\n- and %d more", len(stopped)-max)
			break
		}
		b.WriteString("\n- " + s)
	}
	return b.String()
}

// settleItems ends the steps a thread had running when Solo closed: tool calls that never got a result,
// tasks still marked running, and (unless keepQuestions) questions nobody can answer any more. Their rows
// would spin forever. It returns the commands and subagents it stopped.
func (c *Core) settleItems(threadID string, keepQuestions bool) []string {
	items, err := c.db.Items(context.Background(), threadID, 300)
	if err != nil {
		return nil
	}
	var stopped []string
	listed := map[string]bool{} // tool use ids already named
	for _, it := range items {
		var data any
		switch it.Kind {
		case ItemTool:
			var d ToolData
			if json.Unmarshal(it.Data, &d) != nil || d.Done {
				continue
			}
			if d.Name == "AskUserQuestion" && keepQuestions {
				d.Done = true // the question row carries it now
				data = d
				break
			}
			if s := stoppedTool(d); s != "" {
				stopped = append(stopped, s)
				listed[d.ID] = true
			}
			d.Done, d.IsError = true, true
			if d.Result == "" {
				d.Result = "Stopped: Solo was closed."
			}
			data = d
		case ItemTask:
			var d TaskData
			if json.Unmarshal(it.Data, &d) != nil || (d.Status != "running" && d.Status != "pending" && d.Status != "") {
				continue
			}
			if d.ToolUseID == "" || !listed[d.ToolUseID] {
				stopped = append(stopped, taskKind(d.TaskType)+": "+d.Description)
			}
			d.Status = "stopped"
			data = d
		case ItemQuestion:
			var d QuestionData
			if keepQuestions || json.Unmarshal(it.Data, &d) != nil || d.Cancelled || len(d.Answers) > 0 {
				continue
			}
			d.Cancelled = true
			data = d
		default:
			continue
		}
		raw, _ := json.Marshal(data)
		if err := c.db.UpdateItemData(it.ID, raw); err != nil {
			c.log.Warn("core: settling a step", "thread", threadID, "err", err)
		}
	}
	return stopped
}

// stoppedTool names a tool call cut off by the restart, when it is work that runs on (a command or a
// subagent); quick calls like reading a file aren't worth naming.
func stoppedTool(d ToolData) string {
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(d.Input, &in)
	switch d.Name {
	case "Bash":
		if in.Description != "" {
			return "command: " + in.Description + " (" + clipText(firstLine(in.Command, 120), 120) + ")"
		}
		return "command: " + clipText(firstLine(in.Command, 120), 120)
	case "Agent", "Task":
		return "subagent: " + in.Description
	}
	return ""
}

// keepQuestion lets the question a thread was waiting on when Solo closed still be answered: it is
// pending again (PendingQuestions), and an answer reaches the thread as a message (answerStaleLocked).
// Call before the thread is shared.
func (c *Core) keepQuestion(t *thread) {
	items, err := c.db.Items(context.Background(), t.row.ID, 100)
	if err != nil {
		return
	}
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Kind != ItemQuestion {
			continue
		}
		var d QuestionData
		if json.Unmarshal(it.Data, &d) != nil || d.Cancelled || len(d.Answers) > 0 {
			return
		}
		t.askQs = map[string][]agent.Question{d.ToolUseID: d.Questions}
		t.staleAsks = map[string]int64{d.ToolUseID: it.ID}
		return
	}
}

// answerStaleLocked answers a question asked before Solo restarted: the question's row records the
// answer, and the thread gets it as a message, which resumes its session. Call with c.mu held.
func (c *Core) answerStaleLocked(t *thread, toolUseID string, itemID int64, res agent.AskResult) error {
	qs := t.askQs[toolUseID]
	delete(t.staleAsks, toolUseID)
	delete(t.askQs, toolUseID)
	c.updateItemLocked(itemID, func(raw json.RawMessage) any {
		var d QuestionData
		_ = json.Unmarshal(raw, &d)
		d.Answers, d.Cancelled = res.Answers, res.Cancelled
		return d
	})
	var b strings.Builder
	b.WriteString("Before Solo restarted, you asked the user:")
	for _, q := range qs {
		b.WriteString("\n- " + q.Question)
		if a, ok := res.Answers[q.Question]; ok && !res.Cancelled {
			b.WriteString("\n  Their answer: " + a)
		}
	}
	if res.Cancelled {
		b.WriteString("\nThey skipped it; use your best judgment.")
	}
	b.WriteString("\n\nThe restart stopped anything you had running. Carry on with your task from where you left off; before you repeat a step that changes something, check whether it already took effect.")
	id := t.row.ID
	ch := c.setStatusLocked(t, StatusIdle, "") // so the message starts a turn
	go func() {
		c.notify(Change{Kind: ChangeItems, ThreadID: id}, ch)
		if err := c.SendSynthetic(context.Background(), id, b.String()); err != nil {
			c.log.Warn("core: answering a question from before the restart", "thread", id, "err", err)
		}
	}()
	return nil
}
