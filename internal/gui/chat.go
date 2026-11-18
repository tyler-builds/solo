package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/tyler-builds/jamb/dialog"
	"image"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/markdown"
	"github.com/tyler-builds/jamb/markdown/mdcode"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

const chatMaxWidth = 780

// gutter is the space at each side of a conversation: less in a compact window, whose width the text
// needs.
func gutter(ctx *ui.Context) float32 {
	if isCompact(ctx) {
		return space4
	}
	return space6
}

// code colors the code blocks of every transcript; one highlighter for the app.
var code = mdcode.Highlight()

// orchestratorView is the center panel: the conversation with the orchestrator.
func (a *App) orchestratorView(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	v, _ := a.thread(core.OrchestratorID)
	items := a.itemsOf(core.OrchestratorID).Get()

	var body ui.Element
	if len(items) == 0 && !v.InTurn {
		body = a.welcome(ctx)
	} else {
		body = a.transcript(ctx, v, items, "orch")
	}
	return ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).
		Child(body).
		Child(ui.Div().Column().Align(ui.Center).PaddingX(gutter(ctx)).PaddingBottom(gutter(ctx) - space2).Shrink(0).
			Child(ui.Div().Column().Gap(space2).Width(ui.Full).MaxWidth(ui.Px(chatMaxWidth)).
				// A component of its own: what it reads re-renders it, not the conversation.
				Child(ui.Component(a.hintBar)).
				Child(ui.Component(a.questionStrip)).
				Child(a.composer(ctx, v, "orch-draft", "orch", "Message Solo…", func(text string, images []string) error {
					return a.b.SendToOrchestrator(context.Background(), text, images...)
				})))).
		Background(t.Background)
}

// orchState says what Solo is doing: waiting for an answer, working, or waiting on background work it
// started, for how long. It is empty when Solo is idle. A compact window's bar shows it beside Solo's
// name (orchBarState), and the foot of Solo's conversation in any window (soloStateLine).
func (a *App) orchState(ctx *ui.Context, v core.ThreadView) *ui.DivElement {
	t := ctx.Theme()
	state := ui.Div().Row().Align(ui.Center).Gap(space2).Shrink(1).MinWidth(ui.Px(0))
	switch {
	case v.Status == core.StatusWaiting:
		state = state.Child(statusMark(ctx, core.StatusWaiting)).
			Child(ui.Text("Waiting for your answer").FontSize(12).TextColor(t.Warning).Truncate().MinWidth(ui.Px(0)))
	case v.InTurn:
		state = state.Child(ui.ProgressRing().Size(ui.RingSmall).Label("Working")).
			Child(ui.Text(a.workingLabel(v)).FontSize(12).TextColor(t.TextMuted).Truncate().MinWidth(ui.Px(0)))
	case len(v.Background) > 0:
		now := a.now()
		tip := "Solo's turn ended, but work it started in the background still runs. It carries on when that finishes."
		for _, b := range v.Background {
			tip += "\n• " + b.Description + " (" + b.Kind() + ", " + elapsed(b.Started, now) + ")"
		}
		what := ui.Div().Row().Align(ui.Center).Gap(space1).Shrink(1).MinWidth(ui.Px(0)).
			Child(ui.Text("Waiting on " + bgLine(v)).FontSize(12).TextColor(t.TextMuted).Truncate().MinWidth(ui.Px(0)))
		if s := bgSince(v); !s.IsZero() {
			// Its own text, so a long name shortens before the time does.
			what = what.Child(ui.Text("· " + elapsed(s, now)).FontSize(12).TextColor(t.TextMuted).NoWrap().Shrink(0))
		}
		state = state.Tooltip(tip).Child(statusMark(ctx, core.StatusBackground)).Child(what)
	}
	return state
}

// orchBarState is orchState in a compact window's bar: a component of its own, which reads Solo's view
// and counts its background work's time without rendering the bar again.
func (a *App) orchBarState(ctx *ui.Context) ui.Element {
	v, _ := a.thread(core.OrchestratorID)
	ticking(ctx, len(v.Background) > 0)
	return a.orchState(ctx, v).Key("solo-bar-state")
}

// soloStateLine ends Solo's conversation while Solo isn't idle and the conversation doesn't show it
// already (streaming text, a question card): what orchState says, where the user looks to see whether
// Solo is still at work. While it waits on background work, Stop ends that work.
func (a *App) soloStateLine(ctx *ui.Context, v core.ThreadView) ui.Element {
	ticking(ctx, len(v.Background) > 0)
	line := lineRow().Align(ui.Center).Gap(space2).MinWidth(ui.Px(0)).Key("solo-state").
		Child(a.orchState(ctx, v).Grow(1))
	if !v.InTurn && len(v.Background) > 0 && v.CanInterrupt {
		id := v.ID
		line = line.Child(ui.Button("Stop").Icon(icons.Square).Ghost().Small().Key("solo-state-stop").
			Tooltip("Stop the work Solo runs in the background").
			OnClick(func() {
				a.run("Stopping", func(ctx context.Context) error { return a.b.Interrupt(ctx, id) })
			}))
	}
	return line
}

// soloBusy reports whether Solo's conversation ends with its state line (soloStateLine): in a turn, unless
// it streams text or asks a question, which say so themselves; or after its turn, while work it started
// in the background runs.
func soloBusy(v core.ThreadView) bool {
	if v.IsWorker || v.Status == core.StatusWaiting {
		return false
	}
	if v.InTurn {
		return len(v.Live) == 0 || v.Live[len(v.Live)-1].Thinking
	}
	return len(v.Background) > 0
}

// welcome is Solo's empty chat: what to do, and a few tasks to start with, written about the user's
// own repositories (hints.go). Without any, it offers to add them.
func (a *App) welcome(ctx *ui.Context) ui.Element {
	repos, plain := a.repos.Get(), a.plain.Get()
	col := ui.Div().Column().Align(ui.Center).Gap(space3).Width(ui.Full).MaxWidth(ui.Px(520)).
		Child(brandMark(48)).
		Child(title("What should we work on?").MarginTop(space2)).
		Child(muted(ctx, "Tell Solo what you want done. It starts a thread for each task, each one working in its own branch, keeps track of them, and reports back. You review and merge.").
			Align(ui.Center))
	if prompts := starterPrompts(repos, plain); len(prompts) > 0 {
		col = col.Child(ui.Div().Column().Gap(space2).Width(ui.Full).MarginTop(space4).
			Child(subtle(ctx, "Try one of these, or write your own")).
			Child(a.starterList(ctx, prompts)))
		if len(repos) > 0 {
			col = col.Child(caption(ctx, "Solo knows "+count(len(repos), "repository", "repositories")+": "+nameList(repos, 6)).
				Align(ui.Center).MarginTop(space2))
		}
	} else {
		col = col.Child(ui.Button("Add repository folders").Icon(icons.FolderGit2).Primary().Key("add-folders").
			OnClick(func() { a.openSettings("repos") }))
	}
	return ui.Div().Column().Center().Grow(1).Padding(space6).ScrollY().Child(col)
}

// row is one entry of a transcript list.
type row struct {
	key    string
	item   *core.Item
	steps  []*core.Item // a run of tool calls and thoughts, shown folded
	thread string       // the thread the folded steps act on, if they all act on one
	live   *core.LiveBlock
	busy   bool // Solo's state line at the end of its chat, while it works or waits on background work
	// queued is a message to Solo waiting for its turn to end, shown after everything else.
	queued *core.QueuedMessage
	// In Solo's chat (see summary.go): topic marks the first row of an exchange, which a rule sets apart
	// from the one before; summary is the line that says in words what the threads in said reported and
	// what Solo did in steps.
	topic   bool
	summary bool
	said    []*core.Item
}

// isStep reports whether an item is part of the agent's working steps rather than its conversation.
func isStep(it *core.Item) bool {
	return it.Kind == core.ItemTool || it.Kind == core.ItemThinking || it.Kind == core.ItemTask
}

// transcript renders a thread's items, its streaming text, and a working indicator, newest at the bottom.
func (a *App) transcript(ctx *ui.Context, v core.ThreadView, items []core.Item, scope string) ui.Element {
	rows := a.transcriptRows(scope, items)
	for i := range v.Live {
		// Solo's chat says it is thinking in its working line, not in a block of thoughts.
		if scope == "orch" && v.Live[i].Thinking {
			continue
		}
		rows = append(rows, row{key: fmt.Sprintf("live%d", i), live: &v.Live[i]})
	}
	// A thread's panel pins its working line above the composer; only Solo's transcript says it in line.
	if soloBusy(v) {
		rows = append(rows, row{key: "busy", busy: true})
	}
	// Messages the user sent Solo while it worked come last, each as the exchange it will start.
	for i := range v.Queued {
		rows = append(rows, row{key: fmt.Sprintf("queued%d", v.Queued[i].At.UnixNano()), queued: &v.Queued[i], topic: scope == "orch"})
	}
	h := a.listHandle(scope)
	atEnd := ui.Local(ctx, scope+"-at-end", true)
	// Open on the latest message; after that the list follows new ones while it is at the end, and
	// a message the user sends brings it back there (see composer).
	if started := ui.Local(ctx, scope+"-started", false); !started.Peek() && len(rows) > 0 {
		started.Set(true)
		h.ScrollToEnd()
	}
	list := ui.ListOf(rows, func(r row) string { return r.key }, func(_ int, r row) ui.Element {
		key := r.key
		// Rows are built only near the viewport; which ones are built tells the pill below what has
		// scrolled away.
		return ui.Component(func(rctx *ui.Context) ui.Element {
			a.markBuilt(rctx, scope, key, true)
			// Once for the row, not in each of its renders, which would add a callback each time.
			if once := ui.Local(rctx, "disposer", false); !once.Peek() {
				once.Set(true)
				rctx.OnDispose(func() { a.markBuilt(rctx, scope, key, false) })
			}
			col := ui.Div().Column().Width(ui.Full).MaxWidth(ui.Px(chatMaxWidth)).PaddingY(space1 + 2)
			if r.topic {
				// Solo's chat rules off each exchange from the one before.
				col = col.PaddingTop(space3).
					Child(ui.Div().Height(ui.Px(1)).Shrink(0).Background(rctx.Theme().Border).MarginBottom(space4).AccessHidden())
			}
			return ui.Div().Column().Align(ui.Center).PaddingX(gutter(rctx)).
				Child(col.Child(a.rowView(rctx, v, r)))
		})
	}).Handle(h).SelectionContainer().Key(scope + "-transcript").Label("Conversation").
		Style(ui.Style{Grow: 1, Padding: ui.Edges{Top: space4, Bottom: space2}}).
		OnAtEnd(func(end bool) { atEnd.Set(end) })
	if scope == "orch" {
		list = list.Bottom() // a thread reads from its top, like a document, rather than sinking to the composer
	}
	// The list and, floating over its foot, the way back to the latest message. Solo's chat has one
	// control there that also points at cards below that need the user, and another at its top for
	// those above; all float over the conversation and take no room from it. Selected text in it
	// can be quoted into the composer (see quotable).
	view := ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).Child(list)
	view = a.quotable(ctx, view, v, scope)
	if scope != "orch" {
		return view.Child(ui.Component(func(ctx *ui.Context) ui.Element {
			return jumpControl(ctx, h, bottomJump(atEnd.Get(), nil), false)
		}))
	}
	return view.
		Child(ui.If(!isCompact(ctx), topFade(ctx))). // a compact window has its bar above the chat
		Child(ui.Component(func(ctx *ui.Context) ui.Element {
			return jumpControl(ctx, h, topJump(a.offscreenActions(rows, true)), true)
		})).
		Child(ui.Component(func(ctx *ui.Context) ui.Element {
			return jumpControl(ctx, h, bottomJump(atEnd.Get(), a.offscreenActions(rows, false)), false)
		}))
}

