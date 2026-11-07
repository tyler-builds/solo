package gui

import (
	"context"
	"fmt"
	"github.com/tyler-builds/solo/internal/forge"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/codeedit/highlight"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
)

func (a *App) merge(id string) {
	a.run("Merge", func(ctx context.Context) error {
		commit, err := a.b.Merge(ctx, id)
		if err != nil {
			return err
		}
		th, _ := a.b.Thread(id)
		if len(commit) > 8 {
			commit = commit[:8]
		}
		a.notify(ui.ToastSuccess, "Merged", fmt.Sprintf("%s is in %s (%s).", th.Title, th.BaseBranch, commit))
		return nil
	})
}

// openPR opens a pull request for a thread, as a draft when draft is set.
func (a *App) openPR(id string, draft bool) {
	a.run("Opening the pull request", func(ctx context.Context) error {
		pr, err := a.b.OpenPR(ctx, id, draft)
		if err != nil {
			return err
		}
		what := "Opened"
		if draft {
			what = "Opened draft"
		}
		a.notify(ui.ToastSuccess, fmt.Sprintf("%s %s #%d", what, pr.Kind.Noun(), pr.Number), pr.URL)
		return nil
	})
}

// mergeLocal merges a thread's branch in the local checkout, skipping the pull request.
func (a *App) mergeLocal(id string) {
	a.run("Merge", func(ctx context.Context) error {
		_, err := a.b.MergeLocal(ctx, id)
		return err
	})
}

// landControl is a thread's main landing action: Open PR, Merge (its pull request, or locally without
// a forge), or a note that auto-merge will do it. nil when there is nothing to land.
func (a *App) landControl(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	id := th.ID
	switch {
	case th.CanOpenPR:
		noun := th.Forge.Noun()
		return ui.SplitButton("Open "+noun, func() { a.openPR(id, false) }, func() []ui.MenuItem {
			return []ui.MenuItem{{Label: "Open as draft", Icon: icons.GitPullRequestDraft, OnClick: func() { a.openPR(id, true) }}}
		}).Icon(icons.GitPullRequestArrow).Primary().Small().Key("open-pr-" + id)
	case th.CanMarkReady:
		return ui.Button("Ready for review").Icon(icons.GitPullRequest).Primary().Small().
			Tooltip(fmt.Sprintf("Mark %s #%d ready for review", th.PR.Kind.Noun(), th.PR.Number)).
			OnClick(func() {
				a.run("Marking it ready", func(ctx context.Context) error { return a.b.MarkReady(ctx, id) })
			})
	case th.PR != nil && th.PR.State == forge.StateOpen:
		if th.CanMerge {
			return ui.Button("Merge").Icon(icons.GitMerge).Primary().Small().
				Tooltip(fmt.Sprintf("Merge %s #%d into %s", th.PR.Kind.Noun(), th.PR.Number, th.BaseBranch)).OnClick(func() { a.merge(id) })
		}
		if th.PR.AutoMerge {
			return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(28)).Shrink(0).
				Tooltip("It merges once its required checks pass").
				Child(ui.Icon(icons.Timer).Size(13).Color(t.TextMuted)).
				Child(ui.Text("Auto-merge on").FontSize(12).Medium().TextColor(t.TextMuted).NoWrap())
		}
		return ui.Button("Merge").Icon(icons.GitMerge).Secondary().Small().Disabled(true).
			Tooltip("Can't merge yet: " + th.PR.Blocked)
	case th.CanMerge:
		return ui.Button("Merge").Icon(icons.GitMerge).Primary().Small().
			Tooltip("Merge " + th.Branch + " into " + th.BaseBranch).OnClick(func() { a.merge(id) })
	}
	return nil
}

// prLink opens a thread's pull request in the browser.
func (a *App) prLink(ctx *ui.Context, th core.ThreadView) ui.Element {
	if th.PR == nil {
		return nil
	}
	url := th.PR.URL
	return ui.Button(fmt.Sprintf("#%d", th.PR.Number)).Icon(icons.ExternalLink).Ghost().Small().
		Tooltip("Open the " + th.PR.Kind.Noun() + " in your browser").OnClick(func() { _ = ctx.OpenURL(url) })
}

