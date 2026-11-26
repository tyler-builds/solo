package gui

// The board: the worker threads as cards in five columns, in place of the rail and Solo's conversation
// (Settings.Board). A card is a thread, and where it stands decides its column, so the agents move the
// cards, not the user:
//
//   - To do: the new-task box, and threads starting (a card added here starts at once)
//   - Working: a turn, or its background work, is running
//   - Your turn: it waits on the user: a question, a review, a failure, or an answer to read
//   - Landing: the user pressed Merge or Open PR, and it is on its way in (checks, auto-merge, conflicts)
//   - Done: merged, discarded, or marked done, lately
//
// What a card says comes from its brief (core.Brief), which a small model writes from the thread's
// activity, never the agent's own words. Clicking a card opens the thread beside the board.

import (
	"context"
	"fmt"
	"image"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/dialog"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// Board columns.
const (
	colTodo    = "todo"
	colWorking = "working"
	colYou     = "you"
	colLanding = "landing"
	colDone    = "done"
)

var boardColumns = []struct{ key, title, empty string }{
	{colTodo, "To do", ""},
	{colWorking, "Working", "Nothing running."},
	{colYou, "Your turn", "Nothing needs you."},
	{colLanding, "Landing", "Nothing on its way in."},
	{colDone, "Done", "Nothing finished lately."},
}

const (
	boardColMin  = 248 // a column's narrowest; narrower windows scroll the board sideways
	boardDoneMax = 30  // the Done column shows at most this many
)

// boardDoneFor is how long a finished thread stays in the Done column.
var boardDoneFor = 3 * 24 * time.Hour

// boardColumn is the column a worker thread's card is in. busy says a landing action is under way.
func boardColumn(th core.ThreadView, busy bool) string {
	switch {
	case core.Finished(th.Status) || !th.DoneAt.IsZero():
		return colDone
	case th.Status == core.StatusQueued || th.Status == core.StatusStarting:
		return colTodo
	case th.Card.CardWorking():
		return colWorking // its lead or builder is at it: planning, building, reviewing, answering
	case th.Status == core.StatusWaiting:
		return colYou // a question, even mid-turn
	case th.Resolving || busy:
		return colLanding
	case core.Active(th.Status):
		return colWorking
	}
	if len(cardPRs(th)) > 0 && th.Status != core.StatusFailed && !checksFailing(th) {
		if needsUser(th) {
			return colYou // its checks passed, and it waits for the user's Merge
		}
		return colLanding
	}
	return colYou
}

// cardPRs are a thread's open pull requests: its parts', or the one on the thread.
func cardPRs(th core.ThreadView) []*forge.PR {
	if open := openPRs(th); len(open) > 0 {
		return open
	}
	if th.PR != nil && th.PR.State == forge.StateOpen {
		return []*forge.PR{th.PR}
	}
	return nil
}

// checksFailing reports whether checks failed on one of a thread's open pull requests.
func checksFailing(th core.ThreadView) bool {
	return slices.ContainsFunc(cardPRs(th), func(pr *forge.PR) bool { return pr.Checks == forge.ChecksFailing })
}

// cardReason is why a card in Your turn or Landing is there, in a word or two, and the color it shows in.
func cardReason(ctx *ui.Context, th core.ThreadView, col string) (string, ui.Color) {
	t := ctx.Theme()
	info := ui.Hex(pal(t).info)
	if col == colLanding {
		if th.Resolving {
			return "Resolving conflicts", t.Warning
		}
		prs := cardPRs(th)
		if len(prs) == 1 {
			switch pr := prs[0]; {
			case pr.Checks == forge.ChecksPending || pr.Checks == forge.ChecksWaiting:
				return "Waiting for checks", t.Warning
			case pr.AutoMerge:
				return "Auto-merge on", info
			}
			return "In review", info
		}
		return statusLabel(th), statusColor(ctx, shownStatus(th))
	}
	if col != colYou {
		return "", t.TextSubtle
	}
	need := ""
	if th.Brief != nil {
		need = th.Brief.Need
	}
	if isCard(th) {
		// The lead's review says what the card needs.
		switch {
		case th.Card.Need == core.BriefNeedInput:
			return "Needs input", t.Warning
		case th.Card.Need == core.BriefNeedBlocked:
			return "Blocked", t.Danger
		case checksFailing(th):
			return "Checks failed", t.Danger
		case th.CanOpenPR || th.CanMerge || th.CanMergeLocal:
			return "Ready to merge", t.Success
		case th.Changed <= 0:
			return "Answered", info
		}
		return "Reviewed", t.Success
	}
	switch {
	case th.Status == core.StatusWaiting:
		return "Question", t.Warning
	case th.Status == core.StatusFailed:
		return "Failed", t.Danger
	case checksFailing(th):
		return "Checks failed", t.Danger
	case th.Status == core.StatusInterrupted:
		return "Stopped", t.TextMuted
	case need == core.BriefNeedBlocked:
		return "Blocked", t.Danger
	case need == core.BriefNeedInput:
		return "Needs input", t.Warning
	case th.PR != nil && th.PR.State == forge.StateOpen && th.CanMerge:
		return "Ready to merge", t.Success
	case th.CanOpenPR || th.CanMerge || th.CanMergeLocal:
		return "Ready to merge", t.Success
	case th.InPlace || th.Scratch:
		return "Finished", t.Success
	case th.NoChanges || th.Changed == 0:
		return "Answered", info
	}
	return "Review", t.Success
}

// boardView is the board.
func (a *App) boardView(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	busy := a.busy.Get()
	cols := map[string][]core.ThreadView{}
	running := false
	now := a.now()
	for _, th := range a.threads.Get() {
		if !th.IsWorker {
			continue
		}
		col := boardColumn(th, busy[th.ID])
		if col == colDone && now.Sub(doneAt(th)) > boardDoneFor {
			continue
		}
		cols[col] = append(cols[col], th)
		running = running || th.InTurn || len(th.Background) > 0
	}
	ticking(ctx, running) // elapsed times tick while anything works
	for k, list := range cols {
		switch k {
		case colDone:
			slices.SortStableFunc(list, func(x, y core.ThreadView) int { return doneAt(y).Compare(doneAt(x)) })
			if len(list) > boardDoneMax {
				list = list[:boardDoneMax]
			}
		case colTodo:
			slices.SortStableFunc(list, func(x, y core.ThreadView) int { return x.CreatedAt.Compare(y.CreatedAt) })
		default:
			slices.SortStableFunc(list, func(x, y core.ThreadView) int { return y.UpdatedAt.Compare(x.UpdatedAt) })
		}
		cols[k] = list
	}

	compact := isCompact(ctx)
	board := ui.Div().Row().Gap(space2).Padding(space2).Height(ui.Full).MinWidth(ui.Px(0)).Align(ui.Stretch)
	if compact {
		board = ui.Div().Column().Gap(space4).Padding(space3)
	}
	for _, c := range boardColumns {
		board = board.Child(a.boardColumnView(ctx, c.key, c.title, c.empty, cols[c.key], compact))
	}
	if compact {
		return ui.Div().Column().Size(ui.Full, ui.Full).Background(t.Background).ScrollY().Child(board)
	}
	return ui.Div().Column().Size(ui.Full, ui.Full).ScrollX().Child(board)
}

// doneAt is when a finished thread finished, as near as the view knows.
func doneAt(th core.ThreadView) time.Time {
	if !th.DoneAt.IsZero() {
		return th.DoneAt
	}
	return th.UpdatedAt
}

func (a *App) boardColumnView(ctx *ui.Context, key, title, empty string, list []core.ThreadView, compact bool) ui.Element {
	t := ctx.Theme()
	headColor := t.TextMuted
	if key == colYou && len(list) > 0 {
		headColor = t.Warning
	}
	head := ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(30)).Shrink(0).
		Child(ui.Text(title).FontSize(12).Semibold().TextColor(headColor).NoWrap()).
		Child(ui.If(len(list) > 0, ui.Text(fmt.Sprint(len(list))).FontSize(12).TextColor(t.TextSubtle)))
	cards := ui.Div().Column().Gap(space2).PaddingBottom(space2)
	if key == colTodo {
		cards = cards.Child(ui.Component(a.newTaskBox))
	}
	for _, th := range list {
		if key == colDone {
			cards = cards.Child(a.doneCard(ctx, th))
			continue
		}
		cards = cards.Child(a.boardCard(ctx, th, key))
	}
	if len(list) == 0 && empty != "" {
		cards = cards.Child(ui.Div().PaddingX(space2).PaddingY(space3).Child(caption(ctx, empty)))
	}
	col := ui.Div().Column().Key("board-col-" + key).Label(title)
	if compact {
		return col.Child(head).Child(cards)
	}
	return col.Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(boardColMin)).Height(ui.Full).
		Radius(radiusL).Background(t.Panel).PaddingX(space1).
		Child(head).
		Child(ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).ScrollY().PaddingX(space1).Child(cards))
}