// topFade fades the top of Solo's chat into its panel, which has no bar above it: a message scrolling
// away dissolves at the panel's edge rather than being cut by it. It is the shadow, in the panel's
// color, of a strip just above the chat, where the panel clips the strip itself: nothing lies over the
// rows, so they still take clicks and the wheel right up to the edge.
func topFade(ctx *ui.Context) ui.Element {
	const strip = 64 // tall enough that its shadow is solid along its edge
	return ui.Div().Absolute().Left(-space8).Right(-space8).Top(-strip).Height(ui.Px(strip)).AccessHidden().
		Shadow(ui.Shadow{Y: 16, Blur: 30, Color: ctx.Theme().Background})
}

// listHandle is the handle of the transcript list of scope ("orch", or "thread-" and its ID).
func (a *App) listHandle(scope string) *ui.ListHandle {
	if a.lists == nil {
		a.lists = map[string]*ui.ListHandle{}
	}
	h := a.lists[scope]
	if h == nil {
		h = ui.NewListHandle()
		a.lists[scope] = h
	}
	return h
}

// jump is what a floating control at an edge of a conversation offers.
type jump struct {
	latest bool  // a round arrow back to the latest message
	needs  []int // or: the rows past that edge that wait on the user; wins over latest
	target int   // the nearest of needs, which a click scrolls to
}

// bottomJump decides the one control at the foot of a conversation: the cards below that need the
// user if there are any, otherwise the way back to the latest message while scrolled up from it,
// otherwise nothing.
func bottomJump(atEnd bool, needsBelow []int) jump {
	if len(needsBelow) > 0 {
		return jump{needs: needsBelow, target: needsBelow[0]}
	}
	return jump{latest: !atEnd, target: -1}
}

// topJump points at the nearest of the cards above that need the user, if any.
func topJump(needsAbove []int) jump {
	if len(needsAbove) == 0 {
		return jump{target: -1}
	}
	return jump{needs: needsAbove, target: needsAbove[len(needsAbove)-1]}
}

// jumpControl floats j's control over the top (top=true) or foot of a conversation, centred, with a
// solid background and shadow so it reads over the text under it; a click scrolls to its target.
func jumpControl(ctx *ui.Context, h *ui.ListHandle, j jump, top bool) ui.Element {
	t := ctx.Theme()
	// A strip of no height, so the conversation under it still takes clicks and the wheel.
	strip := ui.Div().Absolute().Left(0).Right(0).Height(ui.Px(0)).ZIndex(1).Row().Justify(ui.Center)
	if top {
		strip = strip.Top(space3).Align(ui.Start)
	} else {
		strip = strip.Bottom(space3).Align(ui.End)
	}
	switch {
	case len(j.needs) > 0:
		target, icon, key := j.target, icons.ArrowDown, "offscreen-pill"
		if top {
			icon, key = icons.ArrowUp, "offscreen-pill-up"
		}
		label := "1 needs you"
		if len(j.needs) > 1 {
			label = fmt.Sprintf("%d need you", len(j.needs))
		}
		return strip.Child(ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space3).Height(ui.Px(28)).Radius(14).
			Shrink(0).Background(t.Surface).Border(1, t.Warning.Alpha(0.55)).Shadow(ui.ShadowMd).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(label).TabIndex(0).Key(key).
			OnClick(func() { h.ScrollToItem(target) }).
			Child(ui.Icon(icon).Size(14).Color(t.Warning)).
			Child(ui.Text(label).FontSize(12).Medium()))
	case j.latest:
		const size = 32
		return strip.Child(ui.Div().Size(ui.Px(size), ui.Px(size)).Radius(size/2).Center().Shrink(0).
			Background(t.Surface).Border(1, t.BorderStrong).Shadow(ui.ShadowMd).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label("Scroll to the latest message").TabIndex(0).Tooltip("Scroll to the latest").
			Key("to-latest").
			OnClick(func() { h.ScrollToEnd() }).
			Child(ui.Icon(icons.ArrowDown).Size(16).Color(t.TextMuted)))
	}
	return strip
}

// rowCache is the rows laid out for a transcript, and what they were laid out from.
type rowCache struct {
	first   *core.Item // the items' first element and their count: the same slice gives the same rows
	n       int
	threads int // how many threads there were: a tool call's row names its thread once the thread exists
	rows    []row
}

// transcriptRows lays out a transcript's items as rows, once for each list of items. Laying them out
// decodes every item, and a transcript renders again far more often than its items change: whenever
// its thread's streaming text does. The rows are not to be appended to in place.
func (a *App) transcriptRows(scope string, items []core.Item) []row {
	var first *core.Item
	if len(items) > 0 {
		first = &items[0]
	}
	threads := len(a.threads.Peek())
	if c := a.rows[scope]; c != nil && c.first == first && c.n == len(items) && c.threads == threads {
		return c.rows[:len(c.rows):len(c.rows)]
	}
	var rows []row
	if scope == "orch" {
		rows = a.orchRows(items)
	} else {
		rows = a.threadRows(items)
	}
	if scope == "orch" {
		rows = a.summarized(a.mergeHandoffs(rows))
	}
	a.rows[scope] = &rowCache{first: first, n: len(items), threads: threads, rows: rows}
	return rows[:len(rows):len(rows)]
}

// orchRows lays out Solo's chat one exchange at a time: each message to Solo, then for each of Solo's
// turns it led to, a row of everything Solo did while it worked, then its cards and its reply. An
// exchange can have several turns: work Solo ran in the background wakes it with no message. Solo's
// last message of each turn shows in full, a later turn's never hiding an earlier one's; what it said
// between steps goes with them, and a turn that ends in "(noted)" says nothing. The steps and the
// thread updates are then said in a sentence (see summarized).
func (a *App) orchRows(items []core.Item) []row {
	rows := make([]row, 0, len(items)/2+4)
	var seg []*core.Item // the turn's visible items
	var said *core.Item  // the turn's last message so far, "(noted)" too
	flush := func() {
		defer func() { seg, said = nil, nil }()
		if len(seg) == 0 {
			return
		}
		var fold []*core.Item
		var rest []row
		thread, mixed, worked := "", false, false
		for _, it := range seg {
			if !isStep(it) && (it.Kind != core.ItemAssistant || it == said) {
				rest = append(rest, row{key: fmt.Sprintf("i%d", it.ID), item: it})
				continue
			}
			fold = append(fold, it)
			worked = worked || it.Kind != core.ItemThinking
			if th := a.itemThread(it); th != "" {
				mixed = mixed || (thread != "" && th != thread)
				thread = th
			}
		}
		if mixed {
			thread = ""
		}
		// Thoughts alone aren't worth a line. The key is the turn's first item, so the line keeps its
		// open state as steps arrive.
		if worked {
			rows = append(rows, row{key: fmt.Sprintf("g%d", seg[0].ID), steps: fold, thread: thread})
		}
		rows = append(rows, rest...)
	}
	for i := range items {
		it := &items[i]
		switch it.Kind {
		case core.ItemAssistant:
			said = it
		case core.ItemTurn:
			// A turn's end (a steer's pause has none). One that went wrong shows, as the turn's last row.
			if visible(it) {
				seg = append(seg, it)
			}
			flush()
			continue
		}
		if !visible(it) {
			continue
		}
		if it.Kind == core.ItemUser {
			flush()
			rows = append(rows, row{key: fmt.Sprintf("i%d", it.ID), item: it})
			continue
		}
		seg = append(seg, it)
	}
	flush()
	return rows
}

// threadRows lays out a thread's transcript: its steps fold in runs between messages. Every run folds,
// a single step too, so what a thread did always reads as the same kind of line.
func (a *App) threadRows(items []core.Item) []row {
	rows := make([]row, 0, len(items)+4)
	var run []*core.Item
	runThread := ""
	flush := func() {
		if len(run) > 0 {
			rows = append(rows, row{key: fmt.Sprintf("g%d", run[0].ID), steps: run, thread: runThread})
		}
		run, runThread = nil, ""
	}
	for i := range items {
		it := &items[i]
		if !visible(it) {
			continue
		}
		if isStep(it) {
			if th := a.itemThread(it); th != runThread && (th != "" || it.Kind == core.ItemTool) {
				if len(run) > 0 {
					flush()
				}
				runThread = th
			}
			run = append(run, it)
			continue
		}
		flush()
		rows = append(rows, row{key: fmt.Sprintf("i%d", it.ID), item: it})
	}
	flush()
	return rows
}

// mergeHandoffs puts the actions Solo took on a thread right after that thread's update in the
// update's row, so one handoff is one row. A handoff that ends in a merge card is the card alone.
func (a *App) mergeHandoffs(rows []row) []row {
	out := rows[:0:0]
	for i := 0; i < len(rows); i++ {
		r := rows[i]
		if r.item != nil && r.item.Kind == core.ItemUser && i+1 < len(rows) {
			var d core.UserData
			r.item.Decode(&d)
			if d.Synthetic {
				if us := a.parseUpdates(ui.DarkTheme(), d.Text); len(us) == 1 && us[0].threadID != "" {
					next := rows[i+1]
					switch {
					case next.steps != nil && next.thread == us[0].threadID:
						r.steps, r.thread = next.steps, us[0].threadID
						i++
					case next.item != nil && next.item.Kind == core.ItemTool && a.itemThread(next.item) == us[0].threadID:
						r.steps, r.thread = []*core.Item{next.item}, us[0].threadID
						i++
					}
				}
			}
		}
		out = append(out, r)
	}
	// A handoff that ends in a merge proposal for the same thread is said by the card alone: the
	// update and the actions go.
	final := out[:0:0]
	for i := 0; i < len(out); i++ {
		r := out[i]
		thread, steps := r.thread, r.steps
		if steps == nil && r.item != nil && r.item.Kind == core.ItemTool {
			thread, steps = a.itemThread(r.item), []*core.Item{r.item}
		}
		if steps != nil && thread != "" && i+1 < len(out) {
			next := out[i+1]
			if next.item != nil && next.item.Kind == core.ItemProposal && next.item.Ref == thread {
				final = append(final, next)
				i++
				continue
			}
		}
		final = append(final, r)
	}
	return final
}

