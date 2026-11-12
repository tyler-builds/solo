//go:build !js

package secure

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/flynn/noise"
)

const origin = "https://solo-relay.example.workers.dev"

type ends struct {
	machine Machine
	device  noise.DHKey
	code    PairingCode
}

func newEnds(t testing.TB) ends {
	t.Helper()
	seed, err := NewSeed(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m, err := MachineFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.NewPairingCode(rand.Reader, time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return ends{m, d, c}
}

func (e ends) pairParams() Params {
	return Params{Purpose: Pair, Origin: origin, Machine: e.machine.ID, Code: &e.code}
}

func (e ends) sessionParams() Params {
	return Params{Purpose: Session, Origin: origin, Machine: e.machine.ID}
}

// shake runs a whole handshake and returns both channels.
func shake(t testing.TB, dp, mp Params, e ends, devicePSK, machinePSK []byte) (device, machine *Channel, err error) {
	t.Helper()
	ini, first, err := Initiate(dp, e.device, e.machine.Noise.Public, devicePSK, rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	machine, answer, err := Respond(mp, e.machine.Noise, machinePSK, first, rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	device, err = ini.Finish(answer)
	return device, machine, err
}

// A device that holds the pairing code and the machine agree on a channel: each opens what the other
// seals, the machine learns the device's key, and both show the same six digits.
func TestPairingHandshake(t *testing.T) {
	e := newEnds(t)
	psk := e.code.PSK()
	d, m, err := shake(t, e.pairParams(), e.pairParams(), e, psk[:], psk[:])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.Peer(), e.device.Public) || !bytes.Equal(d.Peer(), e.machine.Noise.Public) {
		t.Error("the ends did not learn each other's keys")
	}
	if d.SAS() != m.SAS() || len(d.SAS()) != 6 || strings.Trim(d.SAS(), "0123456789") != "" {
		t.Errorf("the device shows %q and the machine %q, want the same six digits", d.SAS(), m.SAS())
	}
	for i, msg := range []string{"hello", "", strings.Repeat("x", MaxPlaintext)} {
		sealed, err := d.Seal([]byte(msg))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(sealed, []byte("hello")) {
			t.Error("a sealed message shows its text")
		}
		got, err := m.Open(sealed)
		if err != nil || string(got) != msg {
			t.Fatalf("message %d did not cross: %v", i, err)
		}
		back, _ := m.Seal([]byte("ack"))
		if got, err := d.Open(back); err != nil || string(got) != "ack" {
			t.Fatalf("answer %d did not cross: %v", i, err)
		}
	}
	if _, err := d.Seal(make([]byte, MaxPlaintext+1)); err == nil {
		t.Error("a message over the limit was sealed")
	}
	// Two pairings never show the same digits by design of the hash; check they at least differ here.
	d2, _, _ := shake(t, e.pairParams(), e.pairParams(), e, psk[:], psk[:])
	if d2.SAS() == d.SAS() {
		t.Log("two handshakes showed the same digits: possible, one in a million")
	}
}

// Everything the two ends must agree on: when any of it differs, the handshake fails, and where it can,
// at the machine's first look, before the machine has shown or stored anything.
func TestHandshakeFailsOnAnyDisagreement(t *testing.T) {
	e := newEnds(t)
	other := newEnds(t)
	psk := e.code.PSK()
	wrong := other.code.PSK()
	atFirst := func(name string, dp, mp Params, dpsk, mpsk []byte) {
		t.Helper()
		ini, first, err := Initiate(dp, e.device, e.machine.Noise.Public, dpsk, rand.Reader)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = ini
		if _, _, err := Respond(mp, e.machine.Noise, mpsk, first, rand.Reader); err == nil {
			t.Errorf("%s: the machine accepted the first message", name)
		}
	}
	atFirst("a wrong pre-shared key", e.pairParams(), e.pairParams(), wrong[:], psk[:])
	session := e.sessionParams()
	atFirst("a pairing message taken for a session's", e.pairParams(), session, psk[:], psk[:])
	elsewhere := e.pairParams()
	elsewhere.Origin = "https://another.example.workers.dev"
	atFirst("another relay", elsewhere, e.pairParams(), psk[:], psk[:])
	otherMachine := e.pairParams()
	otherMachine.Machine = other.machine.ID
	atFirst("another machine's ID", otherMachine, e.pairParams(), psk[:], psk[:])
	altered := e.pairParams()
	code := e.code
	code.Expires = code.Expires.Add(time.Hour)
	altered.Code = &code
	atFirst("an altered pairing code", altered, e.pairParams(), psk[:], psk[:])

	// A device that was given another machine's key (a swapped QR code) cannot finish with this machine.
	ini, first, _ := Initiate(e.pairParams(), e.device, other.machine.Noise.Public, psk[:], rand.Reader)
	if _, _, err := Respond(e.pairParams(), e.machine.Noise, psk[:], first, rand.Reader); err == nil {
		t.Error("the machine accepted a first message made for another machine's key")
	}
	_ = ini

	// Someone who is not the machine cannot answer for it, even holding the pre-shared key.
	ini, first, _ = Initiate(e.pairParams(), e.device, e.machine.Noise.Public, psk[:], rand.Reader)
	_, forged, err := Respond(e.pairParams(), other.machine.Noise, psk[:], first, rand.Reader)
	if err == nil {
		if _, err := ini.Finish(forged); err == nil {
			t.Error("the device accepted an answer from another machine")
		}
	}
	// And a garbled answer is refused.
	ini, first, _ = Initiate(e.pairParams(), e.device, e.machine.Noise.Public, psk[:], rand.Reader)
	_, answer, _ := Respond(e.pairParams(), e.machine.Noise, psk[:], first, rand.Reader)
	answer[len(answer)/2] ^= 1
	if _, err := ini.Finish(answer); err == nil {
		t.Error("the device accepted an altered answer")
	}
}

// A first message can be recorded and sent again. The machine answers it, as it cannot tell, but whoever
// replayed it cannot make a single message that opens: the machine's rule is to act on nothing until one
// does.
func TestReplayedFirstMessageProvesNothing(t *testing.T) {
	e := newEnds(t)
	psk := e.code.PSK()
	_, first, _ := Initiate(e.pairParams(), e.device, e.machine.Noise.Public, psk[:], rand.Reader)
	if _, _, err := Respond(e.pairParams(), e.machine.Noise, psk[:], first, rand.Reader); err != nil {
		t.Fatal(err)
	}
	m, _, err := Respond(e.pairParams(), e.machine.Noise, psk[:], first, rand.Reader)
	if err != nil {
		t.Fatal("a replayed first message is answered like the first; the test's premise")
	}
	for _, guess := range [][]byte{nil, make([]byte, 16), make([]byte, 64), first} {
		if _, err := m.Open(guess); !errors.Is(err, ErrBroken) {
			t.Fatalf("a message from someone without the device's handshake opened: %v", err)
		}
	}
}

// The channel refuses a message that was altered, repeated, dropped or reordered, and is over from then on.
func TestChannelRefusesTampering(t *testing.T) {
	type attack func(msgs [][]byte) []byte
	for name, a := range map[string]attack{
		"altered":   func(m [][]byte) []byte { c := bytes.Clone(m[0]); c[3] ^= 0x40; return c },
		"truncated": func(m [][]byte) []byte { return m[0][:len(m[0])-1] },
		"extended":  func(m [][]byte) []byte { return append(bytes.Clone(m[0]), 0) },
		"reordered": func(m [][]byte) []byte { return m[1] },
		"empty":     func(m [][]byte) []byte { return nil },
	} {
		e := newEnds(t)
		psk := e.code.PSK()
		d, m, err := shake(t, e.pairParams(), e.pairParams(), e, psk[:], psk[:])
		if err != nil {
			t.Fatal(err)
		}
		one, _ := d.Seal([]byte("one"))
		two, _ := d.Seal([]byte("two"))
		if _, err := m.Open(a([][]byte{one, two})); !errors.Is(err, ErrBroken) {
			t.Errorf("%s: the message opened", name)
		}
		if _, err := m.Open(one); !errors.Is(err, ErrBroken) {
			t.Errorf("%s: the channel went on after a message that did not open", name)
		}
	}
	// A repeated message.
	e := newEnds(t)
	psk := e.code.PSK()
	d, m, _ := shake(t, e.pairParams(), e.pairParams(), e, psk[:], psk[:])
	one, _ := d.Seal([]byte("one"))
	if _, err := m.Open(one); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(one); !errors.Is(err, ErrBroken) {
		t.Error("a repeated message opened twice")
	}
	// A message cannot be turned around and sent back to its sender.
	d, m, _ = shake(t, e.pairParams(), e.pairParams(), e, psk[:], psk[:])
	one, _ = d.Seal([]byte("one"))
	if _, err := d.Open(one); !errors.Is(err, ErrBroken) {
		t.Error("a device opened its own message, reflected")
	}
	_ = m
}

// A session is keyed with the key the machine gave the device when it was paired: a device without it,
// or one that was never paired, is refused at the first message.
func TestSessionHandshake(t *testing.T) {
	e := newEnds(t)
	var key [KeySize]byte
	rand.Read(key[:])
	d, m, err := shake(t, e.sessionParams(), e.sessionParams(), e, key[:], key[:])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.Peer(), e.device.Public) {
		t.Error("the machine did not learn which device it is")
	}
	sealed, _ := d.Seal([]byte("hello"))
	if got, err := m.Open(sealed); err != nil || string(got) != "hello" {
		t.Fatal(err)
	}
	// The pairing code's key is not a session's.
	psk := e.code.PSK()
	if _, _, err := shake(t, e.sessionParams(), e.sessionParams(), e, psk[:], key[:]); err == nil {
		t.Error("a session opened with the pairing code's key")
	}
}

func TestPairingCodeRoundTrip(t *testing.T) {
	e := newEnds(t)
	text := e.code.Encode()
	got, err := ParsePairingCode(text)
	if err != nil {
		t.Fatal(err)
	}
	if got.Machine != e.code.Machine || got.MachineKey != e.code.MachineKey || got.Secret != e.code.Secret ||
		got.Expires.Unix() != e.code.Expires.Unix() {
		t.Error("a pairing code did not survive its text form")
	}
	if strings.ContainsAny(text, "+/=# ") {
		t.Errorf("the code %q is not safe in a URL's fragment", text)
	}
	// The three things made from the secret are different keys, and the relay's hash is none of them.
	psk, ticket := e.code.PSK(), e.code.Ticket()
	hash := TicketHash(ticket)
	if psk == ticket || bytes.Equal(hash[:], psk[:]) || bytes.Equal(hash[:], e.code.Secret[:]) || psk == e.code.Secret {
		t.Error("the secret's uses share a key")
	}
	for _, bad := range []string{"", "not a code", text[:len(text)-2], text + "AA", "AA" + text[2:]} {
		if _, err := ParsePairingCode(bad); err == nil {
			t.Errorf("%q parsed as a pairing code", bad)
		}
	}
}

// The relay checks what a machine signs. A token is good only for the relay it was made for, only as it
// was made, and a machine's token only around the time it was made.
func TestRelayTokens(t *testing.T) {
	e := newEnds(t)
	now := time.Now()
	auth, err := e.machine.MachineAuth(origin, now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, id, err := VerifyMachineAuth(origin, auth, now.Add(30*time.Second))
	if err != nil || !bytes.Equal(key, e.machine.RelayKey()) || id != e.machine.ID {
		t.Fatalf("the machine's own token did not verify: %v", err)
	}
	if _, _, err := VerifyMachineAuth("https://other.example", auth, now); err == nil {
		t.Error("a token verified for another relay")
	}
	if _, _, err := VerifyMachineAuth(origin, auth, now.Add(MachineAuthWindow+time.Second)); err == nil {
		t.Error("an old token verified")
	}
	if _, _, err := VerifyMachineAuth(origin, auth, now.Add(-MachineAuthWindow-time.Second)); err == nil {
		t.Error("a token from the future verified")
	}
	dev := DeviceID(e.device.Public)
	v := e.machine.Voucher(origin, dev, now)
	key, m, d, err := VerifyVoucher(origin, v)
	if err != nil || !bytes.Equal(key, e.machine.RelayKey()) || m != e.machine.ID || d != dev {
		t.Fatalf("the machine's voucher did not verify: %v", err)
	}
	if _, _, _, err := VerifyVoucher("https://other.example", v); err == nil {
		t.Error("a voucher verified for another relay")
	}
	// A voucher is not a machine's token, nor the other way.
	if _, _, err := VerifyMachineAuth(origin, v, now); err == nil {
		t.Error("a voucher passed as a machine's token")
	}
	for i := range v {
		c := bytes.Clone(v)
		c[i] ^= 1
		if _, _, _, err := VerifyVoucher(origin, c); err == nil {
			t.Fatalf("a voucher altered at byte %d verified", i)
		}
	}
	for i := range auth {
		c := bytes.Clone(auth)
		c[i] ^= 1
		if _, _, err := VerifyMachineAuth(origin, c, now); err == nil {
			t.Fatalf("a machine's token altered at byte %d verified", i)
		}
	}
}

// A machine's identity is the same every time it is derived from its seed, and different for another.
func TestMachineIdentityFromSeed(t *testing.T) {
	var seed [KeySize]byte
	seed[0] = 1
	a, _ := MachineFromSeed(seed)
	b, _ := MachineFromSeed(seed)
	seed[0] = 2
	c, _ := MachineFromSeed(seed)
	if a.ID != b.ID || !bytes.Equal(a.Noise.Public, b.Noise.Public) || !bytes.Equal(a.RelayKey(), b.RelayKey()) {
		t.Error("the same seed gave two identities")
	}
	if a.ID == c.ID || bytes.Equal(a.Noise.Public, c.Noise.Public) {
		t.Error("two seeds gave one identity")
	}
	if bytes.Equal(a.Noise.Private, a.relay.Seed()) {
		t.Error("the two keys of a machine are one")
	}
	if got, err := ParseID(a.ID.String()); err != nil || got != a.ID {
		t.Error("an ID did not survive its text form")
	}
}

func FuzzParsePairingCode(f *testing.F) {
	f.Add(newEnds(f).code.Encode())
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		c, err := ParsePairingCode(s)
		if err == nil && c.Encode() != s {
			t.Errorf("%q parsed, and encodes as %q", s, c.Encode())
		}
	})
}

// Whatever arrives, the machine's side of a handshake and a channel fail cleanly: a web page built with
// TinyGo cannot recover from a panic, and a machine must not be stopped by one message.
func FuzzRespondAndOpen(f *testing.F) {
	e := newEnds(f)
	psk := e.code.PSK()
	_, first, _ := Initiate(e.pairParams(), e.device, e.machine.Noise.Public, psk[:], rand.Reader)
	f.Add(first)
	f.Add([]byte{})
	f.Add(make([]byte, 200))
	f.Fuzz(func(t *testing.T, msg []byte) {
		if _, _, err := Respond(e.pairParams(), e.machine.Noise, psk[:], msg, rand.Reader); err == nil && !bytes.Equal(msg, first) {
			t.Error("a first message that the device did not make was accepted")
		}
		_, m, _ := shake(t, e.pairParams(), e.pairParams(), e, psk[:], psk[:])
		if _, err := m.Open(msg); err == nil {
			t.Error("a message that the device did not seal opened")
		}
		ini, _, _ := Initiate(e.pairParams(), e.device, e.machine.Noise.Public, psk[:], rand.Reader)
		if _, err := ini.Finish(msg); err == nil {
			t.Error("an answer that the machine did not make was accepted")
		}
	})
}
