package gui

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// skillsQuery lists the skills and commands of a conversation ("" for Settings' list of skills),
// shared by the composer's "/" list and Settings.
func (a *App) skillsQuery(ctx *ui.Context, threadID string, enabled bool) ui.QueryResult[[]core.Skill] {
	return ui.Query(ctx, "skills:"+threadID, func(c context.Context) ([]core.Skill, error) {
		return a.b.Skills(c, threadID)
	}, ui.QueryStaleTime(30*time.Second), ui.QueryEnabled(enabled))
}

// skillIcon is a skill's icon in a list: a plugin's, a command's, or any other skill's.
func skillIcon(s core.Skill) *icons.Icon {
	switch {
	case s.Command:
		return icons.SquareSlash
	case strings.HasPrefix(s.Source, "Plugin"):
		return icons.Puzzle
	}
	return icons.Sparkles
}

// prSkillsPicker chooses the skills whose instructions the PR text model follows, with the typed
// rules. A chosen skill that is no longer there says so; the PR text skips it.
func (a *App) prSkillsPicker(ctx *ui.Context, chosen *ui.Signal[[]string]) ui.Element {
	t := ctx.Theme()
	q := a.skillsQuery(ctx, "", true)
	cur := chosen.Get()
	byName := map[string]core.Skill{}
	for _, s := range q.Data {
		byName[s.Name] = s
	}
	col := ui.Div().Column().Gap(space1)
	for _, name := range cur {
		n := name
		s, ok := byName[n]
		line := ui.Div().Row().Align(ui.Center).Gap(space2).Grow(1).MinWidth(ui.Px(0))
		switch {
		case ok:
			line = line.
				Child(ui.Icon(skillIcon(s)).Size(14).Color(t.TextMuted)).
				Child(ui.Text(n).FontSize(13).Medium().NoWrap().Shrink(0)).
				Child(ui.Text(cmp.Or(s.Description, s.Source)).FontSize(12).TextColor(t.TextSubtle).Truncate().Grow(1).MinWidth(ui.Px(0)))
		case q.Loading:
			line = line.Child(ui.Icon(icons.Sparkles).Size(14).Color(t.TextMuted)).Child(ui.Text(n).FontSize(13).Medium())
		default:
			line = line.
				Child(ui.Icon(icons.TriangleAlert).Size(14).Color(t.Warning)).
				Child(ui.Text(n).FontSize(13).Medium().NoWrap().Shrink(0)).
				Child(ui.Text("Not found: pull requests skip it.").FontSize(12).TextColor(t.Warning).Truncate().Grow(1).MinWidth(ui.Px(0)))
		}
		col = col.Child(ui.Div().Row().Align(ui.Center).Gap(space2).Key("pr-skill-" + n).
			PaddingLeft(space2).PaddingY(2).Radius(radiusS).Background(t.Surface).
			Child(line).
			Child(ui.IconButton(icons.X).Ghost().Small().Label("Stop following " + n).Tooltip("Remove").OnClick(func() {
				chosen.Set(slices.DeleteFunc(slices.Clone(chosen.Peek()), func(s string) bool { return s == n }))
			})))
	}
	// The menu groups the skills by where they come from: the user's, then their organization's,
	// plugins', and the repositories'.
	var add []ui.MenuItem
	section := ""
	skills := slices.Clone(q.Data)
	slices.SortStableFunc(skills, func(x, y core.Skill) int { return cmp.Compare(sourceRank(x.Source), sourceRank(y.Source)) })
	for _, s := range skills {
		if slices.Contains(cur, s.Name) {
			continue
		}
		if g := sourceGroup(s.Source); g != section {
			section = g
			add = append(add, ui.MenuSection(g))
		}
		n := s.Name
		add = append(add, ui.MenuItem{Label: n, Icon: skillIcon(s), OnClick: func() {
			chosen.Set(append(slices.Clone(chosen.Peek()), n))
		}})
	}
	var button ui.Element
	switch {
	case q.Loading:
		button = ui.Button("Finding skills…").Icon(icons.Plus).Ghost().Small().Disabled(true)
	case q.Err != nil:
		button = caption(ctx, "Couldn't list the skills: "+q.Err.Error())
	case len(add) == 0 && len(q.Data) == 0:
		button = caption(ctx, "No skills found in ~/.claude/skills, your repositories' .claude/skills, or plugins.")
	case len(add) == 0:
		button = nil
	default:
		button = ui.DropdownMenu(ui.Button("Add a skill").Icon(icons.Plus).Ghost().Small(), func() []ui.MenuItem { return add })
	}
	if button != nil {
		col = col.Child(ui.Div().Row().Child(button))
	}
	return col
}

// sourceRank orders skills' sources in Settings' menu.
func sourceRank(source string) int {
	switch {
	case source == "Personal":
		return 0
	case source == "Organization":
		return 1
	case strings.HasPrefix(source, "Plugin"):
		return 2
	}
	return 3
}

// sourceGroup is the menu section of a skill's source.
func sourceGroup(source string) string {
	switch sourceRank(source) {
	case 0:
		return "Yours"
	case 1:
		return "Your organization's"
	case 2:
		return "Plugins"
	}
	return "Repositories"
}
