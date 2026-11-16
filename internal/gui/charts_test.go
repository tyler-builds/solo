package gui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/chartspec"
	"github.com/tyler-builds/solo/internal/core"
)

const testChart = "```chart\n" + `{"type": "bar", "title": "Test time by package", "x": "Package", "y": "Seconds",
 "series": [{"name": "main", "data": [["core", 12.5], ["gui", 30.1], ["store", 4]]},
            {"name": "branch", "data": [["core", 9], ["gui", 18.4], ["store", 4.2]]}]}` + "\n```\n"

// badChart has a kind of chart Solo doesn't draw: it shows as code.
const badChart = "```chart\n{\"type\": \"radar\", \"series\": [{\"data\": [[\"a\", 1]]}]}\n```\n"

// partKinds says what each part of a message is: "text:…", "chart:<type>" or "shots:<n>".
func partKinds(parts []messagePart) []string {
	var out []string
	for _, p := range parts {
		switch {
		case p.chart != nil:
			out = append(out, "chart:"+p.chart.Type)
		case p.shots != nil:
			out = append(out, fmt.Sprintf("shots:%d", len(p.shots)))
		default:
			out = append(out, "text:"+p.text)
		}
	}
	return out
}

func TestChartParts(t *testing.T) {
	a := &App{chartSpecs: map[string]parsedChart{}}
	att := "/data/attachments/a.png"
	cases := []struct {
		name   string
		md     string
		images []string
		want   []string
	}{
		{"no chart", "Just text.", nil, nil},
		{"chart among text", "The branch is faster:\n\n" + testChart + "\nMostly in gui.", nil,
			[]string{"text:The branch is faster:", "chart:bar", "text:Mostly in gui."}},
		{"chart alone", testChart, nil, []string{"chart:bar"}},
		{"invalid chart stays code", "Look:\n" + badChart + "Done.", nil, nil},
		{"invalid and valid", badChart + testChart, nil, []string{"text:" + strings.TrimSpace(badChart), "chart:bar"}},
		{"still streaming", "Look:\n```chart\n{\"type\": \"bar\", \"series\": [{\"data\": [[\"a\", 1]", nil, nil},
		{"with images", "Before:\n\n![Shot](" + att + ")\n\n" + testChart, []string{att},
			[]string{"text:Before:", "shots:1", "chart:bar"}},
		{"in a code block", "````markdown\n" + testChart + "````\n", nil, nil},
	}
	for _, c := range cases {
		if got := partKinds(a.chartParts(c.md, c.images)); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	// A spec is read once, however often the message is drawn.
	n := len(a.chartSpecs)
	a.chartParts(testChart, nil)
	if len(a.chartSpecs) != n {
		t.Errorf("the spec was read again: %d cached, then %d", n, len(a.chartSpecs))
	}
	// The cache is bounded.
	for i := 0; i < maxCachedCharts+10; i++ {
		a.chartSpec(fmt.Sprintf("{%d}", i))
	}
	if len(a.chartSpecs) > maxCachedCharts {
		t.Errorf("%d specs cached; the limit is %d", len(a.chartSpecs), maxCachedCharts)
	}
}

// chartNames is the accessible names of the charts on screen.
func chartNames(u *uitest.UI) []string {
	var out []string
	u.Runtime().AccessTree().Walk(func(n *access.Node) bool {
		if n.Role == access.RoleImage && (strings.Contains(n.Name, " chart") || strings.Contains(n.Name, "Scatter plot")) {
			out = append(out, n.Name)
		}
		return true
	})
	return out
}

// An agent's chart block shows as a chart where it stands: in Solo's chat, in a thread's
// conversation, in Solo's messages to a thread and in a merge card's summary. A block Solo can't
// draw stays code and the rest of the message still shows.
func TestAgentMessagesDrawCharts(t *testing.T) {
	f := demoBackend()
	o := core.OrchestratorID
	f.add(o, core.ItemAssistant, core.TextData{Text: "The branch is faster:\n\n" + testChart + "\n" + badChart + "\nMostly in gui."})
	f.add(o, core.ItemProposal, core.ProposalData{ThreadID: formsID, Summary: "Speeds up the tests.\n\n" +
		strings.Replace(testChart, `"bar"`, `"line"`, 1)})
	f.add(formsID, core.ItemUser, core.UserData{From: core.FromOrchestrator, Text: "Here is where the time goes:\n\n" +
		strings.Replace(testChart, `"bar"`, `"area"`, 1)})
	f.add(formsID, core.ItemAssistant, core.TextData{Text: "Done.\n\n" + strings.Replace(testChart, `"bar"`, `"scatter"`, 1)})
	a, u := newUI(t, f)
	u.Settle()
	names := chartNames(u)
	for _, want := range []string{"Bar chart", "Line chart"} {
		if !slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, want) }) {
			t.Errorf("Solo's chat has no %s: %q", want, names)
		}
	}
	if len(names) != 2 {
		t.Errorf("Solo's chat draws %d charts, want 2 (the radar stays code): %q", len(names), names)
	}
	a.openThread(formsID, "conversation")
	u.Settle()
	names = chartNames(u)
	for _, want := range []string{"Area chart", "Scatter plot"} {
		if !slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, want) }) {
			t.Errorf("the thread has no %s: %q", want, names)
		}
	}
}

// Every kind of chart a spec can ask for draws, including the edge cases the checks let through.
func TestEveryChartKindDraws(t *testing.T) {
	specs := []string{
		`{"type": "line", "series": [{"data": [[1, 2], [2, 3], [3, 1]]}]}`,
		`{"type": "line", "series": [{"name": "one point", "data": [[1, 2]]}]}`,
		`{"type": "area", "stacked": true, "series": [{"name": "a", "data": [["x", 1], ["y", 2]]}, {"name": "b", "data": [["x", 3]]}]}`,
		`{"type": "bar", "series": [{"data": [["neg", -4], ["pos", 3]]}]}`,
		`{"type": "bar", "horizontal": true, "stacked": true, "series": [{"name": "a", "data": [["x", 1], ["y", 2]]}, {"name": "b", "data": [["x", 3], ["y", 1]]}]}`,
		`{"type": "scatter", "x": "Size", "y": "Time", "series": [{"data": [[10, 1.5], [10, 1.6], [-3, 0]]}]}`,
		`{"type": "pie", "series": [{"data": [["a", 0], ["b", 0]]}]}`,
		`{"type": "pie", "title": "By number", "series": [{"data": [[1, 3], [2, 5]]}]}`,
		`{"type": "donut", "series": [{"data": [["only", 7]]}]}`,
		`{"type": "bar", "series": [{"data": [["same", 1], ["same", 2]]}]}`,
	}
	for _, src := range specs {
		spec, err := chartspec.Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		_, dark := Themes()
		u := uitest.New(t, func(ctx *ui.Context) ui.Element { return ui.Div().Child(chartView(ctx, spec)) },
			uitest.Size(800, 600), uitest.Theme(dark))
		u.Settle()
		if n := len(chartNames(u)); n != 1 {
			t.Errorf("%s: %d charts drawn", src, n)
		}
		u.Close()
	}
}

func TestExcerptsSkipCharts(t *testing.T) {
	if got := firstLine(stripMarkdown(testChart + "\nThe tests are **faster**.")); got != "The tests are faster." {
		t.Errorf("excerpt %q", got)
	}
}