// boardCard is a thread's card: its title, where it works, why it's in its column, and its brief.
func (a *App) boardCard(ctx *ui.Context, th core.ThreadView, col string) ui.Element {
	t := ctx.Theme()
	open := a.openID.Get() == th.ID
	when, whenColor := a.rowTime(ctx, th)
	unread := a.briefUnread(th, open)

	top := ui.Div().Row().Align(ui.Start).Gap(space2).
		Child(ui.Div().PaddingTop(3).Child(statusMark(ctx, shownStatus(th)).Tooltip(statusLabel(th)))).
		Child(ui.Text(th.Title).FontSize(13).Medium().MaxLines(2).Grow(1).MinWidth(ui.Px(0))).
		Child(ui.If(unread, ui.Div().PaddingTop(5).Child(dot(t.Accent, 7).Tooltip("New since you last looked"))))

	meta := []string{repoLabel(th)}
	if th.Changed > 0 {
		meta = append(meta, plural(th.Changed, "file"))
	}
	if prs := cardPRs(th); len(prs) == 1 {
		meta = append(meta, fmt.Sprintf("%s #%d", prs[0].Kind.Noun(), prs[0].Number))
	}
	metaRow := ui.Div().Row().Align(ui.Center).Gap(6).PaddingLeft(space4 + 2).MinWidth(ui.Px(0)).
		Child(ui.Text(strings.Join(meta, " · ")).FontSize(11).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0))).
		Child(ui.Text(when).FontSize(11).TextColor(whenColor).NoWrap().Shrink(0))

	body := ui.Div().Column().Gap(space1 + 2).PaddingLeft(space4 + 2).MinWidth(ui.Px(0))
	if reason, c := cardReason(ctx, th, col); reason != "" {
		body = body.Child(ui.Div().Row().Child(reasonChip(reason, c)))
	}
	if col == colWorking || col == colTodo {
		body = body.Child(ui.Text(a.nowLine(th)).FontSize(12).Medium().TextColor(workingColor(t)).MaxLines(2))
	}
	if th.Status == core.StatusWaiting {
		if line := a.activityLine(th); line != "" {
			body = body.Child(ui.Text(line).FontSize(12).TextColor(t.Text).MaxLines(3))
		}
	}
	lines := briefLines(th)
	if isCard(th) && !th.Card.CardWorking() {
		lines = reviewLines(th) // what the lead found, not the summary of the builder's turn
	}
	if len(lines) > 0 {
		list := ui.Div().Column().Gap(2)
		for _, l := range lines {
			list = list.Child(ui.Div().Row().Gap(6).Align(ui.Start).
				Child(ui.Text("•").FontSize(12).TextColor(t.TextSubtle)).
				Child(ui.Text(l).FontSize(12).TextColor(t.TextMuted).MaxLines(2).Grow(1).MinWidth(ui.Px(0))))
		}
		body = body.Child(list)
		if col != colWorking && !isCard(th) && th.Brief != nil && th.Brief.Overall != "" {
			body = body.Child(ui.Text(th.Brief.Overall).FontSize(11).TextColor(t.TextSubtle).MaxLines(3))
		}
	} else if col != colWorking && th.Status != core.StatusWaiting && strings.TrimSpace(th.Prompt) != "" {
		body = body.Child(ui.Text(firstLine(th.Prompt)).FontSize(12).TextColor(t.TextSubtle).MaxLines(2))
	}
	if col == colYou || col == colLanding {
		if actions := a.cardActions(ctx, th); actions != nil {
			body = body.Child(actions)
		}
	}

	card := ui.Div().Column().Gap(space1+2).Padding(space3).Radius(radiusL).
		Border(1, t.Border).Background(t.Background).Cursor(ui.CursorPointer).
		Hover(ui.Style{BorderColor: t.BorderStrong}).
		Role(access.RoleButton).Label(th.Title).TabIndex(0).Key("card-" + th.ID).
		ContextMenu(func() []ui.MenuItem { return a.folderMenu(th) }).
		OnClick(func() { a.toggleCard(th.ID) }).
		OnKeyDown(func(e *ui.KeyEvent) {
			if e.Key == input.KeyEnter || e.Key == input.KeySpace {
				e.StopPropagation()
				a.toggleCard(th.ID)
			}
		}).
		Child(top).Child(metaRow).Child(body)
	if open {
		card = card.Border(1, t.Accent)
	}
	return card
}

