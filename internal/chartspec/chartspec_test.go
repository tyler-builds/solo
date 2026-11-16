package chartspec

import (
	"fmt"
	"strings"
	"testing"
)

const barSpec = `{"type": "bar", "title": "Test time", "x": "Package", "y": "Seconds",
 "series": [{"name": "main", "data": [["core", 12.5], ["gui", 30]]}]}`

func fence(info, body string) string { return "```" + info + "\n" + body + "\n```\n" }

func TestBlocks(t *testing.T) {
	chart := fence("chart", barSpec)
	cases := []struct {
		name string
		md   string
		want []string // the bodies found
	}{
		{"one", "Here:\n\n" + chart + "\nDone.", []string{barSpec + "\n"}},
		{"tildes", "~~~chart\n{}\n~~~\n", []string{"{}\n"}},
		{"case and extra words", "```Chart  wide\n{}\n```", []string{"{}\n"}},
		{"indented up to three", "   ```chart\n{}\n   ```\n", []string{"{}\n"}},
		{"longer closing fence", "```chart\n{}\n`````\n", []string{"{}\n"}},
		{"crlf", "```chart\r\n{}\r\n```\r\n", []string{"{}\r\n"}},
		{"two", chart + "and\n" + fence("chart", "{}"), []string{barSpec + "\n", "{}\n"}},
		{"other info", fence("json", barSpec), nil},
		{"no info", fence("", barSpec), nil},
		{"charts word", fence("charts", barSpec), nil},
		{"streaming: not closed yet", "Here:\n```chart\n{\"type\": \"bar\", \"ser", nil},
		{"indented four: code, not a fence", "    ```chart\n    {}\n    ```\n", nil},
		{"closing fence of the other kind", "```chart\n{}\n~~~\n", nil},
		{"closing fence too short", "````chart\n{}\n```\n", nil},
		{"closing fence with text", "```chart\n{}\n``` x\n", nil},
		{"nested in another block", "````markdown\n" + chart + "````\n", nil},
		{"backtick in info: not a fence", "```chart`x\n{}\n```\n", nil},
	}
	for _, c := range cases {
		var got []string
		for _, b := range Blocks(c.md) {
			got = append(got, b.Body)
			if !strings.HasPrefix(c.md[b.Start:], "```") && !strings.HasPrefix(strings.TrimLeft(c.md[b.Start:], " "), "~~~") &&
				!strings.HasPrefix(strings.TrimLeft(c.md[b.Start:], " "), "```") {
				t.Errorf("%s: block starts at %q", c.name, c.md[b.Start:])
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: bodies %q, want %q", c.name, got, c.want)
		}
	}
	md := "Before\n" + chart + "After"
	bs := Blocks(md)
	if len(bs) != 1 || md[:bs[0].Start] != "Before\n" || md[bs[0].End:] != "After" {
		t.Errorf("block spans %+v of %q", bs, md)
	}
}

func TestBlocksLimit(t *testing.T) {
	md := strings.Repeat(fence("chart", "{}"), MaxCharts+5)
	if n := len(Blocks(md)); n != MaxCharts {
		t.Errorf("%d blocks, want %d", n, MaxCharts)
	}
}

func TestParse(t *testing.T) {
	s, err := Parse(barSpec)
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != Bar || s.Title != "Test time" || s.X != "Package" || s.Y != "Seconds" || !s.Categories ||
		len(s.Series) != 1 || s.Series[0].Name != "main" || len(s.Series[0].Points) != 2 ||
		s.Series[0].Points[1] != (Point{Category: "gui", Y: 30}) {
		t.Errorf("parsed %+v", s)
	}
	s, err = Parse(`{"type": " Line ", "stacked": true, "colors": ["red"],
		"series": [{"name": "a", "data": [[1, 2], [2.5, -3e2]]}, {"data": [[0, 0]]}]}`)
	if err != nil {
		t.Fatal(err) // unknown fields are ignored; the type is read loosely
	}
	if s.Type != Line || s.Categories || !s.Stacked || s.Series[0].Points[1] != (Point{X: 2.5, Y: -300}) {
		t.Errorf("parsed %+v", s)
	}
	for _, k := range []string{Line, Area, Bar, Scatter, Pie, Donut} {
		if _, err := Parse(`{"type": "` + k + `", "series": [{"data": [["a", 1], ["b", 2]]}]}`); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	series := func(n, points int) string {
		var ss []string
		for i := 0; i < n; i++ {
			var ps []string
			for j := 0; j < points; j++ {
				ps = append(ps, fmt.Sprintf("[%d, %d]", j, j))
			}
			ss = append(ss, `{"data": [`+strings.Join(ps, ",")+`]}`)
		}
		return `"series": [` + strings.Join(ss, ",") + `]`
	}
	one := series(1, 2)
	cases := []struct{ name, spec, want string }{
		{"empty", ``, "not a valid spec"},
		{"not json", `type: bar`, "not a valid spec"},
		{"truncated", `{"type": "bar", "series": [{"data": [["a", 1]`, "not a valid spec"},
		{"two values", `{"type": "bar", ` + one + `} {}`, "more than one JSON value"},
		{"no type", `{` + one + `}`, `no "type"`},
		{"unknown type", `{"type": "radar", ` + one + `}`, `unknown type "radar"`},
		{"no series", `{"type": "bar"}`, "no series"},
		{"empty series", `{"type": "bar", "series": [{"name": "a", "data": []}]}`, "series 1 has no data"},
		{"too many series", `{"type": "line", ` + series(MaxSeries+1, 1) + `}`, "13 series"},
		{"too many points", `{"type": "line", ` + series(2, MaxPoints/2+1) + `}`, "more than 5000 points"},
		{"pie of two series", `{"type": "pie", ` + series(2, 2) + `}`, "a pie has one series"},
		{"too many slices", `{"type": "donut", ` + series(1, MaxSlices+1) + `}`, "25 slices"},
		{"negative slice", `{"type": "pie", "series": [{"data": [["a", 1], ["b", -1]]}]}`, "can't be negative"},
		{"mixed x", `{"type": "bar", "series": [{"data": [["a", 1]]}, {"data": [[2, 1]]}]}`, "x is a number, where earlier points have a category"},
		{"point of one", `{"type": "bar", "series": [{"data": [[1]]}]}`, "a point is [x, y]"},
		{"point of three", `{"type": "bar", "series": [{"data": [[1, 2, 3]]}]}`, "a point is [x, y]"},
		{"string y", `{"type": "bar", "series": [{"data": [["a", "1"]]}]}`, "y is a number"},
		{"null x", `{"type": "bar", "series": [{"data": [[null, 1]]}]}`, "x is a string or a number"},
		{"huge number", `{"type": "bar", "series": [{"data": [["a", 1e999]]}]}`, "y is a number"},
		{"long title", `{"type": "bar", "title": "` + strings.Repeat("é", MaxTitle+1) + `", ` + one + `}`, "longer than 200"},
		{"long name", `{"type": "bar", "series": [{"name": "` + strings.Repeat("n", MaxName+1) + `", "data": [[1, 1]]}]}`, "name longer"},
		{"long category", `{"type": "bar", "series": [{"data": [["` + strings.Repeat("c", MaxName+1) + `", 1]]}]}`, "category longer"},
		{"too big", `{"type": "bar", "title": "` + strings.Repeat(" ", MaxBytes) + `", ` + one + `}`, "the limit is 65536"},
	}
	for _, c := range cases {
		s, err := Parse(c.spec)
		if err == nil || s != nil {
			t.Errorf("%s: parsed %+v", c.name, s)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q, want it to say %q", c.name, err, c.want)
		}
	}
	// The largest spec within the limits is read.
	if _, err := Parse(`{"type": "line", ` + series(MaxSeries, MaxPoints/MaxSeries) + `}`); err != nil {
		t.Errorf("a spec at the limits: %v", err)
	}
}

func TestWithout(t *testing.T) {
	md := fence("chart", barSpec) + "\nThe tests are faster.\n" + fence("json", "{}")
	if got, want := Without(md), "\nThe tests are faster.\n"+fence("json", "{}"); got != want {
		t.Errorf("Without: %q, want %q", got, want)
	}
	if Without("plain") != "plain" {
		t.Error("Without changed text with no chart")
	}
}
