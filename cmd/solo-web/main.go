//go:build js

// Command solo-web is Solo in a web page: the same UI as the desktop app, built with TinyGo (see
// scripts/web.sh). For now it runs over generated sample data; the page's query sizes it:
//
//	?threads=60&items=400&stream=1
package main

import (
	"strconv"
	"syscall/js"

	"github.com/tyler-builds/jamb/app"

	"github.com/tyler-builds/solo/internal/gui"
	"github.com/tyler-builds/solo/internal/sample"
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
	g := gui.New(sample.New(sample.Options{Threads: num("threads"), Items: num("items"), Stream: num("stream") > 0}))
	// Fingers, when the device's main pointer is coarse: a phone or a tablet.
	if mm := js.Global().Get("matchMedia"); mm.Type() == js.TypeFunction {
		g.Touch = js.Global().Call("matchMedia", "(pointer: coarse)").Get("matches").Bool()
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
		Root:       g.Root,
		Bindings:   gui.Bindings(),
		Chrome:     app.ChromeCustom,
	})
}
