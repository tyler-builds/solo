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

// onEvent applies a session event to its thread: streaming text, transcript items, and status. It runs
// on the agent's read goroutine, in order.
func (c *Core) onEvent(e agent.Event) {
	if e.Kind == agent.EventRateLimit {
		c.setRateLimit(e)
		return
	}
	e = c.embedImages(e)
	c.mu.Lock()
	if job := c.jobs[e.SessionID]; job != nil {
		c.mu.Unlock()
		if e.Kind == agent.EventTurnEnd || e.Kind == agent.EventExited {
			select {
			case job <- e:
			default:
			}
		}
		return
	}
	t := c.bySession[e.SessionID]
	if t == nil || c.closed {
		c.mu.Unlock()
		return
	}
	id := t.row.ID
	var changes []Change
	itemsChanged, streamChanged, threadChanged := false, false, false
	settledBg := false // background work ended after the turn, so the thread settled

	switch e.Kind {
	case agent.EventInit:
		t.model = e.Model
		if e.Cwd != "" {
			t.cwd = e.Cwd
		}
		threadChanged = true

	case agent.EventTurnStart:
		if !t.inTurn { // after a steer the turn carries on, and keeps its start
			t.turnStart = now()
			t.thought = ""
		}
		t.inTurn = true
		t.retry = ""
		t.bgWake, t.bgStopped = false, false
		if !t.row.HasTranscript {
			t.row.HasTranscript = true
			_ = c.saveLocked(t)
		}
		if t.row.Status != StatusRunning && t.row.Status != StatusWaiting {
			changes = append(changes, c.setStatusLocked(t, StatusRunning, ""))
		} else {
			threadChanged = true
		}

	case agent.EventTextDelta, agent.EventThinkingDelta:
		if !e.IsRoot() {
			break
		}
		k := liveKey{e.MessageID, e.Index}
		if t.liveTxt == nil {
			t.liveTxt = map[liveKey]*LiveBlock{}
		}
		b := t.liveTxt[k]
		if b == nil {
			b = &LiveBlock{Thinking: e.Kind == agent.EventThinkingDelta}
			t.liveTxt[k] = b
			t.live = append(t.live, k)
		}
		b.Text += e.Text
		if b.Thinking {
			t.thought = b.Text
		}
		t.retry = ""
		streamChanged = true

	case agent.EventAssistant:
		if !e.IsRoot() {
			break // subagent messages are summarized by their task items
		}
		for _, b := range e.Blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					c.addItemLocked(t, ItemAssistant, "", TextData{Text: b.Text, Images: c.ImagesIn(b.Text)})
				}
			case "thinking":
				if b.Text != "" {
					c.addItemLocked(t, ItemThinking, "", TextData{Text: b.Text})
					t.thought = b.Text
				}
			case "tool_use":
				itemID := c.addItemLocked(t, ItemTool, b.ID, ToolData{ID: b.ID, Name: b.Name, Input: b.Input})
				t.step = &ToolData{ID: b.ID, Name: b.Name, Input: b.Input}
				threadChanged = true
				if t.tools == nil {
					t.tools = map[string]int64{}
				}
				t.tools[b.ID] = itemID
			}
		}
		// The complete message replaces what streamed for it.
		kept := t.live[:0]
		for _, k := range t.live {
			if k.message == e.MessageID {
				delete(t.liveTxt, k)
			} else {
				kept = append(kept, k)
			}
		}
		t.live = kept
		itemsChanged, streamChanged = true, true

	case agent.EventToolResult:
		if !e.IsRoot() {
			break
		}
		if itemID, ok := t.tools[e.ToolUseID]; ok {
			c.updateToolLocked(itemID, func(d *ToolData) {
				d.Result, d.IsError, d.Done = e.Text, e.IsError, true
				t.notePRHintsLocked(*d) // a pull request it opened itself, linked when the turn ends
			})
			delete(t.tools, e.ToolUseID)
			if t.step != nil && t.step.ID == e.ToolUseID {
				t.step = nil
				threadChanged = true
			}
			itemsChanged = true
		}

	case agent.EventTask:
		if e.Ambient {
			break
		}
		if e.Phase == "ended" && t.hasBg(e.TaskID) {
			if t.bgEnd == nil {
				t.bgEnd = map[string]string{}
			}
			t.bgEnd[e.TaskID] = e.Status
		}
		if t.tasks == nil {
			t.tasks = map[string]int64{}
		}
		itemID, ok := t.tasks[e.TaskID]
		if !ok {
			if e.Phase != "started" {
				break
			}
			t.tasks[e.TaskID] = c.addItemLocked(t, ItemTask, e.TaskID, TaskData{
				TaskID: e.TaskID, Description: e.Description, TaskType: e.TaskType, Status: "running",
				ToolUseID: e.ToolUseID, Background: e.Background,
			})
			itemsChanged = true
			break
		}
		c.updateItemLocked(itemID, func(raw json.RawMessage) any {
			var d TaskData
			_ = json.Unmarshal(raw, &d)
			if e.Description != "" {
				d.Description = e.Description
			}
			if e.Summary != "" {
				d.Summary = e.Summary
			}
			if e.ToolUses > 0 {
				d.ToolUses = e.ToolUses
			}
			if e.ToolUseID != "" {
				d.ToolUseID = e.ToolUseID
			}
			d.Background = d.Background || e.Background
			if e.Phase == "ended" {
				d.Status = e.Status
			} else if e.Status != "" {
				d.Status = e.Status
			}
			return d
		})
		if e.Phase == "ended" {
			delete(t.tasks, e.TaskID)
		}
		itemsChanged = true

	case agent.EventBackground:
		wake := t.setBg(e.Tasks)
		threadChanged = true
		if len(t.bg) > 0 || t.inTurn || t.row.Status != StatusBackground {
			break
		}
		// The turn had ended and its background work is over now.
		if wake && !t.bgStopped {
			// It finished by itself, and Claude starts a turn to read the result: the thread carries
			// on rather than turning ready for a moment. If no turn comes, it settles after all.
			t.bgWake = true
			t.bgWakeGen++
			gen := t.bgWakeGen
			time.AfterFunc(bgWakeWait, func() { c.bgWakeTimeout(id, gen) })
			break
		}
		status := StatusIdle
		if t.bgStopped {
			status = StatusInterrupted // the user stopped it
		}
		changes = append(changes, c.settleLocked(t, status, "")...)
		settledBg = true

	case agent.EventCompact:
		c.addItemLocked(t, ItemNotice, "", NoticeData{Level: "info",
			Text: fmt.Sprintf("Context compacted (%dk → %dk tokens).", e.PreTokens/1000, e.PostTokens/1000)})
		itemsChanged = true

	case agent.EventAPIRetry:
		t.retry = fmt.Sprintf("Retrying (%d/%d): %s", e.Attempt, e.MaxRetries, e.Error)
		threadChanged = true

	case agent.EventTurnEnd:
		if e.Steered {
			// The turn stopped only to read a steer and carries on: its cut-off tool calls are done.
			for _, itemID := range t.tools {
				c.updateToolLocked(itemID, func(d *ToolData) { d.Done = true })
			}
			t.tools = nil
			t.step = nil
			t.live, t.liveTxt = nil, nil
			itemsChanged, streamChanged = true, true
			break
		}
		t.inTurn = false
		t.step = nil
		t.retry = ""
		t.thought = ""
		t.live, t.liveTxt = nil, nil
		// Tool calls the turn never answered (an interrupt) are done, without a result.
		for _, itemID := range t.tools {
			c.updateToolLocked(itemID, func(d *ToolData) { d.Done = true })
		}
		t.tools = nil
		turnUUID := ""
		if len(e.UUIDs) > 0 {
			turnUUID = e.UUIDs[len(e.UUIDs)-1]
		}
		c.addItemTurnLocked(t, turnUUID, TurnData{Status: e.Status, Reason: e.Reason, Error: e.Error,
			DurationMs: e.DurationMs, CostUSD: e.TotalCostUSD})
		if e.TotalCostUSD > 0 {
			t.row.CostUSD = t.row.CostBase + e.TotalCostUSD
		}
		if e.ContextTokens > 0 {
			t.row.ContextTokens = e.ContextTokens
		}
		if e.Status == agent.TurnCompleted && e.Result != "" {
			t.row.Summary = e.Result
		}
		itemsChanged, streamChanged = true, true
		if e.QueuedTurns > 0 {
			// Another queued message runs next; stay running.
			_ = c.saveLocked(t)
			threadChanged = true
			break
		}
		// Recover at most once per few minutes, so a session that keeps failing this way can't loop.
		if e.Status == agent.TurnFailed && isLostSession(e.Error) && time.Since(t.recovered) > 5*time.Minute {
			t.recovered = time.Now()
			go c.recoverLostSession(id, turnUUID)
		}
		status, errText := StatusIdle, ""
		switch e.Status {
		case agent.TurnFailed:
			status, errText = StatusFailed, e.Error
		case agent.TurnInterrupted, agent.TurnCancelled:
			status = StatusInterrupted
		}
		changes = append(changes, c.settleLocked(t, status, errText)...)

	case agent.EventExited:
		t.inTurn = false
		t.step = nil
		t.live, t.liveTxt = nil, nil
		if e.Error != "" {
			c.addItemLocked(t, ItemNotice, "", NoticeData{Level: "error", Text: "Claude stopped: " + e.Error})
			itemsChanged = true
			if t.row.Status == StatusStarting || t.row.Status == StatusRunning {
				changes = append(changes, c.setStatusLocked(t, StatusFailed, e.Error))
			}
		}
		// Background work dies with the process.
		if lost := c.dropBgLocked(t); len(lost) > 0 {
			itemsChanged = true
			if t.row.Status == StatusBackground {
				c.addItemLocked(t, ItemNotice, "", NoticeData{Level: "warning",
					Text: "Claude exited, which stopped the work it ran in the background: " + strings.Join(lost, "; ") + "."})
				changes = append(changes, c.setStatusLocked(t, StatusInterrupted, "Claude exited while its background work ran."))
				t.resolving = false
			}
		}
		threadChanged = true
	}
	c.mu.Unlock()

	if itemsChanged {
		changes = append(changes, Change{Kind: ChangeItems, ThreadID: id})
	}
	if streamChanged {
		changes = append(changes, Change{Kind: ChangeStream, ThreadID: id})
	}
	if threadChanged {
		v, _ := c.Thread(id)
		changes = append(changes, Change{Kind: ChangeThread, ThreadID: id, PrevStatus: v.Status, Status: v.Status})
	}
	c.notify(changes...)
	if e.Kind == agent.EventTurnEnd || e.Kind == agent.EventExited || settledBg {
		c.schedule()
		if id == OrchestratorID {
			c.repostCards() // after the subscribers heard, so Solo has queued what it answers next
			if e.Kind != agent.EventExited {
				go c.sendQueued(context.Background(), id) // the next message the user sent while it worked
			}
		} else {
			go c.countChanges(id)
		}
	}
}