// changeData is a worker's changed files and diff.
type changeData struct {
	files []git.FileChange
	diff  string
}

// changes loads a worker's changes, again whenever the thread changes.
func (a *App) changes(ctx *ui.Context, th core.ThreadView) ui.QueryResult[changeData] {
	key := fmt.Sprintf("changes:%s:%s:%d", th.ID, th.Status, th.UpdatedAt.UnixMilli())
	return ui.Query(ctx, key, func(c context.Context) (changeData, error) {
		if !th.HasWorktree {
			return changeData{}, nil
		}
		files, err := a.b.Changes(c, th.ID)
		if err != nil {
			return changeData{}, err
		}
		diff, err := a.b.Diff(c, th.ID)
		return changeData{files, diff}, err
	})
}

// threadPanel shows the open thread beside the orchestrator: its conversation or its changes, and its
// actions.
func (a *App) threadPanel(ctx *ui.Context) ui.Element {
	id := a.openID.Get()
	th, ok := a.thread(id)
	if !ok {
		return ui.Div()
	}
	t := ctx.Theme()
	tab := a.openTab.Get()
	res := a.changes(ctx, th)
	added, removed := 0, 0
	for _, f := range res.Data.files {
		added += f.Added
		removed += f.Removed
	}
	if th.InTurn {
		tick := ui.Local(ctx, "tick", 0)
		tick.Get()
		ctx.After(time.Second, func() { tick.Update(func(n int) int { return n + 1 }) })
	}

	// A narrow panel moves the change size into the meta line and keeps only Merge's icon.
	stat := ui.Div().Row().Gap(6).
		Child(ui.Text(fmt.Sprintf("+%d", added)).FontSize(12).Medium().TextColor(t.Success)).
		Child(ui.Text(fmt.Sprintf("−%d", removed)).FontSize(12).Medium().TextColor(t.Danger))
	actions := ui.Div().Row().Align(ui.Center).Gap(space1).Shrink(0)
	actions = actions.Child(a.prLink(ctx, th))
	switch {
	case th.CanOpenPR || th.CanMerge || th.CanMarkReady || (th.PR != nil && th.PR.State == forge.StateOpen):
		actions = actions.Child(a.landControl(ctx, th))
	case th.CanDone:
		actions = actions.Child(ui.Button("Done").Icon(icons.Check).Primary().Small().
			Tooltip("Take it out of your list. Settings › History can restore it.").
			OnClick(func() { a.markDone(th) }))
	}
	if th.CanDiscard || th.CanInterrupt || th.CanMergeLocal {
		actions = actions.Child(ui.DropdownMenu(
			ui.IconButton(icons.Ellipsis).Ghost().Small().Label("More actions").Tooltip("More"),
			func() []ui.MenuItem {
				var items []ui.MenuItem
				if th.CanInterrupt {
					items = append(items, ui.MenuItem{Label: "Stop", Icon: icons.Square, OnClick: func() {
						a.run("Stopping", func(ctx context.Context) error { return a.b.Interrupt(ctx, id) })
					}})
				}
				if th.CanMergeLocal && th.Forge != forge.None {
					items = append(items, ui.MenuItem{Label: "Merge locally instead", Icon: icons.GitMerge, OnClick: func() { a.mergeLocal(id) }})
				}
				if th.CanDone && (th.CanOpenPR || th.CanMerge || th.PR != nil) {
					items = append(items, ui.MenuItem{Label: "Done", Icon: icons.Check, OnClick: func() { a.markDone(th) }})
				}
				if th.CanDiscard {
					items = append(items, ui.MenuItem{Label: "Discard thread…", Icon: icons.Trash, OnClick: func() { a.confirmDiscard(th) }})
				}
				return items
			}))
	}
	actions = actions.Child(ui.IconButton(icons.X).Ghost().Small().Label("Close thread").Tooltip("Close").
		OnClick(a.closeThread))

	// The mark carries the status, so the line under the title is just where it is and how big.
	repo := ui.Div().Child(ui.Text(filepath.Base(th.RepoPath)).FontSize(12).Medium().TextColor(t.TextMuted).NoWrap())
	if th.Branch != "" {
		repo = repo.Tooltip(branchLabel(th) + " → " + th.BaseBranch)
	}
	meta := ui.Div().Row().Align(ui.Center).Gap(space2).Wrap().MinWidth(ui.Px(0)).Child(repo)
	if added+removed > 0 {
		meta = meta.Child(stat)
	}
	if a.cfg.Get().ShowCosts && th.CostUSD > 0 {
		meta = meta.Child(subtle(ctx, fmt.Sprintf("$%.2f", th.CostUSD)))
	}
	header := ui.Div().Row().Align(ui.Start).Gap(space2).PaddingLeft(space4).PaddingRight(space2).PaddingY(space3).Shrink(0).
		Child(ui.Div().Column().Gap(space1).Grow(1).MinWidth(ui.Px(0)).
			Child(ui.Div().Row().Align(ui.Center).Gap(space2).MinWidth(ui.Px(0)).
				Child(ui.Div().Shrink(0).Tooltip(statusLabel(th)).Child(statusMark(ctx, shownStatus(th)))).
				Child(ui.Div().MinWidth(ui.Px(0)).Tooltip(th.Title).
					Child(ui.Text(th.Title).FontSize(14).Semibold().Truncate()))).
			Child(ui.Div().PaddingLeft(space5 + 2).Child(meta))).
		Child(actions)
	changesLabel := "Changes"
	if n := len(res.Data.files); n > 0 {
		changesLabel = fmt.Sprintf("Changes · %d", n)
	}
	tabs := ui.Div().Row().Align(ui.Center).PaddingX(space4).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(ui.Tabs(a.openTab,
			ui.Tab[string]{Value: "conversation", Label: "Conversation"},
			ui.Tab[string]{Value: "changes", Label: changesLabel, Disabled: !th.HasWorktree},
		))

	var content ui.Element
	if tab == "changes" && th.HasWorktree {
		content = a.changesView(ctx, res)
	} else {
		items := a.itemsOf(id).Get()
		content = ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).
			Child(a.transcript(ctx, th, items, "thread-"+id)).
			Child(ui.Div().Column().Gap(space2).PaddingX(space4).PaddingBottom(space4).Shrink(0).
				Child(a.workingLine(ctx, th)).
				Child(a.composerFor(ctx, th)))
	}
	return ui.Div().Column().Size(ui.Full, ui.Full).Key("thread-panel-" + id).
		Child(header).
		Child(tabs).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).Child(content))
}

