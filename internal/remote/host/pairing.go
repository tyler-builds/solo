//go:build !js

package host

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/secure"
	"github.com/tyler-builds/solo/internal/remote/wire"
)

const (
	// pairingUses is how many connections the relay lets in with one code: the device's, and a retry
	// or two if its connection drops.
	pairingUses = 3
	// maxRequests is how many devices may wait for approval at once.
	maxRequests = 3
	// maxWrongCodes is how many wrong sets of digits end a pairing: guessing six digits takes more.
	maxWrongCodes = 5
	// MaxDevices is how many devices a machine pairs with; the relay carries as many at once.
	MaxDevices = 16
)

// pairing is a pairing code on offer and the devices that asked with it.
type pairing struct {
	code     secure.PairingCode
	link     string
	requests []*pairRequest
	wrong    int
}

// pairRequest is a device that finished the pairing handshake and waits for the person.
type pairRequest struct {
	name   string
	at     time.Time
	digits string
	key    []byte // the device's public key, proven by the handshake
	decide chan device.Decision
}

// end turns down every request. It is safe on a nil pairing.
func (p *pairing) end() {
	if p == nil {
		return
	}
	for _, q := range p.requests {
		select {
		case q.decide <- device.Decision{}:
		default:
		}
	}
	p.requests = nil
}

func constantEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// StartPairing makes a pairing code, which replaces any earlier one, and tells the relay to let in a
// device that holds it. The machine must be connected to its relay.
func (r *Remote) StartPairing() (Pairing, error) {
	r.mu.Lock()
	rc := r.relay
	if rc == nil || r.conn != Online {
		r.mu.Unlock()
		return Pairing{}, errors.New("This machine isn't connected to its relay yet.")
	}
	if len(r.saved.Devices) >= MaxDevices {
		r.mu.Unlock()
		return Pairing{}, errors.New("This machine has as many devices as it can pair with. Remove one first.")
	}
	code, err := rc.machine.NewPairingCode(rand.Reader, time.Now(), r.cfg.PairingFor)
	if err != nil {
		r.mu.Unlock()
		return Pairing{}, err
	}
	old := r.pairing
	p := &pairing{code: code, link: r.saved.Relay + "/#p=" + code.Encode()}
	r.pairing = p
	r.mu.Unlock()
	if old != nil {
		old.end()
		rc.ticket(old.code, false)
	}
	rc.ticket(code, true)
	// The code ends on the machine's clock, whatever the relay does.
	time.AfterFunc(time.Until(code.Expires), func() {
		r.mu.Lock()
		expired := r.pairing == p
		if expired {
			r.pairing = nil
		}
		r.mu.Unlock()
		if expired {
			p.end()
			r.changed()
		}
	})
	r.changed()
	return Pairing{Link: p.link, Expires: code.Expires}, nil
}

// CancelPairing withdraws the pairing code and turns down the devices waiting on it.
func (r *Remote) CancelPairing() {
	r.mu.Lock()
	p, rc := r.pairing, r.relay
	r.pairing = nil
	r.mu.Unlock()
	if p == nil {
		return
	}
	p.end()
	if rc != nil {
		rc.ticket(p.code, false)
	}
	r.changed()
}

