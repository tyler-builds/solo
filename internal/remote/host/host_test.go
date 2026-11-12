//go:build !js

package host

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flynn/noise"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/relaytest"
	"github.com/tyler-builds/solo/internal/remote/rpc"
	"github.com/tyler-builds/solo/internal/remote/secure"
	"github.com/tyler-builds/solo/internal/remote/wire"
	"github.com/tyler-builds/solo/internal/sample"
)

// world is a relay, a machine connected to it, and what a test needs to play devices.
type world struct {
	t       *testing.T
	relay   *relaytest.Relay // nil when the relay is the real Worker (worker_test.go)
	url     string           // the relay's address
	origin  string
	remote  *Remote
	backend *sample.Backend
	dir     string
	secrets MemSecrets
	dial    device.Dialer
}

// worker, when set, is the address of the real Worker running locally, and workerSeed the seed of the
// one machine it lets in: the tests that only need a relay then run against it (worker_test.go).
var (
	worker     string
	workerSeed string
)

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, backend: sample.New(sample.Options{Threads: 8, Items: 30}), dir: t.TempDir(), secrets: MemSecrets{}}
	if worker != "" {
		w.url, w.secrets = worker, MemSecrets{seedName: workerSeed}
	} else {
		w.relay = relaytest.New()
		t.Cleanup(w.relay.Close)
		w.url = w.relay.URL()
	}
	w.origin = device.Origin(strings.TrimPrefix(strings.TrimPrefix(w.url, "http://"), "https://"))
	w.dial = relaytest.Dialer(w.url)
	w.open()
	key, err := w.remote.RelayKey()
	if err != nil {
		t.Fatal(err)
	}
	if w.relay != nil {
		raw, _ := base64.RawURLEncoding.DecodeString(key)
		w.relay.Allow(raw)
	}
	if err := w.remote.SetRelay(w.url); err != nil {
		t.Fatal(err)
	}
	w.online()
	return w
}

// local skips a test that needs to control the relay when the relay is the real Worker.
func (w *world) local() *relaytest.Relay {
	w.t.Helper()
	if w.relay == nil {
		w.t.Skip("needs a relay the test controls")
	}
	return w.relay
}

func (w *world) open() {
	w.t.Helper()
	cfg := Config{Dir: w.dir, Backend: w.backend, Secrets: w.secrets, Name: "box",
		Retry: 10 * time.Millisecond, FlushEvery: 5 * time.Millisecond}
	if worker != "" {
		cfg.HTTP = relaytest.LocalHTTP
	}
	r, err := New(cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	w.remote = r
	w.t.Cleanup(r.Close)
}

func (w *world) wait(what string, ok func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s (state %+v)", what, w.remote.State())
		}
		time.Sleep(3 * time.Millisecond)
	}
}

func (w *world) online() {
	w.t.Helper()
	w.wait("the machine to reach its relay", func() bool {
		return w.remote.State().Connection == Online && (w.relay == nil || w.relay.Machines() > 0)
	})
}

// offer makes a pairing code, and waits until the relay knows its ticket: a person takes seconds to
// scan a code, a test no time at all.
func (w *world) offer() Pairing {
	w.t.Helper()
	p, err := w.remote.StartPairing(false)
	if err != nil {
		w.t.Fatal(err)
	}
	if w.relay != nil {
		w.wait("the relay to hold the ticket", func() bool { return w.relay.Tickets() > 0 })
	} else {
		time.Sleep(150 * time.Millisecond)
	}
	return p
}

// open opens pairing without a code, and waits until the relay knows.
func (w *world) open2() {
	w.t.Helper()
	if _, err := w.remote.StartPairing(true); err != nil {
		w.t.Fatal(err)
	}
	if w.relay != nil {
		w.wait("the relay to open pairing", w.relay.Open)
	} else {
		time.Sleep(150 * time.Millisecond)
	}
}