// visible reports whether an item is shown in the transcript.
func visible(it *core.Item) bool {
	switch it.Kind {
	case core.ItemTurn:
		var d core.TurnData
		it.Decode(&d)
		return d.Status != agent.TurnCompleted
	case core.ItemAssistant:
		var d core.TextData
		it.Decode(&d)
		return !core.IsNoted(d.Text)
	case core.ItemTask:
		// A task started by a tool call in the foreground is that call; its row says it all. Only a task
		// that runs on in the background after its call returned gets a row of its own.
		var d core.TaskData
		it.Decode(&d)
		return d.ToolUseID == "" || d.Background
	}
	return true
}

// workingLabel is what Solo's in-chat working line says: an API retry, the tool call in progress, or a
// line of what Solo last thought this turn, in place of the thoughts themselves.
func (a *App) workingLabel(v core.ThreadView) string {
	switch {
	case v.Retry != "":
		return v.Retry
	case v.Step != nil:
		return a.stepLabel(*v.Step) + "…"
	}
	if s := thoughtExcerpt(v.Thought); s != "" {
		return s
	}
	return "Thinking…"
}

func (a *App) rowView(ctx *ui.Context, v core.ThreadView, r row) ui.Element {
	t := ctx.Theme()
	switch {
	case r.busy:
		return a.soloStateLine(ctx, v)
	case r.live != nil:
		if r.live.Thinking {
			return thinkingView(ctx, r.live.Text, true)
		}
		return a.agentText(ctx, r.live.Text, viewBases(v), nil)
	case r.queued != nil:
		tag := ui.Div().Row().Align(ui.Center).Gap(4).PaddingRight(2).
			Tooltip("Solo reads this when it finishes what it's doing").
			Child(ui.Icon(icons.Clock).Size(12).Color(t.TextSubtle)).
			Child(ui.Text("Queued").FontSize(12).TextColor(t.TextSubtle))
		return a.userMessage(ctx, v, r.queued.Text, r.queued.Images, tag)
	case r.summary:
		return a.summaryView(ctx, r)
	case r.steps != nil:
		return a.stepsView(ctx, r.steps, r.thread)
	}
	it := r.item
	switch it.Kind {
	case core.ItemUser:
		var d core.UserData
		it.Decode(&d)
		if d.Synthetic {
			return a.updateView(ctx, d.Text)
		}
		if d.From == core.FromOrchestrator {
			return ui.Div().Column().Gap(space1).Align(ui.Start).
				Child(ui.Div().Row().Align(ui.Center).Gap(6).PaddingLeft(2).
					Child(brandMark(14)).
					Child(ui.Text("Solo").FontSize(12).Medium().TextColor(t.TextMuted))).
				Child(a.messageImages(ctx, d.Images, ui.Start)).
				Child(ui.Div().PaddingX(space3+2).PaddingY(space2+2).Radius(radiusL).MaxWidth(ui.Percent(92)).
					Background(t.Surface).Border(1, t.Border).
					Child(a.agentText(ctx, d.Text, viewBases(v), nil)))
		}
		var tag ui.Element
		if d.Steer {
			tag = ui.Div().Row().Align(ui.Center).Gap(4).PaddingRight(2).
				Tooltip("Sent while the thread worked: it stopped what it was doing to read this").
				Child(ui.Icon(icons.CornerUpRight).Size(12).Color(t.TextSubtle)).
				Child(ui.Text("Steered").FontSize(12).TextColor(t.TextSubtle))
		}
		return a.userMessage(ctx, v, d.Text, d.Images, tag)
	case core.ItemAssistant:
		var d core.TextData
		it.Decode(&d)
		return a.agentText(ctx, d.Text, viewBases(v), d.Images)
	default:
		return a.stepView(ctx, it, v)
	}
}

// userMessage renders a message the user sent: its images, its text in a bubble, and under them tag,
// if any ("Steered", "Queued").
func (a *App) userMessage(ctx *ui.Context, v core.ThreadView, text string, images []string, tag ui.Element) ui.Element {
	col := ui.Div().Column().Align(ui.End).Gap(space1 + 2).MaxWidth(ui.Percent(85)).
		Child(a.messageImages(ctx, images, ui.End))
	if strings.TrimSpace(text) != "" {
		// The bubble is as wide as its text up to the column's width: a code block with a long line
		// scrolls inside it rather than widening it past the column.
		col = col.Child(ui.Div().PaddingX(space2 + 2).PaddingY(space1 + 2).Radius(radiusL).
			MaxWidth(ui.Percent(100)).Background(ctx.Theme().SurfaceActive).
			Child(a.md(ctx, hardBreaks(text), viewBases(v))))
	}
	if tag != nil {
		col = col.Child(tag)
	}
	return ui.Div().Row().Justify(ui.End).Child(col)
}

// stepView renders a single item that isn't a message.
func (a *App) stepView(ctx *ui.Context, it *core.Item, v core.ThreadView) ui.Element {
	t := ctx.Theme()
	switch it.Kind {
	case core.ItemAssistant:
		var d core.TextData
		it.Decode(&d)
		return a.agentText(ctx, d.Text, viewBases(v), d.Images) // on the edge of the steps around it
	case core.ItemThinking:
		var d core.TextData
		it.Decode(&d)
		return thinkingView(ctx, d.Text, false)
	case core.ItemTool:
		var d core.ToolData
		it.Decode(&d)
		return a.toolView(ctx, d)
	case core.ItemTask:
		var d core.TaskData
		it.Decode(&d)
		return taskView(ctx, d)
	case core.ItemQuestion:
		var d core.QuestionData
		it.Decode(&d)
		return a.questionCard(ctx, v.ID, d)
	case core.ItemProposal:
		var d core.ProposalData
		it.Decode(&d)
		return a.proposalCard(ctx, d)
	case core.ItemTurn:
		var d core.TurnData
		it.Decode(&d)
		return turnView(ctx, d)
	case core.ItemNotice:
		var d core.NoticeData
		it.Decode(&d)
		c := t.TextSubtle
		switch d.Level {
		case "error":
			c = t.Danger
		case "warning":
			c = t.Warning
		}
		return ui.Div().Row().Justify(ui.Center).Child(ui.Text(d.Text).FontSize(12).TextColor(c).Align(ui.Center))
	}
	return ui.Div()
}

// stepsView folds a run of tool calls and thoughts into one line: how many steps, and the latest.
func (a *App) stepsView(ctx *ui.Context, steps []*core.Item, threadID string) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		open := ui.Local(ctx, "open", false)
		running, failures := false, 0
		latest := ""
		tools := 0
		for _, it := range steps {
			if it.Kind == core.ItemTask {
				// A task that runs on in the background: the line names it, and turns while it runs.
				var d core.TaskData
				it.Decode(&d)
				tools++
				running = running || d.Status == "running" || d.Status == "pending" || d.Status == ""
				latest = d.Description
				continue
			}
			if it.Kind != core.ItemTool {
				continue
			}
			tools++
			var d core.ToolData
			it.Decode(&d)
			running = running || !d.Done
			if d.IsError {
				failures++
			}
			_, verb, detail := a.describeTool(d)
			if threadID != "" {
				detail = "" // the thread is named by its chip
			}
			latest = strings.TrimSpace(verb + " " + detail)
		}
		// Only what needs a look gets a mark: a step at work, or one that failed. Done is the quiet default.
		var state ui.Element
		if running {
			state = ui.ProgressRing().Size(ui.RingSmall).Label("Running")
		}
		noun := "step"
		if threadID != "" {
			noun = "action"
		}
		label := fmt.Sprintf("%d %ss", len(steps), noun)
		if len(steps) == 1 {
			label = "1 " + noun
		}
		if tools == 0 {
			latest = "Thought" // a run of thoughts alone
		}
		var failedText ui.Element = ui.Div()
		if failures > 0 {
			failedText = ui.Text(fmt.Sprintf("· %d failed", failures)).FontSize(13).Medium().TextColor(t.Danger).NoWrap().Shrink(0)
			if !running {
				state = nil
			}
		}
		head := lineRow().Align(ui.Center).Gap(space2).Height(ui.Px(28)).Radius(radiusS).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(label).TabIndex(0).
			OnClick(func() { open.Update(func(o bool) bool { return !o }) }).
			Child(ui.Icon(map[bool]*icons.Icon{true: icons.ChevronDown, false: icons.ChevronRight}[open.Get()]).Size(14).Color(t.TextMuted)).
			Child(ui.If(threadID != "", a.threadChip(ctx, threadID))).
			Child(ui.Text(label).FontSize(13).Medium().TextColor(t.TextMuted).NoWrap().Shrink(0)).
			Child(failedText).
			Child(ui.If(state != nil, state)).
			Child(ui.Text(latest).FontSize(12).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0)))
		col := ui.Div().Column().Gap(2).Child(head)
		if open.Get() {
			inner := unfoldedSteps(ctx)
			for _, it := range steps {
				inner = inner.Child(a.stepView(ctx, it, core.ThreadView{}))
			}
			col = col.Child(inner)
		}
		return col
	})
}

// updateLine matches a thread update: `Thread <id> "<title>" (<repos>) <what>`, or with a colon after
// the repos for pull-request events (`... (<repos>): checks passed on PR #3 (<url>).`).
var updateLine = regexp.MustCompile(`^Thread ([0-9a-zA-Z-]{4,}) ("(?:[^"\\]|\\.)*") \(([^)]*)\):? (.*)$`)

// changeRequest matches a pull or merge request as an update names it: `PR #3 (<url>)`, or
// `the <repo> PR #3 (<url>)` in a multi-repo thread.
var changeRequest = regexp.MustCompile(`^(?:the (\S+) )?((?:PR|MR) #\d+)(?: \([^)]*\))?`)

// checkURL matches the link failedChecks puts after a check's name.
var checkURL = regexp.MustCompile(` \(https?://[^)]*\)`)

// update is one line of a thread update the app sent Solo.
type update struct {
	threadID, name, repo, what string
	// quiet: news that asks nothing of the user here (merged, a PR opened, finished work, whose
	// card follows), shown lighter than what does: a question or a failure.
	quiet bool
	icon  *icons.Icon
	color ui.Color
}

