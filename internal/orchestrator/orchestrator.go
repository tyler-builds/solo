// Package orchestrator is the long-running manager the user talks to. It is a Claude session that
// reads but never edits code, and runs the work through Solo's tools: it starts worker threads, follows
// them, relays their questions, and proposes merges. Live state always comes from the tools, so nothing
// important depends on what is still in its context window.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/store"
)

// UpdateDelay batches thread updates that happen close together into one message.
var UpdateDelay = 1500 * time.Millisecond

// Orchestrator wires the orchestrator thread into a Core.
type Orchestrator struct {
	c       *core.Core
	log     *slog.Logger
	workDir string

	mu      sync.Mutex
	pending []string // thread updates not yet delivered
	timer   *time.Timer
	paused  bool   // proactive updates are off (SetUpdates)
	carry   string // a recap for the next message, after NewConversation
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
	c.Subscribe(o.onChange)
	return o, nil
}

// ID is the orchestrator thread's ID.
func (o *Orchestrator) ID() string { return core.OrchestratorID }

// Send sends the user's message to the orchestrator.
func (o *Orchestrator) Send(ctx context.Context, text string) error {
	if carry := o.takeCarry(); carry != "" {
		if err := o.c.SendSynthetic(ctx, core.OrchestratorID, carry+"\n\n---\n\n"+text); err != nil {
			o.putCarry(carry)
			return err
		}
		return nil
	}
	return o.c.Send(ctx, core.OrchestratorID, text)
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
	o.carry = ""
	return c
}

func (o *Orchestrator) putCarry(c string) {
	o.mu.Lock()
	o.carry = c
	o.mu.Unlock()
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
		Title:                 "Solo orchestrator",
		SystemAppend:          systemPrompt(o.c),
		AdditionalDirectories: dirs,
		PermissionMode:        agent.PermissionDefault,
		ToolPolicy:            agent.ToolPolicyReadOnly,
		Tools:                 []string{"Read", "Grep", "Glob", "Bash", "WebFetch", "WebSearch", "TodoWrite", "AskUserQuestion"},
		BashAllow:             bashAllow,
		MCPServerName:         "solo",
		MCPTools:              toolSpecs,
		ClaudePath:            agent.FindClaude(s.ClaudePath),
	}
}

// bashAllow are the shell commands the orchestrator may run: reading git state and listing files.
var bashAllow = []string{
	`git( -C \S+)? (status|log|diff|show|branch|remote|rev-parse|ls-files|blame|shortlog|describe|tag)( .*)?`,
	`ls( .*)?`,
	`wc( .*)?`,
}

// onChange queues an update when a worker reaches a state the orchestrator should hear about.
func (o *Orchestrator) onChange(ch core.Change) {
	if ch.Kind != core.ChangeThread {
		return
	}
	if ch.ThreadID == core.OrchestratorID {
		// The orchestrator finished a turn: deliver what waited for it.
		if v, ok := o.c.Thread(core.OrchestratorID); ok && !v.InTurn {
			o.schedule()
		}
		return
	}
	if ch.PrevStatus == ch.Status {
		return
	}
	v, ok := o.c.Thread(ch.ThreadID)
	if !ok || v.Kind != store.KindWorker {
		return
	}
	var line string
	switch ch.Status {
	case core.StatusIdle:
		line = fmt.Sprintf("%s finished its turn. Summary: %s", label(v), clip(oneLine(v.Summary), 600))
	case core.StatusFailed:
		line = fmt.Sprintf("%s failed: %s", label(v), clip(oneLine(v.Error), 400))
	case core.StatusWaiting:
		line = fmt.Sprintf("%s is asking a question (see get_thread).", label(v))
	case core.StatusInterrupted:
		line = fmt.Sprintf("%s was interrupted.", label(v))
	case core.StatusMerged:
		line = fmt.Sprintf("%s was merged into %s by the user.", label(v), v.BaseBranch)
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

	var b strings.Builder
	b.WriteString("[thread update from Solo, not from the user]\n")
	for _, l := range lines {
		b.WriteString("- " + l + "\n")
	}
	b.WriteString("Tell the user what matters in a sentence or two, and act if your instructions call for it (review finished work, relay or answer questions). If nothing needs saying, reply with just \"(noted)\".")
	text := b.String()
	carry := o.takeCarry()
	if carry != "" {
		text = carry + "\n\n---\n\n" + text
	}
	if err := o.c.SendSynthetic(context.Background(), core.OrchestratorID, text); err != nil {
		if carry != "" {
			o.putCarry(carry)
		}
		o.log.Warn("orchestrator: delivering updates", "err", err)
		o.mu.Lock()
		o.pending = append(lines, o.pending...)
		o.mu.Unlock()
	}
}

// recap catches a fresh orchestrator session up on the conversation so far.
func (o *Orchestrator) recap(v core.ThreadView) string {
	if v.ID != core.OrchestratorID {
		return ""
	}
	items, err := o.c.Items(context.Background(), core.OrchestratorID, 60)
	if err != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Solo: this is a fresh session; your earlier conversation with the user is summarized below. Thread state is current through your tools.]\n")
	for _, it := range items {
		switch it.Kind {
		case core.ItemUser:
			var d core.UserData
			if it.Decode(&d) == nil && !d.Synthetic {
				fmt.Fprintf(&b, "User: %s\n", clip(oneLine(d.Text), 400))
			}
		case core.ItemAssistant:
			var d core.TextData
			if it.Decode(&d) == nil {
				fmt.Fprintf(&b, "You: %s\n", clip(oneLine(d.Text), 400))
			}
		}
	}
	return b.String()
}

func label(v core.ThreadView) string {
	return fmt.Sprintf("Thread %s %q (%s)", short(v.ID), v.Title, filepath.Base(v.RepoPath))
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
