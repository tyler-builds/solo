package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/markdown"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

const chatMaxWidth = 780

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
		Child(a.orchestratorBar(ctx, v)).
		Child(body).
		Child(ui.Div().Column().Align(ui.Center).PaddingX(space6).PaddingBottom(space4).Shrink(0).
			Child(ui.Div().Column().Gap(space2).Width(ui.Full).MaxWidth(ui.Px(chatMaxWidth)).
				Child(a.questionStrip(ctx)).
				Child(a.composer(ctx, v, "orch-draft", "Message the orchestrator…", func(text string) error {
					return a.b.SendToOrchestrator(context.Background(), text)
				})))).
		Background(t.Background)
}

func (a *App) orchestratorBar(ctx *ui.Context, v core.ThreadView) ui.Element {
	t := ctx.Theme()
	state := "Idle"
	if v.InTurn {
		state = "Working…"
	}
	model := v.ActualModel
	if model == "" {
		model = a.b.Settings().OrchestratorModel
	}
	left := ui.Div().Row().Align(ui.Center).Gap(space3).Grow(1).MinWidth(ui.Px(0)).
		Child(ui.Icon(icons.Bot).Size(18).Color(ui.Hex(pal(t).brand))).
		Child(ui.Text("Orchestrator").FontSize(14).Semibold()).
		Child(ui.Div().Row().Align(ui.Center).Gap(6).
			Child(dot(map[bool]ui.Color{true: ui.Hex(pal(t).brand), false: t.TextSubtle}[v.InTurn], 6)).
			Child(caption(ctx, state)))
	right := ui.Div().Row().Align(ui.Center).Gap(space2).Shrink(0)
	if v.ContextTokens > 0 {
		right = right.Child(chip(ctx, fmt.Sprintf("%dk context", v.ContextTokens/1000)).
			Tooltip("Tokens in the orchestrator's context window. Start a new conversation when it gets large; threads are unaffected."))
	}
	right = right.Child(chip(ctx, model)).
		Child(ui.Button("New conversation").Icon(icons.RotateCcw).Ghost().Small().Disabled(v.InTurn).
			Tooltip("Start the orchestrator on a fresh context. Threads keep running.").
			OnClick(func() { a.run("New conversation", a.b.NewConversation) }))
	return ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).Height(ui.Px(52)).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(left).Child(right)
}

func (a *App) welcome(ctx *ui.Context) ui.Element {
	repos := a.repos.Get()
	col := ui.Div().Column().Align(ui.Center).Gap(space3).MaxWidth(ui.Px(520)).
		Child(brandMark(48)).
		Child(title("What should we work on?").MarginTop(space2)).
		Child(muted(ctx, "Tell the orchestrator what you want done. It plans the work, runs Claude workers in their own branches, and reports back. You review and merge.").
			Align(ui.Center))
	if len(repos) == 0 {
		col = col.Child(ui.Button("Add repository folders").Icon(icons.FolderGit2).Primary().
			OnClick(func() { a.settings.Set(true) }))
	} else {
		var names []string
		for i, r := range repos {
			if i == 6 {
				names = append(names, fmt.Sprintf("and %d more", len(repos)-6))
				break
			}
			names = append(names, filepath.Base(r))
		}
		col = col.Child(caption(ctx, fmt.Sprintf("Knows %d repositories: %s", len(repos), strings.Join(names, ", "))).Align(ui.Center))
	}
	return ui.Div().Column().Center().Grow(1).Padding(space6).Child(col)
}

// row is one entry of a transcript list.
type row struct {
	key  string
	item *core.Item
	live *core.LiveBlock
	busy bool // the "working" line at the end of a running turn
}

// transcript renders a thread's items, its streaming text, and a working indicator, newest at the bottom.
func (a *App) transcript(ctx *ui.Context, v core.ThreadView, items []core.Item, scope string) ui.Element {
	rows := make([]row, 0, len(items)+4)
	for i := range items {
		it := &items[i]
		if !visible(it) {
			continue
		}
		rows = append(rows, row{key: fmt.Sprintf("i%d", it.ID), item: it})
	}
	for i := range v.Live {
		rows = append(rows, row{key: fmt.Sprintf("live%d", i), live: &v.Live[i]})
	}
	if v.InTurn && (len(v.Live) == 0 || v.Live[len(v.Live)-1].Thinking) {
		rows = append(rows, row{key: "busy", busy: true})
	}
	h := ui.Local(ctx, scope+"-list", ui.NewListHandle()).Peek()
	return ui.ListOf(rows, func(r row) string { return r.key }, func(_ int, r row) ui.Element {
		return ui.Div().Column().Align(ui.Center).PaddingX(space6).
			Child(ui.Div().Column().Width(ui.Full).MaxWidth(ui.Px(chatMaxWidth)).PaddingY(space2).
				Child(a.rowView(ctx, v, r)))
	}).Bottom().Handle(h).SelectionContainer().Key(scope + "-transcript").Label("Conversation").
		Style(ui.Style{Grow: 1, Padding: ui.Edges{Top: space4, Bottom: space2}})
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
		return strings.TrimSpace(d.Text) != "(noted)"
	}
	return true
}

