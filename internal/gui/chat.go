package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/markdown"
	"github.com/tyler-builds/jamb/markdown/mdcode"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

const chatMaxWidth = 780

// code colors the code blocks of every transcript; one highlighter for the app.
var code = mdcode.Highlight()

func md(text string) ui.Element { return markdown.View(text).Code(code).Key("md") }

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
				Child(a.attentionBar(ctx)).
				Child(a.composer(ctx, v, "orch-draft", "Message the orchestrator…", func(text string) error {
					return a.b.SendToOrchestrator(context.Background(), text)
				})))).
		Background(t.Background)
}

func (a *App) orchestratorBar(ctx *ui.Context, v core.ThreadView) ui.Element {
	t := ctx.Theme()
	state := ui.Div().Row().Align(ui.Center).Gap(6).MinWidth(ui.Px(0))
	if v.InTurn {
		label := "Thinking…"
		if v.Step != nil {
			label = a.stepLabel(*v.Step) + "…"
		}
		state = state.Child(ui.ProgressRing().Size(ui.RingSmall).Label("Working")).
			Child(ui.Text(label).FontSize(12).TextColor(ui.Hex(pal(t).brand)).Truncate().MinWidth(ui.Px(0)))
	} else {
		state = state.Child(dot(t.TextSubtle, 6)).Child(caption(ctx, "Idle"))
	}
	return ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).Height(ui.Px(52)).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(ui.Icon(icons.Bot).Size(18).Color(ui.Hex(pal(t).brand))).
		Child(ui.Text("Orchestrator").FontSize(14).Semibold().NoWrap()).
		Child(state)
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
	key    string
	item   *core.Item
	steps  []*core.Item // a run of tool calls and thoughts, shown folded
	thread string       // the thread the folded steps act on, if they all act on one
	live   *core.LiveBlock
	busy   bool // the "working" line at the end of a running turn
}

// isStep reports whether an item is part of the agent's working steps rather than its conversation.
func isStep(it *core.Item) bool {
	return it.Kind == core.ItemTool || it.Kind == core.ItemThinking || it.Kind == core.ItemTask
}

// minFold is the shortest run of steps folded into one row.
const minFold = 3

// transcript renders a thread's items, its streaming text, and a working indicator, newest at the bottom.
func (a *App) transcript(ctx *ui.Context, v core.ThreadView, items []core.Item, scope string) ui.Element {
	rows := make([]row, 0, len(items)+4)
	// Steps fold in runs. In the orchestrator, a run is also cut where the thread it acts on changes, so
	// each folded group belongs to one thread (or to none).
	var run []*core.Item
	runThread := ""
	flush := func() {
		min := minFold
		if runThread != "" {
			min = 2
		}
		if len(run) >= min {
			rows = append(rows, row{key: fmt.Sprintf("g%d", run[0].ID), steps: run, thread: runThread})
		} else {
			for _, it := range run {
				rows = append(rows, row{key: fmt.Sprintf("i%d", it.ID), item: it})
			}
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
	for i := range v.Live {
		rows = append(rows, row{key: fmt.Sprintf("live%d", i), live: &v.Live[i]})
	}
	if v.InTurn && (len(v.Live) == 0 || v.Live[len(v.Live)-1].Thinking) {
		rows = append(rows, row{key: "busy", busy: true})
	}
	h := ui.Local(ctx, scope+"-list", ui.NewListHandle()).Peek()
	return ui.ListOf(rows, func(r row) string { return r.key }, func(_ int, r row) ui.Element {
		return ui.Div().Column().Align(ui.Center).PaddingX(space6).
			Child(ui.Div().Column().Width(ui.Full).MaxWidth(ui.Px(chatMaxWidth)).PaddingY(space1 + 2).
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
		} else if v.Step != nil {
			label = a.stepLabel(*v.Step) + "…"
		}
		return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).
			Child(ui.ProgressRing().Size(ui.RingSmall).Label("Working")).
			Child(caption(ctx, label))
	case r.live != nil:
		if r.live.Thinking {
			return thinkingView(ctx, r.live.Text, true)
		}
		return md(r.live.Text)
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
					Child(ui.Icon(icons.Bot).Size(13).Color(ui.Hex(pal(t).brand))).
					Child(ui.Text("Orchestrator").FontSize(12).Medium().TextColor(t.TextMuted))).
				Child(ui.Div().PaddingX(space3+2).PaddingY(space2+2).Radius(radiusL).MaxWidth(ui.Percent(92)).
					Background(t.Surface).Border(1, t.Border).
					Child(ui.Text(d.Text).Selectable()))
		}
		return ui.Div().Row().Justify(ui.End).
			Child(ui.Div().PaddingX(space3 + 2).PaddingY(space2 + 2).Radius(radiusL).MaxWidth(ui.Percent(85)).
				Background(t.SurfaceActive).
				Child(ui.Text(d.Text).Selectable()))
	case core.ItemAssistant:
		var d core.TextData
		it.Decode(&d)
		return md(d.Text)
	default:
		return a.stepView(ctx, it, v)
	}
}