// parseUpdates reads the thread updates in a synthetic message, one per line.
func (a *App) parseUpdates(t *ui.Theme, text string) []update {
	var out []update
	for _, l := range strings.Split(text, "\n") {
		m := updateLine.FindStringSubmatch(strings.TrimPrefix(l, "- "))
		if m == nil {
			continue
		}
		name, err := strconv.Unquote(m[2])
		if err != nil {
			name = strings.Trim(m[2], `"`)
		}
		u := update{threadID: a.findThreadID(m[1]), name: name, repo: m[3], what: m[4], icon: icons.Activity, color: t.TextMuted}
		describeUpdate(t, &u, m[4])
		u.quiet = u.color != t.Danger && u.color != t.Warning
		out = append(out, u)
	}
	return out
}

// describeUpdate sets an update's icon, color and short text from what the line says happened.
func describeUpdate(t *ui.Theme, u *update, rest string) {
	info := ui.Hex(pal(t).info)
	switch {
	case strings.HasPrefix(rest, "finished its turn."):
		u.icon, u.color, u.what = icons.CircleCheck, t.Success, "finished"
		if s := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(rest, "finished its turn."), " Summary:")); s != "" {
			u.what += " · " + firstSentence(stripMarkdown(s))
		}
	case strings.HasPrefix(rest, "ended its turn but isn't finished"):
		// "… waits on work it runs in the background (command "Run benchmarks", running 4m), …"
		u.icon, u.color, u.what = icons.Hourglass, t.TextMuted, "waiting on background work"
		if _, after, ok := strings.Cut(rest, "in the background ("); ok {
			if what, _, ok := strings.Cut(after, ", running"); ok {
				if q, err := strconv.Unquote(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(what, "command "), "subagent "), "task ")); err == nil {
					u.what += " · " + q
				}
			}
		}
	case strings.HasPrefix(rest, "resolved the conflicts"):
		u.icon, u.color, u.what = icons.GitMerge, info, "resolved its merge conflicts"
	case strings.HasPrefix(rest, "failed:"):
		u.icon, u.color = icons.CircleAlert, t.Danger
		u.what = "failed · " + strings.TrimSpace(strings.TrimPrefix(rest, "failed:"))
	case strings.HasPrefix(rest, "is asking a question"):
		u.icon, u.color, u.what = icons.MessageCircleQuestionMark, t.Warning, "has a question"
	case strings.HasPrefix(rest, "was interrupted"):
		u.icon, u.color, u.what = icons.CircleStop, t.TextSubtle, "was stopped"
	case strings.HasPrefix(rest, "is merged into "):
		u.icon, u.color = icons.GitMerge, info
		u.what = "merged into " + strings.TrimSuffix(strings.TrimPrefix(rest, "is merged into "), ".")
	case strings.HasPrefix(rest, "was merged into "): // older history
		u.icon, u.color = icons.GitMerge, info
		u.what = "merged into " + strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(rest, "was merged into "), " by the user."), ".")
	case strings.HasPrefix(rest, "every repository's change has landed"):
		u.icon, u.color, u.what = icons.GitMerge, info, "merged in every repository"
	case strings.HasPrefix(rest, "was discarded"):
		u.icon, u.color, u.what = icons.Trash, t.TextSubtle, "was discarded"
	case strings.HasPrefix(rest, "the user opened "):
		if cr, tail, ok := cutChangeRequest(strings.TrimPrefix(rest, "the user opened ")); ok {
			u.icon, u.color, u.what = icons.GitPullRequest, info, "opened "+cr
			if strings.Contains(tail, "Auto-merge is on") {
				u.what += " · auto-merge on"
			}
		}
	case strings.HasPrefix(rest, "checks passed on "):
		if cr, _, ok := cutChangeRequest(strings.TrimPrefix(rest, "checks passed on ")); ok {
			u.icon, u.color, u.what = icons.CircleCheck, t.Success, "checks passed on "+cr
		}
	case strings.HasPrefix(rest, "checks failed on "):
		if cr, tail, ok := cutChangeRequest(strings.TrimPrefix(rest, "checks failed on ")); ok {
			u.icon, u.color, u.what = icons.CircleAlert, t.Danger, "checks failed on "+cr
			// The names come before the instructions for Solo, which start a new sentence.
			names, _, _ := strings.Cut(strings.TrimPrefix(tail, ": "), ". ")
			if names = strings.TrimSuffix(checkURL.ReplaceAllString(names, ""), "."); names != "" {
				u.what += " · " + names
			}
		}
	default:
		cr, tail, ok := cutChangeRequest(rest)
		if !ok {
			return
		}
		switch {
		case strings.HasPrefix(tail, " merged into "):
			u.icon, u.color = icons.GitMerge, info
			u.what = cr + " merged into " + strings.TrimSuffix(strings.TrimPrefix(tail, " merged into "), ".")
		case strings.HasPrefix(tail, " was closed without merging"):
			u.icon, u.color, u.what = icons.GitPullRequestClosed, t.TextSubtle, cr+" closed without merging"
		}
	}
}

// cutChangeRequest reads the pull or merge request an update starts with, shortened to "PR #3" (or
// "web PR #3" in a multi-repo thread), and returns what follows it.
func cutChangeRequest(s string) (cr, tail string, ok bool) {
	m := changeRequest.FindStringSubmatch(s)
	if m == nil {
		return "", s, false
	}
	cr = m[2]
	if m[1] != "" {
		cr = m[1] + " " + cr
	}
	return cr, s[len(m[0]):], true
}

// updateLine renders one thread update; trailing goes before the chevron (an actions toggle).
func (a *App) updateRow(ctx *ui.Context, u update, trailing ui.Element) *ui.DivElement {
	t := ctx.Theme()
	name := ui.Text(u.name).FontSize(13).Medium()
	iconColor, whatColor := u.color, t.TextMuted
	if u.quiet {
		name = ui.Text(u.name).FontSize(13).TextColor(t.TextMuted)
		iconColor, whatColor = u.color.Alpha(0.6), t.TextSubtle
	}
	var line *ui.DivElement
	if a.chatWidth(ctx) < 640 {
		// Narrow: the thread on one line and what happened under it, so neither is cut to a few words.
		line = lineRow().Align(ui.Start).Gap(space2).PaddingY(3).Radius(radiusS).
			Child(ui.Div().Height(ui.Px(18)).Row().Align(ui.Center).Child(ui.Icon(u.icon).Size(14).Color(iconColor))).
			Child(ui.Div().Column().Gap(1).Grow(1).MinWidth(ui.Px(0)).
				Child(name.Truncate()).
				Child(ui.Text(u.repo + " · " + u.what).FontSize(12).TextColor(whatColor).Truncate()))
	} else {
		line = lineRow().Align(ui.Center).Gap(space2).Height(ui.Px(26)).Radius(radiusS).
			Child(ui.Icon(u.icon).Size(14).Color(iconColor)).
			Child(ui.Text(u.repo).FontSize(12).TextColor(t.TextSubtle).Width(ui.Px(56)).Shrink(0).Truncate()).
			Child(name.NoWrap().Shrink(0).MaxWidth(ui.Percent(45)).Truncate()).
			Child(ui.Text(u.what).FontSize(12).TextColor(whatColor).Truncate().Grow(1).MinWidth(ui.Px(0)))
	}
	if trailing != nil {
		line = line.Child(trailing)
	}
	if u.threadID != "" {
		id := u.threadID
		line = line.Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(u.name + " " + u.what).TabIndex(0).
			OnClick(func() { a.openThread(id, "conversation") }).
			Child(ui.Icon(icons.ChevronRight).Size(13).Color(t.TextSubtle))
	}
	return line
}

// updateView shows a thread update the app sent Solo: one clickable line per thread.
func (a *App) updateView(ctx *ui.Context, text string) ui.Element {
	col := ui.Div().Column().Gap(2)
	for _, u := range a.parseUpdates(ctx.Theme(), text) {
		col = col.Child(a.updateRow(ctx, u, nil))
	}
	return col
}

func itemTool(it *core.Item) core.ToolData {
	var d core.ToolData
	it.Decode(&d)
	return d
}

func (a *App) findThreadID(prefix string) string {
	for _, th := range a.threads.Peek() {
		if strings.HasPrefix(th.ID, prefix) {
			return th.ID
		}
	}
	return ""
}

func firstSentence(s string) string {
	s = firstLine(s)
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i]
	}
	return strings.TrimSuffix(s, ".")
}

func thinkingView(ctx *ui.Context, text string, live bool) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		open := ui.Local(ctx, "open", false) // closed, live or not: the working line already says it thinks
		label := "Thought"                   // a finished thought; "Thinking" is only what is happening now
		if live {
			label = "Thinking"
		}
		head := lineRow().Align(ui.Center).Gap(6).Height(ui.Px(26)).Cursor(ui.CursorPointer).
			Role(access.RoleButton).Label(label).TabIndex(0).
			OnClick(func() { open.Update(func(o bool) bool { return !o }) }).
			Child(ui.Icon(icons.Brain).Size(13).Color(t.TextSubtle)).
			Child(subtle(ctx, label)).
			Child(ui.Icon(map[bool]*icons.Icon{true: icons.ChevronDown, false: icons.ChevronRight}[open.Get()]).Size(12).Color(t.TextSubtle))
		col := ui.Div().Column().Gap(space1).Child(head)
		if open.Get() {
			// The thought sits under the label, past the icon (13) and the gap (6).
			col = col.Child(ui.Div().PaddingLeft(13 + 6).
				Child(ui.Text(text).FontSize(12).TextColor(t.TextSubtle).Italic().Selectable()))
		}
		return col
	})
}

// toolView is one tool call: a compact line that opens to show its result.
func (a *App) toolView(ctx *ui.Context, d core.ToolData) ui.Element {
	icon, label, detail := a.describeTool(d)
	threadID := a.toolThread(d)
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		open := ui.Local(ctx, "open", false)
		var state ui.Element
		switch {
		case !d.Done:
			state = ui.ProgressRing().Size(ui.RingSmall).Label("Running")
		case d.IsError:
			state = ui.Icon(icons.CircleAlert).Size(13).Color(t.Danger)
		default:
			state = ui.Div() // done is the quiet default
		}
		head := lineRow().Align(ui.Center).Gap(space2).Height(ui.Px(26)).Radius(radiusS).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(label + " " + detail).TabIndex(0).
			OnClick(func() { open.Update(func(o bool) bool { return !o }) }).
			Child(ui.Icon(icon).Size(14).Color(t.TextMuted)).
			Child(ui.Text(label).FontSize(13).Medium().TextColor(t.TextMuted).NoWrap().Shrink(0)).
			Child(state)
		if threadID != "" {
			head = head.Child(a.threadChip(ctx, threadID)).Child(ui.Div().Grow(1))
		} else {
			head = head.Child(ui.Text(detail).FontSize(12).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0)))
		}
		col := ui.Div().Column().Gap(space1).Child(head)
		if open.Get() {
			body := ui.Div().Column().Gap(space2).MarginLeft(14+space2).Padding(space3).Radius(radiusM).
				Background(t.Surface).Border(1, t.Border)
			if len(d.Input) > 2 {
				body = body.Child(ui.Text(prettyJSON(d.Input)).Mono().FontSize(12).TextColor(t.TextMuted).MaxLines(12).Selectable())
			}
			if d.Result != "" {
				c := t.Text
				if d.IsError {
					c = t.Danger
				}
				body = body.Child(ui.Text(d.Result).Mono().FontSize(12).TextColor(c).MaxLines(24).Selectable())
			}
			col = col.Child(body)
		}
		return col
	})
}

