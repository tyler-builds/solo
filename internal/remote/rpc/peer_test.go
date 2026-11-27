package rpc

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/core"
)

// A linked Solo is sent none of the machine's state and may only read usage and link back; a device
// that isn't one may not link.
func TestLinkedSolo(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusIdle)
	key := bytes.Repeat([]byte{7}, 32)
	var granted []LinkGrant
	h := NewHost(m, Options{
		LinkKey: func() []byte { return key },
		Link:    func(d Device, g LinkGrant) error { granted = append(granted, g); return nil },
	})
	t.Cleanup(h.Close)
	dev, mach := pipe()
	go h.Serve(mach, Device{ID: "solo", Name: "Laptop", Linked: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := OpenPeer(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var rep core.UsageReport
	if err := p.Call(ctx, "usage", struct{}{}, &rep); err != nil {
		t.Errorf("usage: %v", err)
	}
	var e *Error
	if err := p.Call(ctx, "send", sendParams{Thread: "a", Text: "hi"}, nil); !errors.As(err, &e) || e.Code != CodeDenied {
		t.Errorf("send: %v, want denied", err)
	}
	if err := p.Call(ctx, "search", searchParams{Query: "x", Limit: 1}, nil); !errors.As(err, &e) || e.Code != CodeDenied {
		t.Errorf("search: %v, want denied", err)
	}
	var lk linkKeyResult
	if err := p.Call(ctx, "linkKey", struct{}{}, &lk); err != nil || !bytes.Equal(lk.Key, key) {
		t.Errorf("linkKey: %x %v", lk.Key, err)
	}
	g := LinkGrant{Relay: "https://relay.example", ID: "00112233445566778899aabbccddeeff", Key: key, PSK: key,
		Voucher: []byte{1, 2, 3}, Name: "Laptop"}
	if err := p.Call(ctx, "link", g, nil); err != nil || len(granted) != 1 || granted[0].Name != "Laptop" {
		t.Errorf("link: %v, granted %+v", err, granted)
	}
	g.PSK = nil
	if err := p.Call(ctx, "link", g, nil); !errors.As(err, &e) || e.Code != CodeInvalid {
		t.Errorf("an incomplete link: %v, want invalid", err)
	}
	if len(m.sent) != 0 {
		t.Error("a linked Solo changed the machine")
	}

	// A device that isn't a linked Solo can't ask to link.
	dev2, mach2 := pipe()
	go h.Serve(mach2, Device{ID: "phone", Name: "Phone"})
	p2, err := OpenPeer(ctx, dev2)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if err := p2.Call(ctx, "linkKey", struct{}{}, nil); !errors.As(err, &e) || e.Code != CodeDenied {
		t.Errorf("a device's linkKey: %v, want denied", err)
	}
}
