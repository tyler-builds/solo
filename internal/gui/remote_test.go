package gui

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"

	"github.com/tyler-builds/solo/internal/api"
)

// fakeRemote is remote access as the Settings page sees it, held in memory.
type fakeRemote struct {
	mu       sync.Mutex
	st       api.RemoteState
	subs     []func()
	approved []string
	revoked  []string
}

func (f *fakeRemote) State() api.RemoteState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeRemote) Subscribe(fn func()) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, fn)
	return func() {}
}

func (f *fakeRemote) change(fn func(*api.RemoteState)) {
	f.mu.Lock()
	fn(&f.st)
	subs := f.subs
	f.mu.Unlock()
	for _, s := range subs {
		s()
	}
}

func (f *fakeRemote) SetRelay(address string) error {
	f.change(func(st *api.RemoteState) { st.Relay, st.Enabled = address, address != "" })
	return nil
}
func (f *fakeRemote) SetEnabled(on bool) error {
	f.change(func(st *api.RemoteState) { st.Enabled = on })
	return nil
}
func (f *fakeRemote) StartPairing() (api.RemotePairing, error) {
	p := api.RemotePairing{Link: "https://solo-relay.example.workers.dev/#p=" + strings.Repeat("Ab3_", 30), Expires: time.Now().Add(5 * time.Minute)}
	f.change(func(st *api.RemoteState) { st.Pairing = &p })
	return p, nil
}
func (f *fakeRemote) CancelPairing() {
	f.change(func(st *api.RemoteState) { st.Pairing, st.Requests = nil, nil })
}
func (f *fakeRemote) Approve(digits string, readOnly bool) (api.RemoteDevice, error) {
	if strings.ReplaceAll(digits, " ", "") != "482913" { // the real one takes the digits however they are spaced
		return api.RemoteDevice{}, errors.New("No device waiting shows those digits.")
	}
	d := api.RemoteDevice{ID: "d-new", Name: "Tyler's phone", ReadOnly: readOnly, Added: time.Now()}
	f.change(func(st *api.RemoteState) {
		st.Pairing, st.Requests = nil, nil
		st.Devices = append(st.Devices, d)
	})
	f.mu.Lock()
	f.approved = append(f.approved, digits)
	f.mu.Unlock()
	return d, nil
}
func (f *fakeRemote) Revoke(id string) error {
	f.mu.Lock()
	f.revoked = append(f.revoked, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeRemote) RevokeAll() error               { return nil }
func (f *fakeRemote) SetReadOnly(string, bool) error { return nil }

// pairedRemote is a machine connected to its relay with two devices.
func pairedRemote() *fakeRemote {
	now := time.Now()
	return &fakeRemote{st: api.RemoteState{Relay: "https://solo-relay.example.workers.dev", Enabled: true,
		Connection: api.RemoteOnline, Name: "workstation", MachineID: "0f3a",
		Devices: []api.RemoteDevice{
			{ID: "d1", Name: "Pixel 9", Added: now.Add(-9 * 24 * time.Hour), LastSeen: now, Online: true},
			{ID: "d2", Name: "Work laptop", ReadOnly: true, Added: now.Add(-40 * 24 * time.Hour), LastSeen: now.Add(-3 * time.Hour)},
		}}}
}

// Pairing from Settings: the page offers a code, shows who is asking, and pairs the device whose digits
// are typed.
func TestRemotePagePairs(t *testing.T) {
	f := demoBackend()
	a, u := newUIWith(t, f, func(a *App) { a.Remote = pairedRemote() })
	rm := a.Remote.(*fakeRemote)
	a.settingsTab = "remote"
	a.settings.Set(true)
	u.Frame()
	has := func(role access.Role, name string) bool {
		found := false
		u.Tree().Walk(func(n *access.Node) bool {
			found = found || (n.Role == role && strings.Contains(n.Name, name))
			return true
		})
		return found
	}
	if !has(access.RoleTab, "Remote") {
		t.Fatal("Settings has no Remote page")
	}
	u.WaitFor("the devices", func() bool { return has(access.RoleStaticText, "Pixel 9") && has(access.RoleStaticText, "Work laptop") })
	u.ByRole(access.RoleButton, "Add a device").Click()
	u.WaitFor("the pairing code", func() bool { return has(access.RoleImage, "Pairing code") })
	rm.change(func(st *api.RemoteState) { st.Requests = []api.RemoteRequest{{Name: "Tyler's phone", At: time.Now()}} })
	u.WaitFor("the request", func() bool { return has(access.RoleStaticText, "Tyler's phone") })
	u.Get("remote-digits").Click()
	u.Type("482 913")
	u.ByRole(access.RoleButton, "Pair").Click()
	u.WaitFor("the device to be paired", func() bool {
		rm.mu.Lock()
		defer rm.mu.Unlock()
		return len(rm.approved) == 1
	})
	u.WaitFor("the new device in the list", func() bool {
		return has(access.RoleStaticText, "Tyler's phone") && !has(access.RoleImage, "Pairing code")
	})
}

// Without remote access, as in a web page, Settings has no Remote page.
func TestNoRemotePageWithoutRemote(t *testing.T) {
	a, u := newUI(t, demoBackend())
	a.settings.Set(true)
	u.Frame()
	found := false
	u.Tree().Walk(func(n *access.Node) bool {
		found = found || (n.Role == access.RoleTab && n.Name == "Remote")
		return true
	})
	if found {
		t.Error("a Remote page without remote access")
	}
}