func (a *App) rowView(ctx *ui.Context, v core.ThreadView, r row) ui.Element {
	t := ctx.Theme()
	switch {
	case r.busy:
		label := "Thinking…"
		if v.Retry != "" {
			label = v.Retry
		}
		return ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(ui.ProgressRing().Size(ui.RingSmall).Label("Working")).
			Child(caption(ctx, label))
	case r.live != nil:
		if r.live.Thinking {
			return thinkingView(ctx, r.live.Text, true)
		}
		return markdown.View(r.live.Text).Key("md")
	}
	it := r.item
	switch it.Kind {
	case core.ItemUser:
		var d core.UserData
		it.Decode(&d)
		if d.Synthetic {
			return syntheticView(ctx, d.Text)
		}
		return ui.Div().Row().Justify(ui.End).
			Child(ui.Div().PaddingX(space3 + 2).PaddingY(space2 + 2).Radius(radiusL).MaxWidth(ui.Percent(85)).
				Background(t.SurfaceActive).
				Child(ui.Text(d.Text).Selectable()))
	case core.ItemAssistant:
		var d core.TextData
		it.Decode(&d)
		return markdown.View(d.Text).Key("md")
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
		return a.questionCard(ctx, v.ID, d, true)
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
		if d.Level == "error" {
			c = t.Danger
		}
		return ui.Div().Row().Justify(ui.Center).Child(ui.Text(d.Text).FontSize(12).TextColor(c).Align(ui.Center))
	}
	return ui.Div()
}

// syntheticView is a thread update the app sent the orchestrator: a quiet strip, not a user bubble.
func syntheticView(ctx *ui.Context, text string) ui.Element {
	t := ctx.Theme()
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "- ") {
			lines = append(lines, strings.TrimPrefix(l, "- "))
		}
	}
	if len(lines) == 0 {
		lines = []string{firstLine(text)}
	}
	col := ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0))
	for _, l := range lines {
		col = col.Child(ui.Text(l).FontSize(12).TextColor(t.TextMuted).MaxLines(2))
	}
	return ui.Div().Row().Gap(space2+2).PaddingX(space3).PaddingY(space2).Radius(radiusM).
		BorderEdges(ui.Edges{Left: 2}, ui.Hex(pal(t).brand).Alpha(0.6)).Background(t.Surface.Alpha(0.6)).
		Child(ui.Icon(icons.Activity).Size(14).Color(t.TextSubtle)).
		Child(col)
}

func thinkingView(ctx *ui.Context, text string, live bool) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		open := ui.Local(ctx, "open", live)
		head := ui.Div().Row().Align(ui.Center).Gap(6).Cursor(ui.CursorPointer).
			OnClick(func() { open.Update(func(o bool) bool { return !o }) }).
			Child(ui.Icon(icons.Brain).Size(13).Color(t.TextSubtle)).
			Child(subtle(ctx, "Thinking")).
			Child(ui.Icon(map[bool]*icons.Icon{true: icons.ChevronDown, false: icons.ChevronRight}[open.Get()]).Size(12).Color(t.TextSubtle))
		col := ui.Div().Column().Gap(space1).Child(head)
		if open.Get() {
			col = col.Child(ui.Div().PaddingLeft(space4 + 3).
				Child(ui.Text(text).FontSize(12).TextColor(t.TextSubtle).Italic().Selectable()))
		}
		return col
	})
}

