//go:build !js

package host

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/rpc"
	"github.com/tyler-builds/solo/internal/remote/secure"
	"github.com/tyler-builds/solo/internal/remote/wire"
)

// Frame types on the machine's socket (docs/REMOTE.md, "The relay").
const (
	frameData   = 1
	frameOpen   = 2
	frameClose  = 3
	frameTicket = 4
	frameUsage  = 5
)

const (
	maxBackoff     = 5 * time.Minute
	handshakeFor   = 15 * time.Second // how long a new connection has to finish its handshake
	sessionFor     = time.Hour        // how long a session lasts before the device must handshake again
	relayPingEvery = 30 * time.Second
	relayPongFor   = 20 * time.Second
	// queued is how many frames may wait for a session to read them. The relay limits how fast a device
	// sends; a session that falls this far behind is ended.
	queued = 256
)

// start connects to the relay and keeps the connection up. Call with r.mu held.
func (r *Remote) start() {
	if r.stop != nil || r.closed {
		return
	}
	r.stop = make(chan struct{})
	r.conn = Connecting
	go r.run(r.stop, r.saved.Relay)
}

// stopLocked ends the connection to the relay, and with it every session. Call with r.mu held.
func (r *Remote) stopLocked() {
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	if r.relay != nil {
		r.relay.close()
		r.relay = nil
	}
	r.pairing.end()
	r.pairing = nil
	r.conn, r.problem = Off, ""
}

// run keeps a connection to the relay until stop closes.
func (r *Remote) run(stop chan struct{}, address string) {
	wait := r.cfg.Retry
	var skew time.Duration // the relay's clock minus this machine's, once learned
	for {
		began := time.Now()
		problem, refused, retry := r.connect(stop, address, &skew)
		select {
		case <-stop:
			return
		default:
		}
		if retry {
			continue // the clock was off; the next try is signed with the relay's time
		}
		if time.Since(began) > 30*time.Second {
			wait = r.cfg.Retry
		}
		state := Connecting
		if refused {
			state, wait = Refused, max(wait, 30*r.cfg.Retry)
		}
		r.setConn(state, problem)
		// Not all at once: machines that lost the relay together would otherwise return together.
		pause := wait/2 + time.Duration(mrand.Int64N(int64(wait)/2+1))
		select {
		case <-stop:
			return
		case <-time.After(pause):
		}
		wait = min(wait*2, maxBackoff)
	}
}

// connect holds one connection to the relay until it ends. It returns what went wrong, whether the
// relay refused this machine, and whether to try again at once.
func (r *Remote) connect(stop chan struct{}, address string, skew *time.Duration) (problem string, refused, retry bool) {
	u, err := url.Parse(address)
	if err != nil {
		return "The relay's address isn't valid.", false, false
	}
	origin := device.Origin(u.Host)
	target := "wss://" + u.Host + device.PathMachine
	if u.Scheme == "http" {
		target = "ws://" + u.Host + device.PathMachine
	}
	r.mu.Lock()
	m, err := r.identityLocked()
	r.mu.Unlock()
	if err != nil {
		return err.Error(), false, false
	}
	token, err := m.MachineAuth(origin, time.Now().Add(*skew), rand.Reader)
	if err != nil {
		return err.Error(), false, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	dialCtx, dialDone := context.WithTimeout(ctx, 20*time.Second)
	ws, resp, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{HTTPClient: r.cfg.HTTP,
		Subprotocols: []string{device.Subprotocol, base64.RawURLEncoding.EncodeToString(token)}})
	dialDone()
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				// The token's time was off by more than the relay allows: ask the relay the time, once.
				if *skew == 0 {
					if d, ok := relayClock(ctx, r.cfg.HTTP, address); ok && (d > 30*time.Second || d < -30*time.Second) {
						*skew = d
						return "", false, true
					}
				}
				return "The relay didn't accept this machine's key.", true, false
			case http.StatusForbidden:
				return "The relay doesn't list this machine. Set remote access up again from Settings.", true, false
			}
		}
		return "The relay can't be reached.", false, false
	}
	ws.SetReadLimit(1 << 17)
	rc := &relayConn{r: r, ws: ws, origin: origin, machine: m, conns: map[[8]byte]*muxConn{}, ctx: ctx}
	rc.pong.Store(time.Now().UnixNano())
	r.mu.Lock()
	r.relay = rc
	p := r.pairing
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.relay == rc {
			r.relay = nil
		}
		r.mu.Unlock()
		rc.close()
	}()
	if p != nil {
		rc.ticket(p.code, true) // the relay forgot the code if it lost its memory meanwhile
	}
	r.setConn(Online, "")
	go rc.keepalive()
	for {
		typ, frame, err := ws.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == device.CloseReplaced {
				return "Another Solo is connected to the relay as this machine.", true, false
			}
			return "The connection to the relay was lost.", false, false
		}
		if typ == websocket.MessageText {
			if string(frame) == "pong" {
				rc.pong.Store(time.Now().UnixNano())
			}
			continue
		}
		rc.frame(frame)
	}
}

