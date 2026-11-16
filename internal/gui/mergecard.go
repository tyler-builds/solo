package gui

import (
	"fmt"
	"strings"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// proposalHeader is the merge card's header: "Merge proposed" and its checks at the top left, the
// thread's repositories at the top right, and the thread's title on a line of its own under them, so
// long repository names never crowd out the title.
func (a *App) proposalHeader(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	const label = "Merge proposed"
	left := ui.Div().Row().Align(ui.Center).Gap(space2).Shrink(0).Key("card-kind-" + th.ID).
		Child(ui.Icon(icons.GitMerge).Size(15).Color(t.Success)).
		Child(ui.Text(label).FontSize(12).Semibold().TextColor(t.Success).NoWrap())
	used := 15 + space2 + textWidth(ctx, label, 12, ui.FontSemibold)
	if cs, ok := mergeChecks(th); ok {
		left = left.Child(checksBadge(ctx, th))
		// checksBadge: its padding, the dot, the icon and the words, 4 apart.
		used += space2 + space1 + textWidth(ctx, "·", 12, ui.FontRegular) + 4 + 13 + 4 + textWidth(ctx, cs.text, 12, ui.FontRegular)
	}
	top := ui.Div().Row().Align(ui.Center).Gap(space3).MinWidth(ui.Px(0)).Child(left)
	if th.ID == "" {
		return top
	}
	// The card's inner width: the chat column less its padding, and the card's padding and border.
	room := a.chatWidth(ctx) - 2*space6 - 2*cardPad - 2 - used - space3
	top = top.Child(ui.Div().Grow(1)).Child(fitRepos(ctx, th.ID, repoNames(th), room))
	return ui.Div().Column().Gap(6).MinWidth(ui.Px(0)).
		Child(top).
		Child(a.cardTitle(ctx, th))
}

// cardTitle is the thread's title, which opens the thread.
func (a *App) cardTitle(ctx *ui.Context, th core.ThreadView) ui.Element {
	t := ctx.Theme()
	id := th.ID
	return ui.Div().MinWidth(ui.Px(0)).Cursor(ui.CursorPointer).Key("card-title-" + id).
		Role(access.RoleButton).Label("Open " + th.Title).TabIndex(0).Tooltip(th.Title).
		Hover(ui.Style{TextColor: t.Link}).
		OnClick(func() { a.openThread(id, "conversation") }).
		Child(ui.Text(th.Title).FontSize(13).Medium().Truncate())
}

// repoNames is the names of the thread's repositories.
func repoNames(th core.ThreadView) []string {
	if th.MultiRepo() {
		names := make([]string, len(th.Parts))
		for i, p := range th.Parts {
			names[i] = p.Name
		}
		return names
	}
	return []string{repoLabel(th)}
}

// fitRepos lists names "repo · repo · repo", whole names while they fit in room, and the rest
// counted as "+N", which names them on hover. A first name too long for the room on its own is cut
// at its end and shows in full on hover.
func fitRepos(ctx *ui.Context, id string, names []string, room float32) ui.Element {
	t := ctx.Theme()
	sep := 4 + textWidth(ctx, "·", 12, ui.FontRegular) + 4
	more := func(n int) float32 { return sep + textWidth(ctx, fmt.Sprintf("+%d", n), 12, ui.FontMedium) }
	shown, used := 0, float32(0)
	for i, n := range names {
		w := textWidth(ctx, n, 12, ui.FontRegular)
		if i > 0 {
			w += sep
		}
		reserve := float32(0)
		if i < len(names)-1 {
			reserve = more(len(names) - 1 - i)
		}
		if used+w+reserve > room {
			break
		}
		used += w
		shown++
	}
	cut := shown == 0 // even the first name doesn't fit whole
	if cut {
		shown = 1
	}
	row := ui.Div().Row().Align(ui.Center).Gap(4).Shrink(1).MinWidth(ui.Px(0)).Key("card-repos-" + id)
	dot := func() ui.Element { return ui.Text("·").FontSize(12).TextColor(t.TextSubtle).NoWrap() }
	for i, n := range names[:shown] {
		if i > 0 {
			row = row.Child(dot())
		}
		name := ui.Div().MinWidth(ui.Px(0)).Key(fmt.Sprintf("card-repo-%s-%d", id, i)).
			Child(ui.Text(n).FontSize(12).TextColor(t.TextMuted).Truncate())
		if i == shown-1 {
			// The last name gives way if the measure ran short, so it alone can be cut; when it may
			// be, it names itself in full on hover.
			name = name.Shrink(1)
			if cut || used > room-8 {
				name = name.Role(access.RoleStaticText).Label(n).Tooltip(n)
			}
		} else {
			name = name.Shrink(0)
		}
		row = row.Child(name)
	}
	if rest := names[shown:]; len(rest) > 0 {
		row = row.Child(dot()).
			Child(ui.Div().Shrink(0).Key("card-repos-more-" + id).Tooltip(strings.Join(rest, "\n")).
				Role(access.RoleStaticText).Label(fmt.Sprintf("%d more: %s", len(rest), strings.Join(rest, ", "))).
				Child(ui.Text(fmt.Sprintf("+%d", len(rest))).FontSize(12).Medium().TextColor(t.TextMuted).NoWrap()))
	}
	return row
}