// toolView is one tool call: a compact line that opens to show its result.
func (a *App) toolView(ctx *ui.Context, d core.ToolData) ui.Element {
	icon, label, detail := a.describeTool(d)
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
			state = ui.Icon(icons.Check).Size(13).Color(t.TextSubtle)
		}
		head := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).Height(ui.Px(28)).Radius(radiusS).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			OnClick(func() { open.Update(func(o bool) bool { return !o }) }).
			Child(ui.Icon(icon).Size(14).Color(t.TextMuted)).
			Child(ui.Text(label).FontSize(13).Medium().TextColor(t.TextMuted).NoWrap().Shrink(0)).
			Child(ui.Text(detail).FontSize(12).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0))).
			Child(state)
		col := ui.Div().Column().Gap(space1).Child(head)
		if open.Get() {
			body := ui.Div().Column().Gap(space2).MarginLeft(space6).Padding(space3).Radius(radiusM).
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
		return icons.Plus, "Started thread", fmt.Sprintf("%s · %s", str("title"), str("repo"))
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
		return icons.Terminal, "Ran", firstLine(str("command"))
	case "WebFetch":
		return icons.Globe, "Fetched", str("url")
	case "WebSearch":
		return icons.Globe, "Searched the web", str("query")
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
	default:
		state = ui.Icon(icons.CircleAlert).Size(13).Color(t.Danger)
	}
	detail := d.Summary
	if detail == "" && d.ToolUses > 0 {
		detail = fmt.Sprintf("%d tool uses", d.ToolUses)
	}
	return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).Height(ui.Px(28)).
		Child(ui.Icon(icons.Bot).Size(14).Color(t.TextMuted)).
		Child(ui.Text(d.Description).FontSize(13).Medium().TextColor(t.TextMuted).NoWrap().Shrink(0)).
		Child(ui.Text(detail).FontSize(12).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0))).
		Child(state)
}

func turnView(ctx *ui.Context, d core.TurnData) ui.Element {
	t := ctx.Theme()
	if d.Status == agent.TurnInterrupted || d.Status == agent.TurnCancelled {
		return ui.Div().Row().Align(ui.Center).Gap(6).
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

// proposalCard is the orchestrator's recommendation to merge a thread, with the Merge button.
func (a *App) proposalCard(ctx *ui.Context, d core.ProposalData) ui.Element {
	t := ctx.Theme()
	th, _ := a.thread(d.ThreadID)
	brand := ui.Hex(pal(t).brand)
	var actions ui.Element
	switch th.Status {
	case core.StatusMerged:
		actions = statusPill(ctx, th)
	case core.StatusDiscarded:
		actions = statusPill(ctx, th)
	default:
		actions = ui.Div().Row().Gap(space2).
			Child(ui.Button("Review changes").Secondary().Small().OnClick(func() { a.openThread(d.ThreadID, "changes") })).
			Child(ui.Button("Merge").Icon(icons.GitMerge).Primary().Small().Disabled(!th.CanMerge).
				OnClick(func() { a.merge(d.ThreadID) }))
	}
	meta := filepath.Base(th.RepoPath)
	if th.Branch != "" {
		meta += " · " + th.Branch + " → " + th.BaseBranch
	}
	return ui.Div().Column().Gap(space3).Padding(space4).Radius(radiusL).Background(t.Surface).
		Border(1, brand.Alpha(0.35)).
		Child(ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(ui.Icon(icons.GitMerge).Size(16).Color(brand)).
			Child(ui.Text("Ready to merge").FontSize(12).Semibold().TextColor(brand))).
		Child(ui.Div().Column().Gap(space1).
			Child(subhead(d.Title).FontSize(14)).
			Child(caption(ctx, meta))).
		Child(ui.Text(d.Summary).FontSize(13).TextColor(t.TextMuted)).
		Child(actions)
}

// questionCard is an AskUserQuestion: the first unanswered question with its options as buttons.
func (a *App) questionCard(ctx *ui.Context, threadID string, d core.QuestionData, inThread bool) ui.Element {
	t := ctx.Theme()
	if d.Cancelled || len(d.Answers) > 0 || len(d.Questions) == 0 {
		var parts []string
		for _, q := range d.Questions {
			ans := d.Answers[q.Question]
			if d.Cancelled {
				ans = "dismissed"
			}
			parts = append(parts, fmt.Sprintf("%s → %s", q.Question, ans))
		}
		return ui.Div().Row().Align(ui.Center).Gap(6).
			Child(ui.Icon(icons.MessageCircleQuestionMark).Size(13).Color(t.TextSubtle)).
			Child(subtle(ctx, strings.Join(parts, " · ")))
	}
	return a.askForm(ctx, threadID, d.ToolUseID, d.Questions, "")
}

// askForm shows questions with their options; picking options answers. title names the asking thread
// when the form is shown outside it.
func (a *App) askForm(ctx *ui.Context, threadID, toolUseID string, qs []agent.Question, title string) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
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
		head := ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(ui.Icon(icons.MessageCircleQuestionMark).Size(16).Color(t.Warning))
		if title != "" {
			head = head.Child(ui.Text(title).FontSize(12).Semibold().TextColor(t.Warning).Truncate())
		} else {
			head = head.Child(ui.Text("Question").FontSize(12).Semibold().TextColor(t.Warning))
		}
		if q.Header != "" {
			head = head.Child(chip(ctx, q.Header))
		}
		opts := ui.Div().Column().Gap(space1 + 2)
		for _, o := range q.Options {
			label := o.Label
			opts = opts.Child(ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space3).PaddingY(space2).
				Radius(radiusM).Border(1, t.Border).Background(t.Background).Cursor(ui.CursorPointer).
				Hover(ui.Style{Background: t.SurfaceHover, BorderColor: t.BorderStrong}).
				Role(access.RoleButton).Label(o.Label).TabIndex(0).OnClick(func() { pick(label) }).
				Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
					Child(ui.Text(o.Label).FontSize(13).Medium()).
					Child(ui.Text(o.Description).FontSize(12).TextColor(t.TextMuted)).
					Key("opt-" + label)))
		}
		otherInput := ui.Div().Row().Gap(space2).
			Child(ui.TextInput(other).Placeholder("Or type an answer…").Small().Width(ui.Full).
				OnSubmit(func(s string) {
					if strings.TrimSpace(s) != "" {
						pick(strings.TrimSpace(s))
					}
				})).
			Child(ui.Button("Dismiss").Ghost().Small().OnClick(func() {
				a.run("Dismissing", func(context.Context) error { return a.b.Dismiss(threadID, toolUseID) })
			}))
		return ui.Div().Column().Gap(space3).Padding(space4).Radius(radiusL).Background(t.Surface).
			Border(1, t.Warning.Alpha(0.4)).
			Child(head).
			Child(ui.Text(q.Question).FontSize(14).Medium()).
			Child(opts).
			Child(otherInput)
	})
}

