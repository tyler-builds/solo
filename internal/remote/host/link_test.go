//go:build !js

package host

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/rpc"
	"github.com/tyler-builds/solo/internal/remote/secure"
)

// Two Solos, each with its own relay, link both ways with one pairing: the one that links pairs as a
// linked Solo with the digits typed at the other, then links back. Each then reads the other's usage
// and nothing else, and unlinking at one end closes its side.
func TestLinkedSolos(t *testing.T) {
	a, b := newWorld(t), newWorld(t)
	if a.relay == nil {
		t.Skip("needs two relays")
	}
	ctx := context.Background()
	p := a.offer()
	if err := b.remote.LinkTo(p.Link); err != nil {
		t.Fatal(err)
	}
	st := b.remote.State()
	if st.Linking == nil || len(st.Linking.Digits) != 6 {
		t.Fatalf("linking = %+v", st.Linking)
	}
	a.wait("the request", func() bool { return len(a.remote.State().Requests) == 1 })
	if _, err := a.remote.Approve(st.Linking.Digits, true); err != nil {
		t.Fatal(err)
	}
	b.wait("the link", func() bool { l := b.remote.State().Linking; return l != nil && l.Done })
	if l := b.remote.State().Linking; l.Err != "" {
		t.Fatalf("linking failed: %s", l.Err)
	}

	as, bs := a.remote.State(), b.remote.State()
	if len(bs.Links) != 1 || bs.Links[0].ID != as.MachineID || bs.Links[0].OneWay {
		t.Errorf("b's links = %+v", bs.Links)
	}
	if len(as.Links) != 1 || as.Links[0].ID != bs.MachineID || as.Links[0].OneWay {
		t.Errorf("a's links = %+v", as.Links)
	}
	for name, st := range map[string]State{"a": as, "b": bs} {
		if len(st.Devices) != 1 || !st.Devices[0].Linked || st.Devices[0].ReadOnly {
			t.Errorf("%s's devices = %+v", name, st.Devices)
		}
	}
	for name, w := range map[string]*world{"a": a, "b": b} {
		got := w.remote.LinkedUsage(ctx)
		if len(got) != 1 || got[0].Err != "" || got[0].At.IsZero() {
			t.Errorf("%s's linked usage = %+v", name, got)
		}
	}

	// A second grant from the same Solo takes the place of the first, whatever machine it names.
	if err := a.remote.acceptLink(rpc.Device{ID: as.Devices[0].ID, Name: "box", Linked: true}, a.grantFor(t, b)); err != nil {
		t.Fatal(err)
	}
	after := a.remote.State().Links
	if len(after) != 1 || after[0].ID == bs.MachineID {
		t.Fatalf("after a second grant, a's links = %+v", after)
	}

	// A linked Solo is refused anything but usage: b, as a device of a, asks for a thread's messages.
	b.remote.mu.Lock()
	l := b.remote.saved.Links[0]
	b.remote.mu.Unlock()
	peer, err := b.remote.openPeer(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	var e *rpc.Error
	if err := peer.Call(ctx, "search", map[string]any{"query": "token", "limit": 5}, nil); !errors.As(err, &e) || e.Code != rpc.CodeDenied {
		t.Errorf("search by a linked Solo: %v", err)
	}
	peer.Close()

	// Unlinking at a removes b's way in: b can't read a's usage any more.
	if err := a.remote.Unlink(after[0].ID); err != nil {
		t.Fatal(err)
	}
	if st := a.remote.State(); len(st.Links) != 0 || len(st.Devices) != 0 {
		t.Errorf("after unlinking, a = links %+v, devices %+v", st.Links, st.Devices)
	}
	if got := b.remote.LinkedUsage(ctx); len(got) != 1 || got[0].Err == "" || got[0].At.IsZero() {
		t.Errorf("b's linked usage after a unlinked = %+v, want an error and the last copy", got)
	}
}

// A device that isn't a Solo can't pass for one by asking to link without a code.
func TestLinkingNeedsACode(t *testing.T) {
	w := newWorld(t)
	w.open2()
	req, err := device.PairOpen(context.Background(), w.dial, w.origin, newKey(t), "Laptop", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w.wait("the request", func() bool { return len(w.remote.State().Requests) == 1 })
	d, err := w.remote.Approve(req.Digits, false)
	if err != nil || d.Linked {
		t.Errorf("approved %+v, %v", d, err)
	}
}

// A link to this machine's own pairing link is refused.
func TestLinkToSelf(t *testing.T) {
	w := newWorld(t)
	p := w.offer()
	if err := w.remote.LinkTo(p.Link); err == nil {
		t.Error("linked to itself")
	}
	if err := w.remote.LinkTo("https://example.com/"); err == nil {
		t.Error("took a link with no code")
	}
}

// grantFor is a grant from another machine (a stand-in for b, with its own seed) for w's link key.
func (w *world) grantFor(t *testing.T, other *world) rpc.LinkGrant {
	t.Helper()
	seed, err := secure.NewSeed(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m, err := secure.MachineFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return rpc.LinkGrant{Relay: other.url, ID: m.ID.String(), Key: m.Noise.Public, PSK: make([]byte, 32),
		Voucher: m.Voucher(other.origin, secure.DeviceID(w.remote.linkKey()), time.Now()), Name: "elsewhere"}
}
