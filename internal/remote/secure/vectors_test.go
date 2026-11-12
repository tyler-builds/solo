//go:build !js

package secure

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite relay/test/vectors.json")

// zeros is a random source that is not random, so the vectors are the same every time.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0x5a
	}
	return len(p), nil
}

// The relay checks, in TypeScript, the tokens a machine makes here. relay/test/vectors.json holds
// tokens made from a fixed seed and time, with the answers this package gives; the relay's tests check
// the same answers. This test fails when the file is out of date: run it with -update, and the relay's
// tests then say whether the relay still agrees.
func TestRelayVectors(t *testing.T) {
	const origin = "https://solo-relay-test.example.workers.dev"
	var seed [KeySize]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	m, err := MachineFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	seed[0] = 99
	other, _ := MachineFromSeed(seed)
	now := time.UnixMilli(1_800_000_000_000)
	auth, _ := m.MachineAuth(origin, now, zeros{})
	device, _ := ParseID("000102030405060708090a0b0c0d0e0f")
	voucher := m.Voucher(origin, device, now)
	code, _ := m.NewPairingCode(zeros{}, now, 5*time.Minute)
	ticket := code.Ticket()
	hash := TicketHash(ticket)
	b64 := base64.RawURLEncoding.EncodeToString
	flip := func(b []byte, i int) string { c := bytes.Clone(b); c[i] ^= 1; return b64(c) }
	otherAuth, _ := other.MachineAuth(origin, now, zeros{})
	want := map[string]any{
		"origin":      origin,
		"nowMs":       now.UnixMilli(),
		"windowMs":    MachineAuthWindow.Milliseconds(),
		"hostKeys":    b64(m.RelayKey()),
		"machineId":   m.ID.String(),
		"machineKey":  b64(m.RelayKey()),
		"machineAuth": b64(auth),
		"deviceId":    device.String(),
		"voucher":     b64(voucher),
		"pairToken":   b64(code.RelayToken()),
		"ticketHash":  hex.EncodeToString(hash[:]),
		"pairingCode": code.Encode(),
		"rejectAuth": map[string]string{
			"altered signature":  flip(auth, len(auth)-1),
			"altered time":       flip(auth, 1+32+16),
			"altered machine ID": flip(auth, 1+32),
			"wrong version":      flip(auth, 0),
			"truncated":          b64(auth[:len(auth)-1]),
			"a voucher":          b64(voucher),
			"another machine's":  b64(otherAuth),
		},
		"rejectVoucher": map[string]string{
			"altered signature": flip(voucher, len(voucher)-1),
			"altered device":    flip(voucher, 1+32+16),
			"altered machine":   flip(voucher, 1+32),
			"truncated":         b64(voucher[:len(voucher)-1]),
			"a machine's token": b64(auth),
		},
		"otherOrigin": "https://another.example.workers.dev",
	}
	got, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "../../../relay/test/vectors.json"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(have, got) {
		t.Fatalf("relay/test/vectors.json is out of date (%v): run go test ./internal/remote/secure -run TestRelayVectors -update, then the relay's tests", err)
	}
	// The answers the file promises, as this package gives them.
	if key, id, err := VerifyMachineAuth(origin, auth, now); err != nil || id != m.ID || !bytes.Equal(key, m.RelayKey()) {
		t.Errorf("the vector's machine token does not verify: %v", err)
	}
	if _, _, _, err := VerifyVoucher(origin, voucher); err != nil {
		t.Errorf("the vector's voucher does not verify: %v", err)
	}
	if _, _, err := VerifyMachineAuth(origin, otherAuth, now); err != nil {
		t.Errorf("another machine's token is validly signed, by a key the relay's list does not have: %v", err)
	}
}
