// Package rpc is what a device and a machine say to each other inside the secure channel: requests and
// their answers, and the machine's state as it changes. Server serves a device over any api.Backend;
// Client is an api.Backend that keeps a mirror of the machine's state, for the web UI.
//
// The other end is authenticated, not trusted: a paired device may have been taken over, and so may a
// machine. Every size is bounded before anything is kept, requests are checked at the machine (see
// methods.go), and a message that breaks a rule ends the session.
package rpc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Link carries whole messages of up to a megabyte, in order (a wire.Link).
type Link interface {
	Send([]byte) error
	Receive() ([]byte, error)
	Close() error
}

// Protocol is the version both ends must speak.
const Protocol = 1

const (
	// partSize is the most of an envelope sent in one message. A larger envelope (an image, a long
	// diff) is cut into parts, which other messages cross between.
	partSize = 192 << 10
	// window is how many parts of one envelope may be on their way unacknowledged: a receiver that
	// stops reading stops the sender, so nothing piles up at the relay.
	window = 4
	// maxStreams is how many envelopes in parts one end may be sending at a time.
	maxStreams = 8

	// PingEvery is how often a device says it is there, and IdleTimeout how long either end waits in
	// silence before it gives the session up.
	PingEvery   = 5 * time.Second
	IdleTimeout = 15 * time.Second
)

// Frame kinds: the first byte of a message.
const (
	frameWhole = 1 // a whole envelope
	framePart  = 2 // stream(4) seq(4) last(1) data
	frameAck   = 3 // stream(4) seq(4)
	framePing  = 4
	framePong  = 5
)

var (
	errProtocol = errors.New("rpc: a message is not one this session understands")
	errTooLarge = errors.New("rpc: a message is larger than this session takes")
	// ErrClosed is returned by calls on a session that has ended.
	ErrClosed = errors.New("rpc: the connection closed")
)

// Envelope is one message: a request, its answer, or state.
type Envelope struct {
	T  string          `json:"t"`            // the kind (see proto.go)
	ID string          `json:"id,omitempty"` // a request's ID, and its answer's
	M  string          `json:"m,omitempty"`  // a request's method
	P  json.RawMessage `json:"p,omitempty"`  // parameters, a result, or state
	E  *Error          `json:"e,omitempty"`  // why a request failed
	// Blob is bytes carried as they are, after the JSON: an image.
	Blob []byte `json:"-"`
}

func (e Envelope) encode() ([]byte, error) {
	head, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4, 4+len(head)+len(e.Blob))
	binary.BigEndian.PutUint32(out, uint32(len(head)))
	out = append(out, head...)
	return append(out, e.Blob...), nil
}

func decodeEnvelope(b []byte) (Envelope, error) {
	var e Envelope
	if len(b) < 4 {
		return e, errProtocol
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return e, errProtocol
	}
	if err := json.Unmarshal(b[4:4+n], &e); err != nil {
		return e, errProtocol
	}
	if rest := b[4+n:]; len(rest) > 0 {
		e.Blob = rest
	}
	return e, nil
}

// session is one end of a connection: it sends envelopes, whole or in parts, and hands the ones that
// arrive to a handler.
type session struct {
	link  Link
	maxIn int // the largest envelope this end takes

	mu      sync.Mutex
	next    uint32
	out     map[uint32]chan uint32 // acknowledgements for the envelopes being sent in parts
	in      map[uint32]*inbound
	done    chan struct{}
	err     error
	closing sync.Once

	heard atomic.Int64 // when a message last arrived, in Unix nanoseconds

	// streamsIn is how many envelopes in parts the other end may be sending at once. A machine sets it
	// to zero for a device until the device has said hello, and for one that may only look: nothing
	// such a device has to say is large.
	streamsIn atomic.Int32

	// pace is the wait after each part of a large envelope; zero sends as fast as the other end
	// acknowledges. A device sets it: the relay ends a device's connection that sends frames faster
	// than it allows (50 a second after a burst of 200), and a large image is a few hundred of them.
	pace time.Duration
}

type inbound struct {
	data  []byte
	seq   uint32
	began time.Time
}

// streamFor is how long an envelope in parts may take to arrive. One that is begun and never finished
// would otherwise hold its bytes for as long as the session lasts.
const streamFor = 2 * time.Minute

func newSession(link Link, maxIn int) *session {
	s := &session{link: link, maxIn: maxIn, out: map[uint32]chan uint32{}, in: map[uint32]*inbound{},
		done: make(chan struct{})}
	s.heard.Store(time.Now().UnixNano())
	s.streamsIn.Store(maxStreams)
	return s
}

// close ends the session; err is why (the first reason is kept).
func (s *session) close(err error) {
	s.closing.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
		s.link.Close()
	})
}

func (s *session) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return ErrClosed
}

