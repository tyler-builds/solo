package gui

// Solo's design system, after uas (~/code/uas/internal/gui/design.go): neutral near-black or white
// surfaces with hairline borders, and teal kept for the brand, primary actions and small status marks.
// Pages use these tokens and components rather than raw sizes and colors.

import (
	"bytes"
	"embed"
	_ "embed"
	"fmt"
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

// Corner radii.
const (
	radiusS  = 6  // buttons, inputs, chips
	radiusM  = 8  // rows, tool lines
	radiusL  = 10 // cards
	radiusXL = 12 // the content panel
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
	text: "#f2f2f4", textMuted: "#a9a9b3", textSubtle: "#75757f",
	accent: "#14897e", accentHover: "#18978b", accentActive: "#117469", accentText: "#ffffff", link: "#7cc8bf",
	brand:   "#2dd4bf",
	success: "#3ecf8e", warning: "#f5b544", danger: "#f87171", info: "#7aa7ff",
	scrollbar: "#2e2e34", scrim: "#000000b3",
}

var lightPalette = palette{
	background: "#ffffff", panel: "#f5f5f6", surface: "#ffffff", surfaceHover: "#f6f6f8", surfaceActive: "#ececef",
	border: "#e8e8ec", borderStrong: "#d8d8de",
	text: "#0c0c0e", textMuted: "#5d5d66", textSubtle: "#9a9aa3",
	accent: "#0f766e", accentHover: "#115e59", accentActive: "#134e4a", accentText: "#ffffff", link: "#0f766e",
	brand:   "#0d9488",
	success: "#15803d", warning: "#b45309", danger: "#dc2626", info: "#2459c7",
	scrollbar: "#d0d0d6", scrim: "#0c0c0e59",
}

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
		ui.SetThemeStyle(t, markdown.Style{InlineCode: ui.Style{TextColor: t.Link, Background: t.Background}})
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
// statusDone is shown for a worker that finished with nothing to merge (core.ThreadView.Done).
const statusDone = "done"

// shownStatus is the status the UI shows for a thread: its own, or statusDone.
func shownStatus(v core.ThreadView) string {
	if v.Done {
		return statusDone
	}
	return v.Status
}

func statusColor(ctx *ui.Context, status string) ui.Color {
	t := ctx.Theme()
	switch status {
	case statusDone:
		return t.TextMuted
	case core.StatusRunning, core.StatusStarting:
		return ui.Hex(pal(t).brand)
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
	case core.StatusWaiting:
		return "Needs answer"
	case core.StatusIdle:
		switch {
		case v.Done:
			return "Done"
		case v.IsWorker:
			return "Ready to merge"
		}
		return "Idle"
	case core.StatusFailed:
		return "Failed"
	case core.StatusInterrupted:
		return "Stopped"
	case core.StatusMerged:
		return "Merged"
	case core.StatusDiscarded:
		return "Discarded"
	case core.StatusArchived:
		return "Archived"
	}
	return v.Status
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

// The Solo logo: an orchestrator node and three workers on a teal tile. brand/solo.svg is also the
// desktop icon, which keeps a margin around the tile; in the app the tile fills the box.
//
//go:embed brand/solo.svg
var logoSVG []byte

var logo = ui.MustSVG(bytes.Replace(logoSVG, []byte(`viewBox="0 0 128 128"`), []byte(`viewBox="8 8 112 112"`), 1))

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

// statusMark is a thread status's mark: a pulsing teal dot while it works, a check when it is ready,
// and a plain dot otherwise (amber, red, or grey), so no two states look alike.
func statusMark(ctx *ui.Context, status string) ui.Element {
	t := ctx.Theme()
	c := statusColor(ctx, status)
	box := ui.Div().Size(ui.Px(14), ui.Px(14)).Center().Shrink(0)
	switch status {
	case core.StatusRunning, core.StatusStarting:
		return box.Child(dot(c, 8).Play("pulse", []ui.Keyframe{
			{At: 0.5, Motion: ui.Grow(1.35).Opacity(0.45)},
		}, ui.Animation{Duration: 1200 * time.Millisecond, Repeat: true}))
	case core.StatusIdle, statusDone:
		return box.Child(ui.Icon(icons.CircleCheck).Size(14).Color(c))
	case core.StatusArchived:
		return box.Child(ui.Icon(icons.Archive).Size(13).Color(c))
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

// statusIcon is a status as a single icon, for places that take an icon (menus, the palette).
func statusIcon(status string) *icons.Icon {
	switch status {
	case core.StatusWaiting:
		return icons.MessageCircleQuestionMark
	case core.StatusFailed:
		return icons.CircleAlert
	case core.StatusIdle, statusDone:
		return icons.CircleCheck
	case core.StatusArchived:
		return icons.Archive
	case core.StatusRunning, core.StatusStarting, core.StatusQueued:
		return icons.CircleDot
	case core.StatusMerged:
		return icons.GitMerge
	case core.StatusInterrupted:
		return icons.CircleStop
	}
	return icons.Circle
}

// elapsed formats a running duration: "45s", "2m 14s", "1h 05m".
func elapsed(since time.Time) string {
	d := time.Since(since).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}
