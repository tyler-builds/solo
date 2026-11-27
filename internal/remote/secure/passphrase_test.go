package secure

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/flynn/noise"
)

// passRun runs a passphrase handshake between a client that types guess and a machine with rec, the
// two ends naming the machines they think they are at, and returns the channels or why it failed.
func passRun(t *testing.T, rec PassRecord, guess string, clientMachine, serverMachine ID, tamper func(frame int, b []byte)) (*Channel, *Channel, error) {
	t.Helper()
	const origin = "https://relay.example"
	ck, sk := key(t), key(t)
	c, f1, err := StartPass([]byte(guess))
	if err != nil {
		t.Fatal(err)
	}
	if tamper != nil {
		tamper(1, f1)
	}
	s, f2, err := RespondPass(rec, f1)
	if err != nil {
		return nil, nil, err
	}
	if tamper != nil {
		tamper(2, f2)
	}
	f3, err := c.Answer(f2, origin, clientMachine, ck, rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if tamper != nil {
		tamper(3, f3)
	}
	f4, err := s.Verify(f3, origin, serverMachine, sk, rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	f5, cch, err := c.Finish(f4)
	if err != nil {
		return nil, nil, err
	}
	sch, err := s.Finish(f5)
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(cch.Peer(), sk.Public) || !bytes.Equal(sch.Peer(), ck.Public) {
		t.Fatal("the ends didn't learn each other's keys")
	}
	return cch, sch, nil
}

func TestPassphrase(t *testing.T) {
	rec, err := NewPassRecord([]byte("orbit lantern mossy quarry"))
	if err != nil {
		t.Fatal(err)
	}
	// What a machine keeps comes back as it was.
	if back, err := DecodePassRecord(rec.Encode()); err != nil || !bytes.Equal(back.Record, rec.Record) {
		t.Fatalf("the record doesn't keep: %v", err)
	}
	m := ID{1}

	c, s, err := passRun(t, rec, "orbit lantern mossy quarry", m, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := c.Seal([]byte("hello"))
	if got, err := s.Open(sealed); err != nil || string(got) != "hello" {
		t.Fatalf("the channel doesn't carry: %q %v", got, err)
	}
	if !bytes.Equal(c.Derive("member"), s.Derive("member")) || bytes.Equal(c.Derive("member"), c.Derive("other")) {
		t.Error("the ends don't derive the same keys, or one key serves two uses")
	}

	if _, _, err := passRun(t, rec, "orbit lantern mossy quarrz", m, m, nil); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("a wrong passphrase: %v", err)
	}
	// The machine the client meant must be the one that answers: a relay that sends it elsewhere fails it.
	if _, _, err := passRun(t, rec, "orbit lantern mossy quarry", m, ID{2}, nil); err == nil {
		t.Error("a handshake with another machine than meant succeeded")
	}
	// Another machine's record of the same passphrase answers it too: every machine of a network does.
	other, _ := NewPassRecord([]byte("orbit lantern mossy quarry"))
	if _, _, err := passRun(t, other, "orbit lantern mossy quarry", m, m, nil); err != nil {
		t.Errorf("another machine with the same passphrase: %v", err)
	}
	// Whatever is changed on the way fails it.
	for frame := 1; frame <= 3; frame++ {
		_, _, err := passRun(t, rec, "orbit lantern mossy quarry", m, m, func(f int, b []byte) {
			if f == frame {
				b[len(b)/2] ^= 1
			}
		})
		if err == nil {
			t.Errorf("frame %d altered, and the handshake succeeded", frame)
		}
	}
}

func key(t *testing.T) noise.DHKey {
	t.Helper()
	k, err := NewKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