// describeTool names a tool call for people: an icon, a verb phrase and a detail.
func (a *App) describeTool(d core.ToolData) (*icons.Icon, string, string) {
	var in map[string]any
	_ = json.Unmarshal(d.Input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	threadName := func() string {
		id := str("id")
		for _, th := range a.threads.Peek() {
			if strings.HasPrefix(th.ID, id) && id != "" {
				return th.Title
			}
		}
		return id
	}
	switch strings.TrimPrefix(d.Name, "mcp__solo__") {
	case "spawn_thread":
		return icons.CirclePlay, "Started thread", fmt.Sprintf("%s · %s", str("title"), str("repo"))
	case "message_thread":
		return icons.MessageSquare, "Messaged", threadName()
	case "get_thread":
		return icons.Eye, "Checked", threadName()
	case "read_thread":
		return icons.Eye, "Read transcript", threadName()
	case "get_diff":
		return icons.FileCode, "Reviewed changes", threadName()
	case "list_threads":
		return icons.ListTodo, "Listed threads", ""
	case "list_repos":
		return icons.FolderGit2, "Listed repositories", ""
	case "remember":
		return icons.BookmarkPlus, "Remembered", str("text")
	case "recall":
		qs, _ := in["queries"].([]any)
		var words []string
		for _, q := range qs {
			if w, ok := q.(string); ok {
				words = append(words, w)
			}
		}
		return icons.Search, "Searched memory", strings.Join(words, " · ")
	case "forget":
		return icons.BookmarkX, "Forgot a memory", ""
	case "list_todos":
		return icons.ListTodo, "Read the to-do list", ""
	case "add_todo":
		return icons.ListPlus, "Added to do", str("text")
	case "update_todo":
		if done, _ := in["done"].(bool); done {
			return icons.CircleCheck, "Ticked off", str("text")
		}
		return icons.ListTodo, "Updated to do", str("text")
	case "remove_todo":
		return icons.ListX, "Removed to do", ""
	case "init_repo":
		return icons.GitBranchPlus, "Put in git", filepath.Base(str("folder"))
	case "interrupt_thread":
		return icons.CircleStop, "Stopped", threadName()
	case "answer_question":
		return icons.MessageCircleQuestionMark, "Answered", threadName()
	case "propose_merge":
		return icons.GitMerge, "Proposed merge", threadName()
	case "Read":
		return icons.FileCode, "Read", filepath.Base(str("file_path"))
	case "Edit", "MultiEdit":
		return icons.FilePen, "Edited", filepath.Base(str("file_path"))
	case "Write":
		return icons.FilePen, "Wrote", filepath.Base(str("file_path"))
	case "Grep":
		return icons.Search, "Searched", str("pattern")
	case "Glob":
		return icons.Search, "Found files", str("pattern")
	case "Bash":
		// Claude says what a command is for ("Measure baseline durations"): lead with that, then the command.
		if desc := strings.TrimSpace(str("description")); desc != "" {
			return icons.Terminal, desc, firstLine(str("command"))
		}
		return icons.Terminal, "Ran", firstLine(str("command"))
	case "WebFetch":
		return icons.Globe, "Fetched", str("url")
	case "WebSearch":
		return icons.Globe, "Searched the web", str("query")
	case "AskUserQuestion":
		return icons.MessageCircleQuestionMark, "Asked you", ""
	case "TodoWrite":
		return icons.ListTodo, "Updated the plan", ""
	case "Agent", "Task":
		return icons.Bot, "Subagent", str("description")
	case "ToolSearch":
		return icons.Search, "Loaded tools", ""
	}
	return icons.Wrench, d.Name, ""
}

func taskView(ctx *ui.Context, d core.TaskData) ui.Element {
	t := ctx.Theme()
	var state ui.Element
	switch d.Status {
	case "running", "pending", "":
		state = ui.ProgressRing().Size(ui.RingSmall).Label("Running")
	case "completed":
		state = ui.Icon(icons.Check).Size(13).Color(t.TextSubtle)
	case "stopped", "killed":
		state = ui.Icon(icons.CircleStop).Size(13).Color(t.TextSubtle)
	default:
		state = ui.Icon(icons.CircleAlert).Size(13).Color(t.Danger)
	}
	detail := d.Summary
	if detail == "" && d.ToolUses > 0 {
		detail = fmt.Sprintf("%d tool uses", d.ToolUses)
	}
	icon := icons.Bot
	if strings.Contains(d.TaskType, "bash") || strings.Contains(d.TaskType, "shell") {
		icon = icons.SquareTerminal
	}
	return lineRow().Align(ui.Center).Gap(space2).Height(ui.Px(26)).
		Child(ui.Icon(icon).Size(14).Color(t.TextMuted)).
		Child(ui.Text(d.Description).FontSize(13).Medium().TextColor(t.TextMuted).NoWrap().Shrink(0)).
		Child(ui.Text(detail).FontSize(12).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0))).
		Child(state)
}

func turnView(ctx *ui.Context, d core.TurnData) ui.Element {
	t := ctx.Theme()
	if d.Status == agent.TurnInterrupted || d.Status == agent.TurnCancelled {
		return lineRow().Align(ui.Center).Gap(6).
			Child(ui.Icon(icons.CircleStop).Size(13).Color(t.TextSubtle)).
			Child(subtle(ctx, "Stopped"))
	}
	msg := d.Error
	if msg == "" {
		msg = "The turn failed."
	}
	return ui.Div().Row().Gap(space2).Padding(space3).Radius(radiusM).Background(t.Danger.Alpha(0.08)).
		Border(1, t.Danger.Alpha(0.3)).
		Child(ui.Icon(icons.CircleAlert).Size(15).Color(t.Danger)).
		Child(ui.Text(msg).FontSize(13).TextColor(t.Text).Selectable().Grow(1).MinWidth(ui.Px(0)))
}

// cardPad is the padding of the cards in a conversation: the merge card and the question card.
const cardPad = space3

// proposalCard is the orchestrator's recommendation to merge a thread, with the Merge button.
func (a *App) proposalCard(ctx *ui.Context, d core.ProposalData) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		th, _ := a.thread(d.ThreadID)
		var actions *ui.DivElement
		switch th.Status {
		case core.StatusMerged, core.StatusDiscarded:
			actions = ui.Div().Row().Align(ui.Center).Child(statusPill(ctx, th))
		default:
			id, title := d.ThreadID, th.Title
			requestChanges := func() {
				a.orchDraft.Set("Changes for “" + title + "”: ")
				a.orchFocus.Focus()
			}
			// A narrow chat column keeps Merge and Review changes on the row and moves the rest to "…".
			narrow := a.chatWidth(ctx) < 640
			// The same control as the thread's header, and when it can't land yet, the same disabled
			// button saying why.
			land := a.landAction(ctx, th, false)
			if land == nil {
				land = a.landBlocked(th)
			}
			actions = ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(land).
				Child(a.prLink(ctx, th)).
				Child(ui.Button("Review changes").Secondary().Small().OnClick(func() { a.openThread(id, "changes") }))
			if !narrow {
				actions = actions.Child(ui.Button("Request changes").Ghost().Small().
					Tooltip("Tell Solo what to change; it passes it on to the thread").OnClick(requestChanges))
			}
			actions = actions.Child(ui.DropdownMenu(ui.IconButton(icons.Ellipsis).Ghost().Small().Label("More").Tooltip("More"),
				func() []ui.MenuItem {
					var items []ui.MenuItem
					if narrow {
						items = append(items, ui.MenuItem{Label: "Request changes", Icon: icons.MessageSquare, OnClick: requestChanges})
					}
					if th.CanDone {
						label := "Done"
						if asksToLand(th) {
							label = "Mark done…"
						}
						items = append(items, ui.MenuItem{Label: label, Icon: icons.Check, OnClick: func() { a.markDone(th) }})
					}
					return append(items, ui.MenuItem{Label: "Discard thread…", Icon: icons.Trash, OnClick: func() { a.confirmDiscard(th) }})
				}))
		}
		if isCompact(ctx) {
			actions.Wrap()
		}
		card := ui.Div().Column().Gap(space3).Padding(cardPad).Radius(radiusL).Background(t.Surface).Key("proposal-"+d.ThreadID).
			Border(1, t.BorderStrong).
			Child(a.proposalHeader(ctx, th)).
			Child(a.agentText(ctx, d.Summary, a.threadBases(d.ThreadID), nil)).
			Child(actions)
		return card
	})
}

// questionCard is an AskUserQuestion in a transcript: open, or how it was answered.
func (a *App) questionCard(ctx *ui.Context, threadID string, d core.QuestionData) ui.Element {
	t := ctx.Theme()
	if d.Cancelled || len(d.Answers) > 0 || len(d.Questions) == 0 {
		var parts []string
		for _, q := range d.Questions {
			ans := d.Answers[q.Question]
			if d.Cancelled {
				ans = "skipped"
			}
			parts = append(parts, fmt.Sprintf("%s → %s", q.Question, ans))
		}
		return lineRow().Align(ui.Center).Gap(6).
			Child(ui.Icon(icons.MessageCircleQuestionMark).Size(13).Color(t.TextSubtle)).
			Child(subtle(ctx, strings.Join(parts, " · ")))
	}
	return a.askForm(ctx, threadID, d.ToolUseID, d.Questions, "")
}

