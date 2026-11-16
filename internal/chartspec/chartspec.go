// Package chartspec reads the charts agents put in their messages: a fenced code block whose info
// string is "chart", holding a small JSON spec. The app draws a block that reads as a chart where it
// stands; any other block, or one whose spec is invalid, stays an ordinary code block.
//
//	```chart
//	{"type": "bar", "title": "Test time by package", "y": "Seconds",
//	 "series": [{"name": "main", "data": [["core", 12.5], ["gui", 30]]}]}
//	```
//
// The spec:
//
//   - type: line, area, bar, scatter, pie or donut.
//   - title, x, y: the chart's title and its axis labels; optional.
//   - stacked: stacks the series of a bar or area chart. horizontal: runs a bar chart's bars across.
//   - series: one or more of {"name", "data"}; a pie or donut has exactly one. data is a list of
//     [x, y] points, where x is a category (a string) or a number, the same kind in every point
//     of the chart, and y is a number. A pie's or donut's values can't be negative.
//
// Other fields are ignored. The limits (MaxBytes, MaxSeries, MaxPoints and the others) keep a large
// spec from slowing the app: MaxBytes bounds the work of decoding one.
package chartspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Limits on a spec. A spec over any of them is invalid, and shows as code.
const (
	MaxBytes  = 64 << 10 // the JSON
	MaxSeries = 12
	MaxPoints = 5000 // across all the series
	MaxSlices = 24   // of a pie or donut
	MaxTitle  = 200  // characters of the title and of each axis label
	MaxName   = 100  // characters of a series name and of a category
	MaxCharts = 20   // drawn in one message; later ones show as code
)

// Kinds of chart, the values of Spec.Type.
const (
	Line    = "line"
	Area    = "area"
	Bar     = "bar"
	Scatter = "scatter"
	Pie     = "pie"
	Donut   = "donut"
)

// Spec is a chart, as read and checked by Parse.
type Spec struct {
	Type       string
	Title      string
	X, Y       string // axis labels
	Stacked    bool
	Horizontal bool
	Series     []Series
	// Categories says the points are placed by category (Point.Category); otherwise by number (Point.X).
	Categories bool
}

// Series is one series of a chart.
type Series struct {
	Name   string
	Points []Point
}

// Point is a point of a series: its position, a Category or an X, and its value.
type Point struct {
	Category string
	X, Y     float64
}

// Block is a chart block in markdown: text[Start:End] is the whole fenced block, from the start of its
// opening fence line to the end of its closing fence line (with its line break), and Body is what the
// fences hold.
type Block struct {
	Start, End int
	Body       string
}

// Blocks finds the closed, top-level fenced code blocks of markdown whose info string is "chart". A
// fence indented four spaces or more is not a fence, and a block nested in another (a "chart" fence
// inside a ```markdown block) is code, as is a block still open at the end of the text, such as one
// still streaming in.
func Blocks(md string) []Block {
	if !strings.Contains(md, "```") && !strings.Contains(md, "~~~") {
		return nil
	}
	var out []Block
	var (
		open      bool
		fenceChar byte
		fenceLen  int
		isChart   bool
		start     int
		bodyStart int
	)
	off := 0
	for _, line := range strings.SplitAfter(md, "\n") {
		lineStart := off
		off += len(line)
		t := strings.TrimLeft(line, " ")
		if len(line)-len(t) > 3 {
			continue
		}
		t = strings.TrimRight(t, "\r\n")
		if open {
			if n := fenceRun(t, fenceChar); n >= fenceLen && strings.TrimRight(t[n:], " \t") == "" {
				if isChart && len(out) < MaxCharts {
					out = append(out, Block{Start: start, End: off, Body: md[bodyStart:lineStart]})
				}
				open = false
			}
			continue
		}
		for _, c := range []byte{'`', '~'} {
			n := fenceRun(t, c)
			if n < 3 {
				continue
			}
			info := strings.TrimSpace(t[n:])
			if c == '`' && strings.ContainsRune(info, '`') {
				break // not a fence: a backtick fence's info string can't hold a backtick
			}
			open, fenceChar, fenceLen, start, bodyStart = true, c, n, lineStart, off
			word, _, _ := strings.Cut(info, " ")
			isChart = strings.EqualFold(word, "chart")
		}
	}
	return out
}