func newKey(t *testing.T) noise.DHKey {
	t.Helper()
	k, err := secure.NewKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// code reads the pairing code out of the link the machine shows.
func (w *world) code(link string) secure.PairingCode {
	w.t.Helper()
	_, frag, ok := strings.Cut(link, "#p=")
	if !ok || !strings.HasPrefix(link, w.url+"/") {
		w.t.Fatalf("pairing link %q", link)
	}
	code, err := secure.ParsePairingCode(frag)
	if err != nil {
		w.t.Fatal(err)
	}
	return code
}

// pair pairs a new device and returns its key and what it keeps of the machine.
func (w *world) pair(name string, readOnly bool) (noise.DHKey, device.Machine) {
	w.t.Helper()
	p := w.offer()
	key := newKey(w.t)
	req, err := device.Pair(context.Background(), w.dial, w.origin, w.code(p.Link), key, name, rand.Reader)
	if err != nil {
		w.t.Fatal(err)
	}
	w.wait("the request", func() bool { return len(w.remote.State().Requests) == 1 })
	if _, err := w.remote.Approve(req.Digits, readOnly); err != nil {
		w.t.Fatal(err)
	}
	m, err := req.Wait()
	if err != nil {
		w.t.Fatal(err)
	}
	return key, m
}

// session connects a paired device and returns its client, synced, and its Run's result.
func (w *world) session(key noise.DHKey, m device.Machine, c *rpc.Client) chan error {
	w.t.Helper()
	link, err := device.Connect(context.Background(), w.dial, w.origin, m, key, rand.Reader)
	if err != nil {
		w.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(link) }()
	w.wait("the device to sync", func() bool { return c.Status().Synced })
	return done
}

// The whole path: a device pairs with the digits typed at the machine, connects, sees the machine's
// threads and acts on them; the audit log says who did what.
func TestPairAndUse(t *testing.T) {
	w := newWorld(t)
	p := w.offer()
	if time.Until(p.Expires) > 5*time.Minute+time.Second || time.Until(p.Expires) < 4*time.Minute {
		t.Errorf("the code lasts %v", time.Until(p.Expires))
	}
	key := newKey(t)
	req, err := device.Pair(context.Background(), w.dial, w.origin, w.code(p.Link), key, "Tyler's \u202ephone\x07", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w.wait("the request", func() bool { return len(w.remote.State().Requests) == 1 })
	if len(req.Digits) != 6 {
		t.Fatalf("digits %q", req.Digits)
	}
	// Wrong digits approve nobody, and the request stays.
	wrong := "000000"
	if req.Digits == wrong {
		wrong = "111111"
	}
	if _, err := w.remote.Approve(wrong, false); err == nil {
		t.Fatal("wrong digits approved a device")
	}
	if n := len(w.remote.State().Devices); n != 0 {
		t.Fatalf("%d devices after wrong digits", n)
	}
	d, err := w.remote.Approve(req.Digits[:3]+" "+req.Digits[3:], false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := req.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "box" || m.ID != w.remote.State().MachineID || d.Name != "Tyler's phone" {
		t.Errorf("paired: %+v / %+v", m, d)
	}
	if st := w.remote.State(); st.Pairing != nil || len(st.Devices) != 1 {
		t.Errorf("after pairing: %+v", st)
	}
	// The code is spent: it opens nothing again, even at the relay.
	if _, err := device.Pair(context.Background(), w.dial, w.origin, w.code(p.Link), newKey(t), "again", rand.Reader); err == nil {
		t.Error("a spent code was accepted")
	}

	c := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
	w.session(key, m, c)
	if got, want := len(c.Threads()), len(w.backend.Threads()); got != want || got == 0 {
		t.Fatalf("the device sees %d threads, the machine has %d", got, want)
	}
	if st := c.Status(); st.Host.Name != "box" || st.ReadOnly {
		t.Errorf("status %+v", st)
	}
	w.wait("the device to show as online", func() bool {
		st := w.remote.State()
		return len(st.Devices) == 1 && st.Devices[0].Online && !st.Devices[0].LastSeen.IsZero()
	})
	if err := c.SendToOrchestrator(context.Background(), "hello from the phone"); err != nil {
		t.Errorf("send: %v", err)
	}
	if err := c.UpdateSettings(func(s *core.Settings) { s.ClaudePath = "/tmp/evil" }); err == nil {
		t.Error("a device set the machine's Claude path")
	}
	log, err := os.ReadFile(filepath.Join(w.dir, "remote-audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"paired"`, `"event":"send"`, `"name":"Tyler's phone"`, `"event":"updateSettings"`} {
		if !strings.Contains(string(log), want) {
			t.Errorf("the audit log lacks %s:\n%s", want, log)
		}
	}
	for _, f := range []string{"remote.json", "remote-audit.log"} {
		if fi, err := os.Stat(filepath.Join(w.dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi.Mode(), err)
		}
	}
}

// Someone else who got the pairing link can ask too, but shows other digits: the digits typed pick the
// device in the person's hand, and the other is turned down.
func TestStolenPairingLink(t *testing.T) {
	w := newWorld(t)
	p := w.offer()
	code := w.code(p.Link)
	ctx := context.Background()
	mine, err := device.Pair(ctx, w.dial, w.origin, code, newKey(t), "My phone", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := device.Pair(ctx, w.dial, w.origin, code, newKey(t), "My phone", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w.wait("both requests", func() bool { return len(w.remote.State().Requests) == 2 })
	if mine.Digits == theirs.Digits {
		t.Skip("the two handshakes drew the same six digits (one in a million)")
	}
	result := make(chan error, 1)
	go func() { _, err := theirs.Wait(); result <- err }()
	if _, err := w.remote.Approve(mine.Digits, false); err != nil {
		t.Fatal(err)
	}
	if _, err := mine.Wait(); err != nil {
		t.Fatalf("my device: %v", err)
	}
	if err := <-result; !errors.Is(err, device.ErrDenied) {
		t.Errorf("the other device was told %v", err)
	}
	if n := len(w.remote.State().Devices); n != 1 {
		t.Errorf("%d devices paired", n)
	}
}

// A code can't be guessed at: a handful of wrong digits ends it.
func TestWrongDigitsEndThePairing(t *testing.T) {
	w := newWorld(t)
	p := w.offer()
	req, err := device.Pair(context.Background(), w.dial, w.origin, w.code(p.Link), newKey(t), "Phone", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w.wait("the request", func() bool { return len(w.remote.State().Requests) == 1 })
	for i := 0; i < maxWrongCodes; i++ {
		guess := "12345" + string(rune('0'+i))
		if guess == req.Digits {
			guess = "999999"
		}
		if _, err := w.remote.Approve(guess, false); err == nil {
			t.Fatal("a guess was approved")
		}
	}
	if w.remote.State().Pairing != nil {
		t.Error("the pairing went on after the wrong codes")
	}
	if _, err := w.remote.Approve(req.Digits, false); err == nil {
		t.Error("the right digits were approved after the pairing ended")
	}
	if _, err := req.Wait(); !errors.Is(err, device.ErrDenied) {
		t.Errorf("the device was told %v", err)
	}
}

// What doesn't hold the code gets nowhere: a wrong ticket is stopped at the relay, a wrong secret at
// the machine, and nothing is shown to the person.
func TestPairingNeedsTheCode(t *testing.T) {
	w := newWorld(t)
	p := w.offer()
	code := w.code(p.Link)
	ctx := context.Background()

	forged := code
	forged.Secret[0] ^= 1 // another secret: another ticket, which the relay doesn't know
	if _, err := device.Pair(ctx, w.dial, w.origin, forged, newKey(t), "x", rand.Reader); err == nil {
		t.Error("a code with another secret reached the machine")
	}
	// The right ticket with a handshake for another relay's origin: the relay lets it in, the machine's
	// handshake fails.
	if _, err := device.Pair(ctx, w.dial, "https://elsewhere.example", code, newKey(t), "x", rand.Reader); err == nil {
		t.Error("a handshake for another relay was accepted")
	}
	// An expired code.
	old := code
	old.Expires = time.Now().Add(-time.Minute)
	if _, err := device.Pair(ctx, w.dial, w.origin, old, newKey(t), "x", rand.Reader); err == nil {
		t.Error("an altered code was accepted")
	}
	time.Sleep(30 * time.Millisecond)
	if st := w.remote.State(); len(st.Requests) != 0 || len(st.Devices) != 0 {
		t.Errorf("something was shown or kept: %+v", st)
	}
}

// A removed device is cut off at once, and its keys open nothing afterwards; a read-only device looks
// and can't change.
func TestRevokeAndReadOnly(t *testing.T) {
	w := newWorld(t)
	key, m := w.pair("Phone", false)
	c := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
	done := w.session(key, m, c)
	id := w.remote.State().Devices[0].ID
	if err := w.remote.Revoke(id); err != nil {
		t.Fatal(err)
	}
	var end *rpc.EndError
	if err := <-done; !errors.As(err, &end) || end.Code != rpc.CodeDenied {
		t.Errorf("the revoked device's session ended with %v", err)
	}
	// Its voucher still opens the relay; the machine answers nothing.
	if _, err := device.Connect(context.Background(), w.dial, w.origin, m, key, rand.Reader); err == nil {
		t.Error("a revoked device connected")
	}

	key2, m2 := w.pair("Tablet", true)
	if !m2.ReadOnly {
		t.Error("the device wasn't told it is read-only")
	}
	c2 := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
	w.session(key2, m2, c2)
	if err := c2.SendToOrchestrator(context.Background(), "hi"); err == nil {
		t.Error("a read-only device sent a message")
	}
	// Another device's session key doesn't work with this device's key pair, and the reverse.
	stolen := m2
	stolen.PSK = m.PSK
	if _, err := device.Connect(context.Background(), w.dial, w.origin, stolen, key2, rand.Reader); err == nil {
		t.Error("connected with another device's session key")
	}
	if _, err := device.Connect(context.Background(), w.dial, w.origin, m2, newKey(t), rand.Reader); err == nil {
		t.Error("connected with the right session key and another key pair")
	}
}

// The machine comes back after the relay drops it, and a device then resumes; after a restart of Solo
// the machine is the same machine and its devices are still paired.
func TestReconnectAndRestart(t *testing.T) {
	w := newWorld(t)
	key, m := w.pair("Phone", false)
	c := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
	done := w.session(key, m, c)

	w.local().DropMachines()
	if err := <-done; err == nil {
		t.Error("the device's session outlived the machine's connection")
	}
	w.online()
	w.session(key, m, c)
	if len(c.Threads()) == 0 {
		t.Error("nothing after the reconnect")
	}

	id := w.remote.State().MachineID
	w.remote.Close()
	w.open()
	w.online()
	if st := w.remote.State(); len(st.Devices) != 1 || !st.Enabled {
		t.Fatalf("after a restart: %+v", st)
	}
	c2 := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
	w.session(key, m, c2)
	if got := w.remote.State().MachineID; got != id {
		t.Errorf("the machine's ID changed over a restart: %s to %s", id, got)
	}

	// Off means off: the relay connection closes, and with it the session.
	if err := w.remote.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if st := w.remote.State(); st.Connection != Off {
		t.Errorf("turned off: %+v", st)
	}
	if _, err := device.Connect(context.Background(), w.dial, w.origin, m, key, rand.Reader); err == nil {
		t.Error("a device connected to a machine that is off")
	}
}

// A machine whose clock is wrong asks the relay the time and gets in; one the relay doesn't list is
// told so and doesn't hammer it.
func TestClockAndRefusal(t *testing.T) {
	w := newWorld(t)
	w.local()
	w.remote.SetEnabled(false)
	w.relay.SetClockOffset(20 * time.Minute)
	w.remote.SetEnabled(true)
	w.online()

	other := &world{t: t, relay: w.relay, backend: w.backend, dir: t.TempDir(), secrets: MemSecrets{}}
	other.open()
	if err := other.remote.SetRelay(w.url); err != nil {
		t.Fatal(err)
	}
	other.wait("the refusal", func() bool { return other.remote.State().Connection == Refused })
	if p := other.remote.State().Problem; !strings.Contains(p, "list") {
		t.Errorf("the problem shown: %q", p)
	}
	if _, err := other.remote.StartPairing(false); err == nil {
		t.Error("a machine without its relay made a pairing code")
	}
}

// A relay that alters, drops or repeats what it carries ends the session it touched, and nothing it
// made up is acted on.
func TestTamperingRelay(t *testing.T) {
	for name, tamper := range map[string]func(n *int, toDevice bool, frame []byte) []byte{
		"flips a bit to the machine": func(n *int, toDevice bool, f []byte) []byte {
			if !toDevice {
				if *n++; *n == 4 {
					f = append([]byte(nil), f...)
					f[len(f)/2] ^= 0x10
				}
			}
			return f
		},
		"flips a bit to the device": func(n *int, toDevice bool, f []byte) []byte {
			if toDevice {
				if *n++; *n == 4 {
					f = append([]byte(nil), f...)
					f[len(f)-1] ^= 1
				}
			}
			return f
		},
		"drops a frame": func(n *int, toDevice bool, f []byte) []byte {
			if !toDevice {
				if *n++; *n == 4 {
					return nil
				}
			}
			return f
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.local()
			key, m := w.pair("Phone", false)
			c := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
			done := w.session(key, m, c)
			n := 0
			w.relay.Tamper = func(toDevice bool, f []byte) []byte { return tamper(&n, toDevice, f) }
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for i := 0; i < 6; i++ {
				go c.SendToOrchestrator(ctx, "message")
				time.Sleep(5 * time.Millisecond)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the session went on over a tampering relay")
			}
		})
	}
}

// Removing a device, or limiting it to looking, reaches every connection it has, whatever state the
// connection is in: one that finished its handshake and has yet to say hello, and one the relay has
// opened that has yet to start its handshake. A device that was taken over keeps such connections
// ready for the moment it is removed.
func TestRemovalReachesEveryConnection(t *testing.T) {
	ctx := context.Background()
	t.Run("parked after its handshake, then removed", func(t *testing.T) {
		w := newWorld(t)
		key, m := w.pair("Phone", false)
		parked, err := device.Connect(ctx, w.dial, w.origin, m, key, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
		if err := w.remote.Revoke(w.remote.State().Devices[0].ID); err != nil {
			t.Fatal(err)
		}
		c := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
		done := make(chan error, 1)
		go func() { done <- c.Run(parked) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("the parked connection lived on")
		}
		if c.Status().Synced || len(c.Threads()) != 0 {
			t.Error("a removed device got a session on a connection it had parked")
		}
	})
	t.Run("parked after its handshake, then limited to looking", func(t *testing.T) {
		w := newWorld(t)
		key, m := w.pair("Phone", false)
		parked, err := device.Connect(ctx, w.dial, w.origin, m, key, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
		if err := w.remote.SetReadOnly(w.remote.State().Devices[0].ID, true); err != nil {
			t.Fatal(err)
		}
		c := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
		done := make(chan error, 1)
		go func() { done <- c.Run(parked) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("the parked connection lived on with its old access")
		}
		// Its next connection is one that may only look.
		c2 := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
		w.session(key, m, c2)
		if !c2.Status().ReadOnly {
			t.Error("the device's next session can still change things")
		}
		if err := c2.SendToOrchestrator(ctx, "hi"); err == nil {
			t.Error("a device limited to looking sent a message")
		}
	})
	t.Run("opened before its removal, handshake after", func(t *testing.T) {
		w := newWorld(t)
		key, m := w.pair("Phone", false)
		conn, err := w.dial(ctx, device.PathDevice, m.Voucher)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		time.Sleep(30 * time.Millisecond) // the machine has the connection, and waits for its first message
		if err := w.remote.Revoke(w.remote.State().Devices[0].ID); err != nil {
			t.Fatal(err)
		}
		id, _ := secure.ParseID(m.ID)
		ini, first, err := secure.Initiate(secure.Params{Purpose: secure.Session, Origin: w.origin, Machine: id}, key, m.Key, m.PSK, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		conn.Write(first)
		if answer, err := conn.Read(); err == nil {
			if _, err := ini.Finish(answer); err == nil {
				t.Error("the machine answered the handshake of a device removed while it waited")
			}
		}
	})
	t.Run("one device, too many connections", func(t *testing.T) {
		w := newWorld(t)
		key, m := w.pair("Phone", false)
		for i := 0; i < maxDeviceConns; i++ {
			l, err := device.Connect(ctx, w.dial, w.origin, m, key, rand.Reader)
			if err != nil {
				t.Fatalf("connection %d: %v", i+1, err)
			}
			defer l.Close()
		}
		if l, err := device.Connect(ctx, w.dial, w.origin, m, key, rand.Reader); err == nil {
			l.Close()
			t.Errorf("a device opened more than %d connections", maxDeviceConns)
		}
	})
}

// A device is "seen" when it has said hello, not when a handshake's first message arrives: the relay
// can send a first message it kept again, and a lost phone must not look as if it were still in use.
func TestReplayedHandshakeIsNotSeen(t *testing.T) {
	w := newWorld(t)
	key, m := w.pair("Phone", false)
	var first []byte
	w.local().Tamper = func(toDevice bool, f []byte) []byte {
		if !toDevice && first == nil {
			first = append([]byte(nil), f...)
		}
		return f
	}
	c := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
	link, err := device.Connect(context.Background(), w.dial, w.origin, m, key, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	link.Close()
	_ = c
	w.relay.Tamper = nil
	time.Sleep(30 * time.Millisecond)
	if seen := w.remote.State().Devices[0].LastSeen; !seen.IsZero() {
		t.Fatalf("a handshake with no hello counted as the device being seen: %v", seen)
	}
	// The relay sends the kept first message again, on a connection of its own making.
	conn, err := w.dial(context.Background(), device.PathDevice, m.Voucher)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write(first)
	conn.Read()
	time.Sleep(50 * time.Millisecond)
	if seen := w.remote.State().Devices[0].LastSeen; !seen.IsZero() {
		t.Errorf("a replayed first message marked the device as seen at %v", seen)
	}
}

// A pairing code answers a few handshakes and no more, whatever the relay lets through: each answered
// handshake shows its sender a set of digits, and enough of them would show any.
func TestPairingCodeAnswersFewHandshakes(t *testing.T) {
	w := newWorld(t)
	p := w.offer()
	code := w.code(p.Link)
	// A relay that lets in as many as ask.
	hash := secure.TicketHash(code.Ticket())
	for i := 0; i < maxHandshakes+4; i++ {
		w.local().Refill(hash, 3)
		req, err := device.Pair(context.Background(), w.dial, w.origin, code, newKey(t), "x", rand.Reader)
		if err == nil {
			req.Cancel()
		}
		if i >= maxHandshakes && err == nil {
			t.Fatalf("handshake %d with one code was answered", i+1)
		}
	}
	if w.remote.State().Pairing != nil {
		t.Error("the code lived on")
	}
}

// A browser that was only given the relay's address pairs without a code: it asks, shows six digits,
// and the person types them at the machine. It then connects as any paired device does.
func TestPairWithoutACode(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	key := newKey(t)
	// Nobody is let in while pairing isn't open at the machine.
	if _, err := device.PairOpen(ctx, w.dial, w.origin, key, "Desktop", rand.Reader); err == nil {
		t.Fatal("a device paired with no code while no pairing was open")
	}
	// Nor while a code is on offer: the two kinds are never open together.
	w.offer()
	if _, err := device.PairOpen(ctx, w.dial, w.origin, key, "Desktop", rand.Reader); !errors.Is(err, device.ErrNotOpen) {
		t.Fatalf("a device with no code while a code was on offer: %v", err)
	}
	w.open2()
	if st := w.remote.State(); st.Pairing == nil || !st.Pairing.Open || st.Pairing.Link != "" {
		t.Fatalf("pairing: %+v", st.Pairing)
	}
	req, err := device.PairOpen(ctx, w.dial, w.origin, key, "Desktop", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Unconfirmed {
		t.Error("a pairing with no code isn't marked as needing the person's word")
	}
	w.wait("the request", func() bool { return len(w.remote.State().Requests) == 1 })
	wrong := "000000"
	if req.Digits == wrong {
		wrong = "111111"
	}
	if _, err := w.remote.Approve(wrong, false); err == nil {
		t.Fatal("wrong digits approved a device")
	}
	d, err := w.remote.Approve(req.Digits, false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := req.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != w.remote.State().MachineID || m.Name != "box" || d.Name != "Desktop" {
		t.Errorf("paired: %+v / %+v", m, d)
	}
	c := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
	w.session(key, m, c)
	if len(c.Threads()) == 0 {
		t.Error("the device sees no threads")
	}
	// Approving spent the pairing: the next device with no code is turned away.
	if _, err := device.PairOpen(ctx, w.dial, w.origin, newKey(t), "Another", rand.Reader); err == nil {
		t.Error("pairing stayed open after a device was approved")
	}
}

// Someone else who reaches the relay's address while pairing is open can ask too, and shows other
// digits; so does a relay that puts itself between the device and the machine. Only the device whose
// digits are typed is paired.
func TestPairWithoutACodeOthersAsking(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.local()
	w.open2()
	mine, err := device.PairOpen(ctx, w.dial, w.origin, newKey(t), "Mine", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := device.PairOpen(ctx, w.dial, w.origin, newKey(t), "Type 123456", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w.wait("both requests", func() bool { return len(w.remote.State().Requests) == 2 })
	if mine.Digits == theirs.Digits {
		t.Skip("the two handshakes drew the same six digits (one in a million)")
	}
	result := make(chan error, 1)
	go func() { _, err := theirs.Wait(); result <- err }()
	if _, err := w.remote.Approve(mine.Digits, false); err != nil {
		t.Fatal(err)
	}
	if _, err := mine.Wait(); err != nil {
		t.Fatalf("my device: %v", err)
	}
	if err := <-result; !errors.Is(err, device.ErrDenied) {
		t.Errorf("the other device was told %v", err)
	}
	if devs := w.remote.State().Devices; len(devs) != 1 || devs[0].Name != "Mine" {
		t.Errorf("paired: %+v", devs)
	}
}

// A device that doesn't keep its commitment (it reveals a number other than the one it committed to,
// which is how it would choose its digits) is never shown to the person.
func TestPairWithoutACodeHoldsTheCommitment(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.local()
	w.open2()
	key := newKey(t)
	conn, err := w.dial(ctx, device.PathPair, []byte{device.OpenPairToken})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ini, first, _ := secure.InitiateOpen(w.origin, key, rand.Reader)
	conn.Write(first)
	answer, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	third, ch, err := ini.Finish(answer)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(third)
	link := wire.NewLink(conn, ch)
	other := append([]byte(nil), ini.Nonce()...)
	other[5] ^= 1
	req, _ := json.Marshal(device.Request{Name: "Chooser", Nonce: other})
	link.Send(req)
	// The machine hangs up on it, and it was never among those asking.
	if _, err := conn.Read(); err == nil {
		t.Error("the machine went on with a device that broke its commitment")
	}
	if n := len(w.remote.State().Requests); n != 0 {
		t.Errorf("a device that broke its commitment is among the %d asking", n)
	}
}

// A device is paired with the machine that answered it and nobody else, and only when what the machine
// sent hangs together: its ID is the hash of the key the handshake was with and of the key its voucher
// is signed with, and the voucher is for this device. And something that answers "paired" with nobody
// having typed anything is not taken at its word.
func TestDeviceChecksWhatItIsTold(t *testing.T) {
	w := newWorld(t)
	w.local()
	ctx := context.Background()
	// A rogue answers a device's handshake itself, as the relay or another listed machine could, and
	// says "paired" at once.
	rogueSeed, _ := secure.NewSeed(rand.Reader)
	rogue, _ := secure.MachineFromSeed(rogueSeed)
	key := newKey(t)
	answerAs := func(decide func(peer []byte) device.Decision) device.Dialer {
		return func(context.Context, string, []byte) (wire.Conn, error) {
			a, b := make(chan []byte, 8), make(chan []byte, 8)
			dev, mach := &chanConn{in: a, out: b}, &chanConn{in: b, out: a}
			go func() {
				first, _ := mach.Read()
				res, answer, err := secure.RespondOpen(w.origin, rogue.Noise, first, rand.Reader)
				if err != nil {
					return
				}
				mach.Write(answer)
				third, _ := mach.Read()
				ch, err := res.Finish(third)
				if err != nil {
					return
				}
				link := wire.NewLink(mach, ch)
				link.Receive()
				link.Send(decision(decide(ch.Peer())))
			}()
			return dev, nil
		}
	}
	psk := make([]byte, secure.KeySize)
	own := secure.DeviceID(key.Public)
	for name, decide := range map[string]func([]byte) device.Decision{
		"another machine's ID": func([]byte) device.Decision {
			real, _ := secure.ParseID(w.remote.State().MachineID)
			v := rogue.Voucher(w.origin, own, time.Now())
			copy(v[33:49], real[:]) // the real machine's ID in the rogue's voucher
			return device.Decision{OK: true, PSK: psk, Voucher: v, Name: "box", Machine: real.String()}
		},
		"a voucher for another device": func([]byte) device.Decision {
			return device.Decision{OK: true, PSK: psk, Voucher: rogue.Voucher(w.origin, secure.DeviceID([]byte("someone else")), time.Now()), Name: "box"}
		},
	} {
		p, err := device.PairOpen(ctx, answerAs(decide), w.origin, key, "Desktop", rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if m, err := p.Wait(); err == nil {
			t.Errorf("%s: the device kept %+v", name, m)
		}
	}
	// A rogue that is consistent about itself is "paired" as far as the handshake can tell, with its
	// own ID and not the real machine's. That is why the page asks the person before keeping it.
	p, err := device.PairOpen(ctx, answerAs(func([]byte) device.Decision {
		return device.Decision{OK: true, PSK: psk, Voucher: rogue.Voucher(w.origin, own, time.Now()), Name: "box"}
	}), w.origin, key, "Desktop", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Unconfirmed || m.ID == w.remote.State().MachineID || m.ID != rogue.ID.String() {
		t.Errorf("a rogue's answer: unconfirmed %v, id %s", p.Unconfirmed, m.ID)
	}
}

// chanConn is one end of a connection in memory.
type chanConn struct{ in, out chan []byte }

func (c *chanConn) Read() ([]byte, error) {
	b, ok := <-c.in
	if !ok {
		return nil, errors.New("closed")
	}
	return b, nil
}
func (c *chanConn) Write(b []byte) error { c.out <- append([]byte(nil), b...); return nil }
func (c *chanConn) Close() error         { return nil }

// A pairing connection has a short time to finish its handshake and say who is asking, and a request
// then waits a couple of minutes at most: nothing holds a set of digits, unseen, for as long as pairing
// is open. And what it says before it is anyone is one small message, not a megabyte to inflate.
func TestPairingConnectionsDontLinger(t *testing.T) {
	w := newWorld(t)
	w.local()
	ctx := context.Background()
	w.open2()
	conn, err := w.dial(ctx, device.PathPair, []byte{device.OpenPairToken})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ini, first, _ := secure.InitiateOpen(w.origin, newKey(t), rand.Reader)
	conn.Write(first)
	answer, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	third, ch, err := ini.Finish(answer)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(third)
	// Who is asking, as a compressed message in several records: more than one small record.
	link := wire.NewLink(conn, ch)
	link.Compress = true
	req, _ := json.Marshal(device.Request{Name: strings.Repeat("a", 900), Nonce: ini.Nonce()})
	link.Send(req)
	if _, err := conn.Read(); err == nil {
		t.Error("the machine took a compressed request from a device that isn't anyone yet")
	}
	if n := len(w.remote.State().Requests); n != 0 {
		t.Errorf("%d requests", n)
	}
}

// Pairing without a code answers as few handshakes as pairing with one, whatever the relay lets in.
func TestPairWithoutACodeAnswersFewHandshakes(t *testing.T) {
	w := newWorld(t)
	w.local()
	w.open2()
	for i := 0; i < maxHandshakes+4; i++ {
		w.relay.RefillOpen(8)
		req, err := device.PairOpen(context.Background(), w.dial, w.origin, newKey(t), "x", rand.Reader)
		if err == nil {
			req.Cancel()
		}
		if i >= maxHandshakes && err == nil {
			t.Fatalf("handshake %d was answered", i+1)
		}
	}
	if w.remote.State().Pairing != nil {
		t.Error("pairing stayed open")
	}
}
