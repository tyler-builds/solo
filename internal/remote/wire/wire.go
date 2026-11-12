// Package wire carries messages over a secure channel: it cuts a message into records the channel can
// seal, sends each as one frame of the connection underneath, and puts them together again at the
// other end. One message crosses at a time in each direction; anything large is asked for in pages by
// the layer above, so a message is never more than a megabyte and nothing waits long behind another.
//
// The other end is authenticated but not trusted to behave: a paired device may have been taken over.
// So every size is checked before anything is kept, and a message that breaks a rule ends the link.
package wire

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"sync"

	"github.com/tyler-builds/solo/internal/remote/secure"
)

// Conn carries whole frames, in order: the page's WebSocket, or a machine's connection for one device
// through the relay.
type Conn interface {
	Read() ([]byte, error)
	Write([]byte) error
	Close() error
}

const (
	// RecordData is the most message bytes in one record. A record is a flag byte and its data, sealed:
	// 60,017 bytes, under the 64 KiB the relay forwards and the 65,535 a Noise message holds.
	RecordData = 60_000
	// MaxMessage is the most bytes of a message as it crosses, compressed or not.
	MaxMessage = 1 << 20
	// MaxInflated is the most a compressed message may grow to: a small message that inflates to
	// gigabytes would otherwise fill the receiver's memory.
	MaxInflated = 8 << 20
	// compressFrom is the size from which a message is worth compressing.
	compressFrom = 512
)

const (
	flagFinal    = 1 << iota // the message's last record
	flagDeflated             // the message is compressed (set on its first record)
)

// Errors that end a link.
var (
	ErrTooLarge  = errors.New("wire: a message is larger than a link carries")
	ErrMalformed = errors.New("wire: a record is not one this link understands")
)

// Link is one end of a secure channel over a connection.
type Link struct {
	conn Conn
	ch   *secure.Channel
	wmu  sync.Mutex
	// Compress makes Send compress messages worth it. Set it before use. Text from a machine
	// compresses five times or more, and the relay's quota counts frames; a page leaves it off, since
	// what it sends is small.
	Compress bool
}

// NewLink makes a link from a finished handshake's channel and the connection it happened on.
func NewLink(conn Conn, ch *secure.Channel) *Link { return &Link{conn: conn, ch: ch} }

// Send sends one message. It may be called from several goroutines; messages do not interleave.
func (l *Link) Send(msg []byte) error {
	flags := byte(0)
	if l.Compress && len(msg) >= compressFrom {
		if small := deflate(msg); small != nil && len(small) < len(msg) {
			msg, flags = small, flagDeflated
		}
	}
	if len(msg) > MaxMessage {
		return ErrTooLarge
	}
	l.wmu.Lock()
	defer l.wmu.Unlock()
	rec := make([]byte, 0, 1+min(len(msg), RecordData))
	for first := true; first || len(msg) > 0; first = false {
		n := min(len(msg), RecordData)
		f := flags
		if !first {
			f = 0 // the compression flag is the message's, said once
		}
		if n == len(msg) {
			f |= flagFinal
		}
		rec = append(append(rec[:0], f), msg[:n]...)
		msg = msg[n:]
		sealed, err := l.ch.Seal(rec)
		if err != nil {
			return err
		}
		if err := l.conn.Write(sealed); err != nil {
			return err
		}
	}
	return nil
}

// Receive returns the next message. Any error ends the link: the caller closes the connection. Call it
// from one goroutine.
func (l *Link) Receive() ([]byte, error) {
	var msg []byte
	deflated := false
	for first := true; ; first = false {
		sealed, err := l.conn.Read()
		if err != nil {
			return nil, err
		}
		rec, err := l.ch.Open(sealed)
		if err != nil {
			return nil, err
		}
		if len(rec) < 1 || len(rec) > 1+RecordData {
			return nil, ErrMalformed
		}
		flags, data := rec[0], rec[1:]
		switch {
		case flags&^(flagFinal|flagDeflated) != 0:
			return nil, ErrMalformed
		case flags&flagDeflated != 0 && !first:
			return nil, ErrMalformed
		case flags&flagFinal == 0 && len(data) != RecordData:
			// Only a message's last record may be short: a peer cannot spend the relay's quota, which
			// counts frames, on a message of one-byte records.
			return nil, ErrMalformed
		}
		if first {
			deflated = flags&flagDeflated != 0
		}
		if len(msg)+len(data) > MaxMessage {
			return nil, ErrTooLarge
		}
		msg = append(msg, data...)
		if flags&flagFinal != 0 {
			break
		}
	}
	if !deflated {
		return msg, nil
	}
	return inflate(msg)
}

// Close closes the connection underneath.
func (l *Link) Close() error { return l.conn.Close() }

// inflate decompresses a message, and refuses one that grows past MaxInflated.
func inflate(b []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(b))
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, MaxInflated+1))
	if err != nil {
		return nil, ErrMalformed
	}
	if len(out) > MaxInflated {
		return nil, ErrTooLarge
	}
	return out, nil
}
