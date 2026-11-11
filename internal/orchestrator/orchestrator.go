// Package orchestrator is the long-running manager the user talks to. It is a Claude session that reads
// code and may run any shell command, but leaves code changes to workers. It runs the work through Solo's
// tools: it starts worker threads, follows them, relays their questions, and proposes merges. Live state
// always comes from the tools, so nothing important depends on what is still in its context window.
package orchestrator

import (
	"context"
	"fmt"
	"github.com/tyler-builds/solo/internal/forge"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/store"
)

// UpdateDelay batches thread updates that happen close together into one message.
var UpdateDelay = 1500 * time.Millisecond

// Solo's Claude session is replaced, between turns, once its context reaches RotateTokens (or the
// size in Settings), or when the user comes back after RotateIdle with at least RotateIdleTokens of
// context (the prompt cache has gone cold by then, so the next message is read in full anyway). The
// fresh session gets a recap; what lasts longer is in Solo's memory. The user sees one conversation
// throughout.
//
// The default is a balance for subscription plans, where every turn's whole context counts against the
// 5-hour and weekly limits (cached context counts less): a longer session keeps more detail but uses
// the plan faster, and Claude's own compaction, far later, is lossier than a recap and memory.
var (
	RotateTokens     = 120_000
	RotateIdle       = 3 * time.Hour
	RotateIdleTokens = 15_000
)

// carryKey is where a recap waiting for Solo's next message is kept, so a restart doesn't lose it.
const carryKey = "orchestrator_carry"

// Orchestrator wires the orchestrator thread into a Core.
type Orchestrator struct {
	c       *core.Core
	log     *slog.Logger
	workDir string

	mu       sync.Mutex
	pending  []string // thread updates not yet delivered
	timer    *time.Timer
	paused   bool   // proactive updates are off (SetUpdates)
	carry    string // a recap for the next message, after NewConversation or a rotation (saved)
	rotating bool
	rotateAt int // overrides the size in Settings, for tests; 0 uses Settings
	// recalls counts recall calls in the turn that began at recallTurn (takeRecall).
	recallTurn time.Time
	recalls    int
}

// New sets up the orchestrator on c: its session parameters, tools and thread updates. dataDir holds
// its working directory.
func New(c *core.Core, dataDir string, log *slog.Logger) (*Orchestrator, error) {
	if log == nil {
		log = slog.Default()
	}
	o := &Orchestrator{c: c, log: log, workDir: filepath.Join(dataDir, "orchestrator")}
	if err := os.MkdirAll(o.workDir, 0o755); err != nil {
		return nil, err
	}
	if _, err := c.EnsureOrchestrator(); err != nil {
		return nil, err
	}
	c.SetOrchestratorSpec(o.openParams)
	c.SetToolHandler(o.handleTool)
	c.SetRecap(o.recap)
	c.State(carryKey, &o.carry)
	c.Subscribe(o.onChange)
	return o, nil
}

// ID is the orchestrator thread's ID.
func (o *Orchestrator) ID() string { return core.OrchestratorID }

// Send sends the user's message, with any images (attachment paths), to the orchestrator.
func (o *Orchestrator) Send(ctx context.Context, text string, images ...string) error {
	o.rotateIfIdle(ctx)
	carry := o.takeCarry()
	// A recap waiting from a fresh session goes before the message, out of the transcript.
	if err := o.c.SendWithContext(ctx, core.OrchestratorID, carry, text, images...); err != nil {
		if carry != "" {
			o.putCarry(carry)
		}
		return err
	}
	return nil
}

// NewConversation starts the orchestrator on a fresh Claude session; its next message carries a recap of
// the conversation so far. Use it when its context is cluttered; thread state is unaffected.
func (o *Orchestrator) NewConversation(ctx context.Context) error {
	v, _ := o.c.Thread(core.OrchestratorID)
	recap := o.recap(v)
	if err := o.c.ResetSession(ctx, core.OrchestratorID); err != nil {
		return err
	}
	o.putCarry(recap)
	return nil
}

func (o *Orchestrator) takeCarry() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.carry
	if c != "" {
		o.carry = ""
		o.saveCarry("")
	}
	return c
}

