package gui

// Solo's design system, after uas (~/code/uas/internal/gui/design.go): neutral near-black or white
// surfaces with hairline borders, and the indigo of the brand mark kept for links. Primary actions are
// high-contrast neutral (white on dark, near-black on light), so no action reads as a warning.
// Pages use these tokens and components rather than raw sizes and colors.

import (
	"embed"
	_ "embed"
	"fmt"
	"github.com/tyler-builds/solo/internal/forge"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/markdown"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// Spacing scale, in logical pixels.
const (
	space1 = 4
	space2 = 8
	space3 = 12
	space4 = 16
	space5 = 20
	space6 = 24
	space8 = 32
)

// The first-run setup's scale (onboarding.go). The setup has the window to itself, so it reads larger and
// looser than the dense views of the app, and its content grows with the window up to setupMaxWidth.
const (
	space10 = 40
	space12 = 48
	space16 = 64

	setupMaxWidth   = 1080 // the content's widest
	setupSplit      = 880  // content at least this wide lays out in two columns
	setupProseWidth = 660  // a paragraph's widest, so its lines stay easy to read
)

// setupTitleTop is how far under the setup's progress bar each step's title starts, the same on every
// step for a window height: more on a tall window, so the page doesn't sit at its top, and little on a
// short one.
func setupTitleTop(winH float32) float32 {
	return min(max((winH-760)/4+space2, space4), space16)
}

// The setup's type scale: a display title and lead for each step, and its body text.
func displayTitle(ctx *ui.Context, s string) *ui.TextElement {
	size := float32(32)
	if isCompact(ctx) {
		size = 24
	}
	return ui.Text(s).FontSize(size).Semibold().TextColor(ctx.Theme().Text).Key("setup-title")
}

func leadText(ctx *ui.Context, s string) *ui.TextElement {
	size := float32(17)
	if isCompact(ctx) {
		size = 15
	}
	return ui.Text(s).FontSize(size).TextColor(ctx.Theme().TextMuted)
}

func bodyText(ctx *ui.Context, s string) *ui.TextElement {
	return ui.Text(s).FontSize(14).TextColor(ctx.Theme().TextMuted)
}

// sectionLabel names a group of rows: small capitals, as Settings' sections are named.
func sectionLabel(ctx *ui.Context, s string) *ui.TextElement {
	return ui.Text(strings.ToUpper(s)).FontSize(12).FontWeight(ui.FontSemibold).TextColor(ctx.Theme().TextSubtle)
}

// Corner radii.
const (
	radiusS  = 6  // buttons, inputs, chips
	radiusM  = 8  // rows, tool lines
	radiusL  = 10 // cards
	radiusXL = 12 // the content panel
	radius2X = 16 // the setup's large panels
)

type palette struct {
	dark bool

	background, panel, surface, surfaceHover, surfaceActive string
	border, borderStrong                                    string
	text, textMuted, textSubtle                             string
	accent, accentHover, accentActive, accentText, link     string
	brand                                                   string
	success, warning, danger, info                          string
	scrollbar, scrim                                        string
}

var darkPalette = palette{
	dark:       true,
	background: "#0e0e10", panel: "#08080a", surface: "#19191d", surfaceHover: "#202025", surfaceActive: "#2a2a30",
	border: "#222227", borderStrong: "#313137",
	text: "#f2f2f4", textMuted: "#acacb6", textSubtle: "#85858f",
	accent: "#ececf0", accentHover: "#ffffff", accentActive: "#d0d0d6", accentText: "#0e0e10", link: "#9ca5ff",
	brand:   "#7b86ff",
	success: "#3ecf8e", warning: "#f5b544", danger: "#e03e3e", info: "#7aa7ff",
	scrollbar: "#2e2e34", scrim: "#000000b3",
}

var lightPalette = palette{
	background: "#ffffff", panel: "#efeff2", surface: "#ffffff", surfaceHover: "#e8e8ec", surfaceActive: "#dfdfe5",
	border: "#e3e3e8", borderStrong: "#d4d4db",
	text: "#0c0c0e", textMuted: "#4f4f58", textSubtle: "#6c6c76",
	accent: "#18181b", accentHover: "#2c2c31", accentActive: "#3f3f46", accentText: "#ffffff", link: "#4450d6",
	brand:   "#5560e8",
	success: "#15803d", warning: "#a94e07", danger: "#b91c1c", info: "#2459c7",
	scrollbar: "#d0d0d6", scrim: "#0c0c0e59",
}

// codeColor is inline code's color.
func codeColor(t *ui.Theme) ui.Color {
	if t.Dark {
		return ui.Hex("#6fd3c1")
	}
	return ui.Hex("#0b7a6c")
}

// workingColor marks work in progress: a calm blue, so a running thread never reads as an error.
func workingColor(t *ui.Theme) ui.Color { return ui.Hex(pal(t).info) }

func pal(t *ui.Theme) palette {
	if t.Dark {
		return darkPalette
	}
	return lightPalette
}

// Themes returns Solo's light and dark themes, built over jamb's so every widget picks them up.
func Themes() (light, dark *ui.Theme) {
	light, dark = buildTheme(ui.LightTheme(), lightPalette), buildTheme(ui.DarkTheme(), darkPalette)
	for _, t := range []*ui.Theme{light, dark} {
		// Inline code is colored, without a box: a box per word reads as a row of chips.
		// Teal, so code never reads as an error or as a link. No background at all, rather than the
		// page's color, so it has no box in a user's bubble either, and a selection shows over it.
		ui.SetThemeStyle(t, markdown.Style{InlineCode: ui.Style{Clear: ui.ClearBackground, TextColor: codeColor(t)}})
	}
	return light, dark
}

func buildTheme(t *ui.Theme, p palette) *ui.Theme {
	h := ui.Hex
	t.Name = "solo"
	t.Background, t.Panel, t.Surface = h(p.background), h(p.panel), h(p.surface)
	t.SurfaceHover, t.SurfaceActive = h(p.surfaceHover), h(p.surfaceActive)
	t.Border, t.BorderStrong = h(p.border), h(p.borderStrong)
	t.Text, t.TextMuted, t.TextSubtle, t.Disabled = h(p.text), h(p.textMuted), h(p.textSubtle), h(p.textSubtle)
	t.Accent, t.AccentHover, t.AccentActive = h(p.accent), h(p.accentHover), h(p.accentActive)
	t.AccentText, t.Link, t.FocusRing = h(p.accentText), h(p.link), h(p.link)
	t.AccentSoft = h(p.text).Alpha(0.08)
	t.Success, t.Warning, t.Danger = h(p.success), h(p.warning), h(p.danger)
	t.Scrollbar, t.Scrim = h(p.scrollbar), h(p.scrim)
	t.Radius, t.FontSize = radiusS, 13

	t.Sidebar = ui.SidebarStyle{
		Pane: ui.Style{Background: h(p.panel), BorderColor: h(p.panel), Padding: ui.All(space2)},
		Item: ui.Style{Radius: radiusS, FontSize: 13, MinHeight: ui.Px(30), TextColor: h(p.textMuted),
			Padding: ui.XY(space2+2, 0), Gap: space2 + 2},
		ItemHover:    ui.Style{Background: h(p.surfaceHover), TextColor: h(p.text)},
		ItemSelected: ui.Style{Background: h(p.surfaceActive), TextColor: h(p.text), FontWeight: ui.FontMedium},
		Heading: ui.Style{TextColor: h(p.textSubtle), FontSize: 11, FontWeight: ui.FontMedium,
			Padding: ui.Edges{Top: space4, Right: 10, Bottom: space1, Left: 10}},
	}
	t.Button = ui.ButtonStyle{Root: ui.Style{FontWeight: ui.FontMedium}}
	t.Buttons.Primary = ui.ButtonStyle{Root: ui.Style{FontWeight: ui.FontMedium}}
	t.Badge = ui.BadgeStyle{Badge: ui.Style{Background: h(p.surfaceActive)},
		Label: ui.Style{TextColor: h(p.textMuted), FontWeight: ui.FontMedium}}
	t.Buttons.Secondary = ui.ButtonStyle{
		Root:      ui.Style{Background: h(p.surface), BorderColor: h(p.borderStrong)},
		RootHover: ui.Style{Background: h(p.surfaceHover), BorderColor: h(p.borderStrong)},
	}
	t.Field = ui.FieldStyle{
		Frame:            ui.Style{Background: h(p.surface), BorderColor: h(p.border)},
		FrameHover:       ui.Style{BorderColor: h(p.borderStrong)},
		FrameFocusWithin: ui.Style{BorderColor: h(p.textSubtle)},
		Selection:        ui.Style{Background: h(p.link).Alpha(0.35)},
	}
	t.Card = ui.CardStyle{
		Card: ui.Style{Radius: radiusL, Padding: ui.All(space4), Gap: space3, Background: h(p.surface),
			BorderWidth: 1, BorderColor: h(p.border)},
		CardHover: ui.Style{Background: h(p.surfaceHover), BorderColor: h(p.borderStrong), Cursor: ui.CursorPointer},
	}
	return t
}

// Type scale.
func title(s string) *ui.TextElement   { return ui.Text(s).FontSize(22).Semibold() }
func heading(s string) *ui.TextElement { return ui.Text(s).FontSize(15).Semibold() }
func subhead(s string) *ui.TextElement { return ui.Text(s).FontSize(13).Medium() }

func muted(ctx *ui.Context, s string) *ui.TextElement {
	return ui.Text(s).FontSize(13).TextColor(ctx.Theme().TextMuted)
}

func caption(ctx *ui.Context, s string) *ui.TextElement {
	return ui.Text(s).FontSize(12).TextColor(ctx.Theme().TextMuted)
}

func subtle(ctx *ui.Context, s string) *ui.TextElement {
	return ui.Text(s).FontSize(12).TextColor(ctx.Theme().TextSubtle)
}

// statusColor is the color of a thread status's mark.
// statusNoChanges is shown for a worker that finished with nothing to merge (ThreadView.NoChanges).
const statusNoChanges = "no-changes"

// statusFinished is shown for a thread that worked directly in a folder outside git and finished: its
// edits are already in place, so there is nothing to review or merge (ThreadView.InPlace). A
// scratch thread that finished shows it too: the user doesn't mean to land it (store.Thread.Scratch).
const statusFinished = "finished"

// Statuses the UI shows for a worker with an open pull request.
const (
	statusChecksRunning = "checks-running"
	statusInReview      = "in-review"
	statusChecksFailed  = "checks-failed"
)

// shownStatus is the status the UI shows for a thread: its own, or one of the UI's (no changes, a
// pull request waiting for its checks, in review, or with failed checks). A failure in any of its
// pull requests wins over checks still running in another.
func shownStatus(v core.ThreadView) string {
	if v.NoChanges {
		return statusNoChanges
	}
	if (v.InPlace || v.Scratch) && v.Status == core.StatusIdle {
		return statusFinished
	}
	if open := openPRs(v); len(open) > 0 && !core.Active(v.Status) && v.Status != core.StatusFailed {
		if slices.ContainsFunc(open, func(pr *forge.PR) bool { return pr.Checks == forge.ChecksFailing }) {
			return statusChecksFailed
		}
		if slices.ContainsFunc(open, func(pr *forge.PR) bool {
			return pr.Checks == forge.ChecksPending || pr.Checks == forge.ChecksWaiting
		}) {
			return statusChecksRunning
		}
		return statusInReview
	}
	return v.Status
}

// openPRs are a thread's open pull requests, one per repository.
func openPRs(v core.ThreadView) []*forge.PR {
	var out []*forge.PR
	for _, p := range v.Parts {
		if p.PR != nil && p.PR.State == forge.StateOpen {
			out = append(out, p.PR)
		}
	}
	return out
}

// threadRepos are the repositories a thread works in.
func threadRepos(v core.ThreadView) []string {
	if len(v.Parts) == 0 {
		return []string{v.RepoPath}
	}
	out := make([]string, len(v.Parts))
	for i, p := range v.Parts {
		out[i] = p.Repo
	}
	return out
}

// repoLabel names a thread's repositories: "api", or "client · api · db".
func repoLabel(v core.ThreadView) string {
	names := []string{}
	for _, r := range threadRepos(v) {
		names = append(names, filepath.Base(r))
	}
	return strings.Join(names, " · ")
}

// inRepo reports whether a thread works in the repository (any, for "").
func inRepo(v core.ThreadView, repo string) bool {
	return repo == "" || slices.Contains(threadRepos(v), repo)
}

func statusColor(ctx *ui.Context, status string) ui.Color {
	t := ctx.Theme()
	switch status {
	case statusNoChanges:
		return t.TextMuted
	case statusFinished:
		return t.Success
	case statusChecksRunning:
		return t.Warning
	case statusInReview:
		return ui.Hex(pal(t).info)
	case statusChecksFailed:
		return t.Danger
	case core.StatusRunning, core.StatusStarting, core.StatusBackground:
		return workingColor(t)
	case core.StatusWaiting:
		return t.Warning
	case core.StatusIdle:
		return t.Success
	case core.StatusFailed:
		return t.Danger
	case core.StatusMerged:
		return ui.Hex(pal(t).info)
	}
	return t.TextSubtle
}

// statusLabel is a status as shown to the user.
func statusLabel(v core.ThreadView) string {
	switch v.Status {
	case core.StatusQueued:
		return "Starting"
	case core.StatusStarting:
		return "Starting"
	case core.StatusRunning:
		return "Working"
	case core.StatusBackground:
		return "Waiting on background work"
	case core.StatusWaiting:
		return "Needs answer"
	case core.StatusIdle, core.StatusInterrupted:
		switch shownStatus(v) {
		case statusChecksRunning:
			return "Waiting for checks"
		case statusInReview:
			return "In review"
		case statusChecksFailed:
			return "Checks failed"
		}
		if v.Status == core.StatusInterrupted {
			return "Stopped"
		}
		switch {
		case v.NoChanges:
			return "No changes"
		case v.InPlace || v.Scratch:
			return "Finished"
		case v.CanOpenPR:
			return "Ready for a " + v.Forge.Noun()
		case v.IsWorker:
			return "Ready to merge"
		}
		return "Idle"
	case core.StatusFailed:
		return "Failed"
	case core.StatusMerged:
		return "Merged"
	case core.StatusDiscarded:
		return "Discarded"
	case core.StatusArchived:
		return "No changes"
	}
	return v.Status
}

// lineInset is how far a conversation's one-line rows reach past its left and right edges.
const lineInset = space2

// lineRow is a one-line row of a conversation that leads with an icon or a short label: a tool call,
// folded steps, a thread update, the working line. Its padding gives its hover background room, and a
// negative margin of the same takes the room back, so what it leads with sits on the left edge of the
// messages and cards around it and its last child on their right edge.
func lineRow() *ui.DivElement {
	return ui.Div().Row().PaddingX(lineInset).MarginX(-lineInset)
}

// unfoldedSteps holds the steps a folded line shows when opened, indented under the line's leading
// icon (14 px wide) with a rule down its middle.
func unfoldedSteps(ctx *ui.Context) *ui.DivElement {
	return ui.Div().Column().Gap(2).MarginLeft(6).PaddingLeft(space3).
		BorderEdges(ui.Edges{Left: 1}, ctx.Theme().Border)
}

// dot is a small round status mark.
func dot(c ui.Color, size float32) *ui.DivElement {
	return ui.Div().Size(ui.Px(size), ui.Px(size)).Radius(size / 2).Background(c).Shrink(0)
}

// statusPill is a status in a tinted pill with a dot.
func statusPill(ctx *ui.Context, v core.ThreadView) ui.Element {
	c := statusColor(ctx, shownStatus(v))
	return ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(10).Height(ui.Px(22)).Radius(11).Shrink(0).
		Background(c.Alpha(0.14)).
		Child(dot(c, 6)).
		Child(ui.Text(statusLabel(v)).FontSize(12).Semibold().TextColor(c))
}

// chip is a quiet rounded tag.
func chip(ctx *ui.Context, label string) *ui.DivElement {
	t := ctx.Theme()
	return ui.Div().Row().Align(ui.Center).PaddingX(space2).Height(ui.Px(22)).Radius(radiusS).
		Border(1, t.Border).Background(t.Surface).Shrink(0).
		Child(ui.Text(label).FontSize(12).TextColor(t.TextMuted).NoWrap())
}

// The Solo logo: three indigo petals fanning out from an amber dot, on a dark tile. The desktop icon
// (cmd/solo/icon.png) and the web client's icons (cmd/solo-web/web: favicon.ico, icon.svg and
// apple-touch-icon.png) are the same drawing: replace them together with brand/solo.svg.
//
//go:embed brand/solo.svg
var logoSVG []byte

var logo = ui.MustSVG(logoSVG)

// BrandMark is Solo's mark, for screens outside this package (the web client's).
func BrandMark(size float32) ui.Element { return brandMark(size) }

func brandMark(size float32) ui.Element {
	return ui.SVG(logo).Size(ui.Px(size), ui.Px(size)).Alt("Solo")
}

// The UI font is Inter (SIL Open Font License; see fonts/OFL.txt), bundled so the app looks the same on
// every desktop.
//
//go:embed fonts/*.ttf
var fontFiles embed.FS

// Font is a bundled font face.
type Font struct {
	Weight ui.FontWeight
	Data   []byte
}

// Fonts returns the faces of the UI font, for the app to register as its UI family.
func Fonts() []Font {
	faces := []struct {
		file   string
		weight ui.FontWeight
	}{
		{"Inter-Regular.ttf", ui.FontRegular},
		{"Inter-Medium.ttf", ui.FontMedium},
		{"Inter-SemiBold.ttf", ui.FontSemibold},
		{"Inter-Bold.ttf", ui.FontBold},
	}
	out := make([]Font, 0, len(faces))
	for _, f := range faces {
		data, err := fontFiles.ReadFile("fonts/" + f.file)
		if err != nil {
			panic(err)
		}
		out = append(out, Font{f.weight, data})
	}
	return out
}

// statusMark is a thread status's mark: a pulsing blue dot while it works, a check when it is ready,
// and a plain dot otherwise (amber, red, or grey), so no two states look alike.
func statusMark(ctx *ui.Context, status string) *ui.DivElement {
	t := ctx.Theme()
	c := statusColor(ctx, status)
	box := ui.Div().Size(ui.Px(14), ui.Px(14)).Center().Shrink(0).Tooltip(statusMeaning(status))
	switch status {
	case core.StatusRunning, core.StatusStarting:
		return box.Child(dot(c, 8).Play("pulse", []ui.Keyframe{
			{At: 0.5, Motion: ui.Grow(1.35).Opacity(0.45)},
		}, ui.Animation{Duration: 1200 * time.Millisecond, Repeat: true}))
	case core.StatusBackground:
		// Still at work, but waiting rather than busy: an hourglass that breathes slowly.
		return box.Child(ui.Div().Center().Child(ui.Icon(icons.Hourglass).Size(13).Color(c)).Play("breathe", []ui.Keyframe{
			{At: 0.5, Motion: ui.Fade().Opacity(0.4)},
		}, ui.Animation{Duration: 2400 * time.Millisecond, Repeat: true}))
	case core.StatusIdle, statusNoChanges, statusFinished:
		return box.Child(ui.Icon(icons.CircleCheck).Size(14).Color(c))
	case statusChecksRunning:
		return box.Child(ui.Icon(icons.Timer).Size(13).Color(c))
	case statusInReview:
		return box.Child(ui.Icon(icons.GitPullRequest).Size(13).Color(c))
	case statusChecksFailed: // one mark for every failure; the row's line says which
		return box.Child(ui.Icon(icons.CircleAlert).Size(14).Color(c))
	case core.StatusArchived:
		return box.Child(ui.Icon(icons.CircleCheck).Size(14).Color(c))
	case core.StatusMerged:
		return box.Child(ui.Icon(icons.GitMerge).Size(13).Color(c))
	case core.StatusQueued:
		return box.Child(ui.Div().Size(ui.Px(8), ui.Px(8)).Radius(4).Border(1.5, t.TextSubtle))
	case core.StatusWaiting:
		return box.Child(ui.Icon(icons.MessageCircleQuestionMark).Size(14).Color(c))
	case core.StatusFailed:
		return box.Child(ui.Icon(icons.CircleAlert).Size(14).Color(c))
	case core.StatusInterrupted:
		return box.Child(ui.Icon(icons.CircleStop).Size(13).Color(c))
	}
	return box.Child(dot(c, 8))
}

// statusMeaning says what a status mark means, for its tooltip.
func statusMeaning(status string) string {
	switch status {
	case core.StatusQueued:
		return "Starting"
	case core.StatusStarting:
		return "Starting: setting up its worktree"
	case core.StatusRunning:
		return "Working"
	case core.StatusBackground:
		return "Waiting on background work: its turn ended, but a command or subagent it started still runs. It carries on when that finishes."
	case core.StatusWaiting:
		return "Needs your answer"
	case core.StatusIdle:
		return "Ready: its work is done, to merge or open a pull request"
	case statusNoChanges, core.StatusArchived:
		return "Finished with no changes to merge"
	case statusFinished:
		return "Finished: its edits are already in place"
	case statusChecksRunning:
		return "Waiting for checks: its pull request is open and its checks are still running"
	case statusInReview:
		return "In review: its pull request is open"
	case statusChecksFailed:
		return "Checks failed on its pull request"
	case core.StatusMerged:
		return "Merged"
	case core.StatusFailed:
		return "Failed"
	case core.StatusInterrupted:
		return "Stopped"
	case core.StatusDiscarded:
		return "Discarded"
	}
	return status
}

// statusIcon is a status as a single icon, for places that take an icon (menus, the palette).
func statusIcon(status string) *icons.Icon {
	switch status {
	case core.StatusWaiting:
		return icons.MessageCircleQuestionMark
	case core.StatusFailed:
		return icons.CircleAlert
	case core.StatusIdle, statusNoChanges, statusFinished:
		return icons.CircleCheck
	case statusChecksRunning:
		return icons.Timer
	case statusInReview:
		return icons.GitPullRequest
	case statusChecksFailed:
		return icons.CircleX
	case core.StatusArchived:
		return icons.CircleCheck
	case core.StatusRunning, core.StatusStarting, core.StatusQueued:
		return icons.CircleDot
	case core.StatusBackground:
		return icons.Hourglass
	case core.StatusMerged:
		return icons.GitMerge
	case core.StatusInterrupted:
		return icons.CircleStop
	}
	return icons.Circle
}

// ticking renders the component again in a second while on, so the times it shows keep counting.
func ticking(ctx *ui.Context, on bool) {
	if !on {
		return
	}
	tick := ui.Local(ctx, "tick", 0)
	tick.Get()
	ctx.After(time.Second, func() { tick.Update(func(n int) int { return n + 1 }) })
}

// bgSince is when a thread's oldest background work started; zero without any.
func bgSince(v core.ThreadView) time.Time {
	var t time.Time
	for _, b := range v.Background {
		if t.IsZero() || (!b.Started.IsZero() && b.Started.Before(t)) {
			t = b.Started
		}
	}
	return t
}

// bgLine says what a thread waits on in the background: "Run benchmarks", or "Run benchmarks and 2 more".
func bgLine(v core.ThreadView) string {
	if len(v.Background) == 0 {
		return ""
	}
	s := v.Background[0].Description
	if s == "" {
		s = "a " + v.Background[0].Kind()
	}
	if n := len(v.Background) - 1; n > 0 {
		s += fmt.Sprintf(" and %d more", n)
	}
	return s
}

// elapsed formats how long something has run, from since until now: "45s", "2m 14s", "1h 05m". now is
// App.now, the clock since was stamped by. A since after now, which a right clock never gives, shows as
// "0s" rather than a negative time.
func elapsed(since, now time.Time) string {
	d := max(now.Sub(since).Round(time.Second), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}
