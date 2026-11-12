//go:build !js

// Package relaytest is the relay, in Go, for tests and for running Solo's remote access on one machine
// without Cloudflare. It speaks the protocol of relay/ (docs/REMOTE.md, "The relay") and checks the
// same tokens with the same code the machine signs them with; it leaves out the limits and the quota
// counting, which are the Worker's own business and are tested there.
package relaytest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/secure"
)

// Frame types on a machine's socket.
const (
	frameData   = 1
	frameOpen   = 2
	frameClose  = 3
	frameTicket = 4
	frameUsage  = 5
)

// Relay is a relay listening on this machine.
type Relay struct {
	srv *httptest.Server
	// Static, when set, serves everything that isn't the relay's own: the web client.
	Static http.Handler
	// Hosts, when set, are the only addresses the relay answers to (the request's Host): a page of
	// another site that was pointed at this machine's address is refused.
	Hosts []string
	// AllowAny lets every machine in whose token is its own key's: for a relay that listens on this
	// machine only, where the list of machines is whoever can reach it.
	AllowAny bool
	// Tamper, when set, sees every sealed record the relay forwards and returns what to forward in
	// its place (nil drops it): toDevice says which way it goes. It is how tests play a hostile relay.
	Tamper func(toDevice bool, frame []byte) []byte

	mu       sync.Mutex
	keys     [][]byte
	machines map[owner]*machine
	tickets  map[[32]byte]*ticket
	open     []*ticket     // pairing opened to devices with no code, in the order it was opened
	offset   time.Duration // added to the clock, for tests of a machine whose clock is off
}

// owner is how the relay knows a machine: the relay key that vouches for it, and its ID. A machine
// can't take another's place by claiming its ID: under its own key that is another machine.
type owner struct {
	key [32]byte
	id  secure.ID
}

type machine struct {
	ws    *websocket.Conn
	wmu   sync.Mutex
	conns map[[8]byte]*websocket.Conn
}

type ticket struct {
	machine owner
	expires time.Time
	uses    int
}

// New starts a relay that lets no machine in until Allow names its key.
func New() *Relay {
	r := &Relay{machines: map[owner]*machine{}, tickets: map[[32]byte]*ticket{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

// NewAt is New on an address, such as "127.0.0.1:8787".
func NewAt(addr string) (*Relay, error) {
	r := &Relay{machines: map[owner]*machine{}, tickets: map[[32]byte]*ticket{}}
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(r.serve)}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	r.srv = &httptest.Server{Listener: ln, Config: srv}
	r.srv.Start()
	return r, nil
}

// URL is the relay's address, such as "http://127.0.0.1:41234".
func (r *Relay) URL() string { return r.srv.URL }

// Origin is the origin both ends hash into their handshakes for this relay.
func (r *Relay) Origin() string { return device.Origin(strings.TrimPrefix(r.srv.URL, "http://")) }

// Allow lets the machine with this relay key connect, as adding it to HOST_KEYS does.
func (r *Relay) Allow(relayKey []byte) {
	r.mu.Lock()
	r.keys = append(r.keys, append([]byte(nil), relayKey...))
	r.mu.Unlock()
}

// Keys returns the relay keys allowed.
func (r *Relay) Keys() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.keys...)
}

// Tickets returns how many pairing tickets the relay holds, and Machines how many machines are
// connected: a test waits on them rather than race the relay, which hears of both a moment after the
// machine has said them.
func (r *Relay) Tickets() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tickets)
}

// Open reports whether a machine has pairing open to devices with no code.
func (r *Relay) Open() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.open) > 0
}

// RefillOpen gives every open pairing uses again, as a relay would that let in whoever asked.
func (r *Relay) RefillOpen(uses int) {
	r.mu.Lock()
	for _, t := range r.open {
		t.uses = uses
	}
	r.mu.Unlock()
}

func (r *Relay) Machines() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.machines)
}

// Refill gives a ticket uses again, as a relay would that let in whoever asked.
func (r *Relay) Refill(hash [32]byte, uses int) {
	r.mu.Lock()
	if t := r.tickets[hash]; t != nil {
		t.uses = uses
	}
	r.mu.Unlock()
}

// SetClockOffset moves the relay's clock.
func (r *Relay) SetClockOffset(d time.Duration) {
	r.mu.Lock()
	r.offset = d
	r.mu.Unlock()
}

// DropMachines closes every machine's connection, as a network failure would.
func (r *Relay) DropMachines() {
	r.mu.Lock()
	var all []*machine
	for _, m := range r.machines {
		all = append(all, m)
	}
	r.mu.Unlock()
	for _, m := range all {
		m.ws.Close(websocket.StatusGoingAway, "")
	}
}

// Close stops the relay.
func (r *Relay) Close() {
	r.DropMachines()
	r.srv.Close()
}

func (r *Relay) now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().Add(r.offset)
}

func (r *Relay) allowed(key []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.AllowAny {
		return true
	}
	for _, k := range r.keys {
		if string(k) == string(key) {
			return true
		}
	}
	return false
}

