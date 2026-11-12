//go:build js

// Command solo-web is Solo in a web page: the same UI as the desktop app, built with TinyGo (see
// scripts/web.sh), over a machine that runs Solo. The relay that serves the page carries its
// connection, which is encrypted from this page to the machine (docs/REMOTE.md).
//
// Served from this machine, with ?sample=1 it runs over generated data instead, for measuring the UI;
// the query then sizes it:
//
//	?sample=1&threads=60&items=400&stream=1
package main

import (
	"strconv"
	"syscall/js"

	"github.com/tyler-builds/jamb/app"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/gui"
	"github.com/tyler-builds/solo/internal/sample"
	"github.com/tyler-builds/solo/internal/webclient"
	"github.com/tyler-builds/solo/internal/webhost"
)

func main() {
	q := js.Global().Get("URLSearchParams").New(js.Global().Get("location").Get("search"))
	num := func(key string) int {
		v := q.Call("get", key)
		if v.Type() != js.TypeString {
			return 0
		}
		n, _ := strconv.Atoi(v.String())
		return n
	}
	var root func(*ui.Context) ui.Element
	switch {
	case num("sample") > 0 && local():
		// On a developer's machine only: a deployed relay's address shows nothing but the real thing.
		g := gui.New(sample.New(sample.Options{Threads: num("threads"), Items: num("items"), Stream: num("stream") > 0}))
		// Fingers, when the device's main pointer is coarse: a phone or a tablet.
		if mm := js.Global().Get("matchMedia"); mm.Type() == js.TypeFunction {
			g.Touch = js.Global().Call("matchMedia", "(pointer: coarse)").Get("matches").Bool()
		}
		root = g.Root
	case !webhost.Attach():
		panic("the page did not load solo.js")
	default:
		shell, err := webclient.New(webclient.Platform{
			Dial:       webhost.Dial,
			Host:       webhost.Host(),
			Random:     webhost.Random,
			Load:       webhost.Load,
			Store:      webhost.Store,
			Fragment:   webhost.Fragment(),
			DeviceName: webhost.DeviceName(),
			Touch:      webhost.Touch(),
			Reachable:  webhost.Reachable,
			Shown:      webhost.Shown,
		})
		if err != nil {
			panic(err.Error())
		}
		webhost.OnResume(shell.Resume)
		root = shell.Root
	}
	light, dark := gui.Themes()
	a := app.New()
	for _, f := range gui.Fonts() {
		a = a.Font("", f.Weight, f.Data)
	}
	a.Run(app.Window{
		Title:      "Solo",
		Width:      1440,
		Height:     900,
		LightTheme: light,
		DarkTheme:  dark,
		Root:       root,
		Bindings:   gui.Bindings(),
		Chrome:     app.ChromeCustom,
	})
}

// local reports whether the page is served from this machine.
func local() bool {
	h := js.Global().Get("location").Get("hostname").String()
	return h == "127.0.0.1" || h == "localhost"
}
