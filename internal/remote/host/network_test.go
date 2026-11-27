//go:build !js

package host

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/rpc"
	"github.com/tyler-builds/solo/internal/remote/secure"
	"github.com/tyler-builds/solo/internal/sample"
)

// another is a second machine on the same relay as w: the relay lists its key too, as deploying from
// the same Cloudflare account does.
func (w *world) another() *world {
	w.t.Helper()
	o := &world{t: w.t, relay: w.relay, url: w.url, origin: w.origin, dial: w.dial, dir: w.t.TempDir(), secrets: MemSecrets{},
		backend: sample.New(sample.Options{Threads: 2, Items: 5})}
	o.open()
	key, err := o.remote.RelayKey()
	if err != nil {
		w.t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(key)
	w.relay.Allow(raw)
	if err := o.remote.SetRelay(o.url); err != nil {
		w.t.Fatal(err)
	}
	o.wait("the second machine to reach the relay", func() bool { return o.remote.State().Connection == Online })
	return o
}

const pass = "orbit lantern mossy quarry"

// The whole network: a machine sets the passphrase, a second one joins it with the same one, and a
// device that types it waits at both until it is allowed at one; members read each other's usage,
// a new passphrase reaches every member, and wrong guesses turn joining off.
func TestNetwork(t *testing.T) {
	a := newWorld(t)
	if a.relay == nil {
		t.Skip("needs a relay the test controls")
	}
	b := a.another()
	ctx := context.Background()
	if err := a.remote.SetPassphrase("too short"); err == nil {
		t.Error("took a short passphrase")
	}
	if err := a.remote.SetPassphrase(pass); err != nil {
		t.Fatal(err)
	}
	a.wait("the relay to list a", func() bool { ids, _ := device.Network(ctx, a.dial); return len(ids) == 1 })
	if err := b.remote.SetPassphrase(pass); err != nil {
		t.Fatal(err)
	}
	b.wait("b to join a", func() bool {
		return len(a.remote.State().Network.Members) == 1 && len(b.remote.State().Network.Members) == 1
	})
	aID, bID := a.remote.State().MachineID, b.remote.State().MachineID
	if m := a.remote.State().Network.Members[0]; m.ID != bID {
		t.Errorf("a's member = %+v, want b (%s)", m, bID)
	}
	if got := a.remote.MemberUsage(ctx); len(got) != 1 || got[0].Err != "" || got[0].At.IsZero() {
		t.Errorf("a reads b's usage: %+v", got)
	}

	// A device asks both machines; it is allowed at a, and b lets it in on a's word.
	ids, err := device.Network(ctx, a.dial)
	if err != nil || len(ids) != 2 {
		t.Fatalf("the network = %v, %v", ids, err)
	}
	key := newKey(t)
	var pairings []*device.Pairing
	for _, id := range ids {
		p, err := device.PairPass(ctx, a.dial, a.origin, id, []byte(pass), key, device.Request{Name: "Phone"}, rand.Reader)
		if err != nil {
			t.Fatalf("pairing with %s: %v", id, err)
		}
		pairings = append(pairings, p)
	}
	a.wait("the device to wait at a", func() bool { return len(a.remote.State().Network.Joins) == 1 })
	if j := a.remote.State().Network.Joins[0]; j.Name != "Phone" {
		t.Errorf("the join = %+v", j)
	}
	if err := a.remote.AllowJoin(a.remote.State().Network.Joins[0].ID, false); err != nil {
		t.Fatal(err)
	}
	machines := map[string]bool{}
	for _, p := range pairings {
		m, err := p.Wait()
		if err != nil {
			t.Fatalf("waiting: %v", err)
		}
		machines[m.ID] = true
		// The device can use what it was given.
		if _, err := device.Connect(ctx, a.dial, a.origin, m, key, rand.Reader); err != nil {
			t.Errorf("connecting to %s: %v", m.Name, err)
		}
	}
	if !machines[aID] || !machines[bID] {
		t.Errorf("paired with %v, want a and b", machines)
	}
	if n := len(b.remote.State().Devices); n != 1 {
		t.Errorf("b has %d devices", n)
	}

	// A new passphrase set at a reaches b: a device that types it pairs with b.
	const next = "violet harbor seventy kites"
	if err := a.remote.SetPassphrase(next); err != nil {
		t.Fatal(err)
	}
	b.wait("b to take the new passphrase", func() bool {
		p, err := device.PairPass(ctx, a.dial, a.origin, secure.MustParseID(bID), []byte(next), newKey(t), device.Request{Name: "Tablet"}, rand.Reader)
		if err != nil {
			return false
		}
		p.Cancel()
		return true
	})

	// Wrong guesses are counted, and too many turn joining off: the relay lists the machine no more.
	before := b.remote.State().Network.Wrong
	for i := before; i < maxWrong; i++ {
		_, err := device.PairPass(ctx, a.dial, a.origin, secure.MustParseID(bID), []byte("not the passphrase"), newKey(t), device.Request{Name: "X"}, rand.Reader)
		if !errors.Is(err, secure.ErrWrongPassphrase) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	st := b.remote.State().Network
	if st.Locked == "" || st.Wrong != maxWrong {
		t.Fatalf("after %d wrong guesses: %+v", maxWrong, st)
	}
	b.wait("the relay to drop b from the network", func() bool { ids, _ := device.Network(ctx, a.dial); return len(ids) == 1 })
	if _, err := device.PairPass(ctx, a.dial, a.origin, secure.MustParseID(bID), []byte(next), newKey(t), device.Request{Name: "Y"}, rand.Reader); err == nil {
		t.Error("paired by passphrase while joining is off")
	}
	if err := b.remote.SetNetworkOn(true); err != nil {
		t.Fatal(err)
	}
	if st := b.remote.State().Network; st.Locked != "" || st.Wrong != 0 {
		t.Errorf("turned on again: %+v", st)
	}

	// A member may read usage and nothing more.
	b.remote.mu.Lock()
	m := b.remote.saved.Network.Members[0]
	b.remote.mu.Unlock()
	p, err := b.remote.memberSession(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	var e *rpc.Error
	if err := p.Call(ctx, "search", map[string]any{"query": "token", "limit": 5}, nil); !errors.As(err, &e) || e.Code != rpc.CodeDenied {
		t.Errorf("search by a member: %v", err)
	}
	p.Close()

	// Removing a member ends its way in: b can no longer read a's usage.
	if err := a.remote.RemoveMember(bID); err != nil {
		t.Fatal(err)
	}
	if got := b.remote.MemberUsage(ctx); len(got) != 1 || got[0].Err == "" {
		t.Errorf("b reads a's usage after a removed it: %+v", got)
	}
}

// A device that a member allowed, but that reaches this machine only afterwards, is let in on arrival.
func TestNetworkAllowedBeforeArrival(t *testing.T) {
	a := newWorld(t)
	if a.relay == nil {
		t.Skip("needs a relay the test controls")
	}
	if err := a.remote.SetPassphrase(pass); err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	if err := a.remote.allowed(rpc.Device{Name: "b"}, key.Public, true); err != nil {
		t.Fatal(err)
	}
	id := secure.MustParseID(a.remote.State().MachineID)
	p, err := device.PairPass(context.Background(), a.dial, a.origin, id, []byte(pass), key, device.Request{Name: "Phone"}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.Wait()
	if err != nil || !m.ReadOnly {
		t.Errorf("let in: %+v, %v", m, err)
	}
	// Another device isn't.
	other, err := device.PairPass(context.Background(), a.dial, a.origin, id, []byte(pass), newKey(t), device.Request{Name: "Other"}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a.wait("the other device to wait", func() bool { return len(a.remote.State().Network.Joins) == 1 })
	a.remote.DenyJoin(a.remote.State().Network.Joins[0].ID)
	if _, err := other.Wait(); !errors.Is(err, device.ErrDenied) {
		t.Errorf("a device nobody allowed: %v", err)
	}
}