// Approve pairs the device that shows these six digits. The digits come from the handshake the device
// ran, so they pick out the device in the person's hand: someone else who got hold of the pairing code
// shows other digits. The code is spent, and any other device waiting is turned down.
func (r *Remote) Approve(digits string, readOnly bool) (Device, error) {
	digits = strings.Map(func(c rune) rune {
		if c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, digits)
	r.mu.Lock()
	p, rc := r.pairing, r.relay
	if p == nil || !time.Now().Before(p.code.Expires) {
		r.mu.Unlock()
		return Device{}, errors.New("The pairing code has expired. Make a new one.")
	}
	var match []*pairRequest
	for _, q := range p.requests {
		if constantEqual([]byte(q.digits), []byte(digits)) {
			match = append(match, q)
		}
	}
	if len(match) != 1 {
		// Two requests with the same digits can't be told apart, so neither is approved.
		for _, q := range match {
			q.decide <- device.Decision{}
			p.requests = slices.DeleteFunc(p.requests, func(x *pairRequest) bool { return x == q })
		}
		p.wrong++
		spent := p.wrong >= maxWrongCodes
		if spent {
			r.pairing = nil
		}
		r.mu.Unlock()
		if spent {
			p.end()
			if rc != nil {
				rc.ticket(p.code, false)
			}
			r.changed()
			return Device{}, errors.New("Too many wrong codes. Make a new pairing code and try again.")
		}
		r.changed()
		return Device{}, errors.New("No device waiting shows those digits. Check the six digits on the device.")
	}
	q := match[0]
	if rc == nil {
		r.mu.Unlock()
		return Device{}, errors.New("This machine isn't connected to its relay.")
	}
	psk := make([]byte, secure.KeySize)
	if _, err := rand.Read(psk); err != nil {
		r.mu.Unlock()
		return Device{}, err
	}
	id := secure.DeviceID(q.key)
	now := time.Now()
	d := savedDevice{ID: id.String(), Key: q.key, PSK: psk, Name: q.name, ReadOnly: readOnly, Added: now}
	// A device that pairs again replaces its earlier self.
	devices := slices.DeleteFunc(slices.Clone(r.saved.Devices), func(x savedDevice) bool { return x.ID == d.ID })
	if len(devices) >= MaxDevices {
		r.mu.Unlock()
		return Device{}, errors.New("This machine has as many devices as it can pair with. Remove one first.")
	}
	r.saved.Devices = append(devices, d)
	if err := r.saved.write(r.path); err != nil {
		r.saved.Devices = devices
		r.mu.Unlock()
		return Device{}, err
	}
	voucher := rc.machine.Voucher(rc.origin, id, now)
	p.requests = slices.DeleteFunc(p.requests, func(x *pairRequest) bool { return x == q })
	r.pairing = nil
	r.mu.Unlock()

	q.decide <- device.Decision{OK: true, PSK: psk, Voucher: voucher, Name: r.cfg.Name, ReadOnly: readOnly}
	p.end()
	rc.ticket(p.code, false)
	r.rpc.Drop(d.ID) // a session under the device's earlier key ends
	r.audit.event("paired", d.ID, d.Name)
	if r.cfg.Notify != nil {
		r.cfg.Notify("A device was paired", d.Name+" can now use Solo on this machine.")
	}
	r.changed()
	return Device{ID: d.ID, Name: d.Name, ReadOnly: readOnly, Added: now}, nil
}

// servePairing runs one connection that came in with the pairing code's ticket. Nothing is shown to the
// person, and nothing kept, until a message arrives on the channel the handshake made: only a device
// that holds the code's secret can send one.
func (r *Remote) servePairing(rc *relayConn, c *muxConn) {
	defer c.Close()
	r.mu.Lock()
	p := r.pairing
	r.mu.Unlock()
	if p == nil || !time.Now().Before(p.code.Expires) {
		return
	}
	first, err := c.Read()
	if err != nil {
		return
	}
	psk := p.code.PSK()
	params := secure.Params{Purpose: secure.Pair, Origin: rc.origin, Machine: rc.machine.ID, Code: &p.code}
	ch, answer, err := secure.Respond(params, rc.machine.Noise, psk[:], first, rand.Reader)
	if err != nil {
		return
	}
	if err := c.Write(answer); err != nil {
		return
	}
	link := wire.NewLink(c, ch)
	msg, err := link.Receive()
	if err != nil || len(msg) > 1024 {
		return
	}
	var req device.Request
	if json.Unmarshal(msg, &req) != nil {
		return
	}
	name := device.CleanName(req.Name)
	if name == "" {
		name = "A device"
	}
	q := &pairRequest{name: name, at: time.Now(), digits: ch.SAS(), key: append([]byte(nil), ch.Peer()...),
		decide: make(chan device.Decision, 2)}
	r.mu.Lock()
	if r.pairing != p || len(p.requests) >= maxRequests {
		r.mu.Unlock()
		link.Send(decision(device.Decision{}))
		return
	}
	p.requests = append(p.requests, q)
	r.mu.Unlock()
	r.changed()
	c.timeout.Store(0)

	gone := make(chan struct{})
	go func() { // a device has nothing more to say; anything it sends, or its leaving, ends the request
		link.Receive()
		close(gone)
	}()
	select {
	case d := <-q.decide:
		link.Send(decision(d))
		// The relay forwards in order, so the answer is on its way before the close that follows.
	case <-gone:
		r.mu.Lock()
		p.requests = slices.DeleteFunc(p.requests, func(x *pairRequest) bool { return x == q })
		r.mu.Unlock()
		r.changed()
	}
}