// stepView renders a single item that isn't a message.
func (a *App) stepView(ctx *ui.Context, it *core.Item, v core.ThreadView) ui.Element {
	t := ctx.Theme()
	switch it.Kind {
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
		if d.Level == "error" {
			c = t.Danger
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
		var state ui.Element = ui.Icon(icons.Check).Size(13).Color(t.TextSubtle)
		if running {
			state = ui.ProgressRing().Size(ui.RingSmall).Label("Running")
		}
		label := fmt.Sprintf("%d steps", len(steps))
		if threadID != "" {
			label = fmt.Sprintf("%d actions", len(steps))
		}
		labelColor := t.TextMuted
		if failures > 0 {
			label += fmt.Sprintf(" · %d failed", failures)
			labelColor = t.Danger
			if !running {
				state = nil
			}
		}
		head := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).Height(ui.Px(28)).Radius(radiusS).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(label).TabIndex(0).
			OnClick(func() { open.Update(func(o bool) bool { return !o }) }).
			Child(ui.Icon(map[bool]*icons.Icon{true: icons.ChevronDown, false: icons.ChevronRight}[open.Get()]).Size(14).Color(t.TextMuted)).
			Child(ui.If(threadID != "", a.threadChip(ctx, threadID))).
			Child(ui.Text(label).FontSize(13).Medium().TextColor(labelColor).NoWrap().Shrink(0)).
			Child(ui.Text(latest).FontSize(12).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0)))
		if state != nil {
			head = head.Child(state)
		}
		col := ui.Div().Column().Gap(2).Child(head)
		if open.Get() {
			inner := ui.Div().Column().Gap(2).PaddingLeft(space3).MarginLeft(space3).
				BorderEdges(ui.Edges{Left: 1}, t.Border)
			for _, it := range steps {
				inner = inner.Child(a.stepView(ctx, it, core.ThreadView{}))
			}
			col = col.Child(inner)
		}
		return col
	})
}

var updateLine = regexp.MustCompile(`^Thread ([0-9a-zA-Z-]{4,}) "(.*)" \(([^)]*)\) (.*)$`)

