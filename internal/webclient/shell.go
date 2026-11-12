// Package webclient is Solo as a device sees it: the screens for pairing with a machine and connecting
// to it, around the app's own UI once it is connected. The web page runs it (cmd/solo-web); its tests
// run it against a real machine.
package webclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/flynn/noise"

	"github.com/tyler-builds/solo/internal/gui"
	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/rpc"
	"github.com/tyler-builds/solo/internal/remote/secure"
)

// Platform is what the shell needs of where it runs: a browser, or a test.
type Platform struct {
	// Dial opens a connection through the relay the page was served from, and Host is that relay's
	// host, with its port if it has one.
	Dial device.Dialer
	Host string
	// Random is the system's random source. The page stops if the browser has none.
	Random io.Reader
	// Load and Store keep small values on the device, by name; Store with "" forgets one.
	Load  func(name string) string
	Store func(name, value string)
	// Fragment is what followed "#" in the page's address when it opened, already taken out of the
	// address: a pairing link's code.
	Fragment string
	// DeviceName is what to call this device at the machine, until the user says otherwise.
	DeviceName string
	// Touch: the device is used with fingers.
	Touch bool
	// Reachable reports whether the relay answers at all. A browser doesn't say why a connection was
	// refused, so this tells a relay that turned the device away from one that can't be reached. Nil
	// takes it as unreachable.
	Reachable func() bool
	// Shown is told what the page shows whenever that changes: the screen's name, the pairing digits
	// while they are on screen, and the connection's state. A page is one canvas, so this is how
	// anything outside it, a test above all, can tell what is on it. Nil for nobody.
	Shown func(screen, digits, link string)
	// Retry is the first wait before connecting again (default 1 second); tests shorten it.
	Retry time.Duration
}

// Names of what the device keeps.
const (
	storeKey      = "solo.key"      // this device's key pair
	storeMachines = "solo.machines" // the machines it is paired with
	storeCurrent  = "solo.current"  // the machine it last used
)

type screenKind int

const (
	screenWelcome  screenKind = iota // not paired with anything
	screenConfirm                    // a pairing link was opened: pair?
	screenPairing                    // asking; the digits are shown
	screenMachines                   // the machines this device knows
	screenMain                       // a machine's UI, or the wait for it
)

// screen is what the shell shows.
type screen struct {
	kind   screenKind
	digits string // screenPairing
	err    string // what went wrong on the way here
}

// link is where the shell stands with the machine it shows.
type link struct {
	state   linkState
	problem string
}

type linkState int

const (
	linkConnecting linkState = iota
	linkReady                // connected, and showing the machine as it is
	linkOffline              // the machine isn't connected to the relay
	linkLost                 // the relay can't be reached, or the connection keeps dropping
	linkEnded                // the machine turned this device away; nothing is retried
)

// Shell is the web client.
type Shell struct {
	p      Platform
	origin string
	key    noise.DHKey

	mu       sync.Mutex
	post     func(func())
	machines []device.Machine
	current  *session
	pairing  *device.Pairing
	code     *secure.PairingCode
	state    *state
	told     [3]string // what Platform.Shown was last told (UI thread)
}

// session is the shell's connection to one machine, and that machine's UI.
type session struct {
	m      device.Machine
	client *rpc.Client
	app    *gui.App
	stop   chan struct{}
	wake   chan struct{} // connect now, rather than when the wait is over
	once   sync.Once
	synced bool // it has shown the machine once: the UI stays up over a lost connection
}

