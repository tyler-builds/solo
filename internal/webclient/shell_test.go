//go:build !js

package webclient

import (
	"crypto/rand"
	"encoding/base64"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/gui"
	"github.com/tyler-builds/solo/internal/remote/host"
	"github.com/tyler-builds/solo/internal/remote/relaytest"
	"github.com/tyler-builds/solo/internal/sample"
)

// machine is a relay and a machine connected to it.
type machine struct {
	relay  *relaytest.Relay
	remote *host.Remote
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{relay: relaytest.New()}
	t.Cleanup(m.relay.Close)
	r, err := host.New(host.Config{Dir: t.TempDir(), Backend: sample.New(sample.Options{Threads: 6, Items: 20}),
		Secrets: host.MemSecrets{}, Name: "workstation", Retry: 10 * time.Millisecond, FlushEvery: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	m.remote = r
	key, _ := r.RelayKey()
	raw, _ := base64.RawURLEncoding.DecodeString(key)
	m.relay.Allow(raw)
	if err := r.SetRelay(m.relay.URL()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.State().Connection != host.Online || m.relay.Machines() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the machine didn't reach its relay")
		}
		time.Sleep(2 * time.Millisecond)
	}
	return m
}

// offer makes a pairing code and returns its link's fragment, once the relay knows its ticket.
func (m *machine) offer(t *testing.T) string {
	t.Helper()
	p, err := m.remote.StartPairing()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.relay.Tickets() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the relay never got the ticket")
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, frag, _ := strings.Cut(p.Link, "#")
	return frag
}

// storage is a device's storage, which outlives a page.
type storage struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *storage) load(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[name]
}

func (s *storage) store(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == "" {
		delete(s.m, name)
	} else {
		s.m[name] = value
	}
}