// workingLine is pinned above a working thread's composer: how long it has worked, and its step.
func (a *App) workingLine(ctx *ui.Context, th core.ThreadView) ui.Element {
	if !th.InTurn {
		return ui.Div()
	}
	t := ctx.Theme()
	brand := ui.Hex(pal(t).brand)
	text := "Working"
	if !th.TurnStarted.IsZero() {
		text += " · " + elapsed(th.TurnStarted)
	}
	step := "Thinking"
	if th.Step != nil {
		step = a.stepLabel(*th.Step)
	} else if th.Retry != "" {
		step = th.Retry
	}
	return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).Height(ui.Px(30)).Radius(radiusM).
		Background(brand.Alpha(0.07)).
		Child(statusMark(ctx, core.StatusRunning)).
		Child(ui.Text(text).FontSize(12).Semibold().TextColor(brand).NoWrap()).
		Child(ui.Div().Grow(1).MinWidth(ui.Px(0)).
			Play("shimmer", []ui.Keyframe{{At: 0.5, Motion: ui.Fade().Opacity(0.55)}},
				ui.Animation{Duration: 1600 * time.Millisecond, Repeat: true}).
			Child(ui.Text(step).FontSize(12).TextColor(t.TextMuted).Truncate()))
}

// panelWidth estimates the thread panel's width from the window and the split's size.
func (a *App) panelWidth(ctx *ui.Context) float32 {
	w, _ := ctx.WindowSize()
	if l := a.panelSize.Get(); l.Unit == ui.Px(0).Unit {
		return l.Value
	}
	// The split's length is the window less the rail; the pane's padding takes space2 of it.
	return (w-railW(ctx)-space2)*a.panelGoal/100 - space2
}

