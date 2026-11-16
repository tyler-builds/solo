package gui

import (
	"fmt"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

// usageHigh is the share of a limit from which its bar turns to the warning color, even before Claude
// warns.
const usageHigh = 0.8

// claudeWarnFrom is the share of a limit below which Claude Code ignores the server's allowed_warning
// status. The server sets it once usage passes any of its warning thresholds, early ones (25%, say)
// included, so it alone doesn't mean the limit is close; Claude Code only warns from this share on.
const claudeWarnFrom = 0.7

// limitNames are the short label and the full name of each kind of usage limit.
var limitNames = map[string][2]string{
	"five_hour":                  {"5h", "5-hour limit"},
	"seven_day":                  {"7d", "Weekly limit"},
	"seven_day_opus":             {"7d Opus", "Weekly Opus limit"},
	"seven_day_sonnet":           {"7d Sonnet", "Weekly Sonnet limit"},
	"seven_day_overage_included": {"7d extra", "Weekly included extra usage"},
	"overage":                    {"Extra", "Extra usage"},
	"":                           {"Usage", "Usage limit"},
}

// limitName is a limit's short label and full name.
func limitName(kind string) (short, full string) {
	if n, ok := limitNames[kind]; ok {
		return n[0], n[1]
	}
	return strings.ReplaceAll(kind, "_", " "), strings.ReplaceAll(kind, "_", " ") + " limit"
}

// nearLimit reports whether a limit not yet reached is close to it, by how much of it is used: most of
// it, or enough that Claude Code would warn, when the server warns. When how much is used isn't known,
// the server's warning is all there is to go by.
func nearLimit(e agent.Event, used float64, known bool) bool {
	switch {
	case e.Status == "rejected":
		return false
	case !known:
		return e.Status == "allowed_warning"
	}
	return used >= usageHigh || (e.Status == "allowed_warning" && used >= claudeWarnFrom)
}

// usageColor is the color of a limit's bar: the accent, the warning color once it is near the limit, and
// the danger color once it is reached.
func usageColor(t *ui.Theme, e agent.Event, used float64) ui.Color {
	switch {
	case e.Status == "rejected":
		return t.Danger
	case nearLimit(e, used, true):
		return t.Warning
	}
	return t.Accent
}

// resetLine says when a limit resets: "Resets at 3:40 PM (in 2h 14m)", or for one further off,
// "Resets Tue Nov 17, 3:40 PM".
func resetLine(resetsAt float64, now time.Time) string {
	if resetsAt <= 0 {
		return ""
	}
	at := time.Unix(int64(resetsAt), 0).Local()
	d := at.Sub(now)
	switch {
	case d <= 0:
		return "Resets now"
	case d < 24*time.Hour:
		in := fmt.Sprintf("%dm", int(d.Minutes()))
		if d >= time.Hour {
			in = fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
		}
		return fmt.Sprintf("Resets at %s (in %s)", at.Format("3:04 PM"), in)
	}
	return "Resets " + at.Format("Mon Jan 2, 3:04 PM")
}

// usageTip is a limit's tooltip: its full name, how much is used, its state and when it resets.
func usageTip(e agent.Event, used float64, known bool, now time.Time) string {
	_, full := limitName(e.RateLimitType)
	parts := []string{full}
	if known {
		parts[0] += fmt.Sprintf(": %d%% used", percent(used))
	}
	switch {
	case e.Status == "rejected":
		parts = append(parts, "Limit reached")
	case nearLimit(e, used, known):
		parts = append(parts, "Near the limit")
	}
	if r := resetLine(e.ResetsAt, now); r != "" {
		parts = append(parts, r)
	}
	return strings.Join(parts, "\n")
}

func percent(used float64) int { return int(used*100 + 0.5) }

// usagePanel shows Claude's usage limits at the foot of the rail: a row per limit with its short label,
// a thin bar and the percent used. A compact window's drawer is short of height, so there the limits
// share one line, each a label, a short bar and the percent (miniUsage). When Claude has said nothing
// about how much is used, it shows a notice only if a limit is near or reached; otherwise nothing.
func (a *App) usagePanel(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	limits := a.rate.Get()
	now := a.now()
	if isCompact(ctx) {
		if mini := miniUsage(ctx, limits, now); mini != nil {
			return mini
		}
	}
	box := ui.Div().Column().Gap(6).PaddingX(space4).PaddingY(space3).Shrink(0)
	rows := 0
	for _, e := range limits {
		used, ok := core.UsedFraction(e)
		if !ok {
			continue
		}
		rows++
		short, full := limitName(e.RateLimitType)
		c := usageColor(t, e, used)
		pctColor := t.TextMuted
		if c != t.Accent {
			pctColor = c
		}
		box = box.Child(ui.Div().Row().Align(ui.Center).Gap(space2).Height(ui.Px(16)).
			Key("usage-" + e.RateLimitType).Tooltip(usageTip(e, used, true, now)).
			Child(ui.Text(short).FontSize(11).Medium().TextColor(t.TextMuted).NoWrap().Truncate().
				Width(ui.Px(52)).Shrink(0)).
			Child(ui.ProgressBar(used).Label(full).Color(c).HideValue().Grow(1).MinWidth(ui.Px(0)).
				Styles(ui.ProgressBarStyle{Track: ui.Style{Height: ui.Px(4), Radius: 2}, Fill: ui.Style{Radius: 2}})).
			Child(ui.Text(fmt.Sprintf("%d%%", percent(used))).FontSize(11).TextColor(pctColor).NoWrap().
				Width(ui.Px(32)).Shrink(0).Align(ui.End)))
	}
	if rows > 0 {
		return box
	}
	// Nothing says how much is used: say so only when a limit is near or reached.
	var worst *agent.Event
	for i, e := range limits {
		if e.Status == "rejected" || (nearLimit(e, 0, false) && (worst == nil || worst.Status != "rejected")) {
			worst = &limits[i]
		}
	}
	if worst == nil {
		return ui.Div()
	}
	status, c := "Near the usage limit", t.Warning
	if worst.Status == "rejected" {
		status, c = "Usage limit reached", t.Danger
	}
	return box.Child(ui.Div().Row().Align(ui.Center).Gap(6).PaddingX(space2).Height(ui.Px(24)).Radius(12).
		Background(c.Alpha(0.12)).Key("usage-notice").Tooltip(usageTip(*worst, 0, false, now)).
		Child(dot(c, 6)).
		Child(ui.Text(status).FontSize(12).Medium().TextColor(c).Truncate().MinWidth(ui.Px(0))))
}

// miniUsage is the usage limits in one line, for a compact window: each its short label, a short bar
// and the percent used, colored as in the full panel, with the same tooltip. It wraps to a second line
// only when the limits don't fit. Nil when no limit says how much is used.
func miniUsage(ctx *ui.Context, limits []agent.Event, now time.Time) ui.Element {
	t := ctx.Theme()
	line := ui.Div().Row().Wrap().Align(ui.Center).Gap(space2 + 2).PaddingX(space4).PaddingY(space2).Shrink(0).
		Key("usage-mini")
	rows := 0
	for _, e := range limits {
		used, ok := core.UsedFraction(e)
		if !ok {
			continue
		}
		rows++
		short, full := limitName(e.RateLimitType)
		c := usageColor(t, e, used)
		pctColor := t.TextMuted
		if c != t.Accent {
			pctColor = c
		}
		line = line.Child(ui.Div().Row().Align(ui.Center).Gap(3).Height(ui.Px(14)).Shrink(0).
			Key("usage-" + e.RateLimitType).Tooltip(usageTip(e, used, true, now)).
			Child(ui.Text(short).FontSize(11).Medium().TextColor(t.TextMuted).NoWrap()).
			Child(ui.Div().Width(ui.Px(24)).Shrink(0).Child(ui.ProgressBar(used).Label(full).Color(c).HideValue().
				Width(ui.Full).MinWidth(ui.Px(0)).Styles(ui.ProgressBarStyle{Track: ui.Style{Height: ui.Px(3), Radius: 1.5}, Fill: ui.Style{Radius: 1.5}}))).
			Child(ui.Text(fmt.Sprintf("%d%%", percent(used))).FontSize(11).TextColor(pctColor).NoWrap()))
	}
	if rows == 0 {
		return nil
	}
	return line
}
