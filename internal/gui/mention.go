package gui

import (
	"cmp"
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/actions"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/input"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// The composer's "/" and "@" lists, as in t3code's composer: typing "/" at the start of a word lists
// the skills and commands the conversation can invoke, and "@" lists what it can mention: Solo's
// repositories and folders in Solo's conversation, a thread's files and folders in a thread's. Arrow
// keys move through the list, Enter or Tab picks, Escape closes it. What is picked is typed in for the
// user ("/review ", "@internal/auth/ "); Claude Code runs a skill or command sent as "/name", and reads
// a file (or lists a folder) mentioned as "@path" before the turn.
//
// jamb's text area doesn't report where its caret is, so the composer follows it: each edit's change
// to the text gives the caret's new place. A key or click that moves the caret without an edit makes
// it unknown, and the list closes, until the next edit. Picking puts the caret after what was typed in
// with the field's own Move to end, which jamb runs on the focused field.

// trigger is the "/" or "@" word the caret is in.
type trigger struct {
	kind  byte   // '/' or '@'; 0 for none
	start int    // the byte offset of the '/' or '@'
	query string // what follows it, up to the caret
}

// findTrigger finds the trigger at caret (-1 when the caret's place isn't known): a word, from the
// last space before the caret, that starts with "/" or "@". A "/" word with another "/" in it is a
// path, not a command.
func findTrigger(text string, caret int) trigger {
	if caret <= 0 || caret > len(text) {
		return trigger{}
	}
	start := strings.LastIndexFunc(text[:caret], unicode.IsSpace) + 1
	if start >= caret {
		return trigger{}
	}
	end := len(text)
	if i := strings.IndexFunc(text[caret:], unicode.IsSpace); i >= 0 {
		end = caret + i
	}
	switch text[start] {
	case '/':
		if strings.Contains(text[start+1:end], "/") {
			return trigger{}
		}
		return trigger{'/', start, text[start+1 : caret]}
	case '@':
		return trigger{'@', start, text[start+1 : caret]}
	}
	return trigger{}
}

// cursor is where the composer last knew its caret to be, in text.
type cursor struct {
	text  string
	caret int // -1: not known
}

// editCaret finds where the caret is after an edit turned old into new, when it was at prev (-1 if
// not known): after what was typed or pasted, or where text was deleted. When prev doesn't explain the
// edit, the caret is taken to be at the end of what changed.
func editCaret(old, new string, prev int) int {
	if prev >= 0 && prev <= len(old) {
		d := len(new) - len(old)
		switch {
		case d > 0 && new[:prev] == old[:prev] && new[prev+d:] == old[prev:]:
			return prev + d // typed or pasted at the caret
		case d < 0 && prev+d >= 0 && new[:prev+d] == old[:prev+d] && new[prev+d:] == old[prev:]:
			return prev + d // Backspace
		case d < 0 && prev-d <= len(old) && new[:prev] == old[:prev] && new[prev:] == old[prev-d:]:
			return prev // Delete
		}
	}
	p := 0
	for p < len(old) && p < len(new) && old[p] == new[p] {
		p++
	}
	s := 0
	for s < len(old)-p && s < len(new)-p && old[len(old)-1-s] == new[len(new)-1-s] {
		s++
	}
	return len(new) - s
}

// menuItem is a row of the composer's list.
type menuItem struct {
	icon   *icons.Icon
	label  string
	detail string // a description, or where it is
	tag    string // where a skill comes from
	insert string // the text that takes the trigger's place
}

// mentionPath writes a path for an "@" mention, quoted when it has a space, as Claude Code reads it.
func mentionPath(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
}

// repoMention is how a repository or folder of Solo's is mentioned to Solo: its path, with the home
// folder written as ~ where Claude Code reads it so.
func repoMention(p string) string {
	if runtime.GOOS != "windows" {
		p = tildePath(p)
	}
	return mentionPath(filepath.ToSlash(p))
}

// Sizes of the composer's list.
const (
	menuWidth   = 400
	menuRowH    = 30
	menuRows    = 8 // rows shown; the list moves to keep the highlighted one in view
	menuMaxHits = 50
	// pathSearchDelay is how long typing after "@" in a thread pauses before the machine is asked
	// for the files that match: a web page asks it over the relay, and a search for each key would
	// queue round trips behind each other. The list keeps the last answer meanwhile.
	pathSearchDelay = 150 * time.Millisecond
)

// skillItems are the skills and commands matching q, best first.
func skillItems(skills []core.Skill, q string) []menuItem {
	type scored struct {
		menuItem
		score int
	}
	var hits []scored
	for _, s := range skills {
		score, ok := core.MatchScore(q, s.Name)
		if !ok && len(q) >= 2 && strings.Contains(strings.ToLower(s.Description), strings.ToLower(q)) {
			score, ok = 1, true // a word of the description
		}
		if ok {
			hits = append(hits, scored{menuItem{icon: skillIcon(s), label: "/" + s.Name, detail: s.Description, tag: s.Source,
				insert: "/" + s.Name + " "}, score})
		}
	}
	slices.SortStableFunc(hits, func(a, b scored) int { return b.score - a.score })
	out := make([]menuItem, 0, min(len(hits), menuMaxHits))
	for _, h := range hits[:min(len(hits), menuMaxHits)] {
		out = append(out, h.menuItem)
	}
	return out
}

// repoItems are Solo's repositories and plain folders matching q, best first.
func repoItems(repos, plain []string, q string) []menuItem {
	type scored struct {
		menuItem
		score int
	}
	var hits []scored
	for i, p := range append(slices.Clone(repos), plain...) {
		score, ok := core.MatchScore(q, filepath.Base(p))
		if !ok {
			if score, ok = core.MatchScore(q, filepath.ToSlash(p)); ok {
				score /= 2
			}
		}
		if !ok {
			continue
		}
		icon := icons.FolderGit2
		if i >= len(repos) {
			icon = icons.Folder
		}
		hits = append(hits, scored{menuItem{icon: icon, label: filepath.Base(p), detail: tildePath(p), insert: "@" + repoMention(p) + " "}, score})
	}
	slices.SortStableFunc(hits, func(a, b scored) int { return b.score - a.score })
	out := make([]menuItem, 0, min(len(hits), menuMaxHits))
	for _, h := range hits[:min(len(hits), menuMaxHits)] {
		out = append(out, h.menuItem)
	}
	return out
}

// pathItems are a thread's files and folders found for "@".
func pathItems(hits []core.PathHit) []menuItem {
	out := make([]menuItem, 0, len(hits))
	for _, h := range hits {
		p, icon := h.Path, icons.File
		if h.Dir {
			p, icon = p+"/", icons.Folder
		}
		dir, base := "", p
		if i := strings.LastIndex(strings.TrimSuffix(p, "/"), "/"); i >= 0 {
			dir, base = p[:i], p[i+1:]
		}
		out = append(out, menuItem{icon: icon, label: base, detail: dir, insert: "@" + mentionPath(p) + " "})
	}
	return out
}

// sentTexts are the messages the user wrote in a conversation, oldest first, for Up and Down in an
// empty composer.
func sentTexts(items []core.Item) []string {
	var out []string
	for i := range items {
		it := &items[i]
		if it.Kind != core.ItemUser {
			continue
		}
		var u core.UserData
		if it.Decode(&u) != nil || u.Synthetic || u.From != core.FromUser || strings.TrimSpace(u.Text) == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1] == u.Text {
			continue
		}
		out = append(out, u.Text)
	}
	return out
}