// askForm shows questions with their options; picking options answers, as do the number keys. title
// names the asking thread when the form is shown outside it.
func (a *App) askForm(ctx *ui.Context, threadID, toolUseID string, qs []agent.Question, title string) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		bases := a.threadBases(threadID)
		answers := ui.Local(ctx, "answers", map[string]string{})
		other := ui.Local(ctx, "other", "")
		cur := answers.Get()
		var q agent.Question
		for _, cand := range qs {
			if _, ok := cur[cand.Question]; !ok {
				q = cand
				break
			}
		}
		submit := func(ans map[string]string) {
			a.run("Answering", func(context.Context) error { return a.b.Answer(threadID, toolUseID, ans) })
		}
		pick := func(label string) {
			next := map[string]string{}
			for k, v := range cur {
				next[k] = v
			}
			next[q.Question] = label
			if len(next) == len(qs) {
				submit(next)
			}
			answers.Set(next)
			other.Set("")
		}
		typ := "Question"
		if len(qs) > 1 {
			typ = fmt.Sprintf("Question %d of %d", len(cur)+1, len(qs))
		}
		var head ui.Element
		if title != "" {
			head = ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(ui.Div().Grow(1).MinWidth(ui.Px(0)).Child(a.cardHeader(ctx, icons.MessageCircleQuestionMark, t.Warning, typ, threadID))).
				Child(ui.IconButton(icons.ChevronDown).Ghost().Small().Label("Fold the question").Tooltip("Fold").
					OnClick(func() {
						next := map[string]bool{toolUseID: true}
						for k, v := range a.foldedAsks.Peek() {
							next[k] = v
						}
						a.foldedAsks.Set(next)
					}))
		} else {
			head = a.cardHeader(ctx, icons.MessageCircleQuestionMark, t.Warning, typ, "")
		}
		opts := ui.Div().Column().Gap(space1)
		for i, o := range q.Options {
			label := o.Label
			opt := ui.Component(func(ctx *ui.Context) ui.Element {
				hover := ui.Local(ctx, "hover", false)
				o2 := ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space3).PaddingY(space2).
					Radius(radiusM).Border(1, t.Border).Background(t.Background).Cursor(ui.CursorPointer).
					Hover(ui.Style{Background: t.SurfaceHover, BorderColor: t.TextSubtle}).
					OnMouseEnter(func() { hover.Set(true) }).OnMouseLeave(func() { hover.Set(false) }).
					Role(access.RoleButton).Label("Answer: " + label).TabIndex(0).OnClick(func() { pick(label) }).
					Key("opt-" + label)
				text := ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).Child(ui.Text(o.Label).FontSize(13).Medium())
				if o.Description != "" {
					text = text.Child(a.pathText(o.Description, bases).FontSize(12).TextColor(t.TextMuted))
				}
				o2 = o2.Child(text)
				// Picking an option answers at once; the arrow says so on hover, the key otherwise.
				if hover.Get() {
					o2 = o2.Child(ui.Icon(icons.ArrowRight).Size(14).Color(t.Link))
				} else if i < 9 && !a.Touch {
					o2 = o2.Child(ui.Kbd(fmt.Sprint(i + 1)))
				}
				return o2
			})
			opts = opts.Child(opt)
		}
		sendOther := func() {
			if s := strings.TrimSpace(other.Peek()); s != "" {
				pick(s)
			}
		}
		otherRow := ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(ui.TextInput(other).Placeholder("Or write your own answer").Label("Your answer").Small().Width(ui.Full).
				OnSubmit(func(string) { sendOther() })).
			Child(ui.If(strings.TrimSpace(other.Get()) != "", ui.Button("Send").Primary().Small().OnClick(sendOther))).
			Child(ui.Button("Skip").Secondary().Small().
				Tooltip("Skip the question; the agent carries on with its own judgment").
				OnClick(func() {
					a.run("Skipping", func(context.Context) error { return a.b.Dismiss(threadID, toolUseID) })
				}))
		all := q.Question
		for _, o := range q.Options {
			all += "\n" + o.Description
		}
		return ui.Div().Column().Gap(space3).Padding(cardPad).Radius(radiusL).Background(t.Surface).
			Border(1, t.Warning.Alpha(0.4)).
			ContextMenu(func() []ui.MenuItem { return a.pathMenu(findPaths(all, bases)) }).
			OnKeyDown(func(e *ui.KeyEvent) {
				if e.Key >= input.Key1 && e.Key <= input.Key9 && e.Mods == 0 {
					if i := int(e.Key - input.Key1); i < len(q.Options) {
						e.StopPropagation()
						pick(q.Options[i].Label)
					}
				}
			}).
			Child(head).
			Child(a.pathText(q.Question, bases).FontSize(14).Medium()).
			Child(opts).
			Child(otherRow)
	})
}

// questionStrip shows the workers' open questions above the orchestrator's composer. A lone question
// shows in full; several fold to a line each, so a busy afternoon's questions never push Solo's chat
// away, and a click opens one. The thread open beside the chat asks in its own panel instead, and
// while it is open the others stay folded too.
func (a *App) questionStrip(ctx *ui.Context) ui.Element {
	pending := a.pending.Get()
	folded := a.foldedAsks.Get()
	writing := a.orchWriting.Get() // the question folds while the user writes to Solo
	type ask struct {
		th core.ThreadView
		pq core.PendingQuestion
	}
	var asks []ask
	// The strip follows the questions and the threads that ask them, not the whole list of threads,
	// which changes with every chunk of streaming text. The list only gives their order.
	for _, t := range a.threads.Peek() {
		qs := pending[t.ID]
		if len(qs) == 0 || t.ID == core.OrchestratorID || t.ID == a.openID.Get() {
			continue
		}
		if th, ok := a.thread(t.ID); ok {
			asks = append(asks, ask{th, qs[0]})
		}
	}
	open := ""
	if !writing {
		want := a.askOpen.Get()
		for _, k := range asks {
			if key := k.pq.ToolUseID; key == want && !folded[key] {
				open = key
			}
		}
		if open == "" && len(asks) == 1 && a.openID.Get() == "" && !folded[asks[0].pq.ToolUseID] {
			open = asks[0].pq.ToolUseID
		}
	}
	col := ui.Div().Column().Gap(space1 + 2)
	var rest []ask
	for _, k := range asks {
		if key := k.pq.ToolUseID; key == open {
			col = col.Child(ui.Div().Key("ask-" + key).
				Child(a.askForm(ctx, k.th.ID, key, k.pq.Questions, k.th.Title+" asks")))
		} else {
			rest = append(rest, k)
		}
	}
	// The open thread's question asks in its panel, but still counts, so the number matches the rail.
	inPanel := 0
	if id := a.openID.Get(); id != "" && len(pending[id]) > 0 {
		inPanel = 1
	}
	switch {
	case len(rest) == 1 && inPanel == 0:
		col = col.Child(a.foldedAsk(ctx, rest[0].th, rest[0].pq, "").Key("ask-" + rest[0].pq.ToolUseID))
	case len(rest) > 0:
		// The others wait in one line: how many, and the next one, which a click opens.
		label := fmt.Sprintf("%d questions waiting", len(rest)+inPanel)
		if open != "" {
			label = fmt.Sprintf("%d more questions", len(rest)+inPanel)
		}
		col = col.Child(a.foldedAsk(ctx, rest[0].th, rest[0].pq, label).Key("asks"))
	}
	_, h := ctx.WindowSize()
	return col.MaxHeight(ui.Px(max(h*0.5, 200))).ScrollY()
}

// foldedAsk is a pinned question folded to one line. With a lead ("3 questions waiting") it stands for
// several, and shows the lead before the next question.
func (a *App) foldedAsk(ctx *ui.Context, th core.ThreadView, pq core.PendingQuestion, lead string) *ui.DivElement {
	t := ctx.Theme()
	q := ""
	if len(pq.Questions) > 0 {
		q = pq.Questions[0].Question
	}
	key := pq.ToolUseID
	label := "Show the question from " + th.Title
	if lead != "" {
		label = lead + ". Show the next, from " + th.Title
	}
	qText := ui.Text(q).FontSize(13).Medium().Truncate().Grow(1).MinWidth(ui.Px(0))
	if lead != "" {
		qText = ui.Text(q).FontSize(13).TextColor(t.TextMuted).Truncate().Grow(1).MinWidth(ui.Px(0))
	}
	return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).Height(ui.Px(38)).Radius(radiusL).
		Background(t.Surface).Border(1, t.BorderStrong).Cursor(ui.CursorPointer).
		Hover(ui.Style{BorderColor: t.Warning.Alpha(0.6)}).
		Role(access.RoleButton).Label(label).TabIndex(0).
		OnClick(func() {
			next := map[string]bool{}
			for k, v := range a.foldedAsks.Peek() {
				next[k] = v
			}
			delete(next, key)
			a.foldedAsks.Set(next)
			a.askOpen.Set(key)
			a.orchDraft.Set(strings.TrimRight(a.orchDraft.Peek(), " "))
		}).
		Tooltip(q + "\n" + repoLabel(th) + " · " + th.Title).
		Child(ui.Icon(icons.MessageCircleQuestionMark).Size(15).Color(t.Warning)).
		Child(ui.If(lead != "", ui.Text(lead).FontSize(13).Semibold().NoWrap().Shrink(0))).
		Child(qText).
		Child(ui.If(a.chatWidth(ctx) >= 640, // narrow, the tooltip and a click name the thread
			ui.Text(repoLabel(th)+" · "+th.Title).FontSize(12).TextColor(t.TextSubtle).NoWrap().Shrink(1).MaxWidth(ui.Px(220)).MinWidth(ui.Px(60)).Truncate())).
		Child(ui.Text("Answer").FontSize(12).Medium().TextColor(t.TextMuted).NoWrap()).
		Child(ui.Icon(icons.ChevronUp).Size(13).Color(t.TextSubtle))
}

// The composer's box: its padding, and its height with one line of text.
const (
	composerPadL = space2
	composerPadR = space1
	composerPadY = space1
	composerMinH = 44
)

