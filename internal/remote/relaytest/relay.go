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
	// AllowAny lets every machine in whose token is its own key's: for a relay that listens on this
	// machine only, where the list of machines is whoever can reach it.
	AllowAny bool
	// Tamper, when set, sees every sealed record the relay forwards and returns what to forward in
	// its place (nil drops it): toDevice says which way it goes. It is how tests play a hostile relay.
	Tamper func(toDevice bool, frame []byte) []byte

	mu       sync.Mutex
	keys     [][]byte
	machines map[secure.ID]*machine
	tickets  map[[32]byte]*ticket
	offset   time.Duration // added to the clock, for tests of a machine whose clock is off
}

type machine struct {
	ws    *websocket.Conn
	wmu   sync.Mutex
	conns map[[8]byte]*websocket.Conn
}

type ticket struct {
	machine secure.ID
	expires time.Time
	uses    int
}

// New starts a relay that lets no machine in until Allow names its key.
func New() *Relay {
	r := &Relay{machines: map[secure.ID]*machine{}, tickets: map[[32]byte]*ticket{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

// NewAt is New on an address, such as "127.0.0.1:8787".
func NewAt(addr string) (*Relay, error) {
	r := &Relay{machines: map[secure.ID]*machine{}, tickets: map[[32]byte]*ticket{}}
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
		if !r.allowed(key) {
			http.Error(w, "this machine isn't allowed", http.StatusForbidden)
			return
		}
		if ws := accept(); ws != nil {
			r.machine(req.Context(), id, ws)
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
			r.device(req.Context(), mid, 1, did, ws)
		}
	case device.PathPair:
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
		ok := t != nil && t.machine == mid && t.uses > 0 && time.Now().Add(r.offset).Before(t.expires)
		if ok {
			t.uses--
		}
		r.mu.Unlock()
		if !ok {
			http.Error(w, "bad ticket", http.StatusUnauthorized)
			return
		}
		if ws := accept(); ws != nil {
			r.device(req.Context(), mid, 2, secure.ID{}, ws)
		}
	}
}

// machine serves a machine's socket until it closes.
func (r *Relay) machine(ctx context.Context, id secure.ID, ws *websocket.Conn) {
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
			if body[0] == 1 {
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
func (r *Relay) device(ctx context.Context, mid secure.ID, kind byte, did secure.ID, ws *websocket.Conn) {
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