// send sends an envelope. It waits while the other end is behind on a large one, and may be called from
// several goroutines.
func (s *session) send(e Envelope) error {
	data, err := e.encode()
	if err != nil {
		return err
	}
	if len(data) <= partSize {
		return s.write(append([]byte{frameWhole}, data...))
	}
	acks := make(chan uint32, window+1)
	s.mu.Lock()
	if len(s.out) >= maxStreams {
		s.mu.Unlock()
		return errors.New("rpc: too many large messages at once")
	}
	s.next++
	id := s.next
	s.out[id] = acks
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.out, id)
		s.mu.Unlock()
	}()
	acked, seq := uint32(0), uint32(0)
	for len(data) > 0 {
		for seq-acked >= window {
			select {
			case a := <-acks:
				acked = max(acked, a)
			case <-s.done:
				return s.failure()
			}
		}
		n := min(len(data), partSize)
		seq++
		frame := make([]byte, 10, 10+n)
		frame[0] = framePart
		binary.BigEndian.PutUint32(frame[1:], id)
		binary.BigEndian.PutUint32(frame[5:], seq)
		if n == len(data) {
			frame[9] = 1
		}
		if err := s.write(append(frame, data[:n]...)); err != nil {
			return err
		}
		data = data[n:]
		if s.pace > 0 && len(data) > 0 {
			select {
			case <-time.After(s.pace):
			case <-s.done:
				return s.failure()
			}
		}
	}
	return nil
}

func (s *session) write(frame []byte) error {
	select {
	case <-s.done:
		return s.failure()
	default:
	}
	if err := s.link.Send(frame); err != nil {
		s.close(err)
		return err
	}
	return nil
}

// ping says this end is there.
func (s *session) ping() error { return s.write([]byte{framePing}) }

// silent reports how long it has been since anything arrived.
func (s *session) silent() time.Duration {
	return time.Since(time.Unix(0, s.heard.Load()))
}

// run reads messages until the session ends, and hands each envelope to handle, in order, on this
// goroutine: handle must not wait on anything slow. It returns why the session ended.
func (s *session) run(handle func(Envelope) error) error {
	for {
		frame, err := s.link.Receive()
		if err != nil {
			s.close(err)
			return s.failure()
		}
		s.heard.Store(time.Now().UnixNano())
		if err := s.frame(frame, handle); err != nil {
			s.close(err)
			return s.failure()
		}
	}
}

func (s *session) frame(frame []byte, handle func(Envelope) error) error {
	if len(frame) == 0 {
		return errProtocol
	}
	// A stream that was begun and left: whatever arrives next, a ping included, ends the session.
	for _, in := range s.in {
		if time.Since(in.began) > streamFor {
			return errors.New("rpc: a large message took too long to arrive")
		}
	}
	switch body := frame[1:]; frame[0] {
	case frameWhole:
		if len(body) > partSize || len(body) > s.maxIn {
			return errTooLarge
		}
		e, err := decodeEnvelope(body)
		if err != nil {
			return err
		}
		return handle(e)
	case framePart:
		if len(body) < 9 || len(body)-9 > partSize {
			return errProtocol
		}
		id, seq, last, data := binary.BigEndian.Uint32(body), binary.BigEndian.Uint32(body[4:]), body[8], body[9:]
		if last > 1 || (last == 0 && len(data) != partSize) || len(data) == 0 {
			return errProtocol // only the last part may be short, and none is empty
		}
		in := s.in[id]
		if in == nil {
			if seq != 1 || len(s.in) >= int(s.streamsIn.Load()) {
				return errProtocol
			}
			in = &inbound{began: time.Now()}
			s.in[id] = in
		}
		if time.Since(in.began) > streamFor {
			return errors.New("rpc: a large message took too long to arrive")
		}
		if seq != in.seq+1 {
			return errProtocol
		}
		if len(in.data)+len(data) > s.maxIn {
			return errTooLarge
		}
		in.seq = seq
		in.data = append(in.data, data...)
		ack := make([]byte, 9)
		ack[0] = frameAck
		binary.BigEndian.PutUint32(ack[1:], id)
		binary.BigEndian.PutUint32(ack[5:], seq)
		if err := s.write(ack); err != nil {
			return err
		}
		if last == 0 {
			return nil
		}
		delete(s.in, id)
		e, err := decodeEnvelope(in.data)
		if err != nil {
			return err
		}
		return handle(e)
	case frameAck:
		if len(body) != 8 {
			return errProtocol
		}
		s.mu.Lock()
		acks := s.out[binary.BigEndian.Uint32(body)]
		s.mu.Unlock()
		if acks != nil {
			select {
			case acks <- binary.BigEndian.Uint32(body[4:]):
			default: // more acknowledgements than parts: the sender's window is already open
			}
		}
		return nil
	case framePing:
		if len(body) != 0 {
			return errProtocol
		}
		return s.write([]byte{framePong})
	case framePong:
		if len(body) != 0 {
			return errProtocol
		}
		return nil
	}
	return errProtocol
}