// composer is a message box: Enter sends, Shift+Enter starts a new line. Images come in by pasting,
// dropping files on it, or the attach button, and show as thumbnails above the text.
// composer is the box a message is written in. Sending scrolls the transcript of scope to its latest
// message, even when the user had scrolled up: they are here now, and the list then follows their
// message and the reply in.
func (a *App) composer(ctx *ui.Context, v core.ThreadView, key, scope, placeholder string, send func(string, []string) error) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		draft := ui.Local(ctx, key, "")
		images := ui.Local(ctx, key+"-images", []string(nil))
		// The focus handle and quotes live on the App, so quoting from the transcript reaches them.
		id := convID(key)
		focus, quotes := a.composerFocus(id), a.quotesOf(id)
		if key == "orch-draft" {
			draft = a.orchDraft // the pinned question folds while the user writes to Solo
			images = a.orchImages
		}
		var kit mentionKit // the "/" and "@" lists, made below
		submit := func() {
			text, imgs, qs := strings.TrimSpace(draft.Peek()), images.Peek(), quotes.Peek()
			if text == "" && len(imgs) == 0 && len(qs) == 0 {
				return
			}
			text = quoteMessage(qs, text)
			draft.Set("")
			kit.sent()
			images.Set(nil)
			quotes.Set(nil)
			a.listHandle(scope).ScrollToEnd()
			a.run("Sending", func(context.Context) error { return send(text, imgs) })
		}
		// attach adds images as they load; refocus puts the caret back in the text, for images that came
		// from outside it (the file picker or a drop), so Enter sends them.
		attach := func(refocus bool, load func() ([]string, error)) {
			go func() {
				paths, err := load()
				if err != nil {
					a.notify(ui.ToastError, "Couldn't attach the image", err.Error())
				}
				if len(paths) > 0 {
					a.onUI(func() {
						images.Set(append(slices.Clone(images.Peek()), paths...))
						if refocus {
							focus.Focus()
						}
					})
				}
			}()
		}
		importFiles := func(files []string) {
			attach(true, func() ([]string, error) {
				var out []string
				for _, f := range files {
					p, err := a.b.ImportImage(f)
					if err != nil {
						return out, err
					}
					out = append(out, p)
				}
				return out, nil
			})
		}
		// pickedFiles attaches the files of an Open dialog: by path, or by contents in a web page, whose
		// files have no paths.
		pickedFiles := func(files []dialog.File) {
			attach(true, func() ([]string, error) {
				var out []string
				for _, f := range files {
					var p string
					var err error
					if f.Path != "" {
						p, err = a.b.ImportImage(f.Path)
					} else if len(f.Data) > api.MaxImageUpload {
						err = fmt.Errorf("%s is larger than %d MB", f.Name, api.MaxImageUpload>>20)
					} else {
						c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
						p, err = a.b.SaveImageData(c, f.Data)
						cancel()
					}
					if err != nil {
						return out, err
					}
					out = append(out, p)
				}
				return out, nil
			})
		}
		empty := strings.TrimSpace(draft.Get()) == "" && len(images.Get()) == 0 && len(quotes.Get()) == 0
		// As in t3code: while a thread works, the button stops it, or steers it once there's a message.
		// Solo isn't steered: a message to it waits for its turn to end (core.ThreadView.Queued).
		round := ui.Style{Radius: 999}
		var sendBtn *ui.ButtonElement
		switch {
		case v.InTurn && empty && !v.PendingAsk:
			id := v.ID
			sendBtn = ui.IconButton(icons.Square).Small().Style(round).Secondary().Label("Stop").
				Tooltip("Stop the current turn").
				OnClick(func() {
					a.run("Stopping", func(ctx context.Context) error { return a.b.Interrupt(ctx, id) })
				})
		case v.InTurn && !v.PendingAsk && !v.IsWorker:
			sendBtn = ui.IconButton(icons.ArrowUp).Small().Style(round).Primary().Label("Send").
				Tooltip("Send (Enter): Solo reads it when it finishes what it's doing").OnClick(submit)
		case v.InTurn && !v.PendingAsk:
			sendBtn = ui.IconButton(icons.CornerUpRight).Small().Style(round).Primary().Label("Steer").
				Tooltip("Steer (Enter): it stops what it's doing and reads this now").OnClick(submit)
		case empty:
			sendBtn = ui.IconButton(icons.ArrowUp).Small().Style(round).Secondary().Disabled(true).Label("Send")
		default:
			sendBtn = ui.IconButton(icons.ArrowUp).Small().Style(round).Primary().Label("Send").
				Tooltip("Send (Enter) · Shift+Enter for a new line").OnClick(submit)
		}
		actions := ui.Div().Row().Align(ui.Center).Gap(space1).Shrink(0)
		// A compact window gives the text the box's width, and puts the thread's model under it, at the
		// start of the buttons' row.
		under := isCompact(ctx) && v.IsWorker && !core.Finished(v.Status)
		if under {
			actions = ui.Div().Row().Align(ui.Center).Gap(space1).
				Child(a.modelPicker(ctx, v)).Child(ui.Div().Grow(1))
		} else if v.IsWorker && !core.Finished(v.Status) {
			actions = actions.Child(a.modelPicker(ctx, v))
		}
		actions = actions.
			Child(ui.IconButton(icons.ImagePlus).Ghost().Small().Label("Attach images").
				Tooltip("Attach images · you can also paste or drop them").
				OnClick(func() {
					ctx.Window().OpenFiles(dialog.FileOptions{Title: "Attach images", Multiple: true,
						Filters: []dialog.Filter{{Name: "Images", Patterns: []string{"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp"}}}},
						func(r dialog.FileResult) {
							if r.Err != nil {
								a.notify(ui.ToastError, "Couldn't open the file picker", r.Err.Error())
							} else if !r.Canceled {
								files := r.Files
								if len(files) == 0 { // a platform that reports the paths only
									for _, p := range r.Paths {
										files = append(files, dialog.File{Name: filepath.Base(p), Path: p})
									}
								}
								pickedFiles(files)
							}
						})
				}))
		actions = actions.Child(sendBtn)
		// The "/" and "@" lists open over the box, by the caret. Their place is worked out from about how
		// wide the box and its text are.
		boxW := min(a.chatWidth(ctx)-2*gutter(ctx), chatMaxWidth)
		if key != "orch-draft" {
			boxW = a.panelWidth(ctx) - 2*space4
		}
		areaW, below := boxW-composerPadL-composerPadR-2, float32(composerPadY+space1+1)
		if under {
			below += 26 + space1 // the buttons' row, under the text
		} else {
			areaW -= space2 + 2*28 + space1 // the attach and send buttons
			if v.IsWorker && !core.Finished(v.Status) {
				areaW -= 150 // the model picker
			}
		}
		kit = a.mentions(ctx, v, key, draft, focus, areaW, boxW, below)
		// Markdown shows formatted as it is typed; its syntax shows where the caret is.
		marks := markdown.EditMarks(markdown.EditStyle{
			Syntax: ui.Highlight{Color: t.TextSubtle},
			Code:   ui.Highlight{Color: codeColor(t), Background: t.Background},
			Link:   ui.Highlight{Color: t.Link},
		})
		area := ui.TextArea(draft).Placeholder(placeholder).Label(placeholder).MinRows(1).Borderless().
			MaxHeight(ui.Px(220)).Width(ui.Full).Key(key + "-area").FocusHandle(focus).Marks(marks).
			OnPasteImage(func(img image.Image) {
				attach(false, func() ([]string, error) {
					p, err := a.b.SaveImage(img)
					if err != nil {
						return nil, err
					}
					return []string{p}, nil
				})
			}).
			OnChange(kit.onChange).
			OnKeyDown(func(e *ui.KeyEvent) {
				if kit.onKey(e) {
					return
				}
				// A phone's keyboard has no Shift+Enter, so Enter makes a new line there, and the
				// button sends.
				if e.Key == input.KeyEnter && e.Mods&input.ModShift == 0 && !a.Touch {
					e.StopPropagation()
					submit()
				}
			})
		box := ui.Div().Column().Justify(ui.Center).PaddingLeft(composerPadL).PaddingRight(composerPadR).PaddingY(composerPadY).
			MinHeight(ui.Px(composerMinH)).Radius(radiusXL).Border(1, t.BorderStrong).Background(t.Surface).
			FocusWithin(ui.Style{BorderColor: t.TextSubtle}).
			OnDrop(ui.Drop(func(f ui.Files, _ *ui.DropEvent) {
				// Images are attached; other files and folders are mentioned, as "@path".
				var imgs, mentions []string
				for _, p := range f {
					if isImageFile(p) {
						imgs = append(imgs, p)
					} else {
						mentions = append(mentions, dropMention(v, p))
					}
				}
				if len(imgs) > 0 {
					importFiles(imgs)
				}
				if len(mentions) > 0 {
					kit.insert(strings.Join(mentions, " ") + " ")
				}
			})).
			OnClickOutside(kit.close).
			Child(a.quoteStrip(ctx, key, id, quotes.Get()))
		// The quotes and thumbnails go above the text, so the rows are keyed: unkeyed, the text's row
		// would take a new identity when they appear, and the text area would be rebuilt without focus.
		if imgs := images.Get(); len(imgs) > 0 {
			strip := ui.Div().Key(key + "-images").Row().Wrap().Gap(space2).PaddingTop(space1).PaddingBottom(space2)
			for i, p := range imgs {
				idx := i
				strip = strip.Child(a.thumb(ctx, p, 56, func() {
					images.Set(slices.Delete(slices.Clone(images.Peek()), idx, idx+1))
					focus.Focus() // the remove button goes with the image
				}))
			}
			box = box.Child(strip)
		}
		// A press in the text moves its caret, which the lists then no longer know.
		if under {
			box = box.Child(ui.Div().Key(key + "-row").Column().Gap(space1).
				Child(ui.Div().Column().MinWidth(ui.Px(0)).PaddingY(space1).OnMouseDown(kit.onPress).Child(area)).
				Child(actions))
		} else {
			box = box.Child(ui.Div().Key(key + "-row").Row().Align(ui.End).Gap(space2).
				Child(ui.Div().Column().Grow(1).MinWidth(ui.Px(0)).PaddingY(space1).OnMouseDown(kit.onPress).Child(area)).
				Child(actions))
		}
		return box.Child(kit.popup)
	})
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// toolThread is the thread an orchestrator tool call acts on, or "".
func (a *App) toolThread(d core.ToolData) string {
	name := strings.TrimPrefix(d.Name, "mcp__solo__")
	if name == d.Name {
		return ""
	}
	if name == "spawn_thread" {
		// "Started thread a1b2c3d4 …": the new thread's short ID.
		if f := strings.Fields(d.Result); len(f) >= 3 && f[0] == "Started" {
			return a.findThreadID(f[2])
		}
		return ""
	}
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(d.Input, &in) != nil || len(in.ID) < 4 {
		return ""
	}
	return a.findThreadID(in.ID)
}

// itemThread is the thread a transcript step acts on, or "".
func (a *App) itemThread(it *core.Item) string {
	if it.Kind != core.ItemTool {
		return ""
	}
	var d core.ToolData
	it.Decode(&d)
	return a.toolThread(d)
}

// threadChip names a thread inline: its status mark and title. A click opens it.
func (a *App) threadChip(ctx *ui.Context, id string) ui.Element {
	t := ctx.Theme()
	th, ok := a.thread(id)
	if !ok {
		return ui.Div()
	}
	return ui.Div().Row().Align(ui.Center).Gap(space1+2).PaddingLeft(space1).PaddingRight(space2).Height(ui.Px(22)).
		Radius(11).Background(t.Surface).Border(1, t.Border).Cursor(ui.CursorPointer).Shrink(1).MinWidth(ui.Px(0)).
		MaxWidth(ui.Px(300)).Hover(ui.Style{BorderColor: t.BorderStrong, Background: t.SurfaceHover}).
		Role(access.RoleButton).Label("Open " + th.Title).TabIndex(0).Tooltip(th.Title + " · " + statusLabel(th)).
		OnClick(func() { a.openThread(id, "conversation") }).
		Child(statusMark(ctx, shownStatus(th))).
		Child(ui.Text(th.Title).FontSize(12).Medium().Truncate().MinWidth(ui.Px(0)))
}

