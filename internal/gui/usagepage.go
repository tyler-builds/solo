package gui

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/charts"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// usageKinds are the limits the Usage page can chart, in the order of its tabs.
var usageKinds = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet"}

// usageRows is how many threads the Usage page lists before it sums the rest.
const usageRows = 8

// leftPoint is a point of the Usage page's line: how much of a limit was left at a time, in percent.
type leftPoint struct {
	At   time.Time
	Left float64
}

// leftSeries is the line of what was left of one limit, from its readings (oldest first), over the span
// up to now. Where a window reset between two readings, the line climbs back to 100% at the reset; it
// ends at now with what the last reading says is left, or 100% if its window has reset since. Points
// before the span are cut, the span starting with what was left then.
func leftSeries(readings []core.UsageReading, kind string, now time.Time, span time.Duration) []leftPoint {
	left := func(r core.UsageReading) float64 { return 100 * (1 - min(max(r.Used, 0), 1)) }
	var pts []leftPoint
	var last *core.UsageReading
	reset := func(at time.Time, before float64) {
		pts = append(pts, leftPoint{at, before}, leftPoint{at, 100})
	}
	for i := range readings {
		r := readings[i]
		if r.Kind != kind {
			continue
		}
		if last != nil && !last.ResetsAt.IsZero() && !last.ResetsAt.After(r.At) {
			reset(last.ResetsAt, left(*last))
		}
		pts = append(pts, leftPoint{r.At, left(r)})
		last = &readings[i]
	}
	if last == nil {
		return nil
	}
	if !last.ResetsAt.IsZero() && !last.ResetsAt.After(now) {
		reset(last.ResetsAt, left(*last))
		pts = append(pts, leftPoint{now, 100})
	} else {
		pts = append(pts, leftPoint{now, left(*last)})
	}
	start := now.Add(-span)
	cut := 0
	for cut < len(pts) && pts[cut].At.Before(start) {
		cut++
	}
	if cut > 0 {
		pts = append([]leftPoint{{start, pts[cut-1].Left}}, pts[cut:]...)
	}
	return pts
}