func (r *Relay) serve(w http.ResponseWriter, req *http.Request) {
	if len(r.Hosts) > 0 && !slices.Contains(r.Hosts, req.Host) {
		http.Error(w, "not this relay's address", http.StatusMisdirectedRequest)
		return
	}
	switch req.URL.Path {
	case "/v1/info":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"solo": 1, "bundle": 0, "time": r.now().UnixMilli()})
	case device.PathMachine, device.PathDevice, device.PathPair:
		r.upgrade(w, req)
	default:
		if r.Static != nil {
			r.Static.ServeHTTP(w, req)
			return
		}
		http.NotFound(w, req)
	}
}

func (r *Relay) upgrade(w http.ResponseWriter, req *http.Request) {
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected a WebSocket", http.StatusUpgradeRequired)
		return
	}
	var protos []string
	for _, p := range strings.Split(req.Header.Get("Sec-WebSocket-Protocol"), ",") {
		protos = append(protos, strings.TrimSpace(p))
	}
	if len(protos) != 2 || protos[0] != device.Subprotocol {
		http.Error(w, "bad protocol", http.StatusBadRequest)
		return
	}
	token, err := base64.RawURLEncoding.DecodeString(protos[1])
	if err != nil {
		http.Error(w, "bad token", http.StatusBadRequest)
		return
	}
	origin := device.Origin(req.Host)
	browser := func() bool { // a page's own origin; this relay is also reached over plain HTTP
		o := req.Header.Get("Origin")
		return o == origin || o == "http://"+req.Host
	}
	accept := func() *websocket.Conn {
		ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{Subprotocols: []string{device.Subprotocol}, InsecureSkipVerify: true})
		if err != nil {
			return nil
		}
		ws.SetReadLimit(1 << 17)
		return ws
	}
	switch req.URL.Path {
	case device.PathMachine:
		key, id, err := secure.VerifyMachineAuth(origin, token, r.now())
		if err != nil {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		// A machine is a program and sends no Origin: one that does is a page, which is no machine.
		if !r.allowed(key) || req.Header.Get("Origin") != "" {
			http.Error(w, "this machine isn't allowed", http.StatusForbidden)
			return
		}
		if ws := accept(); ws != nil {
			r.machine(req.Context(), owner{[32]byte(key), id}, ws)
		}
	case device.PathDevice:
		key, mid, did, err := secure.VerifyVoucher(origin, token)
		if err != nil {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		if !r.allowed(key) || !browser() {
			http.Error(w, "not allowed", http.StatusForbidden)
			return
		}
		if ws := accept(); ws != nil {
			r.device(req.Context(), owner{[32]byte(key), mid}, 1, did, ws)
		}
	case device.PathPair:
		if len(token) == 1 && token[0] == device.OpenPairToken {
			// A device with no code, for whichever machine has pairing open: the one that opened it
			// last, if several have.
			if !browser() {
				http.Error(w, "not allowed", http.StatusForbidden)
				return
			}
			var open []*ticket
			r.mu.Lock()
			for _, t := range r.open {
				if t.uses > 0 && time.Now().Add(r.offset).Before(t.expires) && r.machines[t.machine] != nil {
					open = append(open, t)
				}
			}
			if len(open) == 1 {
				open[0].uses--
			}
			r.mu.Unlock()
			switch {
			case len(open) == 0:
				http.Error(w, "no machine has pairing open", http.StatusUnauthorized)
			case len(open) > 1:
				// The relay doesn't choose which machine a device is paired with.
				if ws := accept(); ws != nil {
					ws.Close(device.CloseAmbiguous, "")
				}
			default:
				if ws := accept(); ws != nil {
					r.device(req.Context(), open[0].machine, 3, secure.ID{}, ws)
				}
			}
			return
		}
		if len(token) != 49 || token[0] != 1 {
			http.Error(w, "bad token", http.StatusBadRequest)
			return
		}
		if !browser() {
			http.Error(w, "not allowed", http.StatusForbidden)
			return
		}
		var mid secure.ID
		copy(mid[:], token[1:17])
		hash := sha256.Sum256(token[17:])
		r.mu.Lock()
		t := r.tickets[hash]
		ok := t != nil && t.machine.id == mid && t.uses > 0 && time.Now().Add(r.offset).Before(t.expires)
		var to owner
		if ok {
			t.uses--
			to = t.machine // the machine that registered the ticket, under its own key
		}
		r.mu.Unlock()
		if !ok {
			http.Error(w, "bad ticket", http.StatusUnauthorized)
			return
		}
		if ws := accept(); ws != nil {
			r.device(req.Context(), to, 2, secure.ID{}, ws)
		}
	}
}

// machine serves a machine's socket until it closes.
func (r *Relay) machine(ctx context.Context, id owner, ws *websocket.Conn) {
	m := &machine{ws: ws, conns: map[[8]byte]*websocket.Conn{}}
	r.mu.Lock()
	old := r.machines[id]
	r.machines[id] = m
	r.mu.Unlock()
	if old != nil {
		old.ws.Close(device.CloseReplaced, "replaced")
	}
	usage, _ := json.Marshal(map[string]any{"day": time.Now().UTC().Format("2006-01-02"), "frames": 0})
	m.send(append([]byte{frameUsage}, usage...))
	for {
		typ, frame, err := ws.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageText {
			if string(frame) == "ping" {
				m.wmu.Lock()
				ws.Write(ctx, websocket.MessageText, []byte("pong"))
				m.wmu.Unlock()
				continue
			}
			ws.Close(device.CloseProtocol, "")
			break
		}
		if len(frame) == 0 {
			ws.Close(device.CloseProtocol, "")
			break
		}
		switch body := frame[1:]; frame[0] {
		case frameData:
			if len(body) < 8 {
				ws.Close(device.CloseProtocol, "")
				continue
			}
			payload := body[8:]
			if r.Tamper != nil {
				if payload = r.Tamper(true, payload); payload == nil {
					continue
				}
			}
			if dev := m.conn([8]byte(body[:8])); dev != nil {
				dev.Write(ctx, websocket.MessageBinary, payload)
			}
		case frameClose:
			if len(body) != 8 {
				ws.Close(device.CloseProtocol, "")
				continue
			}
			if dev := m.conn([8]byte(body)); dev != nil {
				dev.Close(websocket.StatusNormalClosure, "")
			}
		case frameTicket:
			if len(body) != 1+32+8+1 {
				ws.Close(device.CloseProtocol, "")
				continue
			}
			hash := [32]byte(body[1:33])
			r.mu.Lock()
			if body[0] == 3 || body[0] == 4 { // pairing opened to devices with no code, or closed
				at := int64(binary.BigEndian.Uint64(body[33:41]))
				if body[0] == 3 {
					// For a few minutes and a few connections at most, whatever the frame asks.
					until := min(time.Unix(at, 0).Unix(), time.Now().Add(10*time.Minute).Unix())
					r.open = slices.DeleteFunc(r.open, func(t *ticket) bool { return t.machine == id })
					r.open = append(r.open, &ticket{machine: id, uses: min(int(body[41]), 16), expires: time.Unix(until, 0)})
				} else {
					// The window that ends at the frame's time; zero closes whatever is open.
					r.open = slices.DeleteFunc(r.open, func(t *ticket) bool {
						return t.machine == id && (at == 0 || t.expires.Unix() == at)
					})
				}
			} else if body[0] == 1 {
				r.tickets[hash] = &ticket{machine: id, uses: int(body[41]),
					expires: time.Unix(int64(binary.BigEndian.Uint64(body[33:41])), 0)}
			} else if t := r.tickets[hash]; t != nil && t.machine == id {
				delete(r.tickets, hash)
			}
			r.mu.Unlock()
		default:
			ws.Close(device.CloseProtocol, "")
		}
	}
	r.mu.Lock()
	if r.machines[id] == m {
		delete(r.machines, id)
	}
	r.mu.Unlock()
	m.wmu.Lock()
	conns := m.conns
	m.conns = map[[8]byte]*websocket.Conn{}
	m.wmu.Unlock()
	for _, dev := range conns {
		dev.Close(device.CloseOffline, "machine offline")
	}
	ws.CloseNow()
}

func (m *machine) send(frame []byte) error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.ws.Write(ctx, websocket.MessageBinary, frame)
}