// wrapLines splits text into the lines a box w wide shows it in, breaking between words, as byte
// ranges. It estimates jamb's layout with measure, to put the composer's list by the caret.
func wrapLines(text string, w float32, measure func(string) float32) [][2]int {
	var out [][2]int
	pos := 0
	for _, hard := range strings.SplitAfter(text, "\n") {
		line := strings.TrimSuffix(hard, "\n")
		cur := 0 // the start of the line being filled, in line
		for i := 0; i < len(line); {
			j := i
			for j < len(line) && line[j] != ' ' {
				j++
			}
			for j < len(line) && line[j] == ' ' {
				j++
			}
			if cur < i && measure(strings.TrimRight(line[cur:j], " ")) > w {
				out = append(out, [2]int{pos + cur, pos + i})
				cur = i
			}
			i = j
		}
		out = append(out, [2]int{pos + cur, pos + len(line)})
		pos += len(hard)
	}
	return out
}

// menuPlace is where the composer's list goes in the composer's box, which is boxW wide: left of the
// box, and bottom up from the box's bottom edge, so the list sits just above the caret's line and
// starts at the "/" or "@". The text area is areaW wide and ends below above the box's bottom edge.
func menuPlace(ctx *ui.Context, text string, start, caret int, areaW, boxW, below float32) (left, bottom float32) {
	const size, maxH = 13, 220 // the composer's text size and its text area's MaxHeight
	lh := lineHeight(ctx, size)
	measure := func(s string) float32 { return textWidth(ctx, s, size, ui.FontRegular) }
	lines := wrapLines(text, max(areaW, 40), measure)
	at := 0
	for i, l := range lines {
		if l[0] <= caret {
			at = i
		}
	}
	x := measure(text[lines[at][0]:max(lines[at][0], min(start, lines[at][1]))])
	fromBottom := float32(len(lines)-1-at) * lh
	if float32(len(lines))*lh > maxH {
		fromBottom = min(fromBottom, maxH-lh) // the area scrolls to keep the caret in view
	}
	left = min(max(space3+x-space2, space1), max(boxW-menuWidth-space1, space1))
	return left, below + fromBottom + lh + space1
}