// fenceRun is the number of c at the start of s.
func fenceRun(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// raw is a spec as written.
type raw struct {
	Type       string      `json:"type"`
	Title      string      `json:"title"`
	X          string      `json:"x"`
	Y          string      `json:"y"`
	Stacked    bool        `json:"stacked"`
	Horizontal bool        `json:"horizontal"`
	Series     []rawSeries `json:"series"`
}

type rawSeries struct {
	Name string              `json:"name"`
	Data [][]json.RawMessage `json:"data"`
}

// Parse reads and checks a chart's spec, the body of a Block. It returns an error that says what is
// wrong with an invalid one.
func Parse(body string) (*Spec, error) {
	if len(body) > MaxBytes {
		return nil, fmt.Errorf("the spec is %d bytes; the limit is %d", len(body), MaxBytes)
	}
	dec := json.NewDecoder(strings.NewReader(body))
	var r raw
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("not a valid spec: %v", err)
	}
	if dec.More() {
		return nil, errors.New("not a valid spec: more than one JSON value")
	}
	s := &Spec{Type: strings.ToLower(strings.TrimSpace(r.Type)), Title: r.Title, X: r.X, Y: r.Y,
		Stacked: r.Stacked, Horizontal: r.Horizontal}
	switch s.Type {
	case Line, Area, Bar, Scatter, Pie, Donut:
	case "":
		return nil, errors.New(`no "type"`)
	default:
		return nil, fmt.Errorf("unknown type %q", r.Type)
	}
	for _, t := range []string{s.Title, s.X, s.Y} {
		if utf8.RuneCountInString(t) > MaxTitle {
			return nil, fmt.Errorf("a title or axis label is longer than %d characters", MaxTitle)
		}
	}
	round := s.Type == Pie || s.Type == Donut
	switch {
	case len(r.Series) == 0:
		return nil, errors.New("no series")
	case len(r.Series) > MaxSeries:
		return nil, fmt.Errorf("%d series; the limit is %d", len(r.Series), MaxSeries)
	case round && len(r.Series) != 1:
		return nil, fmt.Errorf("a %s has one series", s.Type)
	}
	total := 0
	kind := ""
	for i, rs := range r.Series {
		if utf8.RuneCountInString(rs.Name) > MaxName {
			return nil, fmt.Errorf("series %d: a name longer than %d characters", i+1, MaxName)
		}
		if len(rs.Data) == 0 {
			return nil, fmt.Errorf("series %d has no data", i+1)
		}
		total += len(rs.Data)
		if total > MaxPoints {
			return nil, fmt.Errorf("more than %d points", MaxPoints)
		}
		if round && len(rs.Data) > MaxSlices {
			return nil, fmt.Errorf("%d slices; the limit is %d", len(rs.Data), MaxSlices)
		}
		ser := Series{Name: rs.Name, Points: make([]Point, 0, len(rs.Data))}
		for j, d := range rs.Data {
			p, k, err := point(d)
			if err != nil {
				return nil, fmt.Errorf("series %d, point %d: %v", i+1, j+1, err)
			}
			if kind == "" {
				kind = k
			} else if k != kind {
				return nil, fmt.Errorf("series %d, point %d: x is a %s, where earlier points have a %s", i+1, j+1, k, kind)
			}
			if round && p.Y < 0 {
				return nil, fmt.Errorf("series %d, point %d: a %s's values can't be negative", i+1, j+1, s.Type)
			}
			ser.Points = append(ser.Points, p)
		}
		s.Series = append(s.Series, ser)
	}
	s.Categories = kind == "category"
	return s, nil
}

// point reads one point, [x, y], and says whether its x is a "category" or a "number".
func point(d []json.RawMessage) (Point, string, error) {
	if len(d) != 2 {
		return Point{}, "", errors.New("a point is [x, y]")
	}
	var p Point
	kind := "number"
	if x := bytes.TrimSpace(d[0]); len(x) > 0 && x[0] == '"' {
		if err := json.Unmarshal(x, &p.Category); err != nil {
			return Point{}, "", err
		}
		if utf8.RuneCountInString(p.Category) > MaxName {
			return Point{}, "", fmt.Errorf("a category longer than %d characters", MaxName)
		}
		kind = "category"
	} else if err := number(x, &p.X); err != nil {
		return Point{}, "", errors.New("x is a string or a number")
	}
	if err := number(d[1], &p.Y); err != nil {
		return Point{}, "", errors.New("y is a number")
	}
	return p, kind, nil
}

// number reads a finite JSON number.
func number(b json.RawMessage, v *float64) error {
	if b = bytes.TrimSpace(b); len(b) == 0 || b[0] != '-' && (b[0] < '0' || b[0] > '9') {
		return errors.New("not a number") // null, which Unmarshal would leave as 0
	}
	if err := json.Unmarshal(b, v); err != nil {
		return err
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) {
		return errors.New("not a finite number")
	}
	return nil
}

// Without returns markdown with its chart blocks taken out, for a one-line excerpt of it.
func Without(md string) string {
	bs := Blocks(md)
	if len(bs) == 0 {
		return md
	}
	var b strings.Builder
	last := 0
	for _, bl := range bs {
		b.WriteString(md[last:bl.Start])
		last = bl.End
	}
	b.WriteString(md[last:])
	return b.String()
}