// splitMinWidth is the narrowest panel that shows a diff side by side.
const splitMinWidth = 640

// widenFor reports whether the thread panel can be at least px wide, and widens it if it can.
// Asking without room changes nothing.
func (a *App) widenFor(ctx *ui.Context, px float32) bool {
	if a.panelWidth(ctx) >= px {
		return true
	}
	w, _ := ctx.WindowSize()
	length := w - railW(ctx) - space2
	pct := (px + space2) / length * 100
	if pct > 75 || length-px < 340 {
		return false
	}
	// Only report here; the caller widens on click (see segmented), so a render doesn't resize.
	return true
}

// widenNow widens the panel to px.
func (a *App) widenNow(ctx *ui.Context, px float32) {
	w, _ := ctx.WindowSize()
	pct := (px + space2) / (w - railW(ctx) - space2) * 100
	a.panelGoal = pct
	a.panelSize.Set(ui.Percent(pct))
}

// confirmDiscard asks before discarding a thread.
func (a *App) confirmDiscard(th core.ThreadView) {
	id := th.ID
	a.confirm.Set(&confirmation{
		title:  "Discard this thread?",
		body:   fmt.Sprintf("This stops %q and deletes its worktree and branch %s, with any work not merged.", th.Title, th.Branch),
		action: "Discard",
		run: func() {
			if a.openID.Peek() == id {
				a.closeThread()
			}
			a.run("Discard", func(ctx context.Context) error { return a.b.Discard(ctx, id) })
		},
	})
}

func (a *App) composerFor(ctx *ui.Context, th core.ThreadView) ui.Element {
	if core.Finished(th.Status) {
		return caption(ctx, "This thread is "+strings.ToLower(statusLabel(th))+".")
	}
	if th.Status == core.StatusQueued {
		return caption(ctx, "This thread is starting.")
	}
	id := th.ID
	return a.composer(ctx, th, "draft-"+id, "Message this thread…", func(text string, images []string) error {
		return a.b.Send(context.Background(), id, text, images...)
	})
}

// changesView lists a worker's changed files over its diff, a collapsible section per file.
func (a *App) changesView(ctx *ui.Context, res ui.QueryResult[changeData]) ui.Element {
	switch {
	case res.Loading && len(res.Data.files) == 0:
		return ui.Div().Column().Center().Grow(1).Child(ui.ProgressRing().Label("Loading changes"))
	case res.Err != nil:
		return ui.Div().Column().Center().Grow(1).Padding(space6).
			Child(ui.Empty("Could not read the changes").Icon(icons.CircleAlert).Description(res.Err.Error()))
	case len(res.Data.files) == 0:
		return ui.Div().Column().Center().Grow(1).Padding(space6).
			Child(ui.Empty("No changes yet").Icon(icons.FileCode).Description("Files the thread changes show up here."))
	}
	files := parseDiff(res.Data.diff)
	return ui.Component(func(ctx *ui.Context) ui.Element {
		// Side by side needs room; a narrow panel shows the unified diff.
		return diffView(ctx, res.Data.files, files, a.panelWidth(ctx) >= splitMinWidth, func() bool { return a.widenFor(ctx, splitMinWidth) }, func() { a.widenNow(ctx, splitMinWidth) })
	})
}

// diffFile is one file of a unified diff.
type diffFile struct {
	path     string
	lines    []diffLine
	colors   map[int][]span // line index → syntax spans
	add, del int
}

type diffLine struct {
	kind     byte // ' ', '+', '-', '@'
	old, new int  // line numbers; 0 when not on that side
	text     string
	// wordStart and wordEnd mark the part of a changed line that differs from its counterpart.
	wordStart, wordEnd int
}

type span struct {
	start, end int
	capture    string
}

// parseDiff splits a unified diff into files, with line numbers, and colors their code.
func parseDiff(diff string) []*diffFile {
	var files []*diffFile
	var f *diffFile
	oldN, newN := 0, 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			f = &diffFile{}
			if i := strings.LastIndex(l, " b/"); i >= 0 {
				f.path = l[i+3:]
			}
			files = append(files, f)
			continue
		case f == nil:
			continue
		case strings.HasPrefix(l, "+++ "):
			if p := strings.TrimPrefix(l, "+++ "); p != "/dev/null" {
				f.path = strings.TrimPrefix(p, "b/")
			}
			continue
		case strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "index "), strings.HasPrefix(l, "similarity"),
			strings.HasPrefix(l, "rename "), strings.HasPrefix(l, "old mode"), strings.HasPrefix(l, "new mode"),
			strings.HasPrefix(l, "new file"), strings.HasPrefix(l, "deleted file"), strings.HasPrefix(l, "Binary files"):
			continue
		case strings.HasPrefix(l, "@@"):
			parts := strings.Fields(l) // @@ -a,b +c,d @@ context
			if len(parts) >= 3 {
				oldN, newN = hunkStart(parts[1]), hunkStart(parts[2])
			}
			f.lines = append(f.lines, diffLine{kind: '@', text: l})
			continue
		}
		if l == "" || strings.HasPrefix(l, `\ `) {
			continue
		}
		text := strings.ReplaceAll(l[1:], "\t", "    ")
		switch l[0] {
		case '+':
			f.lines = append(f.lines, diffLine{kind: '+', new: newN, text: text})
			f.add++
			newN++
		case '-':
			f.lines = append(f.lines, diffLine{kind: '-', old: oldN, text: text})
			f.del++
			oldN++
		default:
			f.lines = append(f.lines, diffLine{kind: ' ', old: oldN, new: newN, text: text})
			oldN++
			newN++
		}
	}
	for _, f := range files {
		f.pairWords()
		f.colorize()
	}
	return files
}

// pairWords marks, in each removed line and the added line that replaces it, the part that changed:
// the text between their common prefix and suffix.
func (f *diffFile) pairWords() {
	for i := 0; i < len(f.lines); {
		if f.lines[i].kind != '-' {
			i++
			continue
		}
		dels := i
		for i < len(f.lines) && f.lines[i].kind == '-' {
			i++
		}
		adds := i
		for i < len(f.lines) && f.lines[i].kind == '+' {
			i++
		}
		for k := 0; dels+k < adds && adds+k < i; k++ {
			d, a := &f.lines[dels+k], &f.lines[adds+k]
			p := commonPrefix(d.text, a.text)
			q := commonSuffix(d.text[p:], a.text[p:])
			// Only mark a real edit inside the line, not a line rewritten from scratch.
			if p+q == 0 {
				continue
			}
			d.wordStart, d.wordEnd = p, len(d.text)-q
			a.wordStart, a.wordEnd = p, len(a.text)-q
		}
	}
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	for n > 0 && n < len(a) && !utf8.RuneStart(a[n]) {
		n--
	}
	return n
}

func commonSuffix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	for n > 0 && n < len(a) && !utf8.RuneStart(a[len(a)-n]) {
		n--
	}
	return n
}

func hunkStart(s string) int {
	s = strings.TrimLeft(s, "-+")
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.Atoi(s)
	return n
}

// colorize lexes the new side (context and added lines) and the old side (removed lines) of the file
// as its language, and keeps each line's spans.
func (f *diffFile) colorize() {
	lang := strings.TrimPrefix(filepath.Ext(f.path), ".")
	if lang == "" || !highlight.Supported(lang) {
		return
	}
	f.colors = map[int][]span{}
	for _, side := range []func(diffLine) bool{
		func(l diffLine) bool { return l.kind == ' ' || l.kind == '+' },
		func(l diffLine) bool { return l.kind == '-' },
	} {
		var b strings.Builder
		var idx, starts []int
		for i, l := range f.lines {
			if !side(l) {
				continue
			}
			idx = append(idx, i)
			starts = append(starts, b.Len())
			b.WriteString(l.text)
			b.WriteByte('\n')
		}
		if len(idx) == 0 {
			continue
		}
		spans, ok := code.HighlightCode(context.Background(), lang, b.String(), func() {})
		if !ok {
			continue
		}
		li := 0
		for _, s := range spans {
			for li+1 < len(starts) && starts[li+1] <= s.Start {
				li++
			}
			lineStart := starts[li]
			lineEnd := lineStart + len(f.lines[idx[li]].text)
			start, end := s.Start-lineStart, min(s.End, lineEnd)-lineStart
			if start < 0 || end <= start {
				continue
			}
			f.colors[idx[li]] = append(f.colors[idx[li]], span{start, end, s.Capture})
		}
	}
}

const diffRow = 22

// diffView shows the file list, then each file's diff: a header that folds it, and its lines with
// old and new line numbers, colored by change and by syntax.
func diffView(ctx *ui.Context, stats []git.FileChange, files []*diffFile, roomy bool, canWiden func() bool, widen func()) ui.Element {
	t := ctx.Theme()
	folded := ui.Local(ctx, "folded", map[string]bool{})
	h := ui.Local(ctx, "list", ui.NewListHandle()).Peek()
	fold := folded.Get()
	split := diffSplit

	type rowRef struct {
		file *diffFile
		line int // -1 for the file's header
	}
	var rows []rowRef
	headerAt := map[string]int{}
	for _, f := range files {
		headerAt[f.path] = len(rows)
		rows = append(rows, rowRef{f, -1})
		if fold[f.path] {
			continue
		}
		for i := range f.lines {
			rows = append(rows, rowRef{f, i})
		}
	}

	added, removed := 0, 0
	list := ui.Div().Column().PaddingX(space2).PaddingY(space2).BorderEdges(ui.Edges{Bottom: 1}, t.Border).Shrink(0).
		MaxHeight(ui.Px(176)).ScrollY()
	for _, f := range stats {
		added += f.Added
		removed += f.Removed
		path := f.Path
		list = list.Child(ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).Height(ui.Px(26)).Radius(radiusS).
			Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
			Role(access.RoleButton).Label(path).TabIndex(0).
			OnClick(func() {
				if i, ok := headerAt[path]; ok {
					h.ScrollToItem(i)
				}
			}).
			Child(ui.Text(fileStatus(f.Status)).Mono().FontSize(12).Semibold().TextColor(fileStatusColor(ctx, f.Status)).Width(ui.Px(12))).
			Child(ui.Text(filepath.Base(path)).FontSize(13).Medium().NoWrap()).
			Child(ui.Text(filepath.Dir(path)).FontSize(12).TextColor(t.TextSubtle).TruncateHead().Grow(1).MinWidth(ui.Px(0))).
			Child(counts(ctx, f.Added, f.Removed, 12)))
	}

	gutter := func(n int) ui.Element {
		s := ""
		if n > 0 {
			s = strconv.Itoa(n)
		}
		return ui.Div().Row().Justify(ui.End).Width(ui.Px(40)).PaddingRight(space2).Shrink(0).
			Child(ui.Text(s).Mono().FontSize(11).TextColor(t.TextSubtle))
	}
	addBg, delBg := t.Success.Alpha(0.16), t.Danger.Alpha(0.16)
	addWord, delWord := t.Success.Alpha(0.36), t.Danger.Alpha(0.36)
	// lineText is a line's code, colored by syntax, with its changed part marked.
	lineText := func(f *diffFile, i int) *ui.TextElement {
		l := f.lines[i]
		txt := ui.Text(l.text).Mono().FontSize(12).NoWrap().TextColor(t.Text)
		for _, sp := range f.colors[i] {
			if _, st, ok := highlight.Closest(t.Syntax, sp.capture); ok && sp.end <= len(l.text) {
				txt = txt.Highlight(sp.start, sp.end, ui.Highlight{Color: st.Color})
			}
		}
		if l.wordEnd > l.wordStart {
			bg := addWord
			if l.kind == '-' {
				bg = delWord
			}
			txt = txt.Highlight(l.wordStart, l.wordEnd, ui.Highlight{Background: bg})
		}
		return txt
	}
	fileHeader := func(f *diffFile) ui.Element {
		path := f.path
		return ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space3).Height(ui.Px(diffRow)).
			Background(t.SurfaceActive).Cursor(ui.CursorPointer).
			Role(access.RoleButton).Label(path).TabIndex(0).
			OnClick(func() {
				next := map[string]bool{}
				for k, v := range folded.Peek() {
					next[k] = v
				}
				next[path] = !next[path]
				folded.Set(next)
			}).
			Child(ui.Icon(map[bool]*icons.Icon{true: icons.ChevronRight, false: icons.ChevronDown}[fold[path]]).Size(13).Color(t.TextMuted)).
			Child(ui.Text(path).Mono().FontSize(12).Semibold().TruncateHead().Grow(1).MinWidth(ui.Px(0))).
			Child(counts(ctx, f.add, f.del, 11)).
			Child(ui.Div().Size(ui.Px(20), ui.Px(20)).Center().Radius(radiusS).Cursor(ui.CursorPointer).
				Hover(ui.Style{Background: t.SurfaceHover}).
				Role(access.RoleButton).Label("Copy path").Tooltip("Copy path").TabIndex(0).
				OnClick(func() {
					ctx.SetClipboard(path)
					ctx.Toast(ui.ToastInfo, path).Title("Path copied")
				}).
				Child(ui.Icon(icons.Copy).Size(12).Color(t.TextSubtle)))
	}
	hunk := func(text string, indent float32) ui.Element {
		return ui.Div().Row().Align(ui.Center).Height(ui.Px(diffRow)).Background(t.Surface).PaddingLeft(indent).
			Child(ui.Text(text).Mono().FontSize(11).TextColor(t.TextSubtle).NoWrap())
	}

	var body ui.Element
	if !split.Get() || !roomy {
		body = ui.UniformList(len(rows), func(i int) ui.Element {
			r := rows[i]
			f := r.file
			if r.line < 0 {
				return fileHeader(f)
			}
			l := f.lines[r.line]
			if l.kind == '@' {
				return hunk(l.text, 96)
			}
			row := ui.Div().Row().Align(ui.Center).Height(ui.Px(diffRow))
			sign, signColor := " ", t.TextSubtle
			switch l.kind {
			case '+':
				row, sign, signColor = row.Background(addBg), "+", t.Success
			case '-':
				row, sign, signColor = row.Background(delBg), "−", t.Danger
			}
			return row.
				Child(gutter(l.old)).
				Child(gutter(l.new)).
				Child(ui.Div().Width(ui.Px(16)).Shrink(0).Child(ui.Text(sign).Mono().FontSize(12).TextColor(signColor))).
				Child(lineText(f, r.line))
		}).Handle(h).Style(ui.Style{Grow: 1}).Label("Diff")
	} else {
		// Side by side: removed lines on the left, added on the right, paired in order.
		type pair struct {
			file        *diffFile
			left, right int // line indexes; -1 for none; both -2 for the file header
		}
		var pairs []pair
		for _, f := range files {
			pairs = append(pairs, pair{f, -2, -2})
			if fold[f.path] {
				continue
			}
			for i := 0; i < len(f.lines); {
				switch f.lines[i].kind {
				case '@', ' ':
					pairs = append(pairs, pair{f, i, i})
					i++
				default:
					// A change block runs until the next unchanged line; its removed lines go left and
					// its added lines right, in order, however they interleave.
					var dels, adds []int
					for i < len(f.lines) && (f.lines[i].kind == '-' || f.lines[i].kind == '+') {
						if f.lines[i].kind == '-' {
							dels = append(dels, i)
						} else {
							adds = append(adds, i)
						}
						i++
					}
					for k := 0; k < max(len(dels), len(adds)); k++ {
						p := pair{f, -1, -1}
						if k < len(dels) {
							p.left = dels[k]
						}
						if k < len(adds) {
							p.right = adds[k]
						}
						pairs = append(pairs, p)
					}
				}
			}
		}
		side := func(f *diffFile, i int, left bool) ui.Element {
			box := ui.Div().Row().Align(ui.Center).Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(0)).Height(ui.Full).
				PaddingRight(space2).ClipContent()
			if i < 0 {
				return box.Background(t.Text.Alpha(0.045))
			}
			l := f.lines[i]
			n := l.new
			if left {
				n = l.old
			}
			switch {
			case l.kind == '-':
				box = box.Background(delBg)
			case l.kind == '+':
				box = box.Background(addBg)
			}
			return box.Child(gutter(n)).Child(lineText(f, i))
		}
		body = ui.UniformList(len(pairs), func(i int) ui.Element {
			p := pairs[i]
			if p.left == -2 {
				return fileHeader(p.file)
			}
			if p.left >= 0 && p.file.lines[p.left].kind == '@' {
				return hunk(p.file.lines[p.left].text, 48)
			}
			return ui.Div().Row().Height(ui.Px(diffRow)).
				Child(side(p.file, p.left, true)).
				Child(ui.Div().Width(ui.Px(1)).Height(ui.Full).Background(t.BorderStrong).Shrink(0)).
				Child(side(p.file, p.right, false))
		}).Handle(h).Style(ui.Style{Grow: 1}).Label("Diff, side by side")
	}

	summary := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space4).Height(ui.Px(36)).Shrink(0).
		BorderEdges(ui.Edges{Bottom: 1}, t.Border).
		Child(subhead(fmt.Sprintf("%d files changed", len(stats)))).
		Child(ui.Text(fmt.Sprintf("+%d", added)).FontSize(12).TextColor(t.Success)).
		Child(ui.Text(fmt.Sprintf("−%d", removed)).FontSize(12).TextColor(t.Danger)).
		Child(ui.Div().Grow(1)).
		Child(segmented(ctx, split, roomy, canWiden, widen))
	return ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).
		Child(summary).
		Child(list).
		Child(body)
}