// updateView shows a thread update the app sent the orchestrator: one clickable line per thread.
func (a *App) updateView(ctx *ui.Context, text string) ui.Element {
	t := ctx.Theme()
	col := ui.Div().Column().Gap(2)
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimPrefix(l, "- ")
		m := updateLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		id, name, repo, rest := m[1], m[2], m[3], m[4]
		icon, c, what := icons.Activity, t.TextMuted, rest
		switch {
		case strings.HasPrefix(rest, "finished its turn."):
			icon, c = icons.CircleCheck, t.Success
			what = "finished"
			if s := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(rest, "finished its turn."), " Summary:")); s != "" {
				what += " · " + firstSentence(stripMarkdown(s))
			}
		case strings.HasPrefix(rest, "failed:"):
			icon, c = icons.CircleAlert, t.Danger
			what = "failed · " + strings.TrimSpace(strings.TrimPrefix(rest, "failed:"))
		case strings.HasPrefix(rest, "is asking a question"):
			icon, c, what = icons.MessageCircleQuestionMark, t.Warning, "has a question"
		case strings.HasPrefix(rest, "was interrupted"):
			icon, c, what = icons.CircleStop, t.TextSubtle, "was stopped"
		case strings.HasPrefix(rest, "was merged into "):
			icon, c = icons.GitMerge, ui.Hex(pal(t).info)
			what = "merged into " + strings.TrimSuffix(strings.TrimPrefix(rest, "was merged into "), " by the user.")
		case strings.HasPrefix(rest, "was discarded"):
			icon, c, what = icons.Trash, t.TextSubtle, "was discarded"
		}
		threadID := a.findThreadID(id)
		line := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).Height(ui.Px(26)).Radius(radiusS).
			Child(ui.Icon(icon).Size(14).Color(c)).
			Child(ui.Text(repo).FontSize(12).TextColor(t.TextSubtle).NoWrap().Shrink(0)).
			Child(ui.Text(name).FontSize(13).Medium().NoWrap().Shrink(0).MaxWidth(ui.Percent(45)).Truncate()).
			Child(ui.Text(what).FontSize(12).TextColor(t.TextMuted).Truncate().Grow(1).MinWidth(ui.Px(0)))
		if threadID != "" {
			line = line.Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
				Role(access.RoleButton).Label(name + " " + what).TabIndex(0).
				OnClick(func() { a.openThread(threadID, "conversation") }).
				Child(ui.Icon(icons.ChevronRight).Size(13).Color(t.TextSubtle))
		}
		col = col.Child(line)
	}
	return col
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
		open := ui.Local(ctx, "open", live)
		head := ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(26)).Cursor(ui.CursorPointer).
			Role(access.RoleButton).Label("Thinking").TabIndex(0).
			OnClick(func() { open.Update(func(o bool) bool { return !o }) }).
			Child(ui.Icon(icons.Brain).Size(13).Color(t.TextSubtle)).
			Child(subtle(ctx, "Thinking")).
			Child(ui.Icon(map[bool]*icons.Icon{true: icons.ChevronDown, false: icons.ChevronRight}[open.Get()]).Size(12).Color(t.TextSubtle))
		col := ui.Div().Column().Gap(space1).Child(head)
		if open.Get() {
			col = col.Child(ui.Div().PaddingLeft(space6 + 2).
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
			state = ui.Icon(icons.Check).Size(13).Color(t.TextSubtle)
		}
		head := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).Height(ui.Px(26)).Radius(radiusS).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(label + " " + detail).TabIndex(0).
			OnClick(func() { open.Update(func(o bool) bool { return !o }) }).
			Child(ui.Icon(icon).Size(14).Color(t.TextMuted)).
			Child(ui.Text(label).FontSize(13).Medium().TextColor(t.TextMuted).NoWrap().Shrink(0))
		if threadID != "" {
			head = head.Child(a.threadChip(ctx, threadID)).Child(ui.Div().Grow(1))
		} else {
			head = head.Child(ui.Text(detail).FontSize(12).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0)))
		}
		head = head.Child(state)
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
	return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).Height(ui.Px(26)).
		Child(ui.Icon(icons.Bot).Size(14).Color(t.TextMuted)).
		Child(ui.Text(d.Description).FontSize(13).Medium().TextColor(t.TextMuted).NoWrap().Shrink(0)).
		Child(ui.Text(detail).FontSize(12).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0))).
		Child(state)
}

