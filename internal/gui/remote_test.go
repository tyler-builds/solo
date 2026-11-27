package gui

import (
	"context"
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
	linked   []api.LinkedUsage // for LinkedUsage
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
func (f *fakeRemote) StartPairing(open bool) (api.RemotePairing, error) {
	p := api.RemotePairing{Open: open, Expires: time.Now().Add(5 * time.Minute)}
	if !open {
		p.Link = "https://solo.example.workers.dev/#p=" + strings.Repeat("Ab3_", 30)
	}
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
func (f *fakeRemote) SetCloudToken(context.Context, string) error {
	f.change(func(st *api.RemoteState) { st.Cloud.HasToken, st.Cloud.Account = true, "Personal" })
	return nil
}
func (f *fakeRemote) ForgetCloudToken() error {
	f.change(func(st *api.RemoteState) { st.Cloud.HasToken = false })
	return nil
}
func (f *fakeRemote) DeployRelay(context.Context) error {
	f.change(func(st *api.RemoteState) {
		st.Cloud.Deployed, st.Relay, st.Enabled, st.Connection = true, "https://solo.example.workers.dev", true, api.RemoteOnline
	})
	return nil
}
func (f *fakeRemote) RemoveRelay(context.Context) error { return nil }
func (f *fakeRemote) AcceptRelay(context.Context, bool) error {
	f.change(func(st *api.RemoteState) { st.Cloud.Paused, st.Cloud.Alert, st.Cloud.Changes = false, "", nil })
	return nil
}
func (f *fakeRemote) RevokeAll() error               { return nil }
func (f *fakeRemote) SetReadOnly(string, bool) error { return nil }
func (f *fakeRemote) LinkTo(string) error            { return nil }
func (f *fakeRemote) CancelLink()                    {}
func (f *fakeRemote) Unlink(string) error            { return nil }
func (f *fakeRemote) LinkedUsage(context.Context) []api.LinkedUsage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.linked
}

// pairedRemote is a machine connected to its relay with two devices.
func pairedRemote() *fakeRemote {
	now := time.Now()
	return &fakeRemote{st: api.RemoteState{Relay: "https://solo.example.workers.dev", Enabled: true,
		Connection: api.RemoteOnline, Name: "workstation", MachineID: "0f3a",
		Cloud: api.RemoteCloud{HasToken: true, Account: "Personal", Deployed: true, CanDeploy: true},
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
	rm.change(func(st *api.RemoteState) { st.Requests = []api.RemoteRequest{{At: time.Now()}} })
	u.WaitFor("the request", func() bool { return has(access.RoleStaticText, "A device is asking to pair") })
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

// Setting remote access up from nothing: the token is pasted and kept, and one button deploys the relay.
func TestRemotePageSetsUp(t *testing.T) {
	rm := &fakeRemote{st: api.RemoteState{Connection: api.RemoteOff, Cloud: api.RemoteCloud{CanDeploy: true}}}
	a, u := newUIWith(t, demoBackend(), func(a *App) { a.Remote = rm })
	a.settingsTab = "remote"
	a.settings.Set(true)
	u.Frame()
	if !u.Get("remote-deploy").Disabled() {
		t.Error("Set up is offered before there is a token")
	}
	u.Get("remote-token").Click()
	u.Type("cf-token-abc")
	u.Click("remote-token-save")
	u.WaitFor("the token to be kept", func() bool { return rm.State().Cloud.HasToken && u.Has("remote-token-remove") })
	if u.Has("remote-token") {
		t.Error("the token's box is still shown after it was kept")
	}
	u.Click("remote-deploy")
	u.WaitFor("the relay", func() bool { return rm.State().Cloud.Deployed && u.Has("remote-update") })
	found := false
	u.Tree().Walk(func(n *access.Node) bool {
		found = found || strings.Contains(n.Name, "solo.example.workers.dev")
		return true
	})
	if !found {
		t.Error("the relay's address isn't shown")
	}
}

// A relay that was changed from elsewhere is said so, whatever the page otherwise shows, with what was
// found and the choices: resume, resume without the devices, or remove the relay.
func TestRemotePagePaused(t *testing.T) {
	for _, relay := range []string{"https://solo.example.workers.dev", ""} {
		rm := pairedRemote()
		rm.st.Relay = relay
		rm.st.Connection = api.RemoteOff
		rm.st.Cloud.Paused = true
		rm.st.Cloud.Alert = "Remote access is paused: the relay is not as this copy of Solo left it."
		rm.st.Cloud.Changes = []string{"A version was uploaded on 12 Nov 2026 at 10:49, through api."}
		a, u := newUIWith(t, demoBackend(), func(a *App) { a.Remote = rm })
		a.settingsTab = "remote"
		a.settings.Set(true)
		u.Frame()
		if !u.Has("remote-alert") || !u.Has("remote-accept") || !u.Has("remote-accept-clean") || !u.Has("remote-remove-paused") {
			t.Fatalf("relay %q: the alert or its choices are missing", relay)
		}
		if u.Has("remote-deploy") {
			t.Errorf("relay %q: setting up is offered while paused", relay)
		}
		found := false
		u.Tree().Walk(func(n *access.Node) bool {
			found = found || strings.Contains(n.Name, "uploaded on 12 Nov 2026")
			return true
		})
		if !found {
			t.Errorf("relay %q: what was found isn't shown", relay)
		}
		u.Close()
	}
}

// For a computer's browser the card switches to pairing without a code: the address to open there, and
// the same box for the digits. The two kinds are never shown together.
func TestRemotePagePairsWithoutACode(t *testing.T) {
	rm := pairedRemote()
	a, u := newUIWith(t, demoBackend(), func(a *App) { a.Remote = rm })
	a.settingsTab = "remote"
	a.settings.Set(true)
	u.Frame()
	has := func(text string) bool {
		found := false
		u.Tree().Walk(func(n *access.Node) bool {
			found = found || strings.Contains(n.Name, text)
			return true
		})
		return found
	}
	u.ByRole(access.RoleButton, "Add a device").Click()
	u.WaitFor("the code", func() bool { return u.Has("remote-pair-open") })
	u.Click("remote-pair-open")
	u.WaitFor("the address", func() bool { return u.Has("remote-pair-code") && has("open this address") })
	if has("Pairing code") {
		t.Error("the code is still shown while pairing is open without one")
	}
	if st := rm.State(); st.Pairing == nil || !st.Pairing.Open {
		t.Errorf("pairing: %+v", st.Pairing)
	}
	rm.change(func(st *api.RemoteState) { st.Requests = []api.RemoteRequest{{At: time.Now()}} })
	u.WaitFor("the request", func() bool { return has("A device is asking to pair") })
	u.Get("remote-digits").Click()
	u.Type("482913")
	u.ByRole(access.RoleButton, "Pair").Click()
	u.WaitFor("the device to be paired", func() bool {
		rm.mu.Lock()
		defer rm.mu.Unlock()
		return len(rm.approved) == 1
	})
}
