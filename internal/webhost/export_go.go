//go:build js && !tinygo

package webhost

import "syscall/js"

// A build with Go has no plain exports, and its collector does not count references, so the host
// script calls these through syscall/js and the bytes are copied across rather than written in place.
func bufAddress(*[bufSize]byte) int { return 0 }

func init() {
	g := js.Global()
	g.Set("soloGoData", js.FuncOf(func(_ js.Value, args []js.Value) any {
		n := js.CopyBytesToGo(inBuf[:], args[0])
		delivered(n, args[1].Truthy())
		return nil
	}))
	g.Set("soloGoState", js.FuncOf(func(_ js.Value, args []js.Value) any {
		stateChanged(args[0].Int())
		return nil
	}))
	g.Set("soloGoOut", js.FuncOf(func(_ js.Value, args []js.Value) any {
		js.CopyBytesToJS(args[0], outBuf[:args[1].Int()])
		return nil
	}))
	g.Set("soloGoIn", js.FuncOf(func(_ js.Value, args []js.Value) any {
		js.CopyBytesToGo(outBuf[:], args[0])
		return nil
	}))
	g.Set("soloGoPage", js.FuncOf(func(js.Value, []js.Value) any {
		pageEvent()
		return nil
	}))
	g.Set("soloGoProbe", js.FuncOf(func(_ js.Value, args []js.Value) any {
		probeResult(args[0].Truthy())
		return nil
	}))
}