func turnView(ctx *ui.Context, d core.TurnData) ui.Element {
	t := ctx.Theme()
	if d.Status == agent.TurnInterrupted || d.Status == agent.TurnCancelled {
		return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).
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
	case core.StatusMerged, core.StatusDiscarded:
		actions = statusPill(ctx, th)
	default:
		actions = ui.Div().Row().Gap(space2).
			Child(ui.Button("Merge").Icon(icons.GitMerge).Primary().Small().Disabled(!th.CanMerge).
				OnClick(func() { a.merge(d.ThreadID) })).
			Child(ui.Button("Review changes").Secondary().Small().OnClick(func() { a.openThread(d.ThreadID, "changes") }))
	}
	return ui.Div().Column().Gap(space3).Padding(space4).Radius(radiusL).Background(t.Surface).
		Border(1, brand.Alpha(0.35)).
		Child(a.cardHeader(ctx, icons.GitMerge, brand, "Merge proposed", d.ThreadID)).
		Child(ui.Text(d.Summary).FontSize(13).TextColor(t.Text)).
		Child(actions)
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
		return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).
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
			head = a.cardHeader(ctx, icons.MessageCircleQuestionMark, t.Warning, typ, threadID)
		} else {
			head = a.cardHeader(ctx, icons.MessageCircleQuestionMark, t.Warning, typ, "")
		}
		opts := ui.Div().Column().Gap(space1)
		for i, o := range q.Options {
			label := o.Label
			opt := ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space3).PaddingY(space2).
				Radius(radiusM).Border(1, t.Border).Background(t.Background).Cursor(ui.CursorPointer).
				Hover(ui.Style{Background: t.SurfaceHover, BorderColor: t.TextSubtle}).
				Role(access.RoleButton).Label(o.Label).TabIndex(0).OnClick(func() { pick(label) }).
				Key("opt-" + label).
				Child(ui.Div().Size(ui.Px(14), ui.Px(14)).Radius(7).Border(1.5, t.TextSubtle).Shrink(0))
			text := ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).Child(ui.Text(o.Label).FontSize(13).Medium())
			if o.Description != "" {
				text = text.Child(ui.Text(o.Description).FontSize(12).TextColor(t.TextMuted))
			}
			opt = opt.Child(text)
			if i < 9 {
				opt = opt.Child(ui.Kbd(fmt.Sprint(i + 1)))
			}
			opts = opts.Child(opt)
		}
		sendOther := func() {
			if s := strings.TrimSpace(other.Peek()); s != "" {
				pick(s)
			}
		}
		otherRow := ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(ui.TextInput(other).Placeholder("Your answer").Label("Your answer").Small().Width(ui.Full).
				OnSubmit(func(string) { sendOther() })).
			Child(ui.Button("Send").Secondary().Small().Disabled(strings.TrimSpace(other.Get()) == "").OnClick(sendOther)).
			Child(ui.Button("Skip").Ghost().Small().
				Tooltip("Skip the question; the agent carries on with its own judgment").
				OnClick(func() {
					a.run("Skipping", func(context.Context) error { return a.b.Dismiss(threadID, toolUseID) })
				}))
		return ui.Div().Column().Gap(space3).Padding(space4).Radius(radiusL).Background(t.Surface).
			Border(1, t.Warning.Alpha(0.4)).
			OnKeyDown(func(e *ui.KeyEvent) {
				if e.Key >= input.Key1 && e.Key <= input.Key9 && e.Mods == 0 {
					if i := int(e.Key - input.Key1); i < len(q.Options) {
						e.StopPropagation()
						pick(q.Options[i].Label)
					}
				}
			}).
			Child(head).
			Child(ui.Text(q.Question).FontSize(14).Medium()).
			Child(opts).
			Child(otherRow)
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