// New loads what the device keeps and decides what to show first.
func New(p Platform) (*Shell, error) {
	if p.Retry <= 0 {
		p.Retry = time.Second
	}
	s := &Shell{p: p, origin: device.Origin(p.Host), state: newState()}
	if err := s.loadKey(); err != nil {
		return nil, err
	}
	if raw := p.Load(storeMachines); raw != "" {
		// What can't be read is as good as not there: the device pairs again.
		json.Unmarshal([]byte(raw), &s.machines)
	}
	if frag, ok := strings.CutPrefix(p.Fragment, "p="); ok {
		code, err := secure.ParsePairingCode(frag)
		switch {
		case err != nil:
			s.state.screen.Set(screen{kind: s.home(), err: "That pairing link isn't complete. Scan the code at the machine again."})
		case !time.Now().Before(code.Expires):
			s.state.screen.Set(screen{kind: s.home(), err: device.ErrExpired.Error()})
		default:
			s.code = &code
			s.state.screen.Set(screen{kind: screenConfirm})
		}
	} else {
		s.state.screen.Set(screen{kind: s.home()})
	}
	if s.state.screen.Peek().kind == screenMain {
		s.open(s.pick())
	}
	return s, nil
}

// home is the screen to show when nothing else is going on.
func (s *Shell) home() screenKind {
	if len(s.machines) == 0 {
		return screenWelcome
	}
	return screenMain
}

// pick returns the machine to open: the one last used, or the first.
func (s *Shell) pick() device.Machine {
	last := s.p.Load(storeCurrent)
	for _, m := range s.machines {
		if m.ID == last {
			return m
		}
	}
	return s.machines[0]
}

// loadKey reads this device's key pair, or makes it the first time.
func (s *Shell) loadKey() error {
	if raw, err := base64.RawURLEncoding.DecodeString(s.p.Load(storeKey)); err == nil && len(raw) == 2*secure.KeySize {
		s.key = noise.DHKey{Private: raw[:secure.KeySize], Public: raw[secure.KeySize:]}
		return nil
	}
	key, err := secure.NewKey(s.p.Random)
	if err != nil {
		return errors.New("this browser has no source of random numbers, which Solo needs to keep the connection private")
	}
	s.key = key
	s.p.Store(storeKey, base64.RawURLEncoding.EncodeToString(append(slices.Clone(key.Private), key.Public...)))
	return nil
}

func (s *Shell) saveMachines() {
	raw, _ := json.Marshal(s.machines)
	s.p.Store(storeMachines, string(raw))
}

// onUI runs f on the UI thread, once the window is up.
func (s *Shell) onUI(f func()) {
	s.mu.Lock()
	post := s.post
	s.mu.Unlock()
	if post != nil {
		post(f)
	} else {
		f() // before the first frame nothing else reads the signals
	}
}

// Resume is called when the page is shown again or the network came back: a connection that is waiting
// to be tried is tried now.
func (s *Shell) Resume() {
	s.mu.Lock()
	cur := s.current
	s.mu.Unlock()
	if cur != nil {
		select {
		case cur.wake <- struct{}{}:
		default:
		}
	}
}

// Close ends the shell's connection.
func (s *Shell) Close() {
	s.mu.Lock()
	cur := s.current
	s.current = nil
	s.mu.Unlock()
	if cur != nil {
		cur.end()
	}
}

func (c *session) end() {
	c.once.Do(func() {
		close(c.stop)
		c.app.Close()
	})
}

// pair asks the machine the code names to pair with this device, under the name the user gave.
func (s *Shell) pair(name string) {
	code := s.code
	if code == nil {
		return
	}
	name = device.CleanName(name)
	if name == "" {
		name = "A device"
	}
	s.state.screen.Set(screen{kind: screenPairing})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		p, err := device.Pair(ctx, s.p.Dial, s.origin, *code, s.key, name, s.p.Random)
		cancel()
		if err != nil {
			s.failPairing(pairingProblem(err))
			return
		}
		s.mu.Lock()
		s.pairing = p
		s.mu.Unlock()
		s.onUI(func() { s.state.screen.Set(screen{kind: screenPairing, digits: p.Digits}) })
		m, err := p.Wait()
		s.mu.Lock()
		mine := s.pairing == p
		s.pairing = nil
		s.mu.Unlock()
		if !mine {
			return // cancelled here
		}
		if err != nil {
			s.failPairing(pairingProblem(err))
			return
		}
		s.onUI(func() { s.paired(m) })
	}()
}

