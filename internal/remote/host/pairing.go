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
	// handshakeWithin is how long a pairing connection has, from its first frame, to finish its
	// handshake and say who is asking; requestFor is how long it then waits for the person. A
	// connection that could idle between the two would hold a set of digits, unseen, for as long as
	// pairing stays open.
	handshakeWithin = 10 * time.Second
	requestFor      = 2 * time.Minute
	// maxHandshakes is how many pairing handshakes one code answers. The relay is asked to let in
	// pairingUses connections, but the relay is not trusted to: one that let in a million could try
	// handshakes until one showed the digits it wanted.
	maxHandshakes = 8
	// MaxDevices is how many devices a machine pairs with; the relay carries as many at once.
	MaxDevices = 16
)

// pairing is a pairing code on offer and the devices that asked with it.
type pairing struct {
	code     secure.PairingCode
	link     string
	requests []*pairRequest
	wrong    int
	answered int // handshakes answered with this code
	// open: pairing without a code, for devices that ask at the relay's address. The code is then not
	// on offer, and only its expiry is used.
	open bool
}

// pairRequest is a device that finished the pairing handshake and waits for the person.
type pairRequest struct {
	name   string
	at     time.Time
	digits string
	key    []byte // the device's public key, proven by the handshake
	decide chan device.Decision
}

// end turns down every request. It is safe on a nil pairing. Call with Remote.mu held: the requests
// are only ever read and changed under it. Sending the decisions doesn't block.
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

// endPairing is end for a caller that doesn't hold r.mu.
func (r *Remote) endPairing(p *pairing) {
	r.mu.Lock()
	p.end()
	r.mu.Unlock()
}

func constantEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// StartPairing opens pairing, in place of any pairing open before. With a code (open false), the relay
// is told to let in a device that holds it. Without one (open true), the relay is told to let in
// whoever asks at its address, for a browser that can't be handed a code. The two are never open at
// once: a device with no code and a device with one are told apart by nothing but the digits they show,
// and pairing with a code should owe nothing to chance. The machine must be connected to its relay.
func (r *Remote) StartPairing(open bool) (Pairing, error) {
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
	p := &pairing{code: code, open: open}
	if !open {
		p.link = r.saved.Relay + "/#p=" + code.Encode()
	}
	r.pairing = p
	if old != nil {
		old.end()
	}
	r.mu.Unlock()
	if old != nil {
		rc.withdraw(old)
	}
	if open {
		rc.openPairing(code.Expires, true)
	} else {
		rc.ticket(code, true)
	}
	// Pairing ends on the machine's clock, whatever the relay does.
	time.AfterFunc(time.Until(code.Expires), func() {
		r.mu.Lock()
		expired := r.pairing == p
		if expired {
			r.pairing = nil
			p.end()
		}
		r.mu.Unlock()
		if expired {
			rc.withdraw(p)
			r.changed()
		}
	})
	r.changed()
	return Pairing{Open: open, Link: p.link, Expires: code.Expires}, nil
}