// relayClock asks the relay the time, and returns its clock minus this machine's.
func relayClock(ctx context.Context, client *http.Client, address string) (time.Duration, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/v1/info", nil)
	if err != nil {
		return 0, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var info struct {
		Solo int   `json:"solo"`
		Time int64 `json:"time"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&info) != nil || info.Solo != 1 || info.Time <= 0 {
		return 0, false
	}
	return time.Until(time.UnixMilli(info.Time)), true
}

// relayConn is the machine's one socket to the relay, which carries every device's connection.
type relayConn struct {
	r       *Remote
	ws      *websocket.Conn
	origin  string
	machine *secure.Machine
	ctx     context.Context
	wmu     sync.Mutex
	pong    atomic.Int64

	mu     sync.Mutex
	conns  map[[8]byte]*muxConn
	closed bool
}

func (rc *relayConn) send(frame []byte) error {
	rc.wmu.Lock()
	defer rc.wmu.Unlock()
	ctx, cancel := context.WithTimeout(rc.ctx, 30*time.Second)
	defer cancel()
	return rc.ws.Write(ctx, websocket.MessageBinary, frame)
}

// keepalive notices a connection that died without saying so.
func (rc *relayConn) keepalive() {
	t := time.NewTicker(relayPingEvery)
	defer t.Stop()
	for {
		select {
		case <-rc.ctx.Done():
			return
		case <-t.C:
			if time.Since(time.Unix(0, rc.pong.Load())) > relayPingEvery+relayPongFor {
				rc.ws.Close(websocket.StatusGoingAway, "no answer")
				return
			}
			rc.wmu.Lock()
			ctx, cancel := context.WithTimeout(rc.ctx, 10*time.Second)
			rc.ws.Write(ctx, websocket.MessageText, []byte("ping"))
			cancel()
			rc.wmu.Unlock()
		}
	}
}

func (rc *relayConn) close() {
	rc.mu.Lock()
	if rc.closed {
		rc.mu.Unlock()
		return
	}
	rc.closed = true
	conns := rc.conns
	rc.conns = map[[8]byte]*muxConn{}
	rc.mu.Unlock()
	for _, c := range conns {
		c.shut()
	}
	rc.ws.CloseNow()
}

// ticket tells the relay to let in, or to stop letting in, connections that hold a pairing code.
func (rc *relayConn) ticket(code secure.PairingCode, add bool) {
	hash := secure.TicketHash(code.Ticket())
	frame := make([]byte, 0, 43)
	op := byte(2)
	if add {
		op = 1
	}
	frame = append(frame, frameTicket, op)
	frame = append(frame, hash[:]...)
	var exp [8]byte
	for i, s := 0, uint64(code.Expires.Unix()); i < 8; i++ {
		exp[7-i] = byte(s >> (8 * i))
	}
	frame = append(frame, exp[:]...)
	frame = append(frame, pairingUses)
	rc.send(frame)
}

// frame takes one frame from the relay. What the relay says is not trusted: a frame that makes no
// sense is dropped, and a connection it invents fails its handshake.
func (rc *relayConn) frame(frame []byte) {
	if len(frame) < 1 {
		return
	}
	body := frame[1:]
	switch frame[0] {
	case frameData:
		if len(body) < 8 {
			return
		}
		rc.mu.Lock()
		c := rc.conns[[8]byte(body[:8])]
		rc.mu.Unlock()
		if c != nil {
			c.deliver(body[8:])
		}
	case frameOpen:
		if len(body) != 8+1+16 {
			return
		}
		id, kind := [8]byte(body[:8]), body[8]
		c := &muxConn{rc: rc, id: id, in: make(chan []byte, queued), done: make(chan struct{})}
		c.timeout.Store(int64(handshakeFor))
		rc.mu.Lock()
		if _, dup := rc.conns[id]; dup || rc.closed || len(rc.conns) >= 64 {
			rc.mu.Unlock()
			return
		}
		rc.conns[id] = c
		rc.mu.Unlock()
		switch kind {
		case 1:
			go rc.r.serveDevice(rc, c, secure.ID(body[9:]))
		case 2:
			go rc.r.servePairing(rc, c)
		default:
			c.Close()
		}
	case frameClose:
		if len(body) != 8 {
			return
		}
		rc.mu.Lock()
		c := rc.conns[[8]byte(body)]
		delete(rc.conns, [8]byte(body))
		rc.mu.Unlock()
		if c != nil {
			c.shut()
		}
	case frameUsage:
		// The day's count of frames; nothing acts on it yet.
	}
}

// muxConn is one device's connection, as the relay carries it on the machine's socket. It is a
// wire.Conn.
type muxConn struct {
	rc      *relayConn
	id      [8]byte
	in      chan []byte
	done    chan struct{}
	once    sync.Once
	timeout atomic.Int64 // how long Read waits, in nanoseconds; 0 for as long as it takes
}

var _ wire.Conn = (*muxConn)(nil)

func (c *muxConn) deliver(frame []byte) {
	select {
	case c.in <- append([]byte(nil), frame...):
	case <-c.done:
	default:
		c.Close() // the session isn't reading; it is over
	}
}

func (c *muxConn) Read() ([]byte, error) {
	var timeout <-chan time.Time
	if d := time.Duration(c.timeout.Load()); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case b := <-c.in:
		return b, nil
	case <-c.done:
		return nil, errors.New("remote: the connection closed")
	case <-timeout:
		c.Close()
		return nil, errors.New("remote: the device took too long")
	}
}

func (c *muxConn) Write(b []byte) error {
	select {
	case <-c.done:
		return errors.New("remote: the connection closed")
	default:
	}
	frame := make([]byte, 0, 9+len(b))
	frame = append(append(append(frame, frameData), c.id[:]...), b...)
	return c.rc.send(frame)
}

// Close ends the connection and tells the relay to close the device's socket.
func (c *muxConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.rc.mu.Lock()
		delete(c.rc.conns, c.id)
		c.rc.mu.Unlock()
		c.rc.send(append([]byte{frameClose}, c.id[:]...))
	})
	return nil
}

// shut ends the connection when the relay already knows it is over.
func (c *muxConn) shut() {
	c.once.Do(func() { close(c.done) })
}

// serveDevice runs a paired device's session: the handshake keyed with that device's own key, then its
// requests, until the connection ends.
func (r *Remote) serveDevice(rc *relayConn, c *muxConn, id secure.ID) {
	defer c.Close()
	r.mu.Lock()
	d, ok := r.deviceLocked(id)
	r.mu.Unlock()
	if !ok {
		return // revoked, or never paired: its voucher still opens the relay, and nothing here
	}
	first, err := c.Read()
	if err != nil {
		return
	}
	p := secure.Params{Purpose: secure.Session, Origin: rc.origin, Machine: rc.machine.ID}
	ch, answer, err := secure.Respond(p, rc.machine.Noise, d.PSK, first, rand.Reader)
	if err != nil {
		return
	}
	// The relay named the device; the handshake must prove it. The session key alone isn't enough: it
	// must come with the public key that was approved at this machine.
	if !constantEqual(ch.Peer(), d.Key) {
		return
	}
	if err := c.Write(answer); err != nil {
		return
	}
	link := wire.NewLink(c, ch)
	link.Compress = true
	c.timeout.Store(0) // from here the session's own silence limit applies
	// A session is keyed afresh every hour: the device connects again and resumes.
	expire := time.AfterFunc(sessionFor, func() { c.Close() })
	defer expire.Stop()
	r.seen(d.ID)
	r.rpc.Serve(link, rpc.Device{ID: d.ID, Name: d.Name, ReadOnly: d.ReadOnly})
	r.changed()
}
