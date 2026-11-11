package gui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain renders every test in the bundled Inter, the font the app registers, so layouts that
// measure text (textWidth) see the widths they will have in the app. fontconfig reads its
// configuration once per process, so it is set before any test draws.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "solo-gui-fonts")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	conf, err := bundledFontConfig(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("FONTCONFIG_FILE", conf)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// bundledFontConfig writes, in dir, a fontconfig file that answers "sans-serif" with the bundled
// Inter (uitest can't register fonts itself). From uas.
func bundledFontConfig(dir string) (string, error) {
	fonts, err := filepath.Abs("fonts")
	if err != nil {
		return "", err
	}
	conf := filepath.Join(dir, "fonts.conf")
	xml := `<?xml version="1.0"?><!DOCTYPE fontconfig SYSTEM "fonts.dtd"><fontconfig>
<include ignore_missing="yes">/etc/fonts/fonts.conf</include><dir>` + fonts + `</dir>
<alias binding="strong"><family>sans-serif</family><prefer><family>Inter</family></prefer></alias>
</fontconfig>`
	return conf, os.WriteFile(conf, []byte(xml), 0o644)
}