func fileStatus(s string) string {
	if s == "?" {
		return "A"
	}
	return s
}

func fileStatusColor(ctx *ui.Context, s string) ui.Color {
	t := ctx.Theme()
	switch s {
	case "A", "?":
		return t.Success
	case "D":
		return t.Danger
	}
	return t.Warning
}

// diffSplit is whether diffs show side by side; it is kept across threads.
var diffSplit = ui.NewSignal(false)

// segmented is the Unified / Split switch of the diff.
func segmented(ctx *ui.Context, split *ui.Signal[bool], roomy bool, canWiden func() bool, widen func()) ui.Element {
	t := ctx.Theme()
	shown := split.Get() && roomy // what the diff actually shows
	canSplit := roomy || canWiden()
	opt := func(label string, on bool) ui.Element {
		d := ui.Div().PaddingX(space3).Height(ui.Px(24)).Radius(radiusS - 1).Center().
			Role(access.RoleButton).Label(label).TabIndex(0).AccessSelected(shown == on)
		if on && !canSplit {
			return d.Opacity(0.45).Tooltip("The window is too narrow to compare side by side").
				Child(ui.Text(label).FontSize(12).TextColor(t.TextMuted))
		}
		d = d.Cursor(ui.CursorPointer).OnClick(func() {
			if on && !roomy {
				widen() // the panel grows to fit; the diff goes side by side on the next render
			}
			split.Set(on)
		})
		if shown == on {
			return d.Background(t.SurfaceActive).Child(ui.Text(label).FontSize(12).Medium())
		}
		return d.Hover(ui.Style{Background: t.SurfaceHover}).Child(ui.Text(label).FontSize(12).TextColor(t.TextMuted))
	}
	return ui.Div().Row().Align(ui.Center).Gap(2).Padding(2).Radius(radiusS+2).Border(1, t.Border).Background(t.Surface).
		Child(opt("Unified", false)).
		Child(opt("Split", true))
}

// counts shows a change's added and removed lines, leaving out a side that is zero.
func counts(ctx *ui.Context, added, removed int, size float32) ui.Element {
	t := ctx.Theme()
	row := ui.Div().Row().Gap(6).Shrink(0)
	if added > 0 || removed == 0 {
		row = row.Child(ui.Text(fmt.Sprintf("+%d", added)).Mono().FontSize(size).TextColor(t.Success))
	}
	if removed > 0 {
		row = row.Child(ui.Text(fmt.Sprintf("−%d", removed)).Mono().FontSize(size).TextColor(t.Danger))
	}
	return row
}
