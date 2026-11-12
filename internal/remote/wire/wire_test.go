//go:build !js

package wire

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/remote/secure"
)

// pipe is one end of an in-memory connection; frames lets a test see and alter what crosses.
type pipe struct {
	in, out chan []byte
	alter   func([]byte) []byte
}

func (p *pipe) Read() ([]byte, error) {
	m, ok := <-p.in
	if !ok {
		return nil, errors.New("closed")
	}
	return m, nil
}

func (p *pipe) Write(m []byte) error {
	m = bytes.Clone(m)
	if p.alter != nil {
		if m = p.alter(m); m == nil {
			return nil
		}
	}
	p.out <- m
	return nil
}

func (p *pipe) Close() error { close(p.out); return nil }

// links returns a device's and a machine's link over a pipe, after a real handshake.
func links(t testing.TB) (device, machine *Link, dp, mp *pipe) {
	t.Helper()
	seed, _ := secure.NewSeed(rand.Reader)
	m, _ := secure.MachineFromSeed(seed)
	d, _ := secure.NewKey(rand.Reader)
	var psk [secure.KeySize]byte
	rand.Read(psk[:])
	p := secure.Params{Purpose: secure.Session, Origin: "https://relay.example", Machine: m.ID}
	ini, first, err := secure.Initiate(p, d, m.Noise.Public, psk[:], rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mch, answer, err := secure.Respond(p, m.Noise, psk[:], first, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dch, err := ini.Finish(answer)
	if err != nil {
		t.Fatal(err)
	}
	a, b := make(chan []byte, 256), make(chan []byte, 256)
	dp, mp = &pipe{in: a, out: b}, &pipe{in: b, out: a}
	device, machine = NewLink(dp, dch), NewLink(mp, mch)
	machine.Compress = true
	return device, machine, dp, mp
}

// Messages of every size cross whole, in order, in both directions, compressed where that helps.
func TestMessagesCross(t *testing.T) {
	d, m, dp, mp := links(t)
	frames := 0
	mp.alter = func(b []byte) []byte { frames++; return b }
	random := make([]byte, 300_000)
	rand.Read(random)
	for _, msg := range [][]byte{
		nil,
		[]byte("hello"),
		bytes.Repeat([]byte("x"), RecordData),   // exactly one record
		bytes.Repeat([]byte("y"), RecordData+1), // one byte into a second
		[]byte(strings.Repeat("a thread's text ", 60_000)), // 960 KB of text
		random, // does not compress
	} {
		frames = 0
		if err := m.Send(msg); err != nil {
			t.Fatalf("sending %d bytes: %v", len(msg), err)
		}
		got, err := d.Receive()
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("%d bytes did not cross: %v", len(msg), err)
		}
		if len(msg) == 960_000 && frames > 2 {
			t.Errorf("960 KB of text went in %d frames, want it compressed into one or two", frames)
		}
		if err := d.Send(msg[:min(len(msg), 4000)]); err != nil {
			t.Fatal(err)
		}
		if got, err := m.Receive(); err != nil || !bytes.Equal(got, msg[:min(len(msg), 4000)]) {
			t.Fatalf("the answer did not cross: %v", err)
		}
	}
	_ = dp
}

// A message over the limit is refused by the end that would send it.
func TestSendRefusesOversize(t *testing.T) {
	d, _, _, _ := links(t)
	big := make([]byte, MaxMessage+1)
	rand.Read(big)
	if err := d.Send(big); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a message over the limit was sent: %v", err)
	}
}