// cardHeader is the line atop a card: its type, then the repository and thread it is about (the
// title opens the thread), on a second line in a narrow chat. threadID "" leaves out the repository and thread.
// extra goes after the type, such as a merge's checks.
func (a *App) cardHeader(ctx *ui.Context, icon *icons.Icon, c ui.Color, typ, threadID string, extra ...ui.Element) ui.Element {
	row := ui.Div().Row().Align(ui.Center).Gap(space2).MinWidth(ui.Px(0)).
		Child(ui.Icon(icon).Size(15).Color(c)).
		Child(ui.Text(typ).FontSize(12).Semibold().TextColor(c).NoWrap())
	for _, e := range extra {
		if e != nil {
			row = row.Child(e)
		}
	}
	th, ok := a.thread(threadID)
	if threadID == "" || !ok {
		return row
	}
	// The repository and thread sit at the right: repo › title; in a narrow chat, on a line of their own.
	where := ui.Div().Row().Align(ui.Center).Gap(space1 + 2).Shrink(1).MinWidth(ui.Px(0))
	if a.chatWidth(ctx) < 640 {
		return ui.Div().Column().Gap(space1).MinWidth(ui.Px(0)).Child(row).Child(a.cardWhere(ctx, where, th))
	}
	return row.Child(ui.Div().Grow(1)).Child(a.cardWhere(ctx, where, th))
}

// cardWhere fills a card header's "repo › title", whose title opens the thread.
func (a *App) cardWhere(ctx *ui.Context, where *ui.DivElement, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	threadID := th.ID
	return where.
		Child(ui.Text(repoLabel(th)).FontSize(12).TextColor(t.TextMuted).NoWrap()).
		Child(ui.Icon(icons.ChevronRight).Size(12).Color(t.TextSubtle)).
		Child(ui.Div().MinWidth(ui.Px(0)).Cursor(ui.CursorPointer).
			Role(access.RoleButton).Label("Open " + th.Title).TabIndex(0).Tooltip("Open this thread").
			Hover(ui.Style{TextColor: t.Link}).
			OnClick(func() { a.openThread(threadID, "conversation") }).
			Child(ui.Text(th.Title).FontSize(12).Medium().Truncate()))
}

// checksBadge says where a merge's checks stand: waiting for them to start, passed (or none
// required), running with how many are done, or failed. A local merge has none to wait for, so it shows as passed. nil before a pull
// request is opened, and once the thread has landed.
func checksBadge(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	cs, ok := mergeChecks(th)
	if !ok {
		return nil
	}
	icon, c := checksLook(ctx, cs.checks)
	return ui.Div().Row().Align(ui.Center).Gap(4).PaddingLeft(space1).Shrink(0).Tooltip(cs.tip).
		Child(ui.Text("·").FontSize(12).TextColor(t.TextSubtle)).
		Child(ui.Icon(icon).Size(13).Color(c)).
		Child(ui.Text(cs.text).FontSize(12).TextColor(c).NoWrap())
}

// mergeChecks is where a merge's checks stand, as checksBadge shows them; false with none to show.
func mergeChecks(th core.ThreadView) (checksState, bool) {
	switch th.Status {
	case core.StatusMerged, core.StatusDiscarded, core.StatusArchived:
		return checksState{}, false
	}
	prs := openPRs(th)
	if len(prs) == 0 && th.PR != nil && th.PR.State == forge.StateOpen {
		prs = []*forge.PR{th.PR}
	}
	switch {
	case len(prs) > 0:
		return checksOf(prs), true
	case th.CanMerge && th.Forge == forge.None:
		return checksState{checks: forge.ChecksPassing, text: "Checks passed", tip: "A local merge has no checks to wait for"}, true
	}
	return checksState{}, false
}

// checksLook is the icon and color of where checks stand: a dashed circle, muted, until they start,
// a check, a timer while they run, a cross once one fails.
func checksLook(ctx *ui.Context, checks string) (*icons.Icon, ui.Color) {
	t := ctx.Theme()
	switch checks {
	case forge.ChecksFailing:
		return icons.CircleX, t.Danger
	case forge.ChecksPending:
		return icons.Timer, t.Warning
	case forge.ChecksWaiting:
		return icons.CircleDashed, t.TextMuted
	}
	return icons.CircleCheck, t.Success
}

// checksState is where a merge's checks stand, as its card says it.
type checksState struct {
	checks    string // forge.ChecksPassing, ChecksWaiting, ChecksPending or ChecksFailing
	text, tip string
	// n sums the checks of every pull request; counted is false when a forge gave none to sum.
	n       forge.CheckCounts
	counted bool
}

// tally is the checks counted by where they stand, zeros left out: "3 passed · 2 running", "4 passed ·
// 1 failed · 1 running". "" without counts.
func (s checksState) tally() string {
	if !s.counted || s.n.Total == 0 {
		return ""
	}
	var parts []string
	for _, c := range []struct {
		n    int
		word string
	}{{s.n.Passed, "passed"}, {s.n.Failed, "failed"}, {s.n.Pending, "running"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.word))
		}
	}
	return strings.Join(parts, " · ")
}

// checksOf sums up the checks of a thread's open pull requests: "Checks: 6/7 passed · 1 running"
// while they run, "Checks failed: 1 of 7" once one fails (failure wins over running), "Waiting for
// checks" while one's checks haven't started yet (a pull request just opened), "Checks passed" when
// done. A forge that reports no counts (GitLab's one pipeline) gets the plain words.
func checksOf(prs []*forge.PR) checksState {
	var n forge.CheckCounts
	var failed []string
	pending, waiting, counted := false, false, true
	for _, pr := range prs {
		n.Total += pr.Counts.Total
		n.Passed += pr.Counts.Passed
		n.Failed += pr.Counts.Failed
		n.Pending += pr.Counts.Pending
		switch pr.Checks {
		case forge.ChecksFailing:
			failed = append(failed, checkNamesOf(pr.Failed)...)
			if len(pr.Failed) == 0 {
				failed = append(failed, fmt.Sprintf("%s #%d", pr.Kind.Noun(), pr.Number))
			}
		case forge.ChecksPending:
			pending = true
		case forge.ChecksWaiting:
			waiting = true
		}
		if pr.Checks != forge.ChecksNone && pr.Checks != forge.ChecksWaiting && pr.Counts.Total == 0 {
			counted = false // checks, but no counts to sum
		}
	}
	running := ""
	if n.Pending > 0 {
		running = fmt.Sprintf(" · %d running", n.Pending)
	}
	switch {
	case len(failed) > 0:
		s := checksState{checks: forge.ChecksFailing, text: "Checks failed", tip: "Failed: " + strings.Join(failed, ", "), n: n, counted: counted}
		if counted {
			s.text = fmt.Sprintf("Checks failed: %d of %d", n.Failed, n.Total) + running
		}
		return s
	case pending:
		s := checksState{checks: forge.ChecksPending, text: "Checks running", tip: "Waiting for the required checks", n: n, counted: counted}
		if counted {
			s.text = fmt.Sprintf("Checks: %d/%d passed", n.Passed, n.Total) + running
			if n.Passed == 0 {
				s.text = fmt.Sprintf("Checks: %d running", n.Pending)
			}
			s.tip = fmt.Sprintf("Waiting for the required checks: %d of %d still running", n.Pending, n.Total)
		}
		return s
	case waiting:
		return checksState{checks: forge.ChecksWaiting, text: "Waiting for checks", tip: "The checks haven't started yet", n: n, counted: counted}
	case n.Total == 0 && !slices.ContainsFunc(prs, func(pr *forge.PR) bool { return pr.Checks == forge.ChecksPassing }):
		return checksState{checks: forge.ChecksPassing, text: "Checks passed", tip: "No checks are required"}
	}
	s := checksState{checks: forge.ChecksPassing, text: "Checks passed", tip: "Every required check passed", n: n, counted: counted}
	if counted && n.Total > 0 {
		s.tip = fmt.Sprintf("All %d checks passed", n.Total)
	}
	return s
}

// markBuilt records that a transcript row is built (near the viewport) or not. UI thread only.
func (a *App) markBuilt(ctx *ui.Context, scope, key string, on bool) {
	if a.built == nil {
		a.built = map[string]bool{}
	}
	k := scope + "/" + key
	if a.built[k] == on {
		return
	}
	if on {
		a.built[k] = true
	} else {
		delete(a.built, k)
	}
	// Not during this render: tell the pill on the next frame.
	ctx.Post(func() { a.builtVer.Update(func(n int) int { return n + 1 }) })
}

// isAction reports whether a transcript row waits on the user: a merge proposal not yet acted on, or
// a question not yet answered.
func (a *App) isAction(r row) bool {
	if r.item == nil {
		return false
	}
	switch r.item.Kind {
	case core.ItemProposal:
		th, ok := a.thread(r.item.Ref)
		return ok && th.CanMerge && !th.Scratch
	case core.ItemQuestion:
		var d core.QuestionData
		r.item.Decode(&d)
		return !d.Cancelled && len(d.Answers) == 0
	}
	return false
}

// offscreenActions returns the rows of Solo's chat that wait on the user and have scrolled out of
// view over its top (top=true) or bottom edge, in order. It subscribes to which rows are built.
func (a *App) offscreenActions(rows []row, top bool) []int {
	a.builtVer.Get()
	built, action := make([]bool, len(rows)), make([]bool, len(rows))
	for i, r := range rows {
		built[i], action[i] = a.built["orch/"+r.key], a.isAction(r)
	}
	return hiddenActions(built, action, top)
}

// hiddenActions returns the indexes i with action[i] that lie before the first built row (top=true)
// or after the last, in order. With nothing built it can't tell what is out of view, so none.
func hiddenActions(built, action []bool, top bool) []int {
	first, last := -1, -1
	for i, b := range built {
		if b {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return nil
	}
	var hidden []int
	for i, act := range action {
		if act && ((top && i < first) || (!top && i > last)) {
			hidden = append(hidden, i)
		}
	}
	return hidden
}

// chatWidth estimates the width of Solo's conversation column.
func (a *App) chatWidth(ctx *ui.Context) float32 {
	w, _ := ctx.WindowSize()
	if isCompact(ctx) {
		return w
	}
	chat := w - railW(ctx) - space2
	if a.openID.Get() != "" {
		chat -= a.panelWidth(ctx) + space2
	}
	return min(chat, chatMaxWidth+2*space6)
}