func (o *Orchestrator) putCarry(c string) {
	o.mu.Lock()
	o.carry = c
	o.saveCarry(c)
	o.mu.Unlock()
}

func (o *Orchestrator) saveCarry(c string) {
	if err := o.c.SetState(carryKey, c); err != nil {
		o.log.Warn("orchestrator: saving the recap", "err", err)
	}
}

// rotate moves Solo to a fresh Claude session, with a recap for its next message. It does nothing while
// Solo is in a turn or waits on the user.
func (o *Orchestrator) rotate(ctx context.Context) {
	o.mu.Lock()
	if o.rotating {
		o.mu.Unlock()
		return
	}
	o.rotating = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.rotating = false
		o.mu.Unlock()
	}()
	v, ok := o.c.Thread(core.OrchestratorID)
	if !ok || v.InTurn || v.PendingAsk || core.Active(v.Status) || !v.HasTranscript {
		return
	}
	recap := o.recap(v)
	if err := o.c.RotateSession(ctx, core.OrchestratorID); err != nil {
		o.log.Warn("orchestrator: starting a fresh session", "err", err)
		return
	}
	if prev := o.takeCarry(); prev != "" {
		recap = prev + "\n\n---\n\n" + recap // a recap Solo hadn't read yet
	}
	o.putCarry(recap)
	o.log.Info("orchestrator: fresh session", "context_tokens", v.ContextTokens)
}

// rotateLimit is the context size that starts a fresh session, or 0 for never.
func (o *Orchestrator) rotateLimit() int {
	o.mu.Lock()
	n := o.rotateAt
	o.mu.Unlock()
	if n == 0 {
		n = o.c.Settings().SoloSessionTokens
	}
	switch {
	case n < 0:
		return 0
	case n == 0:
		return RotateTokens
	}
	return n
}

// rotateIfIdle starts a fresh session before a message that comes after a long quiet spell.
func (o *Orchestrator) rotateIfIdle(ctx context.Context) {
	v, ok := o.c.Thread(core.OrchestratorID)
	if ok && o.rotateLimit() > 0 && v.ContextTokens >= RotateIdleTokens && time.Since(v.UpdatedAt) > RotateIdle {
		o.rotate(ctx)
	}
}

// SetUpdates turns proactive thread updates on or off.
func (o *Orchestrator) SetUpdates(on bool) {
	o.mu.Lock()
	o.paused = !on
	o.mu.Unlock()
}

func (o *Orchestrator) openParams() agent.OpenParams {
	s := o.c.Settings()
	dirs := make([]string, 0, len(s.Folders))
	for _, f := range s.Folders {
		dirs = append(dirs, expandHome(f))
	}
	return agent.OpenParams{
		Cwd:                   o.workDir,
		Model:                 s.OrchestratorModel,
		Effort:                s.OrchestratorEffort,
		Title:                 "Solo orchestrator",
		SystemAppend:          systemPrompt(o.c),
		AdditionalDirectories: dirs,
		PermissionMode:        agent.PermissionDefault,
		// Bash may run anything (compound commands, cd, builds, git writes); Edit and Write aren't offered,
		// as code changes go to workers. The user's to-do list (add_todo) takes the place of TodoWrite.
		ToolPolicy:    agent.ToolPolicyAllow,
		Tools:         []string{"Read", "Grep", "Glob", "Bash", "WebFetch", "WebSearch", "AskUserQuestion"},
		MCPServerName: "solo",
		MCPTools:      toolSpecs,
		ClaudePath:    agent.FindClaude(s.ClaudePath).Path,
	}
}

