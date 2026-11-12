package secure

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/flynn/noise"
)

// Open pairing is for a device that has nothing of the machine's beforehand: a browser on another
// computer, opened at the relay's address, with no camera to scan a code and no way to be handed a
// link. The two ends run a handshake that neither can authenticate (Noise XX: each learns the other's
// key in it), and the person then carries six digits from the device to the machine by hand, which is
// what authenticates it.
//
// Six digits are few, and whoever sits between the two ends, the relay, takes part in both halves of
// what would be two handshakes, one with each end. Without more, it could shape its half of the
// device's handshake until the device showed whatever digits the relay had already got from the
// machine. So the device first commits: before anything else it sends a hash of its own public key and
// of a random number it keeps to itself, and the digits depend on that number, which it reveals only
// once the handshake is done. Now neither handshake can be steered:
//
//   - towards the device, the relay must finish the handshake before it learns the number, so it
//     cannot know what digits the device will show;
//   - towards the machine, the relay is the one who commits, to a key and a number it can no longer
//     change when the machine's random part arrives, and the machine checks both against the
//     commitment.
//
// The two sets of digits are then two independent draws, equal once in a million. The machine answers
// only a handful of handshakes for each time pairing is opened (the caller counts), so that is also
// roughly the chance of whoever controls the relay being paired in place of the device. Pairing with a
// code (handshake.go) has no such chance at all: there the machine's key comes with the code.

// OpenNonceSize is the size of the number a device commits to.
const OpenNonceSize = 32

// Open is the purpose of an open pairing's handshake.
const Open Purpose = 3

// OpenCommitment is what a device sends first: a hash of its public key and its number.
func OpenCommitment(devicePub []byte, nonce []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(Version + "/pair/open/commit"))
	h.Write([]byte{0})
	h.Write(devicePub)
	h.Write(nonce)
	var out [sha256.Size]byte
	h.Sum(out[:0])
	return out
}

func openPrologue(origin string, commit [sha256.Size]byte) ([]byte, error) {
	if origin == "" || len(origin) > 255 {
		return nil, errors.New("secure: a handshake needs the relay's origin")
	}
	b := []byte(Version)
	b = append(b, 0, byte(Open), 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(origin)))
	b = append(b, origin...)
	return append(b, commit[:]...), nil
}

func openConfig(origin string, commit [sha256.Size]byte, static noise.DHKey, random io.Reader) (noise.Config, error) {
	prologue, err := openPrologue(origin, commit)
	if err != nil {
		return noise.Config{}, err
	}
	return noise.Config{CipherSuite: suite, Random: random, Pattern: noise.HandshakeXX, Prologue: prologue, StaticKeypair: static}, nil
}

// OpenInitiator is a device part-way through an open pairing.
type OpenInitiator struct {
	hs    *noise.HandshakeState
	nonce [OpenNonceSize]byte
}

// InitiateOpen starts an open pairing from a device. It returns the first frame for the machine: the
// commitment, then the handshake's first message.
func InitiateOpen(origin string, device noise.DHKey, random io.Reader) (*OpenInitiator, []byte, error) {
	i := &OpenInitiator{}
	if _, err := io.ReadFull(random, i.nonce[:]); err != nil {
		return nil, nil, err
	}
	commit := OpenCommitment(device.Public, i.nonce[:])
	cfg, err := openConfig(origin, commit, device, random)
	if err != nil {
		return nil, nil, err
	}
	cfg.Initiator = true
	if i.hs, err = noise.NewHandshakeState(cfg); err != nil {
		return nil, nil, err
	}
	msg, _, _, err := i.hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return i, append(commit[:], msg...), nil
}

// Finish takes the machine's answer and returns the handshake's last message, for the machine, and the
// channel. The channel's Peer is the machine's public key, which nothing vouches for yet: the device
// keeps it only if the person at the machine approves the digits.
func (i *OpenInitiator) Finish(answer []byte) (third []byte, ch *Channel, err error) {
	payload, _, _, err := i.hs.ReadMessage(nil, answer)
	if err != nil {
		return nil, nil, fmt.Errorf("secure: the machine's answer is not valid: %w", err)
	}
	if len(payload) != 0 {
		return nil, nil, errors.New("secure: a handshake message carries no data")
	}
	third, send, recv, err := i.hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	if send == nil || recv == nil {
		return nil, nil, errors.New("secure: the handshake did not end")
	}
	return third, &Channel{send: send, recv: recv, hash: i.hs.ChannelBinding(), peer: i.hs.PeerStatic()}, nil
}

// Nonce is the number the device committed to. It is sent to the machine on the channel, once the
// handshake is done and not before.
func (i *OpenInitiator) Nonce() []byte { return i.nonce[:] }

// OpenResponder is a machine part-way through an open pairing.
type OpenResponder struct {
	hs     *noise.HandshakeState
	commit [sha256.Size]byte
}

// OpenFirstSize is the size of an open pairing's first frame: the commitment and one public key.
const OpenFirstSize = sha256.Size + KeySize

// RespondOpen answers a device's first frame on the machine.
func RespondOpen(origin string, machine noise.DHKey, first []byte, random io.Reader) (*OpenResponder, []byte, error) {
	if len(first) != OpenFirstSize {
		return nil, nil, errors.New("secure: not the first message of an open pairing")
	}
	r := &OpenResponder{}
	copy(r.commit[:], first)
	cfg, err := openConfig(origin, r.commit, machine, random)
	if err != nil {
		return nil, nil, err
	}
	if r.hs, err = noise.NewHandshakeState(cfg); err != nil {
		return nil, nil, err
	}
	payload, _, _, err := r.hs.ReadMessage(nil, first[sha256.Size:])
	if err != nil {
		return nil, nil, fmt.Errorf("secure: the device's first message is not valid: %w", err)
	}
	if len(payload) != 0 {
		return nil, nil, errors.New("secure: a handshake message carries no data")
	}
	answer, _, _, err := r.hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return r, answer, nil
}

// Finish takes the device's last handshake message and returns the channel. The channel's Peer is the
// device's public key. Nothing is known of the device yet: Digits checks what it committed to.
func (r *OpenResponder) Finish(third []byte) (*Channel, error) {
	payload, recv, send, err := r.hs.ReadMessage(nil, third)
	if err != nil {
		return nil, fmt.Errorf("secure: the device's last message is not valid: %w", err)
	}
	if len(payload) != 0 || send == nil || recv == nil {
		return nil, errors.New("secure: the handshake did not end")
	}
	return &Channel{send: send, recv: recv, hash: r.hs.ChannelBinding(), peer: r.hs.PeerStatic()}, nil
}

// Digits returns the six digits of an open pairing on the machine, given the number the device has now
// revealed. It fails unless the device's key and that number are the ones it committed to before the
// handshake: a device that could choose either afterwards could choose its digits.
func (r *OpenResponder) Digits(ch *Channel, nonce []byte) (string, error) {
	if len(nonce) != OpenNonceSize {
		return "", errors.New("secure: not the number of an open pairing")
	}
	want := OpenCommitment(ch.Peer(), nonce)
	if !equal(want[:], r.commit[:]) {
		return "", errors.New("secure: the device is not the one that committed")
	}
	return OpenDigits(ch, nonce), nil
}

// OpenDigits is the six digits of an open pairing: from the handshake's hash and the device's number.
func OpenDigits(ch *Channel, nonce []byte) string {
	k := derive(append(append([]byte(nil), ch.hash...), nonce...), "pair/open/sas")
	return fmt.Sprintf("%06d", binary.BigEndian.Uint64(k[:8])%1_000_000)
}
