//go:build !js

package secure

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/flynn/noise"
)

const openOrigin = "https://solo.example.workers.dev"

func openKeys(t *testing.T) (device, machine Machine) {
	t.Helper()
	for _, m := range []*Machine{&device, &machine} {
		seed, _ := NewSeed(rand.Reader)
		var err error
		if *m, err = MachineFromSeed(seed); err != nil {
			t.Fatal(err)
		}
	}
	return device, machine
}

// An open pairing with nobody in the middle: both ends get a channel, each learns the other's key, and
// both show the same six digits.
func TestOpenPairing(t *testing.T) {
	d, m := openKeys(t)
	ini, first, err := InitiateOpen(openOrigin, d.Noise, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != OpenFirstSize {
		t.Fatalf("first frame is %d bytes", len(first))
	}
	res, answer, err := RespondOpen(openOrigin, m.Noise, first, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	third, dch, err := ini.Finish(answer)
	if err != nil {
		t.Fatal(err)
	}
	mch, err := res.Finish(third)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dch.Peer(), m.Noise.Public) || !bytes.Equal(mch.Peer(), d.Noise.Public) {
		t.Error("the ends didn't learn each other's keys")
	}
	sealed, _ := dch.Seal([]byte("hello"))
	if plain, err := mch.Open(sealed); err != nil || string(plain) != "hello" {
		t.Errorf("the channel: %q %v", plain, err)
	}
	digits, err := res.Digits(mch, ini.Nonce())
	if err != nil {
		t.Fatal(err)
	}
	if got := OpenDigits(dch, ini.Nonce()); got != digits || len(digits) != 6 {
		t.Errorf("the device shows %q, the machine expects %q", got, digits)
	}

	// The machine holds the device to the number it committed to (and to its key:
	// TestOpenCommitmentBindsTheKey): with another, it would be choosing its digits after the fact.
	other := append([]byte(nil), ini.Nonce()...)
	other[0] ^= 1
	if _, err := res.Digits(mch, other); err == nil {
		t.Error("a number other than the committed one was taken")
	}
	if _, err := res.Digits(mch, other[:8]); err == nil {
		t.Error("a short number was taken")
	}
	// A handshake for another relay, or whose commitment was altered on the way, fails.
	ini2, first2, _ := InitiateOpen(openOrigin, d.Noise, rand.Reader)
	if res2, answer2, err := RespondOpen("https://elsewhere.example", m.Noise, first2, rand.Reader); err == nil {
		if third2, _, err := ini2.Finish(answer2); err == nil {
			if _, err := res2.Finish(third2); err == nil {
				t.Error("a handshake across two origins finished")
			}
		}
	}
	ini3, first3, _ := InitiateOpen(openOrigin, d.Noise, rand.Reader)
	first3[3] ^= 1
	if res3, answer3, err := RespondOpen(openOrigin, m.Noise, first3, rand.Reader); err == nil {
		if third3, _, err := ini3.Finish(answer3); err == nil {
			if _, err := res3.Finish(third3); err == nil {
				t.Error("a handshake whose commitment was altered on the way finished")
			}
		}
	}
	for _, bad := range [][]byte{nil, first[:10], append(append([]byte(nil), first...), 0)} {
		if _, _, err := RespondOpen(openOrigin, m.Noise, bad, rand.Reader); err == nil {
			t.Errorf("a first frame of %d bytes was answered", len(bad))
		}
	}
}

// With the relay in the middle there are two handshakes, and their digits are two independent draws:
// the relay can steer neither. Towards the device it doesn't know the device's number until its own
// part is said; towards the machine it has committed before the machine's random part arrives.
func TestOpenPairingInTheMiddle(t *testing.T) {
	d, m := openKeys(t)
	relay, _ := openKeys(t)
	same := 0
	const rounds = 300
	for i := 0; i < rounds; i++ {
		// Device to relay.
		ini, first, _ := InitiateOpen(openOrigin, d.Noise, rand.Reader)
		toDevice, answer, err := RespondOpen(openOrigin, relay.Noise, first, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		third, dch, err := ini.Finish(answer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := toDevice.Finish(third); err != nil {
			t.Fatal(err)
		}
		shown := OpenDigits(dch, ini.Nonce())

		// Relay to machine, with a key and a number of the relay's own (it has neither of the
		// device's secrets). The device's commitment is no use to it there: the machine would hold it
		// to the device's key.
		fake, first2, _ := InitiateOpen(openOrigin, relay.Noise, rand.Reader)
		res, answer2, err := RespondOpen(openOrigin, m.Noise, first2, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		third2, _, err := fake.Finish(answer2)
		if err != nil {
			t.Fatal(err)
		}
		mch, err := res.Finish(third2)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := res.Digits(mch, fake.Nonce())
		if err != nil {
			t.Fatal(err)
		}
		if expected == shown {
			same++
		}
		// The relay can't pass the device's number off as its own: its key is not the committed one.
		if _, err := res.Digits(mch, ini.Nonce()); err == nil {
			t.Fatal("the machine took a number that wasn't committed with this key")
		}
	}
	if same > 1 { // 300 in a million, twice: about once in twenty million runs
		t.Errorf("the relay's digits matched the device's %d times in %d", same, rounds)
	}

}

// The commitment binds the key as well as the number. The relay can finish a handshake with the machine
// under the device's own commitment, using a key of its own; without the key in the commitment it
// could then try key after key, after seeing the machine's part, until the digits suited it. The
// machine refuses any key but the one committed to.
func TestOpenCommitmentBindsTheKey(t *testing.T) {
	d, m := openKeys(t)
	relay, _ := openKeys(t)
	ini, first, _ := InitiateOpen(openOrigin, d.Noise, rand.Reader)
	// The relay takes the device's commitment and puts its own handshake behind it.
	hs, err := noise.NewHandshakeState(func() noise.Config {
		var commit [32]byte
		copy(commit[:], first)
		cfg, _ := openConfig(openOrigin, commit, relay.Noise, rand.Reader)
		cfg.Initiator = true
		return cfg
	}())
	if err != nil {
		t.Fatal(err)
	}
	msg1, _, _, _ := hs.WriteMessage(nil, nil)
	res, answer, err := RespondOpen(openOrigin, m.Noise, append(append([]byte(nil), first[:32]...), msg1...), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := hs.ReadMessage(nil, answer); err != nil {
		t.Fatal(err)
	}
	third, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mch, err := res.Finish(third)
	if err != nil {
		t.Fatal(err) // the handshake itself finishes: the commitment is only a number to Noise
	}
	// Whatever number the relay then reveals, the device's own included, its key isn't the committed one.
	if _, err := res.Digits(mch, ini.Nonce()); err == nil {
		t.Error("the machine took a key other than the one committed to")
	}
	// And the commitment is to the key: the same number under another key is another commitment.
	if OpenCommitment(d.Noise.Public, ini.Nonce()) == OpenCommitment(relay.Noise.Public, ini.Nonce()) {
		t.Error("the commitment doesn't depend on the key")
	}
}

// The digits depend on the device's number, so they can't be known before it is revealed.
func TestOpenDigitsDependOnTheNumber(t *testing.T) {
	d, m := openKeys(t)
	ini, first, _ := InitiateOpen(openOrigin, d.Noise, rand.Reader)
	_, answer, _ := RespondOpen(openOrigin, m.Noise, first, rand.Reader)
	_, ch, err := ini.Finish(answer)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	n := make([]byte, OpenNonceSize)
	for i := 0; i < 200; i++ {
		rand.Read(n)
		seen[OpenDigits(ch, n)] = true
	}
	if len(seen) < 195 {
		t.Errorf("200 numbers gave %d different sets of digits", len(seen))
	}
}