func (m *machine) conn(id [8]byte) *websocket.Conn {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	return m.conns[id]
}

// device serves a device's or a pairing socket until it closes.
func (r *Relay) device(ctx context.Context, mid owner, kind byte, did secure.ID, ws *websocket.Conn) {
	r.mu.Lock()
	m := r.machines[mid]
	r.mu.Unlock()
	if m == nil {
		ws.Close(device.CloseOffline, "machine offline")
		return
	}
	var conn [8]byte
	rand.Read(conn[:])
	m.wmu.Lock()
	m.conns[conn] = ws
	m.wmu.Unlock()
	open := append([]byte{frameOpen}, conn[:]...)
	open = append(append(open, kind), did[:]...)
	m.send(open)
	for {
		typ, frame, err := ws.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageText {
			if string(frame) == "ping" {
				ws.Write(ctx, websocket.MessageText, []byte("pong"))
				continue
			}
			ws.Close(device.CloseProtocol, "")
			break
		}
		if r.Tamper != nil {
			if frame = r.Tamper(false, frame); frame == nil {
				continue
			}
		}
		if m.send(append(append([]byte{frameData}, conn[:]...), frame...)) != nil {
			break
		}
	}
	m.wmu.Lock()
	_, live := m.conns[conn]
	delete(m.conns, conn)
	m.wmu.Unlock()
	if live {
		m.send(append([]byte{frameClose}, conn[:]...))
	}
	ws.CloseNow()
}