// page opens the web client as a phone would, with what followed "#" in its address.
func (m *machine) page(t *testing.T, st *storage, fragment string) (*Shell, *uitest.UI) {
	t.Helper()
	s, err := New(Platform{Dial: relaytest.Dialer(m.relay.URL()), Host: strings.TrimPrefix(m.relay.URL(), "http://"),
		Random: rand.Reader, Load: st.load, Store: st.store, Fragment: fragment, DeviceName: "Test phone", Touch: true,
		Retry: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, dark := gui.Themes()
	u := uitest.New(t, s.Root, uitest.Size(390, 844), uitest.Theme(dark), uitest.Bindings(gui.Bindings()...))
	t.Cleanup(func() {
		u.Close()
		s.Close()
	})
	return s, u
}

func hasText(u *uitest.UI, text string) bool {
	found := false
	u.Tree().Walk(func(n *access.Node) bool {
		found = found || strings.Contains(n.Name, text)
		return !found
	})
	return found
}

// A phone opens a pairing link, shows the digits, is approved at the machine and gets the machine's
// UI; opened again later, it connects without pairing.
func TestPairThenUse(t *testing.T) {
	m := newMachine(t)
	st := &storage{m: map[string]string{}}
	frag := m.offer(t)
	s, u := m.page(t, st, frag)
	u.WaitFor("the question", func() bool { return hasText(u, "Pair this device?") })
	if got := u.Get("device-name").Value(); got != "Test phone" {
		t.Errorf("the name offered is %q", got)
	}
	u.Click("pair")
	u.WaitFor("the digits", func() bool { return u.Has("digits") })
	digits := strings.ReplaceAll(u.Get("digits").Text(), " ", "")
	if len(digits) != 6 {
		t.Fatalf("digits shown: %q", digits)
	}
	u.WaitFor("the request at the machine", func() bool { return len(m.remote.State().Requests) == 1 })
	if name := m.remote.State().Requests[0].Name; name != "Test phone" {
		t.Errorf("the machine was told %q", name)
	}
	if _, err := m.remote.Approve(digits, false); err != nil {
		t.Fatal(err)
	}
	u.WaitFor("the machine's UI", func() bool { return u.Has("open-drawer") })
	u.Click("open-drawer")
	u.WaitFor("the machine's name in the drawer", func() bool { return hasText(u, "Machine: workstation") })
	if st.load(storeMachines) == "" || st.load(storeKey) == "" {
		t.Error("nothing was kept on the device")
	}
	if strings.Contains(st.load(storeMachines), frag) {
		t.Error("the pairing code was kept")
	}
	s.Close()
	u.Close()

	// Later: the page is opened with no link.
	_, u2 := m.page(t, st, "")
	u2.WaitFor("the machine's UI again", func() bool { return u2.Has("open-drawer") })
	if hasText(u2, "Pair this device?") {
		t.Error("a paired device was asked to pair")
	}
}

// The machine going away and coming back is shown, and what the device has stays on screen; a device
// removed at the machine is told so and stops trying.
func TestOfflineAndRemoved(t *testing.T) {
	m := newMachine(t)
	st := &storage{m: map[string]string{}}
	frag := m.offer(t)
	_, u := m.page(t, st, frag)
	u.WaitFor("the question", func() bool { return u.Has("pair") })
	u.Click("pair")
	u.WaitFor("the digits", func() bool { return u.Has("digits") })
	if _, err := m.remote.Approve(u.Get("digits").Text(), false); err != nil {
		t.Fatal(err)
	}
	u.WaitFor("the machine's UI", func() bool { return u.Has("open-drawer") })
	if u.Has("link-bar") {
		t.Error("a connected page shows a connection problem")
	}

	m.remote.SetEnabled(false)
	u.WaitFor("the offline line", func() bool { return u.Has("link-bar") && hasText(u, "offline") })
	if !u.Has("open-drawer") {
		t.Error("the UI went away with the connection")
	}
	m.remote.SetEnabled(true)
	u.WaitFor("the line to go", func() bool { return !u.Has("link-bar") })

	id := m.remote.State().Devices[0].ID
	if err := m.remote.Revoke(id); err != nil {
		t.Fatal(err)
	}
	u.WaitFor("being told of the removal", func() bool { return hasText(u, "removed at the machine") })
}

// A page opened with nothing, with a link that is damaged, and with one that has expired.
func TestWelcomeAndBadLinks(t *testing.T) {
	m := newMachine(t)
	_, u := m.page(t, &storage{m: map[string]string{}}, "")
	u.WaitFor("the welcome", func() bool { return hasText(u, "Use Solo from this device") })
	if u.Has("pair") {
		t.Error("a page with no code offers to pair")
	}
	_, u2 := m.page(t, &storage{m: map[string]string{}}, "p=not-a-code")
	u2.WaitFor("the problem", func() bool { return hasText(u2, "isn't complete") })

	// A code for a machine that isn't there: the relay turns the device away, and the page says what to do.
	frag := m.offer(t)
	m.remote.CancelPairing()
	_, u3 := m.page(t, &storage{m: map[string]string{}}, frag)
	u3.WaitFor("the question", func() bool { return u3.Has("pair") })
	u3.Click("pair")
	u3.WaitFor("the failure", func() bool { return hasText(u3, "Pairing didn't finish") })
	if len(m.remote.State().Requests) != 0 {
		t.Error("a withdrawn code reached the machine")
	}
}

// TestShots writes pictures of the shell's screens at a phone's size, for looking at:
//
//	SOLO_SHOTS=/tmp/shots go test ./internal/webclient -run TestShots
func TestShots(t *testing.T) {
	out := os.Getenv("SOLO_SHOTS")
	if out == "" {
		t.Skip("set SOLO_SHOTS to an output directory")
	}
	os.MkdirAll(out, 0o755)
	save := func(u *uitest.UI, name string) {
		for i := 0; i < 20; i++ { // what arrives from the machine is applied a moment after it arrives
			u.Frame()
			time.Sleep(10 * time.Millisecond)
		}
		u.Settle()
		f, err := os.Create(filepath.Join(out, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		png.Encode(f, u.Screenshot())
	}
	m := newMachine(t)
	_, u := m.page(t, &storage{m: map[string]string{}}, "")
	save(u, "web-welcome")

	st := &storage{m: map[string]string{}}
	frag := m.offer(t)
	_, u = m.page(t, st, frag)
	u.WaitFor("the question", func() bool { return u.Has("pair") })
	save(u, "web-confirm")
	u.Click("pair")
	u.WaitFor("the digits", func() bool { return u.Has("digits") })
	save(u, "web-digits")
	m.remote.Approve(u.Get("digits").Text(), false)
	u.WaitFor("the machine's UI", func() bool { return u.Has("open-drawer") })
	save(u, "web-main")
	u.Click("open-drawer")
	u.WaitFor("the drawer", func() bool { return hasText(u, "Machine: workstation") })
	save(u, "web-drawer")
	u.Click("machine")
	u.WaitFor("the machines", func() bool { return hasText(u, "Machines") })
	save(u, "web-machines")
	u.Click("open-" + m.remote.State().MachineID)
	u.WaitFor("the machine's UI", func() bool { return u.Has("open-drawer") })
	m.remote.SetEnabled(false)
	u.WaitFor("the offline line", func() bool { return u.Has("link-bar") })
	save(u, "web-offline")
}