// mentionKit is a composer's "/" and "@" lists and prompt history.
type mentionKit struct {
	popup    ui.Element              // the list, laid over the composer's box; nil when closed
	onChange func(string)            // the text area's OnChange
	onKey    func(*ui.KeyEvent) bool // runs first in the text area's OnKeyDown; true when it took the key
	onPress  func(*ui.MouseEvent)    // a press in the text area moves the caret
	close    func()                  // closes the list, as a click outside the composer does
	insert   func(string)            // types text at the caret, or at the end when its place isn't known
	sent     func()                  // the draft was sent
}

// pathWait is the path search that waits for typing to pause (see pathQuery).
type pathWait struct {
	open   bool   // the list was open at the last render
	query  string // the query that waits
	cancel func()
}

// pathQuery returns the query that the "@" list of a composer searches the machine's files for: what is
// typed when the list opens, and then what is typed once typing pauses for pathSearchDelay. Each key
// would otherwise ask the machine again.
func (a *App) pathQuery(ctx *ui.Context, key, typed string, searching bool) string {
	q := ui.Local(ctx, key+"-path-query", typed)
	w := ui.Local(ctx, key+"-path-wait", &pathWait{}).Peek()
	if w.cancel != nil && (!searching || typed != w.query) {
		w.cancel()
		*w = pathWait{open: w.open}
	}
	if !searching {
		w.open = false
		return typed
	}
	if !w.open {
		w.open = true
		q.Set(typed) // the list opens with what is typed then, at once
	} else if typed != q.Peek() && w.cancel == nil {
		w.query, w.cancel = typed, ctx.After(pathSearchDelay, func() { w.cancel = nil; q.Set(typed) })
	}
	return q.Get()
}

