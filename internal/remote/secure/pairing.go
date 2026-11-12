package secure

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

// A PairingCode is what a machine shows, as a QR code and a link, to pair one device: which machine it
// is, the public key the device must find at the other end, and a secret used once. Whoever holds the
// code within its few minutes can ask to pair; the person at the machine still approves (see SAS).
type PairingCode struct {
	Machine    ID
	MachineKey [KeySize]byte // the machine's Noise public key
	Secret     [KeySize]byte
	Expires    time.Time
}

// codeVersion starts an encoded code, so a later format can be told from this one.
const codeVersion = 1

const codeSize = 1 + len(ID{}) + KeySize + KeySize + 8

// Encode returns the code as text for a URL's fragment, where no server sees it.
func (c PairingCode) Encode() string {
	b := make([]byte, 0, codeSize)
	b = append(b, codeVersion)
	b = append(b, c.Machine[:]...)
	b = append(b, c.MachineKey[:]...)
	b = append(b, c.Secret[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(c.Expires.Unix()))
	return base64.RawURLEncoding.EncodeToString(b)
}

// ParsePairingCode reads a code. It does not check the expiry: the machine's clock decides that, and
// the device's may be wrong.
func ParsePairingCode(s string) (PairingCode, error) {
	var c PairingCode
	// A code has one text form, so two texts never mean the same code: strict decoding refuses spare
	// bits, and the length check refuses the line breaks that base64 decoding skips.
	if len(s) != base64.RawURLEncoding.EncodedLen(codeSize) {
		return c, errors.New("secure: not a pairing code")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != codeSize || b[0] != codeVersion {
		return c, errors.New("secure: not a pairing code")
	}
	b = b[1:]
	b = b[copy(c.Machine[:], b):]
	b = b[copy(c.MachineKey[:], b):]
	b = b[copy(c.Secret[:], b):]
	c.Expires = time.Unix(int64(binary.BigEndian.Uint64(b)), 0)
	return c, nil
}

// PSK is the pre-shared key of the pairing handshake.
func (c PairingCode) PSK() [KeySize]byte { return derive(c.Secret[:], "pair/psk") }

// Ticket admits the pairing connection at the relay, which is given only its hash (TicketHash): the
// relay can tell that a connection holds the code, and learns nothing that helps it pair.
func (c PairingCode) Ticket() [KeySize]byte { return derive(c.Secret[:], "pair/ticket") }

// TicketHash is what the relay stores of a ticket.
func TicketHash(ticket [KeySize]byte) [sha256.Size]byte { return sha256.Sum256(ticket[:]) }

// hash binds a handshake to the whole code, so a code altered on its way to the device (another
// machine's key swapped in, say) cannot complete one.
func (c PairingCode) hash() []byte {
	sum := sha256.Sum256([]byte(c.Encode()))
	return sum[:]
}
