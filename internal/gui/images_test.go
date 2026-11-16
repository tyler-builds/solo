package gui

import (
	"image/color"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tyler-builds/jamb/access"

	"github.com/tyler-builds/solo/internal/core"
)

func TestInlineImages(t *testing.T) {
	att := "/data/attachments/a.png"
	md := "Before ![before](" + att + ") and [![linked](" + att + ")](https://x) ![other](/tmp/b.png) `![code](" + att + ")`"
	want := "Before [![before](" + att + ")](" + att + ") and [![linked](" + att + ")](https://x) ![other](/tmp/b.png) `![code](" + att + ")`"
	if got := inlineImages(md, []string{att}); got != want {
		t.Errorf("inlineImages\n got %q\nwant %q", got, want)
	}
	if p, ok := shownImage(`C:/solo/attachments/a.png`, []string{`C:\solo\attachments\a.png`}); !ok || p != `C:\solo\attachments\a.png` {
		t.Error("a Windows attachment written with forward slashes isn't found")
	}
	if _, ok := shownImage("/tmp/b.png", []string{att}); ok {
		t.Error("an image that isn't the message's own is shown")
	}
}

// An agent's message shows its own images inline, and a click opens one full size. An image it names
// any other way isn't read.
func TestAgentMessageShowsItsImages(t *testing.T) {
	f := demoBackend()
	dir := t.TempDir()
	att := sampleScreenshot(dir, color.RGBA{47, 214, 193, 255})
	other := sampleScreenshot(dir, color.RGBA{90, 120, 230, 255})
	f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{
		Text: "The field ![icon](" + core.ImageDest(att) + ") no longer overlaps the button:\n\n![After the fix](" + core.ImageDest(att) + ")\n\n" +
			"![Elsewhere](" + filepath.ToSlash(other) + ")",
		Images: []string{att},
	})
	a, u := newUI(t, f)
	u.WaitFor("the image to load", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.imageReads, att)
	})
	u.Settle()
	f.mu.Lock()
	reads := slices.Clone(f.imageReads)
	f.mu.Unlock()
	if slices.Contains(reads, other) {
		t.Errorf("read an image the message doesn't own: %v", reads)
	}
	// On a line of its own, an image is a preview; among text, the markdown view draws it in a link.
	for i, open := range []func(){
		func() { u.ByRole(access.RoleLink, "icon").Click() },
		func() { u.ByRole(access.RoleButton, "View After the fix").Click() },
	} {
		a.lightbox.Set("")
		u.Settle()
		open()
		if a.lightbox.Get() != att {
			t.Errorf("%d: lightbox shows %q", i, a.lightbox.Get())
		}
	}
}

func TestImageLines(t *testing.T) {
	a, b := "/data/attachments/a.png", "/data/attachments/b.png"
	md := "Done.\n\n![Before](" + a + ") ![After](" + b + ")\n\n![Again](" + a + ")\n\nText ![inline](" + a + ") here.\n" +
		"![Not ours](/tmp/x.png)\n```\n![fenced](" + a + ")\n```"
	var got []string
	for _, p := range imageLines(md, []string{a, b}) {
		if p.shots == nil {
			got = append(got, "text:"+p.text)
			continue
		}
		var s []string
		for _, sh := range p.shots {
			s = append(s, sh.alt+"="+sh.path)
		}
		got = append(got, "row:"+strings.Join(s, ","))
	}
	want := []string{
		"text:Done.",
		"row:Before=" + a + ",After=" + b + ",Again=" + a,
		"text:Text ![inline](" + a + ") here.\n![Not ours](/tmp/x.png)\n```\n![fenced](" + a + ")\n```",
	}
	if !slices.Equal(got, want) {
		t.Errorf("imageLines\n got %q\nwant %q", got, want)
	}
	if imageLines("no images", nil) != nil {
		t.Error("parts for a message without images")
	}
}

// A one-line excerpt of a summary (the rail, a notification) leaves its screenshots out.
func TestExcerptsSkipImages(t *testing.T) {
	summary := "Moved the **email** field.\n\n![Before](/data/attachments/a.png) ![After](/data/attachments/b.png)\n"
	if got := lastLine(summary); got != "Moved the email field." {
		t.Errorf("lastLine %q", got)
	}
	if got := firstLine(stripMarkdown("![Before](/data/attachments/a.png)\n\nMoved it.")); got != "Moved it." {
		t.Errorf("first line %q", got)
	}
}
