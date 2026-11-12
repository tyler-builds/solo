//go:build js

// Package webhost is what a web page built with TinyGo gets from its browser through Solo's host
// script (cmd/solo-web/web/solo.js): a WebSocket, a place to keep small values, random bytes, and word
// of the page being shown again.
//
// Messages arrive through functions the script calls with a length, after copying the bytes into a
// buffer of the app's memory, the way jamb delivers frames and input. A listener made with syscall/js
// would make a reference to a JavaScript value for every message, and TinyGo collects after every 32
// of them, for a time that grows with the heap: a stream of small messages would stop the page many
// times a second. And a JavaScript exception would end the app, which has no way to recover from a
// panic, so everything that can throw is caught in the script.
package webhost

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"syscall/js"
	"time"

	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/wire"
)

// The buffers the host script reads and writes. A message longer than a buffer goes in pieces.
const bufSize = 64 * 1024

var (
	inBuf, outBuf [bufSize]byte
	host          js.Value
	conn          *Conn // the page's one connection
	pageEvents    = make(chan struct{}, 1)
	probes        = make(chan bool, 1)
)

// ErrClosed reports a connection that closed, or never opened.
var ErrClosed = errors.New("the connection is closed")

// Attach finds the host script. It reports false when the page didn't load it.
func Attach() bool {
	host = js.Global().Get("soloHost")
	if host.Type() != js.TypeObject {
		return false
	}
	host.Call("buffers", bufAddress(&inBuf), bufAddress(&outBuf), bufSize)
	return true
}

// Host is the page's host, with its port if it has one, and Secure whether the page came over HTTPS.
func Host() string { return js.Global().Get("location").Get("host").String() }

func secure() bool { return js.Global().Get("location").Get("protocol").String() == "https:" }

// Conn is the page's connection. It is a wire.Conn.
type Conn struct {
	in     chan []byte   // whole messages, in order
	state  chan int      // 0 when it opens; its close code when it closes
	closed chan struct{} // closed when the connection is
	code   int           // why it closed, once it has
}

var _ wire.Conn = (*Conn)(nil)

// Dial opens a WebSocket to the relay the page came from. A page has one at a time: an earlier one is
// closed.
func Dial(ctx context.Context, path string, token []byte) (wire.Conn, error) {
	if old := conn; old != nil {
		old.shut(0)
	}
	scheme := "ws://"
	if secure() {
		scheme = "wss://"
	}
	c := &Conn{in: make(chan []byte, 256), state: make(chan int, 4), closed: make(chan struct{})}
	conn = c
	host.Call("open", scheme+Host()+path, device.Subprotocol, base64.RawURLEncoding.EncodeToString(token))
	select {
	case code := <-c.state:
		if code != 0 {
			c.shut(code)
			return nil, c.err()
		}
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	}
	go func() {
		select {
		case code := <-c.state: // the next change of state is its closing
			c.shut(code)
		case <-c.closed:
		}
	}()
	return c, nil
}

func (c *Conn) shut(code int) {
	select {
	case <-c.closed:
	default:
		c.code = code
		close(c.closed)
	}
}

func (c *Conn) err() error {
	if c.code >= 1000 && c.code != 1006 {
		return &device.CloseError{Code: c.code}
	}
	return ErrClosed
}

// Read returns the next message, or why the connection closed once its messages are read.
func (c *Conn) Read() ([]byte, error) {
	select {
	case m := <-c.in:
		return m, nil
	default:
	}
	select {
	case m := <-c.in:
		return m, nil
	case <-c.closed:
		return nil, c.err()
	}
}

// Write sends one message. Call it from one goroutine at a time.
func (c *Conn) Write(m []byte) error {
	select {
	case <-c.closed:
		return c.err()
	default:
	}
	if conn != c {
		return ErrClosed
	}
	for {
		n := copy(outBuf[:], m)
		m = m[n:]
		last := 0
		if len(m) == 0 {
			last = 1
		}
		host.Call("send", n, last) // numbers only: no reference to a JavaScript value is made
		if last == 1 {
			return nil
		}
	}
}

// Close closes the connection.
func (c *Conn) Close() error {
	if conn == c {
		host.Call("close")
	}
	c.shut(0)
	return nil
}

// delivered takes the n bytes the host script put in inBuf: a piece of a message, the last when final.
// The script calls it in no goroutine, so it must not block: a full queue closes the connection rather
// than wait, and connecting again catches up.
func delivered(n int, final bool) {
	if conn == nil || n < 0 || n > bufSize {
		return
	}
	if !final {
		return // the script sends a message whole, or not at all
	}
	m := append([]byte(nil), inBuf[:n]...)
	select {
	case conn.in <- m:
	default:
		// The reader is too far behind. The script closes the socket and reports it, which ends the
		// connection here; connecting again catches up.
		host.Call("dropped")
	}
}

// stateChanged takes the connection's opening (0) or its close code.
func stateChanged(code int) {
	if conn == nil {
		return
	}
	select {
	case conn.state <- code:
	default:
	}
}

func pageEvent() {
	select {
	case pageEvents <- struct{}{}:
	default:
	}
}

func probeResult(ok bool) {
	select {
	case probes <- ok:
	default:
	}
}

// OnResume calls f whenever the page is shown again or the network comes back.
func OnResume(f func()) {
	go func() {
		for range pageEvents {
			f()
		}
	}()
}

// Reachable reports whether the relay answers, when a connection to it failed and the browser won't say
// why.
func Reachable() bool {
	select {
	case <-probes:
	default:
	}
	host.Call("probe")
	select {
	case ok := <-probes:
		return ok
	case <-time.After(10 * time.Second):
		return false
	}
}

// Load returns a value kept on this device, or "".
func Load(name string) string {
	v := host.Call("load", name)
	if v.Type() != js.TypeString {
		return ""
	}
	return v.String()
}

// Store keeps a value on this device; "" forgets it.
func Store(name, value string) { host.Call("store", name, value) }

// Fragment returns what followed "#" in the page's address and takes it out of the address, so that a
// pairing code doesn't stay in the address bar or the history.
func Fragment() string {
	v := host.Call("takeFragment")
	if v.Type() != js.TypeString {
		return ""
	}
	return v.String()
}

// DeviceName is a name for this device, from what the browser says it is.
func DeviceName() string {
	v := host.Call("deviceName")
	if v.Type() != js.TypeString {
		return ""
	}
	return v.String()
}

// Touch reports whether the device's main pointer is a finger.
func Touch() bool { return host.Call("touch").Truthy() }

// Random is the browser's random source, with nothing to fall back on: when the browser has none, Read
// fails, and the page must not make keys.
var Random io.Reader = randomReader{}

type randomReader struct{}

func (randomReader) Read(p []byte) (int, error) {
	for done := 0; done < len(p); {
		n := min(len(p)-done, bufSize)
		// The script fills the outgoing buffer, which nothing else is using: Write is synchronous.
		if !host.Call("random", n).Truthy() {
			return done, errors.New("this browser has no source of random numbers")
		}
		copy(p[done:], outBuf[:n])
		for i := range outBuf[:n] {
			outBuf[i] = 0
		}
		done += n
	}
	return len(p), nil
}

// Shown tells the page's script what is on screen, which it keeps in soloHost.shown.
func Shown(screen, digits, link string) { host.Call("show", screen, digits, link) }