// usageSpan is how far back the Usage page's line goes for a limit: a day for the 5-hour one, a week
// for the others.
func usageSpan(kind string) time.Duration {
	if kind == "five_hour" {
		return 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

// openUsage opens the Usage page.
func (a *App) openUsage() {
	a.drawer.Set(false)
	a.usageOpen.Set(true)
}

// usageQuery fetches the usage history. It fetches again when a limit's reading changes or a thread's
// cost does, so the page follows the turns that run while it is open.
func (a *App) usageQuery(ctx *ui.Context) ui.QueryResult[core.UsageReport] {
	var key strings.Builder
	key.WriteString("usage")
	for _, e := range a.rate.Get() {
		if u, ok := core.UsedFraction(e); ok {
			fmt.Fprintf(&key, ":%s=%.4f@%.0f", e.RateLimitType, u, e.ResetsAt)
		}
	}
	var cost float64
	for _, th := range a.threads.Get() {
		cost += th.CostUSD
	}
	fmt.Fprintf(&key, ":%.6f", cost)
	return ui.Query(ctx, key.String(), func(c context.Context) (core.UsageReport, error) {
		return a.b.Usage(c)
	})
}

// usageDialog is the Usage page: each limit as the rail shows it, but larger; a line of what was left
// of one over time; and what Solo's threads used of it.
func (a *App) usageDialog(ctx *ui.Context) ui.Element {
	if !a.usageOpen.Get() {
		return nil
	}
	winW, winH := ctx.WindowSize()
	w := min(760, winW-2*space2)
	res := a.usageQuery(ctx)
	now := a.now()
	body := ui.Div().Column().Gap(space6).Width(ui.Full).MaxHeight(ui.Px(max(winH*0.86-120, 240))).ScrollY().
		PaddingBottom(space2).PaddingRight(space3).
		Child(a.usageLimits(ctx, now)).
		Child(ui.Component(func(ctx *ui.Context) ui.Element { return a.usageHistory(ctx, res, now) })).
		Child(a.usageThreads(ctx, res))
	return ui.Modal("Usage", body).Width(ui.Px(w)).MaxWidth(ui.Px(w)).CloseButton(true).CloseOnBackdrop().
		OnClose(func() { a.usageOpen.Set(false) }).Key("usage-page")
}

// usageSection is a part of the Usage page: its title, a line under it, and what it shows.
func usageSection(ctx *ui.Context, title, about string, trailing ui.Element, content ...ui.Element) ui.Element {
	t := ctx.Theme()
	head := ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
		Child(ui.Text(title).FontSize(13).Semibold().TextColor(t.Text))
	if about != "" {
		head = head.Child(ui.Text(about).FontSize(12).TextColor(t.TextMuted))
	}
	row := ui.Div().Row().Wrap().Align(ui.Center).Gap(space3).Child(head)
	if trailing != nil {
		row = row.Child(trailing)
	}
	return ui.Div().Column().Gap(space3).Child(row).Children(content...)
}

// usageLimits is each limit Claude reported, a card apiece: how much is used, a bar, and when it resets.
func (a *App) usageLimits(ctx *ui.Context, now time.Time) ui.Element {
	t := ctx.Theme()
	grid := ui.Div().Row().Wrap().Gap(space3)
	n := 0
	for _, e := range a.rate.Get() {
		used, ok := core.UsedFraction(e)
		if !ok {
			continue
		}
		n++
		_, full := limitName(e.RateLimitType)
		c := usageColor(t, e, used)
		top := ui.Div().Row().Align(ui.Center).Gap(space2).
			Child(ui.Text(full).FontSize(12).Medium().TextColor(t.TextMuted).Truncate().Grow(1).MinWidth(ui.Px(0)))
		switch {
		case e.Status == "rejected":
			top = top.Child(ui.Text("Limit reached").FontSize(11).Medium().TextColor(t.Danger).NoWrap())
		case nearLimit(e, used, true):
			top = top.Child(ui.Text("Near the limit").FontSize(11).Medium().TextColor(t.Warning).NoWrap())
		}
		card := ui.Div().Column().Gap(space2).Grow(1).Basis(ui.Px(180)).MinWidth(ui.Px(160)).
			Padding(space3).Radius(radiusL).Border(1, t.Border).Background(t.Surface).
			Key("usage-card-" + e.RateLimitType).
			Child(top).
			Child(ui.Text(fmt.Sprintf("%d%% used", percent(used))).FontSize(20).Semibold().TextColor(t.Text).NoWrap()).
			Child(ui.ProgressBar(used).Label(full).Color(c).HideValue().Width(ui.Full).
				Styles(ui.ProgressBarStyle{Track: ui.Style{Height: ui.Px(4), Radius: 2}, Fill: ui.Style{Radius: 2}}))
		if r := resetLine(e.ResetsAt, now); r != "" {
			card = card.Child(ui.Text(r).FontSize(11).TextColor(t.TextMuted).NoWrap().Truncate())
		}
		grid = grid.Child(card)
	}
	if n == 0 {
		return muted(ctx, "Claude hasn't reported your usage yet. It does during a turn.")
	}
	return grid
}

// usageHistory is the line of what was left of a limit over time, with a tab for each limit that has
// readings.
func (a *App) usageHistory(ctx *ui.Context, res ui.QueryResult[core.UsageReport], now time.Time) ui.Element {
	t := ctx.Theme()
	have := map[string]bool{}
	for _, r := range res.Data.Readings {
		have[r.Kind] = true
	}
	var tabs []ui.Tab[string]
	for _, k := range usageKinds {
		if have[k] {
			_, full := limitName(k)
			tabs = append(tabs, ui.Tab[string]{Value: k, Label: strings.TrimSuffix(full, " limit")})
		}
	}
	kind := ui.Local(ctx, "usage-kind", "seven_day")
	if !have[kind.Peek()] && len(tabs) > 0 {
		kind.Set(tabs[0].Value)
	}
	k := kind.Get()
	_, full := limitName(k)
	title := full + " left, past week"
	if k == "five_hour" {
		title = full + " left, past day"
	}
	var trailing ui.Element
	if len(tabs) > 1 {
		trailing = ui.Tabs(kind, tabs...).Key("usage-tabs")
	}
	about := "From the readings Claude sends during Solo's turns. Between turns the line is straight, so use elsewhere shows at Solo's next turn."
	pts := leftSeries(res.Data.Readings, k, now, usageSpan(k))
	if len(pts) == 0 {
		msg := "No readings yet. Solo keeps one each time Claude reports your usage during a turn."
		switch {
		case res.Loading:
			msg = "Loading…"
		case res.Err != nil:
			msg = "Couldn't load the usage history: " + res.Err.Error()
		}
		return usageSection(ctx, "Left over time", "", nil, muted(ctx, msg))
	}
	pct := func(v float64) string { return fmt.Sprintf("%.0f%%", math.Abs(math.Round(v))) }
	// A tick at midnight names the day; the others, the hour.
	tick := func(v float64) string {
		at := time.Unix(int64(v), 0).Local()
		if at.Hour() == 0 && at.Minute() == 0 {
			return at.Format("Mon")
		}
		return at.Format("3 PM")
	}
	chart := charts.Line(charts.Series[leftPoint]{
		Name:  "Left",
		Data:  pts,
		Time:  func(p leftPoint) time.Time { return p.At },
		Y:     func(p leftPoint) float64 { return p.Left },
		Color: t.Accent,
	}).Label(title).Legend(charts.LegendNone).Format(pct).
		XAxis(charts.Axis{Format: tick}).
		YAxis(charts.Axis{Format: pct, Ticks: 5}.Min(0).Max(100)).
		Size(ui.Full, ui.Px(220)).Key("usage-line-" + k)
	return usageSection(ctx, title, about, trailing,
		ui.Div().Width(ui.Full).Padding(space3).Radius(radiusL).Border(1, t.Border).Background(t.Surface).Child(chart))
}

// usageThreads is what Solo's threads used since the weekly window began, as shares of all they used,
// most first: the biggest few by name, and the rest summed.
func (a *App) usageThreads(ctx *ui.Context, res ui.QueryResult[core.UsageReport]) ui.Element {
	t := ctx.Theme()
	rep := res.Data
	about := "Since " + rep.Since.Local().Format("Mon Jan 2, 3:04 PM") +
		". Each thread's share of what Solo's turns cost, at API prices."
	if rep.Since.IsZero() {
		about = ""
	}
	var total float64
	for _, u := range rep.Threads {
		total += u.CostUSD
	}
	if total <= 0 {
		msg := "No turns have finished since the week began."
		if res.Loading {
			msg = "Loading…"
		} else if res.Err != nil {
			msg = "Couldn't load the usage history: " + res.Err.Error()
		}
		return usageSection(ctx, "What Solo used", about, nil, muted(ctx, msg))
	}
	type row struct {
		id, title, kind string
		cost            float64
		turns           int
	}
	var rows []row
	for i, u := range rep.Threads {
		if i == usageRows && len(rep.Threads) > usageRows+1 {
			rest := row{id: "", title: plural(len(rep.Threads)-usageRows, "other thread")}
			for _, o := range rep.Threads[usageRows:] {
				rest.cost += o.CostUSD
				rest.turns += o.Turns
			}
			rows = append(rows, rest)
			break
		}
		title := u.Title
		if title == "" {
			title = "Untitled thread"
		}
		rows = append(rows, row{id: u.ThreadID, title: title, kind: u.Kind, cost: u.CostUSD, turns: u.Turns})
	}
	top := rows[0].cost
	for _, r := range rows {
		top = max(top, r.cost)
	}
	showCost := a.cfg.Get().ShowCosts
	list := ui.Div().Column().Gap(2)
	for _, r := range rows {
		label := ui.Div().Row().Align(ui.Center).Gap(6).Width(ui.Px(220)).Shrink(1).MinWidth(ui.Px(0)).
			Child(ui.Text(r.title).FontSize(12).TextColor(t.Text).NoWrap().Truncate().Shrink(1).MinWidth(ui.Px(0)))
		switch r.kind {
		case core.UsageKindCard:
			label = label.Child(ui.Text("card").FontSize(11).TextColor(t.TextSubtle).NoWrap().Shrink(0))
		case "":
			if r.id != "" {
				label = label.Child(ui.Text("deleted").FontSize(11).TextColor(t.TextSubtle).NoWrap().Shrink(0))
			}
		}
		share := r.cost / total
		bar := ui.Div().Grow(1).Basis(ui.Px(0)).MinWidth(ui.Px(40)).Height(ui.Px(10)).
			Child(ui.Div().Height(ui.Full).Width(ui.Percent(float32(100 * r.cost / top))).MinWidth(ui.Px(2)).
				Radius(3).Background(t.Accent))
		tip := plural(r.turns, "turn")
		if showCost {
			tip = fmt.Sprintf("$%.2f at API prices · %s", r.cost, tip)
		}
		line := ui.Div().Row().Align(ui.Center).Gap(space3).Height(ui.Px(28)).PaddingX(space2).Radius(radiusS).
			Key("usage-row-" + cmp.Or(r.id, "rest")).Tooltip(tip).
			Child(label).Child(bar).
			Child(ui.Text(fmt.Sprintf("%d%%", max(1, int(100*share+0.5)))).FontSize(12).TextColor(t.TextMuted).
				NoWrap().Width(ui.Px(36)).Shrink(0).Align(ui.End))
		if _, ok := a.thread(r.id); ok && r.id != "" && r.kind != core.UsageKindSolo {
			id := r.id
			line = line.Cursor(ui.CursorPointer).Hover(ui.Style{Background: t.SurfaceHover}).
				Role(access.RoleButton).Label("Open " + r.title).TabIndex(0).
				OnClick(func() { a.usageOpen.Set(false); a.openThread(id, "conversation") })
		}
		list = list.Child(line)
	}
	note := ui.Text("Only Solo's turns on this machine count here. Claude Code elsewhere and claude.ai use the same limits.").
		FontSize(11).TextColor(t.TextMuted)
	return usageSection(ctx, "What Solo used", about, nil, list, note)
}
