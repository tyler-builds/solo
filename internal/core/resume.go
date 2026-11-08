package core

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tyler-builds/solo/internal/store"
)

// resumeText tells a thread that Solo restarted under it.
const resumeText = "Solo was closed while you were working and has just restarted, which stopped anything you had running " +
	"(commands, subagents). Carry on with your task from where you left off; check the state of what was running before you rely on it."

// resumeQuestion is added when the thread was waiting on the user: the question went with the old process.
const resumeQuestion = " Your question to the user was lost in the restart; ask it again if you still need the answer."

// ResumeInterrupted carries on the threads that were working when Solo last closed, each from where it
// stopped: its session resumes with a note that Solo restarted. A worker that hadn't got going yet starts
// again. Call it once, after the orchestrator is set up.
func (c *Core) ResumeInterrupted() {
	c.mu.Lock()
	todo := c.resume
	c.resume = nil
	c.mu.Unlock()
	for _, r := range todo {
		go c.resumeOne(r)
	}
}

func (c *Core) resumeOne(r resumeInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c.mu.Lock()
	t := c.threads[r.id]
	if t == nil || t.row.Status != StatusInterrupted || !t.row.DoneAt.IsZero() {
		c.mu.Unlock()
		return // the user moved on already
	}
	worker, hasTranscript, prompt := t.row.Kind == store.KindWorker, t.row.HasTranscript, t.row.Prompt
	if worker && !hasTranscript {
		// It never got as far as Claude: start it over (its worktree, if made, is reused).
		ch := c.setStatusLocked(t, StatusQueued, "")
		c.mu.Unlock()
		c.notify(ch)
		c.schedule()
		return
	}
	c.mu.Unlock()
	text := resumeText
	if r.status == StatusWaiting {
		text += resumeQuestion
	}
	if !hasTranscript && prompt != "" {
		text = prompt
	}
	c.addNotice(r.id, "info", "Solo restarted; carrying on where this thread left off.")
	if err := c.SendSynthetic(ctx, r.id, text); err != nil {
		c.log.Warn("core: resuming a thread", "thread", r.id, "err", err)
	}
}

// settleItems ends the steps a thread had running when Solo closed: tool calls that never got a result,
// tasks still marked running, and questions nobody can answer any more. Their rows would spin forever.
func (c *Core) settleItems(threadID string) {
	items, err := c.db.Items(context.Background(), threadID, 300)
	if err != nil {
		return
	}
	for _, it := range items {
		var data any
		switch it.Kind {
		case ItemTool:
			var d ToolData
			if json.Unmarshal(it.Data, &d) != nil || d.Done {
				continue
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
			d.Status = "stopped"
			data = d
		case ItemQuestion:
			var d QuestionData
			if json.Unmarshal(it.Data, &d) != nil || d.Cancelled || len(d.Answers) > 0 {
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
}
