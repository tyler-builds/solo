package gui

import (
	"strconv"

	"github.com/tyler-builds/jamb/charts"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/chartspec"
)

// chartHeight is the height of a chart in a message, and chartMaxWidth its widest.
const (
	chartHeight   = 280
	chartMaxWidth = 720
	chartBarRow   = 26  // the height a horizontal bar chart gives each category
	chartMaxTall  = 640 // the tallest a horizontal bar chart grows
)

// maxCachedCharts bounds App.chartSpecs: past it, the cache starts over.
const maxCachedCharts = 256

// parsedChart is a chart block's spec as chartspec.Parse read it: nil when it is invalid.
type parsedChart struct{ spec *chartspec.Spec }

// chartSpec reads a chart block's body, once: a message is drawn again on each render, and while it
// streams in. Call on the UI thread, in a render.
func (a *App) chartSpec(body string) *chartspec.Spec {
	if p, ok := a.chartSpecs[body]; ok {
		return p.spec
	}
	if len(a.chartSpecs) >= maxCachedCharts {
		a.chartSpecs = map[string]parsedChart{}
	}
	spec, _ := chartspec.Parse(body)
	a.chartSpecs[body] = parsedChart{spec}
	return spec
}

// chartParts splits an agent's message at its chart blocks that read as charts, each becoming a part
// of its own; the text between them is split at its images (see imageLines). A block that doesn't
// read as a chart stays in the text, where the markdown view shows it as code.
func (a *App) chartParts(md string, images []string) []messagePart {
	var parts []messagePart
	text := func(s string) {
		if sub := imageLines(s, images); sub != nil {
			parts = append(parts, sub...)
		} else if s != "" {
			parts = append(parts, messagePart{text: s})
		}
	}
	last := 0
	for _, b := range chartspec.Blocks(md) {
		spec := a.chartSpec(b.Body)
		if spec == nil {
			continue
		}
		text(trimBlank(md[last:b.Start]))
		parts = append(parts, messagePart{chart: spec})
		last = b.End
	}
	if last == 0 {
		return imageLines(md, images)
	}
	text(trimBlank(md[last:]))
	return parts
}

// trimBlank drops the blank lines around markdown, keeping the indentation of its first line.
func trimBlank(s string) string {
	for {
		i := 0
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r') {
			i++
		}
		if i < len(s) && s[i] == '\n' {
			s = s[i+1:]
			continue
		}
		if i == len(s) {
			return ""
		}
		break
	}
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// chartView draws a chart from a message, in a card as wide as the message (at most chartMaxWidth).
// Hovering shows a point's values, the arrow keys walk them, and the legend shows and hides a series.
func chartView(ctx *ui.Context, s *chartspec.Spec) ui.Element {
	t := ctx.Theme()
	c := buildChart(s)
	h := float32(chartHeight)
	if s.Type == chartspec.Bar && s.Horizontal {
		h = min(max(h, float32(positions(s)*chartBarRow+96)), chartMaxTall)
	}
	c = c.Size(ui.Full, ui.Px(h))
	return ui.Div().Column().Width(ui.Full).MaxWidth(ui.Px(chartMaxWidth)).
		Padding(space3).Radius(radiusL).Border(1, t.Border).Background(t.Surface).
		Child(c)
}

// buildChart makes the jamb chart of a spec, which Parse has checked: the series place their points
// on one kind of axis, and a pie has one series.
func buildChart(s *chartspec.Spec) *charts.Chart {
	type pt = chartspec.Point
	round := s.Type == chartspec.Pie || s.Type == chartspec.Donut
	series := make([]charts.Series[pt], len(s.Series))
	for i, ss := range s.Series {
		cs := charts.Series[pt]{Name: ss.Name, Data: ss.Points, Y: func(p pt) float64 { return p.Y }}
		switch {
		case s.Categories:
			cs.Category = func(p pt) string { return p.Category }
		case round: // a slice is named by its category: a number names it as written
			cs.Category = func(p pt) string { return strconv.FormatFloat(p.X, 'g', -1, 64) }
		default:
			cs.X = func(p pt) float64 { return p.X }
		}
		series[i] = cs
	}
	var c *charts.Chart
	switch s.Type {
	case chartspec.Line:
		c = charts.Line(series...)
	case chartspec.Area:
		c = charts.Area(series...)
	case chartspec.Bar:
		c = charts.Bar(series...)
	case chartspec.Scatter:
		c = charts.Scatter(series...)
	case chartspec.Pie:
		c = charts.Pie(series[0])
	default:
		c = charts.Donut(series[0])
	}
	if s.Title != "" {
		c = c.Title(s.Title)
	}
	if !round {
		c = c.XAxis(charts.Axis{Label: s.X}).YAxis(charts.Axis{Label: s.Y})
		if len(s.Series) == 1 && s.Series[0].Name == "" {
			c = c.Legend(charts.LegendNone) // nothing to tell apart
		}
	}
	if s.Stacked {
		c = c.Stacked()
	}
	if s.Horizontal {
		c = c.Horizontal()
	}
	return c
}

// positions is the number of distinct positions of a spec's points: its categories, or its x values.
func positions(s *chartspec.Spec) int {
	seen := map[string]bool{}
	for _, ss := range s.Series {
		for _, p := range ss.Points {
			k := p.Category
			if !s.Categories {
				k = strconv.FormatFloat(p.X, 'g', -1, 64)
			}
			seen[k] = true
		}
	}
	return len(seen)
}