// mentions builds a composer's lists. boxW is about the composer's width, and areaW its text's.
func (a *App) mentions(ctx *ui.Context, v core.ThreadView, key string, draft *ui.Signal[string], focus *ui.FocusHandle,
	areaW, boxW, below float32) mentionKit {
	t := ctx.Theme()
	text := draft.Get()
	cur := ui.Local(ctx, key+"-cursor", cursor{caret: -1})
	sel := ui.Local(ctx, key+"-sel", 0)
	top := ui.Local(ctx, key+"-top", 0)
	dismissed := ui.Local(ctx, key+"-dismissed", trigger{})
	hist := ui.Local(ctx, key+"-hist", -1) // which sent message the draft is, while Up and Down go through them

	c := cur.Get()
	if c.text != text {
		c = cursor{text, -1} // changed from outside, such as by sending
		if text == "" {
			c.caret = 0
		}
	}
	tr := findTrigger(text, c.caret)
	// Escape closes the list until the word changes for another.
	d := dismissed.Get()
	open := tr.kind != 0 && (d.kind != tr.kind || d.start != tr.start)
	worker := v.IsWorker

	// The list's rows. Each list loads only while it is open.
	skills := a.skillsQuery(ctx, v.ID, open && tr.kind == '/')
	searching := open && tr.kind == '@' && worker
	query := a.pathQuery(ctx, key, tr.query, searching)
	paths := ui.Query(ctx, "paths:"+v.ID+"\x00"+query, func(cx context.Context) ([]core.PathHit, error) {
		return a.b.SearchPaths(cx, v.ID, query, menuMaxHits)
	}, ui.QueryEnabled(searching), ui.QueryKeepPrevious(), ui.QueryStaleTime(5*time.Second))
	var items []menuItem
	title, loading, failed := "", false, ""
	if open {
		switch {
		case tr.kind == '/':
			title = "Skills and commands"
			items, loading = skillItems(skills.Data, tr.query), skills.Loading
			if skills.Err != nil {
				failed = skills.Err.Error()
			}
		case worker:
			title = "Files and folders"
			items, loading = pathItems(paths.Data), paths.Loading && len(paths.Data) == 0
			if paths.Err != nil {
				failed = paths.Err.Error()
			}
		default:
			title = "Repositories and folders"
			items = repoItems(a.repos.Get(), a.plain.Get(), tr.query)
		}
	}
	idx := min(sel.Get(), max(len(items)-1, 0))

	setCursor := func(s string, caret int) { cur.Set(cursor{s, caret}) }
	// put replaces the text with before+after and leaves the caret between them: the field is given
	// before alone and moves its caret to its end, and then the rest, which leaves the caret there.
	put := func(before, after string) {
		focus.FocusNow()
		draft.Set(before)
		ctx.Dispatch(actions.MoveToEnd)
		draft.Set(before + after)
		setCursor(before+after, len(before))
	}
	choose := func(it menuItem) {
		s, cc := draft.Peek(), cur.Peek()
		tr := findTrigger(s, cc.caret)
		if cc.text != s || tr.kind == 0 {
			return
		}
		// What is typed in ends with a space, which takes the place of one already after the word.
		put(s[:tr.start]+it.insert, strings.TrimPrefix(s[cc.caret:], " "))
		sel.Set(0)
		top.Set(0)
	}
	moveSel := func(d int) {
		if len(items) == 0 {
			return
		}
		i := (idx + d + len(items)) % len(items)
		sel.Set(i)
		if f := top.Peek(); i < f {
			top.Set(i)
		} else if i >= f+menuRows {
			top.Set(i - menuRows + 1)
		}
	}
	lost := func() { // the caret moved without an edit
		if cc := cur.Peek(); cc.caret >= 0 {
			setCursor(cc.text, -1)
		}
	}

	k := mentionKit{
		onChange: func(s string) {
			// The field has set the draft by now; cc is what the composer last knew. editCaret only
			// trusts its caret when it explains the edit.
			cc := cur.Peek()
			prev := cc.caret
			caret := editCaret(cc.text, s, prev)
			was, now := findTrigger(cc.text, prev), findTrigger(s, caret)
			setCursor(s, caret)
			if was.kind != now.kind || was.start != now.start || was.query != now.query {
				sel.Set(0)
				top.Set(0)
			}
			if d := dismissed.Peek(); d.kind != 0 && (d.kind != now.kind || d.start != now.start) {
				dismissed.Set(trigger{})
			}
			hist.Set(-1)
		},
		onPress: func(*ui.MouseEvent) { lost() },
		close: func() {
			if open {
				dismissed.Set(tr)
			}
		},
		insert: func(s string) {
			cc, text := cur.Peek(), draft.Peek()
			at := cc.caret
			if cc.text != text || at < 0 || at > len(text) {
				at = len(text)
			}
			before := text[:at]
			if before != "" && !strings.HasSuffix(before, " ") && !strings.HasSuffix(before, "\n") {
				before += " "
			}
			put(before+s, text[at:])
		},
		sent: func() {
			setCursor("", 0)
			hist.Set(-1)
		},
	}
	k.onKey = func(e *ui.KeyEvent) bool {
		plain := e.Mods&(input.ModShift|input.ModCtrl|input.ModAlt|input.ModSuper) == 0
		if open && plain {
			switch e.Key {
			case input.KeyUp:
				e.StopPropagation()
				moveSel(-1)
				return true
			case input.KeyDown:
				e.StopPropagation()
				moveSel(1)
				return true
			case input.KeyEnter, input.KeyTab:
				if len(items) > 0 {
					e.StopPropagation()
					choose(items[idx])
					return true
				}
			case input.KeyEscape:
				e.StopPropagation()
				dismissed.Set(tr)
				return true
			}
		}
		// Up and Down in an empty composer, or one showing a sent message as it was recalled, go
		// through the messages sent here, as t3code's composer does.
		if (e.Key == input.KeyUp || e.Key == input.KeyDown) && plain {
			s, cc, h := draft.Peek(), cur.Peek(), hist.Peek()
			sent := sentTexts(a.itemsOf(v.ID).Peek())
			recalled := h >= 0 && h < len(sent) && s == sent[h] && cc.text == s && cc.caret == len(s)
			if len(sent) > 0 && (s == "" && h < 0 || recalled) {
				switch {
				case e.Key == input.KeyUp && h < 0:
					h = len(sent) - 1
				case e.Key == input.KeyUp:
					h = max(h-1, 0)
				case h < 0:
					return false // Down in an empty composer: nothing newer
				default:
					h++
				}
				e.StopPropagation()
				if h >= len(sent) {
					put("", "")
					hist.Set(-1)
					return true
				}
				put(sent[h], "")
				hist.Set(h)
				return true
			}
		}
		switch e.Key {
		case input.KeyLeft, input.KeyRight, input.KeyUp, input.KeyDown, input.KeyHome, input.KeyEnd, input.KeyPageUp, input.KeyPageDown:
			lost()
		case input.KeyA:
			if e.Mods&(input.ModCtrl|input.ModSuper) != 0 {
				lost() // Select all
			}
		}
		return false
	}

	if !open {
		return k
	}
	// The list.
	list := ui.Div().Column().Gap(1)
	switch {
	case loading:
		list = list.Child(menuNote(ctx, "Searching…"))
	case failed != "":
		list = list.Child(menuNote(ctx, "Couldn't list them: "+failed))
	case len(items) == 0 && tr.kind == '/':
		list = list.Child(menuNote(ctx, "No skill or command matches. Enter sends the text as it is."))
	case len(items) == 0:
		list = list.Child(menuNote(ctx, "Nothing matches."))
	}
	first := min(top.Get(), max(len(items)-menuRows, 0))
	if idx < first {
		first = idx
	} else if idx >= first+menuRows {
		first = idx - menuRows + 1
	}
	for i := first; i < min(len(items), first+menuRows); i++ {
		it, n := items[i], i
		on := i == idx
		row := ui.Div().Row().Align(ui.Center).Gap(space2).Height(ui.Px(menuRowH)).PaddingX(space2).Radius(radiusS).
			Key("menu-" + it.insert).Cursor(ui.CursorPointer).KeepFocus().
			Role(access.RoleListItem).Label(it.label).
			OnMouseEnter(func() { sel.Set(n) }).
			OnClick(func() { choose(it) }).
			Child(ui.Icon(it.icon).Size(14).Color(t.TextMuted)).
			Child(ui.Text(it.label).FontSize(13).Medium().NoWrap().Shrink(0).MaxWidth(ui.Px(menuWidth * 0.6)).Truncate())
		if on {
			row = row.Background(t.SurfaceHover)
		}
		if it.detail != "" {
			row = row.Child(ui.Text(it.detail).FontSize(12).TextColor(t.TextSubtle).NoWrap().Truncate().Grow(1).MinWidth(ui.Px(0)))
		} else {
			row = row.Child(ui.Div().Grow(1))
		}
		if it.tag != "" {
			row = row.Child(ui.Text(it.tag).FontSize(11).TextColor(t.TextSubtle).NoWrap().Shrink(0))
		}
		list = list.Child(row)
	}
	foot := ""
	if more := len(items) - menuRows; more > 0 {
		foot = "↑↓ to move · Enter or Tab to pick · Esc to close"
	} else if len(items) > 0 {
		foot = "Enter or Tab to pick · Esc to close"
	}
	head := ui.Div().Row().Align(ui.Center).PaddingX(space2).PaddingTop(space1).PaddingBottom(2).
		Child(ui.Text(strings.ToUpper(title)).FontSize(10).FontWeight(ui.FontSemibold).TextColor(t.TextSubtle).Grow(1))
	box := ui.Div().Column().Padding(space1).Gap(2).Width(ui.Px(min(menuWidth, max(boxW-2*space1, 200)))).
		Radius(radiusL).Border(1, t.BorderStrong).Background(t.Surface).Shadow(ui.ShadowMd).
		Role(access.RoleList).Label(title).
		Child(head).Child(list)
	if foot != "" {
		box = box.Child(ui.Div().PaddingX(space2).PaddingY(2).Child(ui.Text(foot).FontSize(11).TextColor(t.TextSubtle)))
	}
	left, bottom := menuPlace(ctx, text, tr.start, c.caret, areaW, boxW, below)
	k.popup = ui.Div().Absolute().Left(left).Bottom(bottom).ZIndex(5).KeepFocus().Key(key + "-menu").Child(box)
	return k
}

// menuNote is a line in the composer's list that isn't a choice.
func menuNote(ctx *ui.Context, s string) ui.Element {
	return ui.Div().Row().Align(ui.Center).Height(ui.Px(menuRowH)).PaddingX(space2).
		Child(ui.Text(s).FontSize(12).TextColor(ctx.Theme().TextMuted).Truncate())
}

// dropMention is how a dropped file or folder is mentioned in a conversation: by its path in the
// thread's folder when it is in it, or else by its full path.
func dropMention(v core.ThreadView, path string) string {
	dir := ""
	switch {
	case !v.IsWorker:
	case len(v.Parts) == 1:
		dir = cmp.Or(v.Parts[0].Worktree, v.Parts[0].Repo)
	case len(v.Parts) == 0:
		dir = cmp.Or(v.WorktreePath, v.RepoPath)
	}
	if dir != "" {
		if rel, err := filepath.Rel(dir, path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return "@" + mentionPath(filepath.ToSlash(rel))
		}
	}
	return "@" + repoMention(path)
}

// isImageFile reports whether a dropped file is one the composer attaches as an image.
func isImageFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}
