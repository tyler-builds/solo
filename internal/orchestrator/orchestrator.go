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

// pendingKey is where thread updates not yet delivered are kept, so a restart doesn't lose them.
const pendingKey = "orchestrator_pending"

// notesKey is where notes waiting for Solo's next message are kept, so a restart doesn't lose them.
const notesKey = "orchestrator_notes"

// An update that fails to reach Solo is tried again after RetryDelay, doubling with each failure, up
// to RetryLimit times; after that it waits for the next update, the end of Solo's turn, or a restart.
var (
	RetryDelay = 5 * time.Second
	RetryLimit = 5
)

// Orchestrator wires the orchestrator thread into a Core.
type Orchestrator struct {
	c       *core.Core
	log     *slog.Logger
	workDir string

	mu       sync.Mutex
	pending  []string // thread updates not yet delivered (saved)
	timer    *time.Timer
	sending  bool   // deliver is sending updates
	failures int    // deliveries failed in a row
	paused   bool   // proactive updates are off (SetUpdates)
	carry    string // a recap for the next message, after NewConversation or a rotation (saved)
	// notes say what happened that Solo should know but needn't answer (the user answering a thread's
	// question in its card): they go, hidden, with its next message, whatever starts it (saved).
	notes    []string
	rotating bool
	rotateAt int // overrides the size in Settings, for tests; 0 uses Settings
	// update sends thread updates to Solo (a field so tests can make it fail).
	update func(ctx context.Context, threadID, hidden, text string) error
	// recalls counts recall calls in the turn that began at recallTurn (takeRecall).
	recallTurn time.Time
	recalls    int
	// reopening holds the threads reopen_thread is restoring, whose return Solo needn't hear about.
	reopening map[string]bool
}

// New sets up the orchestrator on c: its session parameters, tools and thread updates. dataDir holds
// its working directory.
func New(c *core.Core, dataDir string, log *slog.Logger) (*Orchestrator, error) {
	if log == nil {
		log = slog.Default()
	}
	o := &Orchestrator{c: c, log: log, workDir: filepath.Join(dataDir, "orchestrator"), update: c.SendSyntheticWithContext,
		reopening: map[string]bool{}}
	if err := os.MkdirAll(o.workDir, 0o755); err != nil {
		return nil, err
	}
	if _, err := c.EnsureOrchestrator(); err != nil {
		return nil, err
	}
	c.SetOrchestratorSpec(o.openParams)
	c.SetToolHandler(o.handleTool)
	c.SetRecap(o.recap)
	c.SetSoloBusy(o.busy)
	c.SetSoloContext(o.takeHidden)
	c.State(carryKey, &o.carry)
	c.State(pendingKey, &o.pending)
	c.State(notesKey, &o.notes)
	c.Subscribe(o.onChange)
	o.schedule() // updates a restart interrupted
	return o, nil
}

// ID is the orchestrator thread's ID.
func (o *Orchestrator) ID() string { return core.OrchestratorID }

