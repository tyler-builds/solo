// Package secure is the cryptography of Solo's remote access: who a machine and a device are, how a
// device is paired with a machine, and the channel the two then talk over. The relay between them
// carries its bytes and is trusted with nothing: not to read them, not to change them, and not to say
// who is at the other end.
//
// It composes, and adds no primitive of its own: the Noise protocol (github.com/flynn/noise) with
// X25519, ChaCha20-Poly1305 and BLAKE2s, and HKDF to derive keys from secrets. docs/REMOTE.md
// describes the protocol and what it defends against.
//
// The package builds for a web page too. What only a machine does (its identity from a seed, signing
// for the relay) is in files a page leaves out.
package secure

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"

	"github.com/flynn/noise"
	"golang.org/x/crypto/hkdf"
)

// Version is the protocol's version: it is in every key derivation and every handshake, so two ends on
// different versions cannot agree on anything.
const Version = "solo/remote/v1"

// MachineAuthSize and VoucherSize are the sizes of the two tokens a machine signs for the relay
// (machine.go).
const (
	MachineAuthSize = 137
	VoucherSize     = 137
)

// KeySize is the size of every key and secret here: X25519 keys, pre-shared keys, seeds.
const KeySize = 32

// ID names a machine or a device: a hash of its public keys, so a name cannot be claimed without them.
type ID [16]byte

func (id ID) String() string { return hex.EncodeToString(id[:]) }

// ParseID reads an ID from its hexadecimal form.
func ParseID(s string) (ID, error) {
	var id ID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, errors.New("secure: an ID is 32 hexadecimal characters")
	}
	copy(id[:], b)
	return id, nil
}

// DeviceID is the ID of the device whose Noise public key is pub.
func DeviceID(pub []byte) ID { return idOf("device", pub) }

// MachineID is the ID of the machine with these Noise and relay public keys.
func MachineID(noisePub, relayPub []byte) ID { return idOf("machine", noisePub, relayPub) }

func idOf(kind string, parts ...[]byte) ID {
	h := sha256.New()
	h.Write([]byte(Version + "/id/" + kind))
	for _, p := range parts {
		h.Write([]byte{byte(len(p))})
		h.Write(p)
	}
	var id ID
	copy(id[:], h.Sum(nil))
	return id
}

// derive makes a key from a secret for one named use, so no key serves two purposes.
func derive(secret []byte, use string) [KeySize]byte {
	var out [KeySize]byte
	r := hkdf.New(sha256.New, secret, nil, []byte(Version+"/"+use))
	if _, err := io.ReadFull(r, out[:]); err != nil {
		panic("secure: hkdf: " + err.Error()) // cannot happen for 32 bytes
	}
	return out
}

// suite is the one cipher suite: there is nothing to negotiate, and so nothing to downgrade.
var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

// NewKey makes a Noise key pair from the random source, which must be the system's.
func NewKey(random io.Reader) (noise.DHKey, error) { return suite.GenerateKeypair(random) }

// equal compares two secrets or keys without telling, by how long it takes, where they differ.
func equal(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }
