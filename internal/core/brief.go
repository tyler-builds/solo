//go:build !js

package core

// Briefs: a small model's high-level overview of each worker thread, for its card. While a turn runs it
// writes what the turn has done so far and what the agent is doing now; when the turn ends, what it did,
// where the whole task stands, and what the thread needs (an answer, a review, or help past a block).
// The model reads a digest of the turn's transcript built here (tool calls, the user's messages, the
// agent's messages, errors), never the agent's thinking, and the card shows only what it writes.
//
// Briefs are written only while the board is on, since nothing else shows them. Calls are spaced out
// (the first a few seconds into a turn, then at most every briefEvery while there is new activity the
// digest shows, and one when the turn ends), and at most cap(briefSlots) run at once, the rest waiting
// their turn, so a board full of working threads stays cheap. A card's builder gets no turn-end brief:
// its card shows the lead's review instead, so its last running brief stands until then.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyler-builds/solo/claudesdk"
	"github.com/tyler-builds/solo/internal/store"
)

// SummarizeRequest is one call to the small model (Config.Summarize).
type SummarizeRequest struct {
	ClaudePath string
	Model      string
	System     string
	Prompt     string
}

// SummarizeWithClaude is Config.Summarize with the user's Claude Code, through claudesdk: a one-shot
// call with no tools, MCP servers or settings.
func SummarizeWithClaude(ctx context.Context, req SummarizeRequest) (string, error) {
	return claudesdk.Complete(ctx, claudesdk.CompleteParams{
		CLIPath: req.ClaudePath, Model: req.Model, System: req.System, Prompt: req.Prompt, Entrypoint: "solo",
	})
}

// Pacing, as variables so tests can shorten them.
var (
	briefFirstWait = 4 * time.Second  // the first brief of a turn, after it starts doing things
	briefEvery     = 20 * time.Second // at most this often while it runs
	briefBackoff   = 2 * time.Minute  // after a call fails
	briefTimeout   = 60 * time.Second
)

// briefSlots bounds the briefs being written at once, across threads.
var briefSlots = make(chan struct{}, 2)

// briefState is a thread's brief and the bookkeeping of writing it.
type briefState struct {
	cur     *Brief // decoded from row.Brief, lazily (cur)
	loaded  bool
	now     string // what it is doing at the moment, while a turn runs
	acts    int    // activity seen in the running turn
	done    int    // the activity the turn's last brief covered
	running bool   // a brief is being written
	final   bool   // a turn-end brief is due once the running one finishes
	timer   *time.Timer
	last    time.Time // when the last brief was asked for
	failed  time.Time // when the last call failed
}

// briefOf returns the thread's brief, decoding the saved one on first use. Call with c.mu held.
func (t *thread) briefOf() *Brief {
	if !t.bs.loaded {
		t.bs.loaded = true
		if t.row.Brief != "" {
			var b Brief
			if json.Unmarshal([]byte(t.row.Brief), &b) == nil {
				t.bs.cur = &b
			}
		}
	}
	if t.bs.cur == nil {
		return nil
	}
	b := *t.bs.cur
	return &b
}

func (c *Core) briefsOnLocked(t *thread) bool {
	return c.cfg.Summarize != nil && t.row.Kind == store.KindWorker && c.settings.Board && !c.settings.NoSummaries
}

// briefTurnStartLocked starts a new turn's bookkeeping (not a steer, which carries the turn on).
func (c *Core) briefTurnStartLocked(t *thread) {
	t.bs.acts, t.bs.done, t.bs.now, t.bs.final = 0, 0, "", false
}

// briefActivityLocked notes something the running turn did, and schedules a brief.
func (c *Core) briefActivityLocked(t *thread) {
	if !t.inTurn || !c.briefsOnLocked(t) {
		return
	}
	t.bs.acts++
	c.scheduleBriefLocked(t)
}

func (c *Core) scheduleBriefLocked(t *thread) {
	if t.bs.running || t.bs.timer != nil || !t.inTurn {
		return
	}
	wait := briefEvery - time.Since(t.bs.last)
	if t.bs.done == 0 && wait > briefFirstWait {
		wait = briefFirstWait // the turn's first brief comes soon
	}
	if b := briefBackoff - time.Since(t.bs.failed); b > wait {
		wait = b
	}
	id := t.row.ID
	t.bs.timer = time.AfterFunc(max(wait, 0), func() { c.runBrief(id, false) })
}

// briefTurnEndLocked asks for the brief of a turn that is over.
func (c *Core) briefTurnEndLocked(t *thread) {
	t.bs.now = ""
	if t.bs.timer != nil {
		t.bs.timer.Stop()
		t.bs.timer = nil
	}
	if !c.briefsOnLocked(t) || t.row.Card != "" {
		return
	}
	if t.bs.running {
		t.bs.final = true
		return
	}
	id := t.row.ID
	go c.runBrief(id, true)
}

// briefStopLocked forgets the running turn's brief work: its process is gone.
func (c *Core) briefStopLocked(t *thread) {
	t.bs.now = ""
	if t.bs.timer != nil {
		t.bs.timer.Stop()
		t.bs.timer = nil
	}
}

// runBrief writes a thread's brief: a turn-end one with final, else one of the running turn.
func (c *Core) runBrief(id string, final bool) {
	c.mu.Lock()
	t := c.threads[id]
	if t == nil || c.closed {
		c.mu.Unlock()
		return
	}
	if !final {
		t.bs.timer = nil
	}
	if t.bs.running {
		t.bs.final = t.bs.final || final
		c.mu.Unlock()
		return
	}
	if !final && (!t.inTurn || t.bs.acts == t.bs.done) {
		c.mu.Unlock()
		return
	}
	t.bs.running = true
	t.bs.last = time.Now()
	acts := t.bs.acts
	row := t.row
	prev := t.briefOf()
	s := c.settings
	c.mu.Unlock()

	briefSlots <- struct{}{} // waits behind other threads' briefs; each thread has at most one waiting
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		<-briefSlots
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), briefTimeout)
	out, err := c.writeBrief(ctx, row, prev, final, s)
	cancel()
	<-briefSlots

	c.mu.Lock()
	t.bs.running = false
	changed := false
	switch {
	case err != nil:
		t.bs.failed = time.Now()
		c.log.Warn("brief", "thread", id, "err", err)
	case c.closed:
	case final:
		b := Brief{Turn: out.Turn, Overall: out.Overall, Need: out.Need, At: now()}
		if b.Overall == "" && prev != nil {
			b.Overall = prev.Overall
		}
		c.setBriefLocked(t, b)
		changed = true
	case t.bs.final: // a turn-end brief due now supersedes this one
	case t.inTurn || t.row.Card != "": // a card's builder has no turn-end brief, so keep this one
		b := Brief{Turn: out.Turn, At: now()}
		if prev != nil {
			b.Overall = prev.Overall
		}
		c.setBriefLocked(t, b)
		if t.inTurn {
			t.bs.now = out.Now
		}
		t.bs.done = acts
		changed = true
	}
	if t.bs.final {
		t.bs.final = false
		go c.runBrief(id, true)
	} else if t.inTurn && t.bs.acts > t.bs.done {
		c.scheduleBriefLocked(t)
	}
	c.mu.Unlock()
	if changed {
		v, _ := c.Thread(id)
		c.notify(Change{Kind: ChangeThread, ThreadID: id, PrevStatus: v.Status, Status: v.Status})
	}
}

func (c *Core) setBriefLocked(t *thread, b Brief) {
	t.bs.cur, t.bs.loaded = &b, true
	raw, _ := json.Marshal(b)
	t.row.Brief = string(raw)
	if err := c.saveLocked(t); err != nil {
		c.log.Warn("brief: save", "thread", t.row.ID, "err", err)
	}
}

// briefOut is the model's answer.
type briefOut struct {
	Now     string `json:"now"`
	Turn    string `json:"turn"`
	Overall string `json:"overall"`
	Need    string `json:"need"`
}

const briefSystem = `You write the status card for one task on a developer's kanban board. A coding agent works on the task, and you get a log of what it did. Write for a busy engineer who glances at the card for two seconds: very high level, plain words, what was done and what was found or decided. Don't narrate tool use ("ran a command", "read files"), don't name tools, don't quote the agent, don't say that a turn ended or completed, no filler, no markdown except the "- " bullets asked for. Reply with one JSON object and nothing else.`

func (c *Core) writeBrief(ctx context.Context, row store.Thread, prev *Brief, final bool, s Settings) (briefOut, error) {
	items, err := c.db.Items(ctx, row.ID, 400)
	if err != nil {
		return briefOut{}, err
	}
	digest := briefDigest(turnItems(items, final), final)
	if digest == "" {
		return briefOut{}, errors.New("nothing to summarize")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Task: %s\n\nWhat the user first asked for:\n%s\n\n", row.Title, clipText(strings.TrimSpace(row.Prompt), 1200))
	stood := "(this is the task's first turn)"
	if prev != nil && prev.Overall != "" {
		stood = prev.Overall
	}
	fmt.Fprintf(&b, "Where the task stood before this turn: %s\n\n", stood)
	if final {
		fmt.Fprintf(&b, "This turn, which has ended:\n%s\n\n", digest)
		b.WriteString(`Reply with JSON: {"turn": "...", "overall": "...", "need": "..."}
- turn: what this turn did and how it ended: 1 to 3 lines, each starting with "- ", at most 12 words each.
- overall: where the whole task stands now, in one or two short sentences.
- need: "input" if the agent asked the user a question or needs a decision before it can go on; "blocked" if it could not finish because of an error, missing access, or a failing step it couldn't fix; otherwise "review".`)
	} else {
		fmt.Fprintf(&b, "This turn so far (it is still running):\n%s\n\n", digest)
		b.WriteString(`Reply with JSON: {"now": "...", "turn": "..."}
- now: what the agent is doing at this moment: present tense, at most 8 words, no period ("Running the test suite", "Tracing why login fails").
- turn: what this turn has done so far: 1 to 3 lines, each starting with "- ", at most 12 words each.`)
	}
	text, err := c.cfg.Summarize(ctx, SummarizeRequest{
		ClaudePath: c.claudePath(s), Model: s.SummaryModel, System: briefSystem, Prompt: b.String(),
	})
	if err != nil {
		return briefOut{}, err
	}
	return parseBrief(text, final)
}

const titleSystem = `You name tasks on a developer's kanban board. Reply with only the card's title: 2 to 6 words, sentence case, no quotes, no period.`

// nameTitle has the summary model name a thread the user started without a title, replacing the
// title made from the prompt's first line (fallback) unless it changed since.
func (c *Core) nameTitle(id, fallback string) {
	c.mu.Lock()
	t := c.threads[id]
	if t == nil || c.cfg.Summarize == nil || c.settings.NoSummaries {
		c.mu.Unlock()
		return
	}
	prompt, s := t.row.Prompt, c.settings
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), briefTimeout)
	defer cancel()
	text, err := c.cfg.Summarize(ctx, SummarizeRequest{ClaudePath: c.claudePath(s), Model: s.SummaryModel,
		System: titleSystem, Prompt: "Task:\n" + clipText(strings.TrimSpace(prompt), 2000)})
	title := strings.Trim(firstLine(text, 70), "\"'`. ")
	if err != nil || title == "" {
		if err != nil {
			c.log.Warn("naming a thread", "thread", id, "err", err)
		}
		return
	}
	c.mu.Lock()
	t = c.threads[id]
	if t == nil || t.row.Title != fallback {
		c.mu.Unlock()
		return
	}
	t.row.Title = title
	_ = c.saveLocked(t)
	v := c.viewLocked(t)
	c.mu.Unlock()
	c.notify(Change{Kind: ChangeThread, ThreadID: id, PrevStatus: v.Status, Status: v.Status})
}

// parseBrief reads the model's JSON answer, tolerating text or code fences around it.
func parseBrief(text string, final bool) (briefOut, error) {
	i, j := strings.IndexByte(text, '{'), strings.LastIndexByte(text, '}')
	if i < 0 || j < i {
		return briefOut{}, fmt.Errorf("no JSON in the answer: %q", firstLine(text, 80))
	}
	var out briefOut
	if err := json.Unmarshal([]byte(text[i:j+1]), &out); err != nil {
		return briefOut{}, fmt.Errorf("reading the answer: %w", err)
	}
	out.Now = strings.TrimRight(firstLine(out.Now, 60), ".")
	out.Turn = clipText(strings.TrimSpace(out.Turn), 500)
	out.Overall = clipText(strings.TrimSpace(out.Overall), 400)
	switch out.Need = strings.ToLower(strings.TrimSpace(out.Need)); out.Need {
	case BriefNeedInput, BriefNeedReview, BriefNeedBlocked:
	default:
		out.Need = BriefNeedReview
	}
	if !final {
		out.Need = ""
	}
	if out.Turn == "" {
		return briefOut{}, errors.New("the answer has no summary")
	}
	return out, nil
}

// turnItems are the items of the running turn (those after the last turn end), or with final those of
// the turn that just ended (between the last two turn ends, the last included).
func turnItems(items []*store.Item, final bool) []*store.Item {
	end := len(items)
	if final {
		for end > 0 && items[end-1].Kind != ItemTurn {
			end--
		}
		if end == 0 {
			return items
		}
	}
	before := end
	if final {
		before = end - 1 // its own turn end
	}
	start := 0
	for i := before - 1; i >= 0; i-- {
		if items[i].Kind == ItemTurn {
			start = i + 1
			break
		}
	}
	return items[start:end]
}

const briefDigestMax = 9000

// briefDigest is the log the model reads: one line per thing the turn did.
func briefDigest(items []*store.Item, final bool) string {
	var lines []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s == "" {
			return
		}
		// Runs of reads become one line.
		if rest, ok := strings.CutPrefix(s, "Read "); ok && len(lines) > 0 {
			if last := lines[len(lines)-1]; strings.HasPrefix(last, "Read ") && len(last) < 300 {
				lines[len(lines)-1] = last + ", " + rest
				return
			}
		}
		lines = append(lines, s)
	}
	lastText := -1
	for i, it := range items {
		if it.Kind == ItemAssistant {
			lastText = i
		}
	}
	firstUser := true
	for i, it := range items {
		switch it.Kind {
		case ItemUser:
			var d UserData
			_ = json.Unmarshal(it.Data, &d)
			who := "The user"
			if d.From == FromOrchestrator || d.From == FromSolo {
				who = "The orchestrator"
			}
			text := strings.TrimSpace(d.Text)
			if len(d.Images)+len(d.Files) > 0 {
				text += fmt.Sprintf(" [with %d attachment(s)]", len(d.Images)+len(d.Files))
			}
			if firstUser {
				add(who + " said: " + clipText(text, 1500))
				firstUser = false
			} else {
				add(who + " added: " + clipText(text, 600))
			}
		case ItemAssistant:
			var d TextData
			_ = json.Unmarshal(it.Data, &d)
			if IsNoted(d.Text) {
				continue
			}
			n := 400
			if final && i == lastText {
				n = 2500 // its report at the end of the turn
			}
			add("Agent said: " + clipText(strings.TrimSpace(d.Text), n))
		case ItemTool:
			var d ToolData
			_ = json.Unmarshal(it.Data, &d)
			add(toolLine(d))
		case ItemTask:
			var d TaskData
			_ = json.Unmarshal(it.Data, &d)
			line := "Subagent: " + d.Description
			if d.Status != "" && d.Status != "running" {
				line += " (" + d.Status + ")"
			}
			if d.Summary != "" {
				line += ": " + clipText(d.Summary, 200)
			}
			add(line)
		case ItemQuestion:
			var d QuestionData
			_ = json.Unmarshal(it.Data, &d)
			var qs []string
			for _, q := range d.Questions {
				qs = append(qs, q.Question)
			}
			line := "Asked the user: " + strings.Join(qs, " / ")
			for q, a := range d.Answers {
				line += fmt.Sprintf(" [%s → %s]", firstLine(q, 60), a)
			}
			add(line)
		case ItemNotice:
			var d NoticeData
			_ = json.Unmarshal(it.Data, &d)
			if d.Level == "error" || d.Level == "warning" {
				add("Note: " + clipText(d.Text, 200))
			}
		case ItemTurn:
			var d TurnData
			_ = json.Unmarshal(it.Data, &d)
			line := "The turn ended: " + d.Status
			if d.Error != "" {
				line += " (" + clipText(d.Error, 200) + ")"
			}
			add(line)
		}
	}
	out := strings.Join(lines, "\n")
	if len(out) <= briefDigestMax || len(lines) < 2 {
		return clipText(out, briefDigestMax)
	}
	// Keep what was asked, and the newest lines that fit.
	head := lines[0]
	tail := lines[1:]
	size := len(head)
	k := len(tail)
	for k > 0 && size+len(tail[k-1])+1 < briefDigestMax-40 {
		size += len(tail[k-1]) + 1
		k--
	}
	return head + fmt.Sprintf("\n… (%d earlier steps left out)\n", k) + strings.Join(tail[k:], "\n")
}

