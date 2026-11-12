//go:build !js

package secure

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/flynn/noise"
	"golang.org/x/crypto/curve25519"
)

// Machine is a machine's identity: the key it speaks Noise with, and the key it signs for the relay
// with. Both come from one seed, which is the only thing a machine has to keep secret.
type Machine struct {
	ID    ID
	Noise noise.DHKey
	relay ed25519.PrivateKey
}

// NewSeed makes a machine's seed from the system's random source.
func NewSeed(random io.Reader) ([KeySize]byte, error) {
	var seed [KeySize]byte
	_, err := io.ReadFull(random, seed[:])
	return seed, err
}

// MachineFromSeed derives a machine's identity from its seed.
func MachineFromSeed(seed [KeySize]byte) (Machine, error) {
	priv := derive(seed[:], "machine/noise")
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return Machine{}, err
	}
	rseed := derive(seed[:], "machine/relay")
	relay := ed25519.NewKeyFromSeed(rseed[:])
	m := Machine{Noise: noise.DHKey{Private: priv[:], Public: pub}, relay: relay}
	m.ID = MachineID(pub, m.RelayKey())
	return m, nil
}

// RelayKey is the public key the relay knows the machine by: the relay's list of them is what lets a
// machine connect, and only someone with the Cloudflare account can change that list.
func (m Machine) RelayKey() []byte { return []byte(m.relay.Public().(ed25519.PublicKey)) }

// NewPairingCode makes a code for one device, good for ttl.
func (m Machine) NewPairingCode(random io.Reader, now time.Time, ttl time.Duration) (PairingCode, error) {
	c := PairingCode{Machine: m.ID, Expires: now.Add(ttl)}
	copy(c.MachineKey[:], m.Noise.Public)
	_, err := io.ReadFull(random, c.Secret[:])
	return c, err
}

// What the machine signs for the relay. Each is its fields and a signature over a context string, the
// relay's origin and those fields, so a signature made for one purpose or one relay is no use for
// another. The relay checks them without keeping anything (relay/src/auth.ts).
const (
	authVersion = 1
	// MachineAuthSize and VoucherSize are the sizes of the two tokens.
	MachineAuthSize = 1 + ed25519.PublicKeySize + 16 + 8 + 16 + ed25519.SignatureSize
	VoucherSize     = 1 + ed25519.PublicKeySize + 16 + 16 + 8 + ed25519.SignatureSize
)

func signed(context, origin string, body []byte) []byte {
	b := []byte(Version + "/relay/" + context)
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(origin)))
	b = append(b, origin...)
	return append(b, body...)
}

// MachineAuth is what the machine presents to connect to the relay at origin: its relay key, its ID,
// the time and a nonce, signed. The relay accepts it for a couple of minutes around its own clock.
func (m Machine) MachineAuth(origin string, now time.Time, random io.Reader) ([]byte, error) {
	b := make([]byte, 0, MachineAuthSize)
	b = append(b, authVersion)
	b = append(b, m.RelayKey()...)
	b = append(b, m.ID[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(now.UnixMilli()))
	var nonce [16]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return nil, err
	}
	b = append(b, nonce[:]...)
	return append(b, ed25519.Sign(m.relay, signed("machine", origin, b))...), nil
}

// Voucher is what a paired device presents to the relay to reach this machine: the machine's word, for
// the relay at origin, that the device is one of its own. It is a pass to the relay, not to the machine:
// the machine still demands the device's own key in the handshake.
func (m Machine) Voucher(origin string, device ID, now time.Time) []byte {
	b := make([]byte, 0, VoucherSize)
	b = append(b, authVersion)
	b = append(b, m.RelayKey()...)
	b = append(b, m.ID[:]...)
	b = append(b, device[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(now.Unix()))
	return append(b, ed25519.Sign(m.relay, signed("voucher", origin, b))...)
}

// VerifyMachineAuth and VerifyVoucher check a token as the relay does, and return the relay key that
// signed it; the relay then looks that key up in its list. They are here so the Go side and the relay's
// TypeScript are tested against the same tokens.
func VerifyMachineAuth(origin string, token []byte, now time.Time) (relayKey []byte, machine ID, err error) {
	if len(token) != MachineAuthSize || token[0] != authVersion {
		return nil, machine, errors.New("secure: not a machine's token")
	}
	body, sig := token[:len(token)-ed25519.SignatureSize], token[len(token)-ed25519.SignatureSize:]
	key := body[1 : 1+ed25519.PublicKeySize]
	copy(machine[:], body[1+ed25519.PublicKeySize:])
	at := time.UnixMilli(int64(binary.BigEndian.Uint64(body[1+ed25519.PublicKeySize+16:])))
	if d := now.Sub(at); d > MachineAuthWindow || d < -MachineAuthWindow {
		return nil, machine, errors.New("secure: the machine's token is not from now")
	}
	if !ed25519.Verify(ed25519.PublicKey(key), signed("machine", origin, body), sig) {
		return nil, machine, errors.New("secure: the machine's token is not signed by its key")
	}
	return key, machine, nil
}

// MachineAuthWindow is how far from the relay's clock a machine's token may be.
const MachineAuthWindow = 2 * time.Minute

// VerifyVoucher checks a voucher as the relay does.
func VerifyVoucher(origin string, voucher []byte) (relayKey []byte, machine, device ID, err error) {
	if len(voucher) != VoucherSize || voucher[0] != authVersion {
		return nil, machine, device, errors.New("secure: not a voucher")
	}
	body, sig := voucher[:len(voucher)-ed25519.SignatureSize], voucher[len(voucher)-ed25519.SignatureSize:]
	key := body[1 : 1+ed25519.PublicKeySize]
	copy(machine[:], body[1+ed25519.PublicKeySize:])
	copy(device[:], body[1+ed25519.PublicKeySize+16:])
	if !ed25519.Verify(ed25519.PublicKey(key), signed("voucher", origin, body), sig) {
		return nil, machine, device, errors.New("secure: the voucher is not signed by its machine")
	}
	return key, machine, device, nil
}