func (a *App) toggleCard(id string) {
	if th, ok := a.thread(id); ok && isCard(th) {
		a.openCard(id)
		return
	}
	if a.openID.Peek() == id {
		a.closeThread()
		return
	}
	a.openThread(id, "conversation")
}

// cardActions are a Your turn or Landing card's buttons: its landing action, or Done when it has
// nothing to land. A click on them doesn't open the card.
func (a *App) cardActions(ctx *ui.Context, th core.ThreadView) ui.Element {
	row := ui.Div().Row().Align(ui.Center).Gap(space1).PaddingTop(2).OnClick(func() {})
	n := 0
	if land := a.landAction(ctx, th, true); land != nil {
		row = row.Child(land)
		n++
	}
	if th.CanDone && !asksToLand(th) {
		row = row.Child(ui.Button("Done").Icon(icons.Check).Small().Ghost().Key("card-done-" + th.ID).
			Tooltip("Take it off the board").OnClick(func() { a.markDone(th) }))
		n++
	}
	if n == 0 {
		return nil
	}
	return row
}

// nowLine is what a working card says it is doing: its brief's line, or until there is one, the step
// it is on.
func (a *App) nowLine(th core.ThreadView) string {
	if line := cardPhaseLine(th); line != "" {
		return line
	}
	switch {
	case th.Status == core.StatusQueued || th.Status == core.StatusStarting:
		return a.activityLine(th)
	case th.Now != "":
		return th.Now
	case th.Retry != "":
		return th.Retry
	case th.Status == core.StatusBackground:
		return a.activityLine(th)
	case th.Step != nil:
		return a.stepLabel(*th.Step)
	}
	return "Thinking"
}