// withdraw tells the relay that a pairing is over: its ticket, or its open window.
func (rc *relayConn) withdraw(p *pairing) {
	if p.open {
		rc.openPairing(p.code.Expires, false)
	} else {
		rc.ticket(p.code, false)
	}
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
	r.endPairing(p)
	if rc != nil {
		rc.withdraw(p)
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
			r.endPairing(p)
			if rc != nil {
				rc.withdraw(p)
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

	q.decide <- device.Decision{OK: true, PSK: psk, Voucher: voucher, Name: r.cfg.Name, ReadOnly: readOnly,
		Machine: rc.machine.ID.String()}
	r.endPairing(p)
	rc.withdraw(p)
	r.cut(d.ID) // whatever the device had open under its earlier key ends
	r.audit.event("paired", d.ID, d.Name)
	if r.cfg.Notify != nil {
		r.cfg.Notify("A device was paired", d.Name+" can now use Solo on this machine.")
	}
	r.changed()
	return Device{ID: d.ID, Name: d.Name, ReadOnly: readOnly, Added: now}, nil
}

// answering counts a connection against the pairing that is open, and returns that pairing, or nil when
// there is none of its kind or it has answered its share. A pairing answers a handful of handshakes,
// whatever the relay lets through: each answered handshake is a set of digits its sender gets to see,
// and enough of them would hold any six digits.
func (r *Remote) answering(rc *relayConn, open bool) *pairing {
	r.mu.Lock()
	p := r.pairing
	if p == nil || p.open != open || !time.Now().Before(p.code.Expires) {
		r.mu.Unlock()
		return nil
	}
	p.answered++
	spent := p.answered > maxHandshakes
	if spent {
		r.pairing = nil
		p.end()
	}
	r.mu.Unlock()
	if spent {
		rc.withdraw(p)
		r.changed()
		return nil
	}
	return p
}

// request reads who is asking: one small message, the first on the channel.
func request(link *wire.Link) (device.Request, bool) {
	var req device.Request
	msg, err := link.ReceiveSmall(1024)
	if err != nil || json.Unmarshal(msg, &req) != nil {
		return req, false
	}
	return req, true
}

// servePairing runs one connection that came in with the pairing code's ticket. Nothing is shown to the
// person, and nothing kept, until a message arrives on the channel the handshake made: only a device
// that holds the code's secret can send one.
func (r *Remote) servePairing(rc *relayConn, c *muxConn) {
	defer c.Close()
	first, err := c.Read()
	if err != nil {
		return
	}
	p := r.answering(rc, false)
	if p == nil {
		return
	}
	// From here to "who is asking", one short deadline for the lot.
	late := time.AfterFunc(handshakeWithin, func() { c.Close() })
	defer late.Stop()
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
	req, ok := request(link)
	if !ok {
		return
	}
	late.Stop()
	r.awaitApproval(p, c, link, req.Name, ch.SAS(), ch.Peer())
}

// serveOpenPairing runs one connection of a device that holds no code (secure/open.go). The device's
// key is unknown and nothing vouches for the channel; what makes it trustworthy is the person typing,
// here, the digits that device shows. So the same rule holds as with a code: nothing is shown, and
// nothing kept, until the handshake is done and the device has kept its commitment.
func (r *Remote) serveOpenPairing(rc *relayConn, c *muxConn) {
	defer c.Close()
	// Counted as it arrives, before it has said anything: the relay counts the same connections, and
	// the two counts end the window together.
	p := r.answering(rc, true)
	if p == nil {
		return
	}
	late := time.AfterFunc(handshakeWithin, func() { c.Close() })
	defer late.Stop()
	first, err := c.Read()
	if err != nil {
		return
	}
	res, answer, err := secure.RespondOpen(rc.origin, rc.machine.Noise, first, rand.Reader)
	if err != nil {
		return
	}
	if err := c.Write(answer); err != nil {
		return
	}
	third, err := c.Read()
	if err != nil {
		return
	}
	ch, err := res.Finish(third)
	if err != nil {
		return
	}
	link := wire.NewLink(c, ch)
	req, ok := request(link)
	if !ok {
		return
	}
	digits, err := res.Digits(ch, req.Nonce)
	if err != nil {
		return // not the key and the number it committed to: it was choosing its digits
	}
	late.Stop()
	r.awaitApproval(p, c, link, req.Name, digits, ch.Peer())
}

// awaitApproval lists a device that finished a pairing handshake among those asking, and waits for the
// person, for a while: the decision goes to this device, on this channel, and nowhere else.
func (r *Remote) awaitApproval(p *pairing, c *muxConn, link *wire.Link, name, digits string, key []byte) {
	name = device.CleanName(name)
	if name == "" {
		name = "A device"
	}
	q := &pairRequest{name: name, at: time.Now(), digits: digits, key: append([]byte(nil), key...),
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
	leave := func() {
		r.mu.Lock()
		p.requests = slices.DeleteFunc(p.requests, func(x *pairRequest) bool { return x == q })
		r.mu.Unlock()
		r.changed()
	}
	wait := time.NewTimer(requestFor)
	defer wait.Stop()
	select {
	case d := <-q.decide:
		link.Send(decision(d))
		// The relay forwards in order, so the answer is on its way before the close that follows.
	case <-gone:
		leave()
	case <-wait.C:
		leave()
		link.Send(decision(device.Decision{}))
	}
}
