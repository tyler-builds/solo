package rpc

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/core"
)

// A member of the network is sent none of the machine's state and may only read usage, let a device
// in and pass on the passphrase; a device that isn't a member may do neither of the last two.
func TestMember(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusIdle)
	var allowed [][]byte
	var passphrases []string
	h := NewHost(m, Options{
		Allow:      func(d Device, key []byte, readOnly bool) error { allowed = append(allowed, key); return nil },
		Passphrase: func(d Device, p string) error { passphrases = append(passphrases, p); return nil },
	})
	t.Cleanup(h.Close)
	dev, mach := pipe()
	go h.Serve(mach, Device{ID: "member", Name: "laptop", Member: true})
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
	key := bytes.Repeat([]byte{7}, 32)
	if err := p.Call(ctx, "allow", allowParams{Key: key}, nil); err != nil || len(allowed) != 1 || !bytes.Equal(allowed[0], key) {
		t.Errorf("allow: %v, allowed %x", err, allowed)
	}
	if err := p.Call(ctx, "allow", allowParams{Key: key[:5]}, nil); !errors.As(err, &e) || e.Code != CodeInvalid {
		t.Errorf("allow with a short key: %v, want invalid", err)
	}
	if err := p.Call(ctx, "passphrase", passphraseParams{Passphrase: "new one"}, nil); err != nil || len(passphrases) != 1 {
		t.Errorf("passphrase: %v, %v", err, passphrases)
	}
	if len(m.sent) != 0 {
		t.Error("a member changed the machine")
	}

	// A device that isn't a member can't let devices in.
	dev2, mach2 := pipe()
	go h.Serve(mach2, Device{ID: "phone", Name: "Phone"})
	p2, err := OpenPeer(ctx, dev2)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if err := p2.Call(ctx, "allow", allowParams{Key: key}, nil); !errors.As(err, &e) || e.Code != CodeDenied {
		t.Errorf("a device's allow: %v, want denied", err)
	}
}
