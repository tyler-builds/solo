//go:build js && tinygo

package webhost

import "unsafe"

func bufAddress(b *[bufSize]byte) int { return int(uintptr(unsafe.Pointer(&b[0]))) }

// The functions the host script calls. They are plain exports, which run on the caller's stack, in no
// goroutine, and leave nothing to collect; the script runs the app's scheduler afterwards.

//export solo_ws_data
func wsData(n, final int32) { delivered(int(n), final != 0) }

//export solo_ws_state
func wsState(code int32) { stateChanged(int(code)) }

//export solo_page_event
func pageEventExport() { pageEvent() }

//export solo_probe_result
func probeResultExport(ok int32) { probeResult(ok != 0) }