// Send sends the user's message, with any images (attachment paths), to the orchestrator.
func (o *Orchestrator) Send(ctx context.Context, text string, images ...string) error {
	o.rotateIfIdle(ctx)
	// A recap waiting from a fresh session, and any notes, go before the message, out of the transcript
	// (SetSoloContext): when Solo is handed it, which for a message sent while Solo works is after its turn.
	return o.c.Send(ctx, core.OrchestratorID, text, images...)
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

// takeHidden takes what goes, hidden, before Solo's next message: a recap waiting from a fresh session,
// then the notes. restore puts them back, for when the message doesn't get through.
func (o *Orchestrator) takeHidden() (hidden string, restore func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	carry, notes := o.carry, o.notes
	if carry != "" {
		o.carry = ""
		o.saveCarry("")
	}
	if len(notes) > 0 {
		o.notes = nil
		o.saveNotesLocked()
	}
	var parts []string
	if carry != "" {
		parts = append(parts, carry)
	}
	if len(notes) > 0 {
		parts = append(parts, strings.Join(notes, "\n"))
	}
	restore = func() {
		if carry != "" {
			o.putCarry(carry)
		}
		if len(notes) > 0 {
			o.mu.Lock()
			o.notes = append(notes, o.notes...)
			o.saveNotesLocked()
			o.mu.Unlock()
		}
	}
	return strings.Join(parts, "\n\n"), restore
}

// note keeps a note for Solo's next message, and saves it. It starts no turn of its own.
func (o *Orchestrator) note(n string) {
	o.mu.Lock()
	o.notes = append(o.notes, n)
	o.saveNotesLocked()
	o.mu.Unlock()
}

func (o *Orchestrator) saveNotesLocked() {
	if err := o.c.SetState(notesKey, o.notes); err != nil {
		o.log.Warn("orchestrator: saving notes", "err", err)
	}
}

// queue adds thread updates for Solo, and saves them.
func (o *Orchestrator) queue(lines ...string) {
	o.mu.Lock()
	o.pending = append(o.pending, lines...)
	o.savePendingLocked()
	o.mu.Unlock()
	o.schedule()
}

func (o *Orchestrator) savePendingLocked() {
	if err := o.c.SetState(pendingKey, o.pending); err != nil {
		o.log.Warn("orchestrator: saving thread updates", "err", err)
	}
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
		// The Artifact tools read and publish claude.ai artifacts; listing them here doesn't turn them
		// on, Claude Code does that for Solo's entrypoint (sidecar/src/launch.ts).
		ToolPolicy:    agent.ToolPolicyAllow,
		Tools:         []string{"Read", "Grep", "Glob", "Bash", "WebFetch", "WebSearch", "AskUserQuestion", "Artifact", "ArtifactComments", "ArtifactData"},
		MCPServerName: "solo",
		MCPTools:      toolSpecs,
		ClaudePath:    agent.FindClaude(s.ClaudePath).Path,
	}
}