// bgWakeWait is how long a thread whose background work finished between turns waits for the turn
// Claude starts to read it, before it settles without one.
var bgWakeWait = 30 * time.Second

// hasBg reports whether the task is running in the thread's background.
func (t *thread) hasBg(taskID string) bool {
	return slices.ContainsFunc(t.bg, func(b BackgroundTask) bool { return b.ID == taskID })
}

// setBg replaces the thread's background work with the tasks a background event lists, keeping when
// each was first seen. It reports whether a task that left finished by itself (or ended unreported),
// which is when Claude starts a turn to read it; tasks stopped don't wake it.
func (t *thread) setBg(tasks []agent.BackgroundTask) (wake bool) {
	prev := t.bg
	t.bg = nil
	for _, bt := range tasks {
		b := BackgroundTask{ID: bt.TaskID, Type: bt.TaskType, Description: bt.Description, Started: now()}
		for _, o := range prev {
			if o.ID == bt.TaskID {
				b.Started = o.Started
			}
		}
		t.bg = append(t.bg, b)
	}
	for _, o := range prev {
		if t.hasBg(o.ID) {
			continue
		}
		st, ok := t.bgEnd[o.ID]
		wake = wake || !ok || (st != "stopped" && st != "killed")
		delete(t.bgEnd, o.ID)
	}
	return wake
}