func pairingProblem(err error) string {
	var closed *device.CloseError
	switch {
	case errors.Is(err, device.ErrDenied):
		return "The request was turned down at the machine, or the pairing was cancelled there."
	case errors.As(err, &closed) && closed.Code == device.CloseOffline:
		return "The machine isn't connected right now. Check that Solo is open there, then scan a new code."
	case errors.As(err, &closed) && closed.Code == device.CloseLimit:
		return "Too many devices are trying to pair at once. Make a new code at the machine."
	}
	return "Pairing didn't finish. The code may have expired or been used: make a new one at the machine and scan it."
}

func (s *Shell) failPairing(msg string) {
	s.onUI(func() {
		s.code = nil
		s.state.screen.Set(screen{kind: s.home(), err: msg})
		if s.home() == screenMain && s.current == nil {
			s.open(s.pick())
		}
	})
}

// cancelPairing gives a pairing up.
func (s *Shell) cancelPairing() {
	s.mu.Lock()
	p := s.pairing
	s.pairing = nil
	s.mu.Unlock()
	if p != nil {
		p.Cancel()
	}
	s.code = nil
	s.state.screen.Set(screen{kind: s.home()})
	if s.home() == screenMain && s.current == nil {
		s.open(s.pick())
	}
}

// paired keeps a machine this device was just paired with, and opens it. UI thread.
func (s *Shell) paired(m device.Machine) {
	s.code = nil
	// A machine this device knows, under another key, is not that machine: a code made to look like
	// it must not take its place in the list.
	for _, known := range s.machines {
		if known.ID == m.ID && !bytes.Equal(known.Key, m.Key) {
			s.state.screen.Set(screen{kind: s.home(), err: "That code is not from the machine it claims to be: this device already knows that machine by another key. Nothing was changed. If the machine was set up anew, forget it here first."})
			if s.home() == screenMain && s.current == nil {
				s.open(s.pick())
			}
			return
		}
	}
	s.machines = append(slices.DeleteFunc(s.machines, func(x device.Machine) bool { return x.ID == m.ID }), m)
	s.saveMachines()
	s.state.machines.Set(slices.Clone(s.machines))
	s.open(m)
}

// forget drops a machine: its keys are deleted from this device. UI thread.
func (s *Shell) forget(id string) {
	s.machines = slices.DeleteFunc(s.machines, func(x device.Machine) bool { return x.ID == id })
	s.saveMachines()
	s.state.machines.Set(slices.Clone(s.machines))
	s.mu.Lock()
	cur := s.current
	if cur != nil && cur.m.ID == id {
		s.current = nil
	} else {
		cur = nil
	}
	s.mu.Unlock()
	if cur != nil {
		cur.end()
	}
	if len(s.machines) == 0 {
		s.state.screen.Set(screen{kind: screenWelcome})
	} else {
		s.state.screen.Set(screen{kind: screenMachines})
	}
}

// open shows a machine: it starts connecting to it, and its UI appears when it has synced.
func (s *Shell) open(m device.Machine) {
	s.mu.Lock()
	old := s.current
	s.mu.Unlock()
	if old != nil && old.m.ID == m.ID {
		s.state.screen.Set(screen{kind: screenMain})
		return
	}
	if old != nil {
		old.end()
	}
	c := &session{m: m, stop: make(chan struct{}), wake: make(chan struct{}, 1)}
	c.client = rpc.NewClient(rpc.ClientOptions{Random: s.p.Random, Limit: itemLimit})
	c.app = gui.New(c.client)
	c.app.Touch = s.p.Touch
	c.app.RemoteImages = true
	c.app.ItemLimit = itemLimit
	c.app.Machine = m.Name
	c.app.OnMachine = func() { s.state.screen.Set(screen{kind: screenMachines}) }
	s.mu.Lock()
	s.current = c
	s.mu.Unlock()
	s.p.Store(storeCurrent, m.ID)
	s.state.link.Set(link{state: linkConnecting})
	s.state.shown.Set(m.ID)
	s.state.screen.Set(screen{kind: screenMain})
	go s.run(c)
}