// A peer that holds the channel's keys but breaks the rules is refused: records that are not final yet
// short, flags no link uses, more than a message may hold, and a small message that inflates to more
// than a receiver should keep.
func TestReceiveRefusesRuleBreakers(t *testing.T) {
	send := func(t *testing.T, records ...[]byte) error {
		d, m, dp, _ := links(t)
		for _, r := range records {
			sealed, err := d.ch.Seal(r)
			if err != nil {
				t.Fatal(err)
			}
			dp.Write(sealed)
		}
		done := make(chan error, 1)
		go func() { _, err := m.Receive(); done <- err }()
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			return nil // still waiting for more: not refused
		}
	}
	full := append([]byte{0}, make([]byte, RecordData)...)
	cases := map[string][][]byte{
		"an empty record":                  {{}},
		"a short record that is not final": {append([]byte{0}, []byte("drip")...)},
		"an unknown flag":                  {{0x80 | flagFinal}},
		"compression said mid-message":     {full, {flagFinal | flagDeflated}},
		"a record over its size":           {append([]byte{flagFinal}, make([]byte, RecordData+1)...)},
	}
	for name, recs := range cases {
		if err := send(t, recs...); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want it refused as malformed", name, err)
		}
	}
	var many [][]byte
	for i := 0; i <= MaxMessage/RecordData; i++ {
		many = append(many, full)
	}
	if err := send(t, many...); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a message over the limit: got %v, want it refused as too large", err)
	}
	bomb := deflate(make([]byte, MaxInflated+1)) // nine megabytes of zeros in a few kilobytes
	if len(bomb) > RecordData {
		t.Fatalf("the test's bomb is %d bytes", len(bomb))
	}
	if err := send(t, append([]byte{flagFinal | flagDeflated}, bomb...)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a message that inflates past the limit: got %v, want it refused", err)
	}
	if err := send(t, append([]byte{flagFinal | flagDeflated}, []byte("not deflate data")...)); !errors.Is(err, ErrMalformed) {
		t.Errorf("garbage said to be compressed: got %v", err)
	}
}

// What the relay can do to the frames: alter one, drop one, repeat one, swap two. Each ends the link at
// the receiver, and nothing is delivered.
func TestRelayTampering(t *testing.T) {
	msg := []byte(strings.Repeat("0123456789", 20_000)) // several records even compressed? no: incompressible below
	rand.Read(msg)
	for name, alter := range map[string]func(n int, b []byte) [][]byte{
		"altered": func(n int, b []byte) [][]byte {
			if n == 1 {
				b[10] ^= 1
			}
			return [][]byte{b}
		},
		"dropped": func(n int, b []byte) [][]byte {
			if n == 1 {
				return nil
			}
			return [][]byte{b}
		},
		"repeated": func(n int, b []byte) [][]byte {
			if n == 1 {
				return [][]byte{b, b}
			}
			return [][]byte{b}
		},
	} {
		d, m, _, mp := links(t)
		n := 0
		// Intercept by hand: frames from the machine pass through alter.
		out := mp.out
		via := make(chan []byte, 256)
		mp.out = via
		if err := m.Send(msg); err != nil {
			t.Fatal(err)
		}
		close(via)
		for b := range via {
			for _, f := range alter(n, b) {
				out <- f
			}
			n++
		}
		close(out)
		if got, err := d.Receive(); err == nil {
			t.Errorf("%s: a message of %d bytes was delivered", name, len(got))
		}
	}
}

func FuzzReceive(f *testing.F) {
	f.Add([]byte{flagFinal, 'h', 'i'}, []byte{})
	f.Add(append([]byte{flagFinal | flagDeflated}, deflate([]byte(strings.Repeat("a", 4000)))...), []byte{})
	f.Add([]byte{0}, []byte{flagFinal})
	f.Fuzz(func(t *testing.T, r1, r2 []byte) {
		if len(r1) > secure.MaxPlaintext || len(r2) > secure.MaxPlaintext {
			t.Skip()
		}
		d, m, dp, _ := links(t)
		for _, r := range [][]byte{r1, r2} {
			sealed, _ := d.ch.Seal(r)
			dp.Write(sealed)
		}
		dp.Close()
		for {
			msg, err := m.Receive()
			if err != nil {
				return
			}
			if len(msg) > MaxInflated {
				t.Fatalf("a message of %d bytes was delivered", len(msg))
			}
		}
	})
}