// dropBgLocked forgets the thread's background work, which died with its process: their task rows end
// as stopped. It returns what they were, as "command: …". Call with c.mu held.
func (c *Core) dropBgLocked(t *thread) []string {
	var lost []string
	for _, b := range t.bg {
		lost = append(lost, b.Kind()+": "+b.Description)
		if itemID, ok := t.tasks[b.ID]; ok {
			c.updateItemLocked(itemID, func(raw json.RawMessage) any {
				var d TaskData
				_ = json.Unmarshal(raw, &d)
				d.Status = "stopped"
				return d
			})
			delete(t.tasks, b.ID)
		}
	}
	t.bg, t.bgEnd, t.bgWake = nil, nil, false
	return lost
}

// settleLocked gives a thread whose turn is over the status it ends on: status (idle, failed,
// interrupted), except that a turn that ended well while background work still runs waits on it
// (StatusBackground). A thread with nothing left running takes a pending effort change, and one
// resolving merge conflicts merges once it is idle. Call with c.mu held; notify the changes after.
func (c *Core) settleLocked(t *thread, status, errText string) []Change {
	id := t.row.ID
	if status == StatusIdle && len(t.bg) > 0 {
		status = StatusBackground
	}
	if status != StatusBackground {
		t.bgWake, t.bgStopped = false, false
	}
	if t.reopen && len(t.bg) == 0 {
		t.reopen = false
		go c.closeSession(context.Background(), id) // takes the new effort on its next message
	}
	var changes []Change
	if !Finished(t.row.Status) {
		changes = append(changes, c.setStatusLocked(t, status, errText))
	}
	if t.resolving {
		switch status {
		case StatusIdle:
			go c.mergeResolved(id) // Resolving stays set until it has merged
		case StatusBackground: // it merges once that work is done
		default:
			t.resolving = false
		}
	}
	return changes
}

