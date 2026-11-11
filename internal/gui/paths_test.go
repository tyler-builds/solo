package gui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindPaths(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "brand", "preview-orbit.png")
	src := filepath.Join(dir, "repo", "internal", "gui", "chat.go")
	for _, p := range []string{img, src} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bases := []string{filepath.Join(dir, "repo")}
	cases := []struct {
		in   string
		want []string // the matched text of each path
		line int
	}{
		{"The previews are in " + img + ".", []string{img}, 0},
		{"see (" + img + ") and dark / light", []string{img}, 0},
		{"edit internal/gui/chat.go:30 next", []string{"internal/gui/chat.go:30"}, 30},
		{"internal/gui/chat.go#L12", []string{"internal/gui/chat.go#L12"}, 12},
		{"https://example.com" + img, nil, 0},
		{"missing " + filepath.Join(dir, "nope.png") + " and and/or", nil, 0},
	}
	for _, c := range cases {
		got := findPaths(c.in, bases)
		var texts []string
		for _, p := range got {
			texts = append(texts, c.in[p.start:p.end])
		}
		if strings.Join(texts, "|") != strings.Join(c.want, "|") {
			t.Errorf("findPaths(%q) = %q, want %q", c.in, texts, c.want)
			continue
		}
		if len(got) == 1 && got[0].line != c.line {
			t.Errorf("findPaths(%q) line = %d, want %d", c.in, got[0].line, c.line)
		}
	}
}

func TestLinkMarkdown(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "a_b", "x.png")
	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	url := fileURL(filePath{path: img})
	in := "Look at " + img + " and `" + img + "`.\n```\n" + img + "\n```\n[it](" + img + ")"
	got := linkMarkdown(in, nil)
	want := "Look at [" + escapeLinkText(img) + "](<" + url + ">) and [`" + img + "`](<" + url + ">).\n```\n" + img + "\n```\n[it](" + img + ")"
	if got != want {
		t.Errorf("linkMarkdown:\n got %q\nwant %q", got, want)
	}
	fp, ok := pathFromLink(url, nil)
	if !ok || fp.path != img {
		t.Errorf("pathFromLink(%q) = %q, %v", url, fp.path, ok)
	}
	if _, ok := pathFromLink("https://example.com/a/b", nil); ok {
		t.Error("a web link read as a path")
	}
}
func TestHardBreaks(t *testing.T) {
	cases := map[string]string{
		"one":                    "one",
		"a\nb":                   "a  \nb",
		"a \t\nb":                "a  \nb",
		"a\n\nb":                 "a\n\nb",
		"x\n```\nc\nd\n```\ny":   "x  \n```\nc\nd\n```\ny",
		"**A** one\n**B** two\n": "**A** one  \n**B** two\n",
	}
	for in, want := range cases {
		if got := hardBreaks(in); got != want {
			t.Errorf("hardBreaks(%q) = %q, want %q", in, got, want)
		}
	}
}