// attentionBar lists, above the composer, the threads that wait on the user for something other than
// a question (those show in full above it): ready to merge, and failed.
func (a *App) attentionBar(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	// A merge proposal among the last few messages is already in view; don't repeat it.
	shown := map[string]bool{}
	items := a.itemsOf(core.OrchestratorID).Get()
	for i := len(items) - 1; i >= 0 && i >= len(items)-4; i-- {
		if items[i].Kind == core.ItemProposal {
			shown[items[i].Ref] = true
		}
	}
	var ready, failed []core.ThreadView
	for _, th := range a.threads.Get() {
		if !th.IsWorker {
			continue
		}
		switch th.Status {
		case core.StatusIdle:
			if th.CanMerge && !shown[th.ID] {
				ready = append(ready, th)
			}
		case core.StatusFailed:
			failed = append(failed, th)
		}
	}
	if len(ready) == 0 && len(failed) == 0 {
		return ui.Div()
	}
	pill := func(icon *icons.Icon, c ui.Color, text string, th core.ThreadView, tab string) ui.Element {
		return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space3).Height(ui.Px(28)).Radius(14).Shrink(1).
			MinWidth(ui.Px(0)).MaxWidth(ui.Px(360)).
			Border(1, c.Alpha(0.35)).Background(c.Alpha(0.08)).Cursor(ui.CursorPointer).
			Hover(ui.Style{Background: c.Alpha(0.16)}).
			Role(access.RoleButton).Label(text).TabIndex(0).Tooltip(text).
			OnClick(func() { a.openThread(th.ID, tab) }).
			Child(ui.Icon(icon).Size(13).Color(c)).
			Child(ui.Text(text).FontSize(12).Medium().Truncate().MinWidth(ui.Px(0)))
	}
	row := ui.Div().Row().Align(ui.Center).Gap(space2).MinWidth(ui.Px(0))
	if len(ready) > 0 {
		text := filepath.Base(ready[0].RepoPath) + " · " + ready[0].Title + " is ready to merge"
		if len(ready) > 1 {
			text = fmt.Sprintf("%d ready to merge", len(ready))
		}
		row = row.Child(pill(icons.GitMerge, t.Success, text, ready[0], "changes"))
	}
	if len(failed) > 0 {
		text := filepath.Base(failed[0].RepoPath) + " · " + failed[0].Title + " failed"
		if len(failed) > 1 {
			text = fmt.Sprintf("%d failed", len(failed))
		}
		row = row.Child(pill(icons.CircleAlert, t.Danger, text, failed[0], "conversation"))
	}
	return row.Child(ui.Div().Grow(1)).
		Child(ui.Div().Shrink(0).Tooltip("Open the next thread that needs you").Child(ui.Kbd("Ctrl+J")))
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
		empty := strings.TrimSpace(draft.Get()) == ""
		sendBtn := ui.IconButton(icons.ArrowUp).Small().Label("Send").Tooltip("Send (Enter) · Shift+Enter for a new line").OnClick(submit)
		if empty {
			sendBtn = sendBtn.Secondary().Disabled(true)
		} else {
			sendBtn = sendBtn.Primary()
		}
		actions := ui.Div().Row().Align(ui.Center).Gap(space1).Shrink(0)
		if v.InTurn {
			id := v.ID
			actions = actions.Child(ui.IconButton(icons.Square).Ghost().Small().Label("Stop").
				Tooltip("Stop the current turn").
				OnClick(func() {
					a.run("Stopping", func(ctx context.Context) error { return a.b.Interrupt(ctx, id) })
				}))
		}
		actions = actions.Child(sendBtn)
		area := ui.TextArea(draft).Placeholder(placeholder).Label(placeholder).MinRows(1).Borderless().
			MaxHeight(ui.Px(220)).Width(ui.Full).Key(key + "-area").
			OnKeyDown(func(e *ui.KeyEvent) {
				if e.Key == input.KeyEnter && e.Mods&input.ModShift == 0 {
					e.StopPropagation()
					submit()
				}
			})
		return ui.Div().Row().Align(ui.End).Gap(space2).PaddingLeft(space3).PaddingRight(space2).PaddingY(space2).
			MinHeight(ui.Px(52)).Radius(radiusXL).Border(1, t.BorderStrong).Background(t.Surface).
			FocusWithin(ui.Style{BorderColor: t.TextSubtle}).
			Child(ui.Div().Column().Grow(1).MinWidth(ui.Px(0)).PaddingY(space1).Child(area)).
			Child(actions)
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
		Child(statusMark(ctx, th.Status)).
		Child(ui.Text(th.Title).FontSize(12).Medium().Truncate().MinWidth(ui.Px(0)))
}

// cardHeader is the one line atop a card: its type, then the repository and thread it is about (the
// title opens the thread). threadID "" leaves out the repository and thread.
func (a *App) cardHeader(ctx *ui.Context, icon *icons.Icon, c ui.Color, typ, threadID string) ui.Element {
	t := ctx.Theme()
	row := ui.Div().Row().Align(ui.Center).Gap(space2).MinWidth(ui.Px(0)).
		Child(ui.Icon(icon).Size(15).Color(c)).
		Child(ui.Text(typ).FontSize(12).Semibold().TextColor(c).NoWrap())
	th, ok := a.thread(threadID)
	if threadID == "" || !ok {
		return row
	}
	sep := func() ui.Element { return ui.Text("·").FontSize(12).TextColor(t.TextSubtle) }
	return row.
		Child(sep()).
		Child(ui.Text(filepath.Base(th.RepoPath)).FontSize(12).TextColor(t.TextMuted).NoWrap()).
		Child(sep()).
		Child(ui.Div().MinWidth(ui.Px(0)).Cursor(ui.CursorPointer).
			Role(access.RoleButton).Label("Open " + th.Title).TabIndex(0).Tooltip("Open this thread").
			Hover(ui.Style{TextColor: t.Link}).
			OnClick(func() { a.openThread(threadID, "conversation") }).
			Child(ui.Text(th.Title).FontSize(12).Medium().Truncate()))
}