// bgWakeTimeout settles a thread whose background work finished between turns when the turn Claude
// should have started to read it hasn't come.
func (c *Core) bgWakeTimeout(id string, gen int) {
	c.mu.Lock()
	t := c.threads[id]
	if t == nil || c.closed || !t.bgWake || t.bgWakeGen != gen || t.inTurn || len(t.bg) > 0 || t.row.Status != StatusBackground {
		c.mu.Unlock()
		return
	}
	t.bgWake = false
	changes := c.settleLocked(t, StatusIdle, "")
	c.mu.Unlock()
	c.notify(changes...)
	c.schedule()
	go c.countChanges(id)
}

// addItemLocked saves a transcript item and returns its ID. Call with c.mu held.
func (c *Core) addItemLocked(t *thread, kind, ref string, data any) int64 {
	raw, _ := json.Marshal(data)
	it := &store.Item{ThreadID: t.row.ID, Kind: kind, Ref: ref, Data: raw, CreatedAt: now()}
	if err := c.db.AddItem(it); err != nil {
		c.log.Error("core: saving item", "err", err)
	}
	return it.ID
}

func (c *Core) addItemTurnLocked(t *thread, turnUUID string, data TurnData) {
	raw, _ := json.Marshal(data)
	it := &store.Item{ThreadID: t.row.ID, Kind: ItemTurn, TurnUUID: turnUUID, Data: raw, CreatedAt: now()}
	if err := c.db.AddItem(it); err != nil {
		c.log.Error("core: saving item", "err", err)
	}
}