// onChange queues an update when a worker reaches a state the orchestrator should hear about.
func (o *Orchestrator) onChange(ch core.Change) {
	if ch.Kind == core.ChangeAnswer {
		o.onAnswer(ch)
		return
	}
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
	if ch.ScratchSet {
		o.onScratch(ch)
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
	basePR := false // the pull request is its base branch's, which it added to
	for _, p := range v.Parts {
		if p.Repo == ch.Repo {
			pr = p.PR
			basePR = pr != nil && pr.Head != "" && pr.Head == p.BaseBranch
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
		case core.PRFound:
			line = fmt.Sprintf("%s: found %s, opened outside Solo (by the thread itself, say). Solo follows it now like one it opened: checks, merge.", label(v), noun)
			if basePR {
				line = fmt.Sprintf("%s: its changes are in %s, the branch of open %s, which hasn't merged. Solo follows it now like one it opened (checks, merge, its later commits), and the thread is merged when it merges.", label(v), pr.Head, noun)
			}
			if pr.AutoMerge {
				line += " Auto-merge is on, so it merges once its required checks pass."
			}
		case core.PRChecksPassed:
			line = fmt.Sprintf("%s: checks passed on %s.", label(v), noun)
			if v.Scratch {
				line += scratchNote
			}
		case core.PRClosed:
			line = fmt.Sprintf("%s: %s was closed without merging.", label(v), noun)
		case core.PRChecksFailed:
			line = fmt.Sprintf("%s: checks failed on %s: %s.", label(v), noun, failedChecks(pr.Failed))
			if v.Scratch {
				line += scratchNote
			} else if o.c.Settings().OnChecksFail == core.ChecksNotify {
				line += " The user handles failed checks themselves: tell them, and don't ask the thread to fix it."
			} else {
				line += " Have the thread fix them with message_thread (it can read the failures with gh pr checks or glab ci view); Solo pushes its new commits to the " + pr.Kind.Noun() + "."
			}
		}
		o.queue(line)
		return
	}
	if ch.PrevStatus == ch.Status {
		return
	}
	if ch.Status == core.StatusIdle && core.Finished(ch.PrevStatus) && v.DoneAt.IsZero() {
		// Restored from History: no turn finished. Solo hears of it with its next message, unless it
		// reopened the thread itself.
		o.mu.Lock()
		own := o.reopening[v.ID]
		o.mu.Unlock()
		if !own {
			o.note(fmt.Sprintf("[Solo: the user restored %s from History; it's back in their list, idle.]", label(v)))
		}
		return
	}
	if o.c.Viewing(v.ID) && o.onOpen(v, ch.Status) {
		return
	}
	switch ch.Status {
	case core.StatusIdle:
		line = fmt.Sprintf("%s finished its turn. Summary: %s", label(v), o.clipImages(oneLine(v.Summary), 600))
		if v.Resolving {
			line = fmt.Sprintf("%s resolved the conflicts its merge had; Solo is merging it now, so don't propose it again.", label(v))
		} else if v.Scratch && !v.InPlace {
			line += scratchNote
		}
	case core.StatusBackground:
		line = fmt.Sprintf("%s ended its turn but isn't finished: it waits on work it runs in the background (%s), "+
			"and carries on by itself when that ends. Don't review it or propose landing yet; you hear when its next turn finishes. "+
			"Summary so far: %s", label(v), core.DescribeBackground(v.Background), o.clipImages(oneLine(v.Summary), 400))
	case core.StatusFailed:
		line = fmt.Sprintf("%s failed: %s", label(v), clip(oneLine(v.Error), 400))
	case core.StatusWaiting:
		line = askingLine(v)
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
	o.queue(line)
}

// onOpen handles a turn of a thread the user has open ending (status: what it ended on), reporting
// whether it did. The user read the reply there themselves, so Solo is spared its recap: a note for its
// next message says what happened, and starts no turn. Only finished work that still needs a merge card
// gets a turn, a quiet one (finishedOpen). The rest (the merge after conflicts are resolved, pull
// requests, merges, discards) goes to Solo as for any thread.
func (o *Orchestrator) onOpen(v core.ThreadView, status string) bool {
	switch status {
	case core.StatusIdle:
		if v.Resolving {
			return false
		}
		go o.finishedOpen(v) // it reads the branch: not in a Subscribe callback
	case core.StatusBackground:
		o.noteOpen(v, fmt.Sprintf("it ended its turn but isn't finished: it waits on work it runs in the background (%s), and carries on by itself when that ends. "+
			"Nothing is owed yet; you hear when its next turn finishes.]", core.DescribeBackground(v.Background)))
	case core.StatusFailed:
		o.noteOpen(v, fmt.Sprintf("it failed: %s. They saw it there; nothing is owed unless they ask.]", clip(oneLine(v.Error), 400)))
	case core.StatusWaiting:
		// The thread's panel shows the question, and the user answers it there (or above your composer,
		// where it moves once they close the thread): relaying it would only repeat it.
		o.noteOpen(v, "it asked a question, which they can answer there. Don't relay it; you hear when they answer.]")
	case core.StatusInterrupted:
		o.noteOpen(v, "it was interrupted. Nothing is owed unless they ask.]")
	default:
		return false
	}
	return true
}

// finishedOpen handles a finished turn of a thread the user has open. Work that needs a merge card (it
// changed files, is in git, isn't scratch, and has no card or open pull request yet) gets a quiet turn:
// Solo reviews it and proposes the merge, without recapping it. Anything else is a note: there is
// nothing to land, or the card it has is kept current by the app (repostCards).
func (o *Orchestrator) finishedOpen(v core.ThreadView) {
	summary := o.clipImages(oneLine(v.Summary), 600)
	var why string
	switch {
	case v.InPlace:
		why = "It works in a folder that isn't in git: nothing to land."
	case v.Scratch:
		why = "It's a scratch thread: nothing to land."
	case slices.ContainsFunc(v.Parts, func(p core.PartView) bool { return p.PR != nil && p.PR.State == forge.StateOpen }):
		why = "Its pull request is open and takes its new commits; its merge card, if it has one, stays."
	case o.c.HasProposal(v.ID):
		why = "Its merge card is up: call propose_merge again only if its summary no longer describes the change."
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		files, err := o.c.Changes(ctx, v.ID)
		cancel()
		if err != nil || len(files) > 0 {
			o.queue(fmt.Sprintf("%s finished its turn. Summary: %s%s", label(v), summary, openLine))
			return
		}
		why = "It changed no files: nothing to land."
	}
	o.noteOpen(v, fmt.Sprintf("it finished its turn, and read its reply there. %s No review or recap is owed unless they ask. Summary: %s]", why, summary))
}

// openLine ends the update of a finished turn the user read in the thread itself.
const openLine = " [The user had this thread open and read its reply there: review it as usual and propose_merge if it's right, but don't recap it.]"

// noteOpen keeps a note, for Solo's next message, of what a thread the user has open did: what follows
// "the user had <thread> open when ". It replaces the thread's earlier such note, so a long exchange in
// the thread leaves one note, of where it stands.
func (o *Orchestrator) noteOpen(v core.ThreadView, what string) {
	prefix := fmt.Sprintf("[Solo: the user had %s open when ", label(v))
	o.mu.Lock()
	o.notes = slices.DeleteFunc(o.notes, func(n string) bool { return strings.HasPrefix(n, prefix) })
	o.notes = append(o.notes, prefix+what)
	o.saveNotesLocked()
	o.mu.Unlock()
}

// scratchNote ends a thread update about a scratch thread (store.Thread.Scratch).
const scratchNote = " It's a scratch thread: the user doesn't mean to land it, so there is nothing to land. Report what it found or did; don't propose a merge or have it fix checks."

// onScratch notes, for Solo's next message, that the user turned a thread's scratch mode on or
// off themselves (set_scratch tells Solo when it does). It starts no turn.
func (o *Orchestrator) onScratch(ch core.Change) {
	if ch.ScratchBy != core.FromUser {
		return
	}
	v, ok := o.c.Thread(ch.ThreadID)
	if !ok || v.Kind != store.KindWorker {
		return
	}
	n := fmt.Sprintf("[Solo: the user made %s a scratch thread: they don't mean to land it, and its merge card, if it had one, is gone. Report what it finds; don't propose a merge.]", label(v))
	if !v.Scratch {
		n = fmt.Sprintf("[Solo: the user made %s a normal thread again, no longer scratch: propose_merge it when its work looks right.]", label(v))
	}
	o.note(n)
}

func askingLine(v core.ThreadView) string {
	return fmt.Sprintf("%s is asking a question (see get_thread).", label(v))
}

// onAnswer notes, for Solo's next message, that the user answered or skipped a worker's question
// themselves (in its card, not through Solo), so Solo doesn't go on thinking the thread waits on them.
// It starts no turn: the thread carries on, and Solo hears when its turn ends.
func (o *Orchestrator) onAnswer(ch core.Change) {
	a := ch.Answer
	if a == nil || a.From != core.FromUser {
		return // Solo answered it (answer_question), so it knows
	}
	v, ok := o.c.Thread(ch.ThreadID)
	if !ok || v.Kind != store.KindWorker {
		return
	}
	if s := core.SecretOf(a.Questions); s != nil {
		n := fmt.Sprintf("[Solo: the user saved the secret %s that %s asked for, and the thread carries on.]", s.Name, label(v))
		if a.Skipped {
			n = fmt.Sprintf("[Solo: the user declined to give the secret %s that %s asked for; the thread carries on without it.]", s.Name, label(v))
		}
		o.dropAsking(v)
		o.note(n)
		return
	}
	var parts []string
	for _, q := range a.Questions {
		part := fmt.Sprintf("%q", clip(oneLine(q.Question), 200))
		if !a.Skipped {
			if ans, ok := a.Answers[q.Question]; ok {
				part += fmt.Sprintf(" → %q", clip(oneLine(ans), 400))
			} else {
				part += " → (no answer)"
			}
		}
		parts = append(parts, part)
	}
	n := fmt.Sprintf("[Solo: the user answered %s's question themselves, and the thread carries on: %s]", label(v), strings.Join(parts, "; "))
	if a.Skipped {
		n = fmt.Sprintf("[Solo: the user skipped %s's question without answering it, so the thread carries on with its own judgment. It asked: %s]", label(v), strings.Join(parts, "; "))
	}
	o.dropAsking(v)
	o.note(n)
}

// dropAsking takes the update that v is asking a question out of the ones waiting for Solo: Solo hasn't
// heard the question yet, and the note about its answer says all there is.
func (o *Orchestrator) dropAsking(v core.ThreadView) {
	o.mu.Lock()
	ask := askingLine(v)
	if i := slices.Index(o.pending, ask); i >= 0 {
		o.pending = slices.Delete(o.pending, i, i+1)
		o.savePendingLocked()
	}
	o.mu.Unlock()
}

// busy reports whether thread updates are on their way to Solo: waiting to be sent, or being sent.
func (o *Orchestrator) busy() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.paused && (o.timer != nil || o.sending)
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
	// Running outside a turn: a message (one the user queued, say) was just sent, and its turn starts next.
	if !ok || v.InTurn || v.Status == core.StatusWaiting || v.Status == core.StatusRunning || len(o.pending) == 0 || o.paused {
		// Busy: the end of its turn calls schedule again.
		o.mu.Unlock()
		return
	}
	// The saved copy keeps these lines until they are delivered.
	lines := o.pending
	o.pending = nil
	o.sending = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.sending = false
		o.mu.Unlock()
	}()
	o.rotateIfIdle(context.Background())

	var b strings.Builder
	b.WriteString("[thread update from Solo, not from the user]\n")
	for _, l := range lines {
		b.WriteString("- " + l + "\n")
	}
	b.WriteString("Tell the user what matters in a sentence or two, and act if your instructions call for it (review finished work, relay or answer questions). If nothing needs saying, reply with just \"(noted)\".")
	if slices.ContainsFunc(lines, func(l string) bool { return strings.HasSuffix(l, openLine) }) {
		b.WriteString(" A thread the user had open needs no recap: once its card is up, reply \"(noted)\" unless your review found a problem.")
	}
	if todos := o.openTodos(); todos != "" {
		b.WriteString("\n\nThe user's open todos, to tick off with update_todo any this update finishes (ones with threads tick off when those threads' work lands):\n" + todos)
	}
	text := b.String()
	hidden, restore := o.takeHidden()
	if err := o.update(context.Background(), core.OrchestratorID, hidden, text); err != nil {
		restore()
		o.log.Warn("orchestrator: delivering updates", "err", err)
		o.mu.Lock()
		o.pending = append(lines, o.pending...)
		o.savePendingLocked()
		o.failures++
		if o.failures <= RetryLimit && o.timer == nil && !o.paused {
			o.timer = time.AfterFunc(RetryDelay<<(o.failures-1), o.deliver)
		}
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	o.failures = 0
	o.savePendingLocked() // what came in meanwhile, if anything
	o.mu.Unlock()
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
			if it.Decode(&d) == nil && !core.IsNoted(d.Text) {
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

// clipImages is clip for a text that may show images (a thread's screenshots): those clipping cuts off
// are listed after it, so Solo can still read them and show them to the user.
func (o *Orchestrator) clipImages(s string, n int) string {
	out := clip(s, n)
	var lost []string
	for _, p := range o.c.ImagesIn(s) {
		if d := core.ImageDest(p); !strings.Contains(out, d) {
			lost = append(lost, d)
		}
	}
	if len(lost) > 0 {
		out += " [Images: " + strings.Join(lost, ", ") + "]"
	}
	return out
}

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
