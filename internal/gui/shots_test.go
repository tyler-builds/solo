package gui

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
)

// TestShots renders the main views to PNGs for design review:
//
//	SOLO_SHOTS=/tmp/shots go test ./internal/gui -run TestShots
func TestShots(t *testing.T) {
	out := os.Getenv("SOLO_SHOTS")
	if out == "" {
		t.Skip("set SOLO_SHOTS to an output directory")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	useBundledFont(t)
	light, dark := Themes()
	shots := []struct {
		name  string
		setup func(a *App, f *fakeBackend)
	}{
		{"main", func(a *App, f *fakeBackend) {}},
		{"thread", func(a *App, f *fakeBackend) { a.openThread("c3d4e5f6-forms", "conversation") }},
		{"changes", func(a *App, f *fakeBackend) { a.openThread("c3d4e5f6-forms", "changes") }},
		{"working", func(a *App, f *fakeBackend) { a.openThread("a1f3c9d2-oauth", "conversation") }},
		{"repo-filter", func(a *App, f *fakeBackend) { a.navSel.Set("repo:/home/dev/code/api") }},
		{"settings", func(a *App, f *fakeBackend) { a.settings.Set(true) }},
		{"empty", nil},
	}
	for _, theme := range []struct {
		name string
		t    *ui.Theme
	}{{"dark", dark}, {"light", light}} {
		for _, sh := range shots {
			if theme.name == "light" && sh.name != "main" && sh.name != "thread" {
				continue
			}
			f := demoBackend()
			if sh.setup == nil {
				f = &fakeBackend{items: map[string][]core.Item{}, repos: f.repos, settings: f.settings}
			}
			a := New(f)
			if sh.setup != nil {
				sh.setup(a, f)
			}
			u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(theme.t))
			for i := 0; i < 10; i++ {
				u.Advance(100 * time.Millisecond)
				time.Sleep(20 * time.Millisecond)
			}
			u.Settle()
			file, err := os.Create(filepath.Join(out, theme.name+"-"+sh.name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			png.Encode(file, u.Screenshot())
			file.Close()
			u.Close()
		}
	}
}

// useBundledFont makes fontconfig answer "sans-serif" with the bundled Inter, so the shots show the font
// the app registers (uitest can't register fonts itself). From uas.
func useBundledFont(t *testing.T) {
	dir, err := filepath.Abs("fonts")
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(t.TempDir(), "fonts.conf")
	xml := `<?xml version="1.0"?><!DOCTYPE fontconfig SYSTEM "fonts.dtd"><fontconfig>
<include ignore_missing="yes">/etc/fonts/fonts.conf</include><dir>` + dir + `</dir>
<alias binding="strong"><family>sans-serif</family><prefer><family>Inter</family></prefer></alias>
</fontconfig>`
	if err := os.WriteFile(conf, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FONTCONFIG_FILE", conf)
}