// updateItemLocked rewrites an item's data from its current data. Call with c.mu held.
func (c *Core) updateItemLocked(itemID int64, f func(json.RawMessage) any) {
	items, err := c.db.ItemByID(itemID)
	if err != nil {
		c.log.Error("core: reading item", "err", err)
		return
	}
	raw, _ := json.Marshal(f(items.Data))
	if err := c.db.UpdateItemData(itemID, raw); err != nil {
		c.log.Error("core: updating item", "err", err)
	}
}

func (c *Core) updateToolLocked(itemID int64, f func(*ToolData)) {
	c.updateItemLocked(itemID, func(raw json.RawMessage) any {
		var d ToolData
		_ = json.Unmarshal(raw, &d)
		f(&d)
		return d
	})
}

// onToolCall serves an app tool call from a session.
func (c *Core) onToolCall(ctx context.Context, p agent.ToolCallParams) (agent.ToolCallResult, error) {
	c.mu.Lock()
	t := c.bySession[p.SessionID]
	h := c.tools
	c.mu.Unlock()
	if t == nil || h == nil {
		return agent.ToolCallResult{Text: "This tool is not available here.", IsError: true}, nil
	}
	text, isErr := h(ctx, t.row.ID, p.Name, p.Args)
	return agent.ToolCallResult{Text: text, IsError: isErr}, nil
}

// onAsk shows a session's AskUserQuestion and waits for the answer (Answer or Dismiss), or until the
// session withdraws it.
func (c *Core) onAsk(ctx context.Context, p agent.AskParams) (agent.AskResult, error) {
	c.mu.Lock()
	t := c.bySession[p.SessionID]
	if t == nil {
		c.mu.Unlock()
		return agent.AskResult{Cancelled: true}, nil
	}
	id := t.row.ID
	if id == OrchestratorID {
		for _, w := range c.threads {
			if w != t && len(w.askQs) > 0 {
				c.mu.Unlock()
				return agent.AskResult{Cancelled: true, Message: "Not shown: the user already sees the question " +
					"\"" + w.row.Title + "\" is waiting on, pinned above your composer, and answers it there. Don't ask it " +
					"again. Anything else for the user, ask in your reply."}, nil
			}
		}
	}
	key := p.ToolUseID
	if key == "" {
		key = agent.NewUUID()
	}
	itemID := c.addItemLocked(t, ItemQuestion, key, QuestionData{ToolUseID: key, Questions: p.Questions})
	ch := make(chan agent.AskResult, 1)
	if t.asks == nil {
		t.asks = map[string]chan agent.AskResult{}
	}
	t.asks[key] = ch
	if t.askQs == nil {
		t.askQs = map[string][]agent.Question{}
	}
	t.askQs[key] = p.Questions
	statusCh := c.setStatusLocked(t, StatusWaiting, "")
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeItems, ThreadID: id}, statusCh)

	var res agent.AskResult
	select {
	case res = <-ch:
	case <-ctx.Done():
		res = agent.AskResult{Cancelled: true}
	}

	c.mu.Lock()
	delete(t.asks, key)
	delete(t.askQs, key)
	if c.closed && res.Cancelled {
		// Solo is closing, not the user skipping: the question stays open for after the restart.
		c.mu.Unlock()
		return res, nil
	}
	c.updateItemLocked(itemID, func(raw json.RawMessage) any {
		var d QuestionData
		_ = json.Unmarshal(raw, &d)
		d.Answers, d.Cancelled = res.Answers, res.Cancelled
		return d
	})
	var back Change
	if t.row.Status == StatusWaiting && len(t.asks) == 0 {
		back = c.setStatusLocked(t, StatusRunning, "")
	}
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeItems, ThreadID: id}, back)
	return res, nil
}