// itemLimit is how many of a transcript's newest items a page holds: fewer than the desktop's 400,
// since a page's memory and its pauses grow with what it holds.
const itemLimit = 150

// run keeps a connection to a session's machine until the session ends.
func (s *Shell) run(c *session) {
	wait := s.p.Retry
	first := true
	closedEarly := 0 // connections the machine closed before answering, in a row
	refused := 0     // connections a relay that is there didn't let in, in a row
	set := func(l link) {
		s.onUI(func() {
			s.mu.Lock()
			mine := s.current == c
			s.mu.Unlock()
			if mine {
				s.state.link.Set(l)
			}
		})
	}
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		began := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		l, err := device.Connect(ctx, s.p.Dial, s.origin, c.m, s.key, s.p.Random)
		cancel()
		var problem link
		if err == nil {
			closedEarly, refused = 0, 0
			done := make(chan error, 1)
			go func() { done <- c.client.Run(l) }()
			// The UI shows once the machine has been read in full; until then it would be empty.
			synced := make(chan struct{})
			go func() {
				for !c.client.Status().Synced {
					select {
					case <-done:
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
				close(synced)
			}()
			select {
			case <-synced:
				c.synced = true
				set(link{state: linkReady})
				select {
				case err = <-done:
				case <-c.stop:
					l.Close()
					return
				}
			case err = <-done:
			case <-c.stop:
				l.Close()
				return
			}
			var end *rpc.EndError
			if errors.As(err, &end) {
				switch end.Code {
				case rpc.CodeDenied:
					set(link{state: linkEnded, problem: "This device was removed at the machine. To use Solo here again, pair it again there."})
					return
				case rpc.CodeVersion:
					set(link{state: linkEnded, problem: end.Message})
					return
				}
			}
			problem = link{state: linkLost, problem: "The connection was lost."}
		} else {
			var closed *device.CloseError
			switch {
			case errors.As(err, &closed) && closed.Code == device.CloseOffline:
				closedEarly = 0
				problem = link{state: linkOffline, problem: c.m.Name + " isn't connected. Solo has to be open there, with remote access on."}
			case errors.As(err, &closed) && closed.Code == device.CloseNormal:
				// The relay let this device in and the machine hung up without a word: it doesn't
				// know this device. Once could be a restart in mid-handshake; three times is an answer.
				if closedEarly++; closedEarly >= 3 {
					set(link{state: linkEnded, problem: "This device is no longer paired with " + c.m.Name + ". Pair it again at the machine."})
					return
				}
				problem = link{state: linkLost, problem: "The machine didn't answer."}
			default:
				// The relay is there and didn't let this device in: its voucher is no longer good,
				// as after the relay was set up again. Three times, and it isn't a blip.
				if s.p.Reachable != nil && s.p.Reachable() {
					if refused++; refused >= 3 {
						set(link{state: linkEnded, problem: "The relay no longer lets this device in. Pair it again at " + c.m.Name + "."})
						return
					}
				} else {
					refused = 0
				}
				problem = link{state: linkLost, problem: "The relay can't be reached. Check this device's connection."}
			}
		}
		if time.Since(began) > 30*time.Second {
			wait = s.p.Retry // it was up for a while: start the waits over
			first = true
		}
		set(problem)
		pause := wait
		if first {
			pause, first = wait/4, false // one quick try: most drops are a blip
		} else {
			wait = min(wait*2, 30*s.p.Retry) // 30 seconds as shipped
		}
		select {
		case <-c.stop:
			return
		case <-c.wake:
		case <-time.After(pause):
		}
	}
}