// toolLine says what a tool call did, in a few words.
func toolLine(d ToolData) string {
	var in map[string]any
	_ = json.Unmarshal(d.Input, &in)
	str := func(k string) string { v, _ := in[k].(string); return strings.TrimSpace(v) }
	var line string
	switch d.Name {
	case "Bash", "PowerShell":
		cmd := firstLine(str("command"), 120)
		if desc := str("description"); desc != "" {
			line = "Ran: " + desc + " (" + cmd + ")"
		} else {
			line = "Ran: " + cmd
		}
	case "Edit", "MultiEdit", "NotebookEdit":
		line = "Edited " + shortPath(str("file_path")+str("notebook_path"))
	case "Write":
		line = "Wrote " + shortPath(str("file_path"))
	case "Read":
		line = "Read " + shortPath(str("file_path"))
	case "Grep":
		line = "Searched the code for " + firstLine(str("pattern"), 80)
	case "Glob":
		line = "Looked for files " + firstLine(str("pattern"), 80)
	case "WebSearch":
		line = "Searched the web: " + firstLine(str("query"), 100)
	case "WebFetch":
		line = "Read a web page: " + firstLine(str("url"), 100)
	case "TodoWrite":
		var todos []string
		if list, ok := in["todos"].([]any); ok {
			for _, x := range list {
				m, _ := x.(map[string]any)
				content, _ := m["content"].(string)
				status, _ := m["status"].(string)
				mark := "[ ]"
				switch status {
				case "completed":
					mark = "[x]"
				case "in_progress":
					mark = "[>]"
				}
				todos = append(todos, mark+" "+firstLine(content, 60))
			}
		}
		line = "Its plan: " + strings.Join(todos, "; ")
	case "Agent", "Task":
		line = "Started a subagent: " + str("description")
	case "AskUserQuestion":
		return "" // the question item says it
	case "Skill":
		line = "Used the skill " + str("skill")
	default:
		name := d.Name
		if parts := strings.Split(name, "__"); len(parts) == 3 && parts[0] == "mcp" {
			name = parts[2]
		}
		line = "Used " + name
	}
	if d.IsError {
		line += " — failed: " + firstLine(d.Result, 200)
	}
	return line
}

// shortPath is a file's last two path elements, enough to know it by.
func shortPath(p string) string {
	if p == "" {
		return "a file"
	}
	dir, file := filepath.Split(filepath.Clean(p))
	if parent := filepath.Base(dir); parent != "" && parent != "." && parent != string(filepath.Separator) {
		return parent + "/" + file
	}
	return file
}