// questionStrip shows the workers' open questions above the orchestrator's composer.
func (a *App) questionStrip(ctx *ui.Context) ui.Element {
	pending := a.pending.Get()
	col := ui.Div().Column().Gap(space2)
	for _, th := range a.threads.Get() {
		qs := pending[th.ID]
		if len(qs) == 0 || th.ID == core.OrchestratorID {
			continue
		}
		col = col.Child(ui.Div().Key("ask-" + qs[0].ToolUseID).
			Child(a.askForm(ctx, th.ID, qs[0].ToolUseID, qs[0].Questions, th.Title+" asks")))
	}
	return col
}

// composer is a message box: Enter sends, Shift+Enter starts a new line.
func (a *App) composer(ctx *ui.Context, v core.ThreadView, key, placeholder string, send func(string) error) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		draft := ui.Local(ctx, key, "")
		submit := func() {
			text := strings.TrimSpace(draft.Peek())
			if text == "" {
				return
			}
			draft.Set("")
			a.run("Sending", func(context.Context) error { return send(text) })
		}
		sendBtn := ui.IconButton(icons.ArrowUp).Primary().Small().Label("Send").Tooltip("Send (Enter)").
			Disabled(strings.TrimSpace(draft.Get()) == "").OnClick(submit)
		actions := ui.Div().Row().Align(ui.Center).Gap(space2)
		if v.InTurn {
			id := v.ID
			actions = actions.Child(ui.Button("Stop").Icon(icons.Square).Ghost().Small().
				Tooltip("Stop the current turn").
				OnClick(func() {
					a.run("Stopping", func(ctx context.Context) error { return a.b.Interrupt(ctx, id) })
				}))
		}
		actions = actions.Child(sendBtn)
		area := ui.TextArea(draft).Placeholder(placeholder).Label(placeholder).MinRows(2).Borderless().
			MaxHeight(ui.Px(220)).Width(ui.Full).Key(key + "-area").
			OnKeyDown(func(e *ui.KeyEvent) {
				if e.Key == input.KeyEnter && e.Mods&input.ModShift == 0 {
					e.StopPropagation()
					submit()
				}
			})
		hint := "Enter to send · Shift+Enter for a new line"
		if v.InTurn {
			hint = "Messages sent now are read when the current step finishes"
		}
		return ui.Div().Column().Gap(space1).PaddingX(space3).PaddingTop(space2).PaddingBottom(space2).
			Radius(radiusXL).Border(1, t.BorderStrong).Background(t.Surface).
			FocusWithin(ui.Style{BorderColor: t.TextSubtle}).
			Child(area).
			Child(ui.Div().Row().Align(ui.Center).
				Child(subtle(ctx, hint).Grow(1)).
				Child(actions))
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