// briefLines are the lines of a card's brief, without their bullets: at most three.
func briefLines(th core.ThreadView) []string {
	if th.Brief == nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(th.Brief.Turn, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-•*"))
		if l != "" {
			out = append(out, l)
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// briefUnread reports whether a card's brief changed since the user last had the thread open. What was
// there when the board first showed it counts as read. UI thread only.
func (a *App) briefUnread(th core.ThreadView, open bool) bool {
	if th.Brief == nil {
		return false
	}
	if a.briefSeen == nil {
		a.briefSeen = map[string]time.Time{}
	}
	seen, ok := a.briefSeen[th.ID]
	if !ok || open {
		a.briefSeen[th.ID] = th.Brief.At
		return false
	}
	return th.Brief.At.After(seen)
}

// reasonChip is a card's reason, tinted with its color.
func reasonChip(label string, c ui.Color) ui.Element {
	return ui.Div().Row().Align(ui.Center).Gap(5).PaddingX(space2).Height(ui.Px(20)).Radius(10).Shrink(0).
		Background(c.Alpha(0.14)).
		Child(dot(c, 5)).
		Child(ui.Text(label).FontSize(11).Semibold().TextColor(c).NoWrap())
}

// doneCard is a finished thread, in one line.
func (a *App) doneCard(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	row := ui.Div().Row().Align(ui.Center).Gap(space2).PaddingX(space2).Height(ui.Px(30)).Radius(radiusM).
		Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
		Role(access.RoleButton).Label(th.Title).TabIndex(0).Key("card-" + th.ID).
		Tooltip(statusLabel(th)).
		OnClick(func() { a.toggleCard(th.ID) }).
		Child(statusMark(ctx, shownStatus(th))).
		Child(ui.Text(th.Title).FontSize(12).TextColor(t.TextMuted).Truncate().Grow(1).MinWidth(ui.Px(0))).
		Child(ui.Text(shortAgo(doneAt(th), a.now())).FontSize(11).TextColor(t.TextSubtle).NoWrap().Shrink(0))
	if a.openID.Get() == th.ID {
		row = row.Background(t.SurfaceActive)
	}
	return row
}

// newTaskBox is the box at the top of To do that adds a card: a repository, what to do, and any images
// or files. The card starts at once.
func (a *App) newTaskBox(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	draft := ui.Local(ctx, "board-draft", "")
	atts := ui.Local(ctx, "board-attachments", []attachment(nil))
	repo := ui.Local(ctx, "board-repo", "")
	focus := a.composerFocus("board")
	attach := attacher{a: a, atts: atts, focus: func() { focus.Focus() }}

	var options []ui.Option[string]
	for _, r := range append(slices.Clone(a.repos.Get()), a.plain.Get()...) {
		options = append(options, ui.Option[string]{Value: r, Label: filepath.Base(r)})
	}
	if repo.Peek() == "" && len(options) > 0 {
		repo.Set(options[0].Value)
	}

	submit := func() {
		text, list, r := strings.TrimSpace(draft.Peek()), atts.Peek(), repo.Peek()
		if text == "" {
			return
		}
		if r == "" {
			a.notify(ui.ToastInfo, "Pick a repository", "Add the folders that hold your repositories in Settings.")
			return
		}
		if attachmentsBusy(list) {
			a.notify(ui.ToastInfo, "Files are still attaching", "Add the task once they have arrived, or remove the ones that didn't attach.")
			return
		}
		spec := core.WorkerSpec{Repo: r, Prompt: text, FromUser: true}
		for _, ref := range attachmentRefs(list) {
			if core.IsFileAttachment(ref) {
				spec.Files = append(spec.Files, ref)
			} else {
				spec.Images = append(spec.Images, ref)
			}
		}
		draft.Set("")
		atts.Set(nil)
		a.run("Adding the task", func(ctx context.Context) error {
			_, err := a.b.CreateWorker(ctx, spec)
			return err
		})
	}

	area := ui.TextArea(draft).Placeholder("Add a task…").Label("New task").MinRows(2).Borderless().
		MaxHeight(ui.Px(200)).Width(ui.Full).Key("board-draft-area").FocusHandle(focus).
		OnPasteImage(func(img image.Image) {
			attach.add("Pasted image", -1, true, false, func(context.Context, func(int64)) (string, bool, error) {
				p, err := a.b.SaveImage(img)
				return p, true, err
			})
		}).
		OnKeyDown(func(e *ui.KeyEvent) {
			if e.Key == input.KeyEnter && e.Mods&input.ModShift == 0 && !a.Touch {
				e.StopPropagation()
				submit()
			}
		})
	empty := strings.TrimSpace(draft.Get()) == ""
	addBtn := ui.Button("Add").Icon(icons.Plus).Small().Primary().Disabled(empty).Key("board-add").
		Tooltip("Add the task (Enter) · Shift+Enter for a new line · it starts right away").OnClick(submit)
	attachBtn := ui.IconButton(icons.Paperclip).Ghost().Small().Key("board-attach").Label("Attach files").
		Tooltip("Attach files · you can also paste or drop images").
		OnClick(func() {
			ctx.Window().OpenFiles(dialog.FileOptions{Title: "Attach files", Multiple: true}, func(r dialog.FileResult) {
				if r.Err != nil {
					a.notify(ui.ToastError, "Couldn't open the file picker", r.Err.Error())
				} else if !r.Canceled {
					files := r.Files
					if len(files) == 0 {
						for _, p := range r.Paths {
							files = append(files, dialog.File{Name: filepath.Base(p), Path: p})
						}
					}
					attach.picked(files)
				}
			})
		})
	var picker ui.Element = caption(ctx, "No repositories yet")
	if len(options) > 0 {
		picker = ui.Select(repo, options...).Small().Label("Repository").Key("board-repo").MinWidth(ui.Px(0)).Width(ui.Full)
	}
	box := ui.Div().Column().Gap(space1).Padding(space2).Radius(radiusL).
		Border(1, t.BorderStrong).Background(t.Surface).
		FocusWithin(ui.Style{BorderColor: t.TextSubtle}).
		OnDrop(ui.Drop(func(f ui.Files, _ *ui.DropEvent) {
			for i, p := range f {
				attach.importPath(p, i == len(f)-1)
			}
		}))
	if list := atts.Get(); len(list) > 0 {
		box = box.Child(a.attachmentStrip(ctx, "board", attach, list))
	}
	return box.
		Child(area).
		Child(ui.Div().Row().Align(ui.Center).Gap(space1).
			Child(ui.Div().Grow(1).MinWidth(ui.Px(0)).Child(picker)).
			Child(attachBtn).
			Child(addBtn))
}

// viewToggle switches the window between Solo's conversation, beside the rail of threads, and the
// board.
func (a *App) viewToggle(ctx *ui.Context) ui.Element {
	board := ui.Local(ctx, "view-board", a.cfg.Peek().Board)
	if v := a.cfg.Get().Board; board.Peek() != v {
		board.Set(v)
	}
	return ui.ToggleGroup(board,
		ui.Option[bool]{Value: false, Label: "Solo", Icon: icons.MessagesSquare},
		ui.Option[bool]{Value: true, Label: "Board", Icon: icons.Kanban},
	).IconOnly().Small().Label("View").Key("view-toggle").OnChange(func(on bool) {
		a.run("Switching the view", func(context.Context) error {
			return a.b.UpdateSettings(func(s *core.Settings) { s.Board = on })
		})
	})
}
