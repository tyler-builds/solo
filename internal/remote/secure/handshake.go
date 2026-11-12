package secure

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/flynn/noise"
)

// Purpose says what a handshake is for. It is part of what both ends hash, so a message of a pairing
// can never be taken for one of a session.
type Purpose byte

const (
	// Pair is the handshake of a device that holds a pairing code, keyed with the code's secret.
	Pair Purpose = 1
	// Session is the handshake of a device already paired, keyed with the key the machine gave it.
	Session Purpose = 2
)

// Params is what both ends must agree on before a handshake means anything.
type Params struct {
	Purpose Purpose
	// Origin is the relay's origin, as the device's address bar has it: "https://name.sub.workers.dev".
	Origin  string
	Machine ID
	// Code is the pairing code of a Pair handshake.
	Code *PairingCode
}

// prologue is hashed into the handshake by both ends: if they differ in purpose, relay, machine or
// pairing code, the handshake fails.
func (p Params) prologue() []byte {
	b := []byte(Version)
	b = append(b, 0, byte(p.Purpose), 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(p.Origin)))
	b = append(b, p.Origin...)
	b = append(b, p.Machine[:]...)
	if p.Code != nil {
		b = append(b, p.Code.hash()...)
	}
	return b
}

func (p Params) check() error {
	switch {
	case p.Purpose != Pair && p.Purpose != Session:
		return errors.New("secure: a handshake needs a purpose")
	case p.Origin == "" || len(p.Origin) > 255:
		return errors.New("secure: a handshake needs the relay's origin")
	case (p.Purpose == Pair) != (p.Code != nil):
		return errors.New("secure: a pairing handshake, and only one, carries its code")
	}
	return nil
}

// The handshake is Noise's IK with a pre-shared key mixed in at the first message (IKpsk1): the device
// knows the machine's key beforehand, and the machine rejects, at the first message, a device that does
// not hold the pre-shared key. Placement 2 would have the machine answer anyone who knows its public
// key. Neither message carries data: a first message can be replayed, and so nothing may depend on one.
func config(p Params, static noise.DHKey, psk []byte, random io.Reader) noise.Config {
	return noise.Config{CipherSuite: suite, Random: random, Pattern: noise.HandshakeIK, Prologue: p.prologue(),
		StaticKeypair: static, PresharedKey: psk, PresharedKeyPlacement: 1}
}

// Initiator is a device part-way through a handshake.
type Initiator struct{ hs *noise.HandshakeState }

// Initiate starts a handshake from a device to the machine whose public key it has. It returns the first
// message, for the machine.
func Initiate(p Params, device noise.DHKey, machineKey, psk []byte, random io.Reader) (*Initiator, []byte, error) {
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	if len(machineKey) != KeySize || len(psk) != KeySize {
		return nil, nil, errors.New("secure: a handshake needs the machine's key and a pre-shared key")
	}
	cfg := config(p, device, psk, random)
	cfg.Initiator, cfg.PeerStatic = true, machineKey
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, nil, err
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return &Initiator{hs}, msg, nil
}

// Finish takes the machine's answer and returns the channel. It fails unless the answer came from the
// machine the device meant, which holds the same pre-shared key.
func (i *Initiator) Finish(answer []byte) (*Channel, error) {
	payload, send, recv, err := i.hs.ReadMessage(nil, answer)
	if err != nil {
		return nil, fmt.Errorf("secure: the machine's answer is not valid: %w", err)
	}
	if len(payload) != 0 || send == nil || recv == nil {
		return nil, errors.New("secure: the machine's answer is not a handshake's end")
	}
	return &Channel{send: send, recv: recv, hash: i.hs.ChannelBinding(), peer: i.hs.PeerStatic()}, nil
}

// Respond answers a device's first message on the machine. It fails unless the device holds the
// pre-shared key. The channel's Peer is the device's public key, which a session must find in the
// machine's registry before anything else; the answer goes back to the device.
//
// The channel is not yet proof of anyone: a first message can be replayed. Only a message that Opens on
// it shows the device is there now, so the machine keeps what it has, and says nothing, until one does.
func Respond(p Params, machine noise.DHKey, psk, first []byte, random io.Reader) (*Channel, []byte, error) {
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	if len(psk) != KeySize {
		return nil, nil, errors.New("secure: a handshake needs a pre-shared key")
	}
	hs, err := noise.NewHandshakeState(config(p, machine, psk, random))
	if err != nil {
		return nil, nil, err
	}
	payload, _, _, err := hs.ReadMessage(nil, first)
	if err != nil {
		return nil, nil, fmt.Errorf("secure: the device's first message is not valid: %w", err)
	}
	if len(payload) != 0 {
		return nil, nil, errors.New("secure: a first message carries no data")
	}
	answer, recv, send, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	if send == nil || recv == nil {
		return nil, nil, errors.New("secure: the handshake did not end")
	}
	return &Channel{send: send, recv: recv, hash: hs.ChannelBinding(), peer: hs.PeerStatic()}, answer, nil
}

// MaxPlaintext is the most a Channel seals at once: a Noise message is at most 65535 bytes, 16 of them
// its authentication tag.
const MaxPlaintext = 65535 - 16

// Channel is an end of a finished handshake: it seals what this end sends and opens what the other
// sent. Messages must arrive whole, once and in order, as a WebSocket delivers them; one that does not
// open means the channel is over, and the caller closes it. Sealing and opening may happen on different
// goroutines.
type Channel struct {
	sendMu, recvMu sync.Mutex
	send, recv     *noise.CipherState
	hash           []byte
	peer           []byte
	dead           bool
}

// Peer is the other end's Noise public key.
func (c *Channel) Peer() []byte { return c.peer }

// Seal encrypts a message for the other end.
func (c *Channel) Seal(plaintext []byte) ([]byte, error) {
	if len(plaintext) > MaxPlaintext {
		return nil, errors.New("secure: a sealed message is at most 65519 bytes")
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.send.Encrypt(nil, nil, plaintext)
}

// ErrBroken is what Open returns for a message that was altered, repeated, dropped or reordered, and
// ever after: the two ends no longer agree, and only a new handshake fixes that.
var ErrBroken = errors.New("secure: a message did not open; the channel is over")

// Open decrypts the other end's next message.
func (c *Channel) Open(sealed []byte) ([]byte, error) {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()
	if c.dead {
		return nil, ErrBroken
	}
	plain, err := c.recv.Decrypt(nil, nil, sealed)
	if err != nil {
		c.dead = true
		return nil, ErrBroken
	}
	return plain, nil
}

// SAS is the six digits both ends of a pairing show. They come from the handshake's hash, which no one
// in the middle can make equal for two different handshakes, so equal digits mean the device in the
// user's hand is the one the machine is about to trust. The user types the device's digits into the
// machine, which approves the request that has them: a request from someone else who got hold of the
// code has other digits.
func (c *Channel) SAS() string {
	k := derive(c.hash, "pair/sas")
	return fmt.Sprintf("%06d", binary.BigEndian.Uint64(k[:8])%1_000_000)
}