// onChange queues an update when a worker reaches a state the orchestrator should hear about.
func (o *Orchestrator) onChange(ch core.Change) {
	if ch.Kind != core.ChangeThread {
		return
	}
	if ch.ThreadID == core.OrchestratorID {
		// The orchestrator finished a turn: deliver what waited for it, and start a fresh session if
		// this one has grown long.
		if v, ok := o.c.Thread(core.OrchestratorID); ok && !v.InTurn {
			if limit := o.rotateLimit(); limit > 0 && v.ContextTokens >= limit {
				go o.rotate(context.Background())
			}
			o.schedule()
		}
		return
	}
	if ch.PrevStatus == ch.Status && ch.PREvent == "" {
		return
	}
	v, ok := o.c.Thread(ch.ThreadID)
	if !ok || v.Kind != store.KindWorker {
		return
	}
	var line string
	var pr *forge.PR
	for _, p := range v.Parts {
		if p.Repo == ch.Repo {
			pr = p.PR
		}
	}
	if pr != nil && ch.PREvent != "" && ch.PREvent != core.PRMerged {
		noun := fmt.Sprintf("%s #%d (%s)", pr.Kind.Noun(), pr.Number, pr.URL)
		if v.MultiRepo() {
			noun = fmt.Sprintf("the %s %s #%d (%s)", filepath.Base(ch.Repo), pr.Kind.Noun(), pr.Number, pr.URL)
		}
		switch ch.PREvent {
		case core.PROpened:
			line = fmt.Sprintf("%s: the user opened %s.", label(v), noun)
			if pr.AutoMerge {
				line += " Auto-merge is on, so it merges once its required checks pass."
			}
		case core.PRChecksPassed:
			line = fmt.Sprintf("%s: checks passed on %s.", label(v), noun)
		case core.PRClosed:
			line = fmt.Sprintf("%s: %s was closed without merging.", label(v), noun)
		case core.PRChecksFailed:
			line = fmt.Sprintf("%s: checks failed on %s: %s.", label(v), noun, failedChecks(pr.Failed))
			if o.c.Settings().OnChecksFail == core.ChecksNotify {
				line += " The user handles failed checks themselves: tell them, and don't ask the thread to fix it."
			} else {
				line += " Have the thread fix them with message_thread (it can read the failures with gh pr checks or glab ci view); Solo pushes its new commits to the " + pr.Kind.Noun() + "."
			}
		}
		o.mu.Lock()
		o.pending = append(o.pending, line)
		o.mu.Unlock()
		o.schedule()
		return
	}
	if ch.PrevStatus == ch.Status {
		return
	}
	switch ch.Status {
	case core.StatusIdle:
		line = fmt.Sprintf("%s finished its turn. Summary: %s", label(v), clip(oneLine(v.Summary), 600))
		if v.Resolving {
			line = fmt.Sprintf("%s resolved the conflicts its merge had; Solo is merging it now, so don't propose it again.", label(v))
		}
	case core.StatusBackground:
		line = fmt.Sprintf("%s ended its turn but isn't finished: it waits on work it runs in the background (%s), "+
			"and carries on by itself when that ends. Don't review it or propose landing yet; you hear when its next turn finishes. "+
			"Summary so far: %s", label(v), core.DescribeBackground(v.Background), clip(oneLine(v.Summary), 400))
	case core.StatusFailed:
		line = fmt.Sprintf("%s failed: %s", label(v), clip(oneLine(v.Error), 400))
	case core.StatusWaiting:
		line = fmt.Sprintf("%s is asking a question (see get_thread).", label(v))
	case core.StatusInterrupted:
		line = fmt.Sprintf("%s was interrupted.", label(v))
	case core.StatusMerged:
		line = fmt.Sprintf("%s is merged into %s.", label(v), v.BaseBranch)
		if v.PR != nil {
			line = fmt.Sprintf("%s: %s #%d merged into %s.", label(v), v.PR.Kind.Noun(), v.PR.Number, v.BaseBranch)
		} else if v.MultiRepo() {
			line = fmt.Sprintf("%s: every repository's change has landed.", label(v))
		}
	case core.StatusDiscarded:
		line = fmt.Sprintf("%s was discarded by the user.", label(v))
	default:
		return
	}
	o.mu.Lock()
	o.pending = append(o.pending, line)
	o.mu.Unlock()
	o.schedule()
}

// schedule delivers pending updates after UpdateDelay, once the orchestrator is between turns.
func (o *Orchestrator) schedule() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.pending) == 0 || o.paused || o.timer != nil {
		return
	}
	o.timer = time.AfterFunc(UpdateDelay, o.deliver)
}

func (o *Orchestrator) deliver() {
	o.mu.Lock()
	o.timer = nil
	v, ok := o.c.Thread(core.OrchestratorID)
	if !ok || v.InTurn || v.Status == core.StatusWaiting || len(o.pending) == 0 || o.paused {
		// Busy: the end of its turn calls schedule again.
		o.mu.Unlock()
		return
	}
	lines := o.pending
	o.pending = nil
	o.mu.Unlock()
	o.rotateIfIdle(context.Background())

	var b strings.Builder
	b.WriteString("[thread update from Solo, not from the user]\n")
	for _, l := range lines {
		b.WriteString("- " + l + "\n")
	}
	b.WriteString("Tell the user what matters in a sentence or two, and act if your instructions call for it (review finished work, relay or answer questions). If nothing needs saying, reply with just \"(noted)\".")
	if todos := o.openTodos(); todos != "" {
		b.WriteString("\n\nThe user's open todos, to tick off with update_todo any this update finishes (ones with threads tick off when those threads' work lands):\n" + todos)
	}
	text := b.String()
	carry := o.takeCarry()
	if err := o.c.SendSyntheticWithContext(context.Background(), core.OrchestratorID, carry, text); err != nil {
		if carry != "" {
			o.putCarry(carry)
		}
		o.log.Warn("orchestrator: delivering updates", "err", err)
		o.mu.Lock()
		o.pending = append(lines, o.pending...)
		o.mu.Unlock()
	}
}

// recap catches a fresh orchestrator session up: what's open now, from the app's state, and the last
// few messages. Older conversation is a recall away.
func (o *Orchestrator) recap(v core.ThreadView) string {
	if v.ID != core.OrchestratorID {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Solo: this is a fresh Claude session; the user sees one continuous conversation. Your memories are in your system prompt, and recall searches them, the earlier conversation and past threads. Here is what's open now, and the last few messages.]\n")
	var threads []string
	for _, t := range o.c.Threads() {
		if t.IsWorker && t.DoneAt.IsZero() && t.Status != core.StatusDiscarded {
			threads = append(threads, fmt.Sprintf("- %s: %s", label(t), statusText(t)))
		}
	}
	if len(threads) > 0 {
		b.WriteString("\nOpen threads:\n" + strings.Join(threads, "\n") + "\n")
	}
	if todos := o.openTodos(); todos != "" {
		b.WriteString("\nOpen todos:\n" + todos + "\n")
	}
	items, err := o.c.Items(context.Background(), core.OrchestratorID, 80)
	if err != nil {
		return b.String()
	}
	// The newest messages, up to recapLines lines and recapChars characters, then in order.
	var lines []string
	size := 0
	for i := len(items) - 1; i >= 0 && len(lines) < recapLines && size < recapChars; i-- {
		var line string
		switch it := items[i]; it.Kind {
		case core.ItemUser:
			var d core.UserData
			if it.Decode(&d) != nil {
				continue
			}
			if !d.Synthetic {
				line = "User: " + clip(oneLine(d.Text), 400)
				break
			}
			var updates []string
			for _, l := range strings.Split(d.Text, "\n") {
				if strings.HasPrefix(l, "- Thread ") && len(updates) < 3 {
					updates = append(updates, clip(strings.TrimPrefix(l, "- "), 200))
				}
			}
			if len(updates) > 0 {
				line = "App: " + strings.Join(updates, " / ")
			}
		case core.ItemAssistant:
			var d core.TextData
			if it.Decode(&d) == nil && strings.TrimSpace(d.Text) != "(noted)" {
				line = "You: " + clip(oneLine(d.Text), 400)
			}
		}
		if line != "" {
			lines = append(lines, line)
			size += len(line)
		}
	}
	if len(lines) > 0 {
		slices.Reverse(lines)
		b.WriteString("\nLast messages:\n" + strings.Join(lines, "\n") + "\n")
	}
	return b.String()
}

// The recap's share of the conversation: enough to carry on mid-topic.
const (
	recapLines = 12
	recapChars = 4000
)

func label(v core.ThreadView) string {
	return fmt.Sprintf("Thread %s %q (%s)", short(v.ID), v.Title, repoList(v))
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

func failedChecks(cs []forge.Check) string {
	var out []string
	for _, c := range cs {
		if c.URL != "" {
			out = append(out, c.Name+" ("+c.URL+")")
		} else {
			out = append(out, c.Name)
		}
	}
	if len(out) == 0 {
		return "see the checks"
	}
	return strings.Join(out, ", ")
}
