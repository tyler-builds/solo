//go:build !js

package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/rpc"
	"github.com/tyler-builds/solo/internal/remote/secure"
	"github.com/tyler-builds/solo/internal/remote/wire"
)

// A network is the machines on one relay that share a passphrase (docs/REMOTE.md, "Networks"). The
// passphrase is checked with OPAQUE (secure/passphrase.go), so it never crosses the relay, and each
// guess takes a run of the exchange with a machine, which counts it here: too many wrong ones turn
// joining off until the person turns it on again.
//
//   - A machine joins the others when the person sets the passphrase on it: it connects to each machine
//     the relay lists, as a machine (the relay checks it is one of the account's, and says which), proves
//     the passphrase, and the two keep each other as members, with a key for later sessions.
//   - A device joins by typing the passphrase: it proves it to each machine, and waits until the person
//     allows it at one of them. That machine tells the others, and one allow lets it into all of them.
//   - Members read each other's usage, let devices in on each other's word, and pass on a new
//     passphrase. Nothing else: they are sent none of each other's threads.

const (
	networkSecret = "remote-network" // the passphrase's record (secure.PassRecord)
	// maxWrong is how many wrong passphrases turn joining off.
	maxWrong   = 10
	maxMembers = 8
	// joinFor is how long a device that proved the passphrase waits to be let in.
	joinFor = 2 * time.Minute
	// allowedFor is how long a device a member let in stays let in here, for a device that reaches this
	// machine after the person allowed it elsewhere.
	allowedFor = 5 * time.Minute
	peerFor    = 20 * time.Second // what a session with a member, or joining one, may take
	maxJoins   = 4                // devices waiting to be let in at once
)

// The first byte of a peer connection: a member's session, or a machine joining.
const (
	peerSession = 1
	peerJoin    = 2
)

// savedNetwork is what the machine keeps of its network, in remote.json. The passphrase's record is
// in the keyring.
type savedNetwork struct {
	Passphrase bool          `json:"passphrase,omitempty"` // a record is kept
	On         bool          `json:"on,omitempty"`
	AutoAllow  bool          `json:"autoAllow,omitempty"`
	Locked     string        `json:"locked,omitempty"`
	Wrong      int           `json:"wrong,omitempty"`
	Members    []savedMember `json:"members,omitempty"`
}

// savedMember is another machine of the network: its key, and the key the two agreed on when one
// joined, for their sessions.
type savedMember struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Key   []byte    `json:"key"`
	PSK   []byte    `json:"psk"`
	Added time.Time `json:"added"`
}

// joinRequest is a device that proved the passphrase and waits to be let in.
type joinRequest struct {
	id     string
	name   string
	at     time.Time
	key    []byte
	decide chan device.Decision
}

// netState is the network's state in memory. Its fields are under Remote.mu.
type netState struct {
	record  *secure.PassRecord // read from the keyring when first needed
	joins   []*joinRequest
	allowed map[string]allowedKey // devices a member let in, by key in hex
	status  string                // how joining the other machines last went
	problem string                // why the relay can't carry a network
	usage   map[string]api.MemberUsage
}

type allowedKey struct {
	until    time.Time
	readOnly bool
}

func (r *Remote) networkState() api.RemoteNetwork {
	n := r.saved.Network
	st := api.RemoteNetwork{Passphrase: n.Passphrase, On: n.On, Locked: n.Locked, Wrong: n.Wrong, AutoAllow: n.AutoAllow,
		Status: r.net.status, Problem: r.net.problem}
	for _, m := range n.Members {
		st.Members = append(st.Members, api.RemoteMember{ID: m.ID, Name: m.Name, Added: m.Added})
	}
	for _, j := range r.net.joins {
		st.Joins = append(st.Joins, api.RemoteJoin{ID: j.id, Name: j.name, At: j.at})
	}
	return st
}

// SetPassphrase sets the network's passphrase here, turns joining by it on, passes it to the other
// members, and joins the machines on the relay that have it.
func (r *Remote) SetPassphrase(passphrase string) error {
	if err := api.CheckPassphrase(passphrase); err != nil {
		return err
	}
	if err := r.keepPassphrase(passphrase); err != nil {
		return err
	}
	r.audit.event("passphrase set", "", "")
	go r.sharePassphrase(passphrase)
	go r.joinAll(passphrase)
	return nil
}

// keepPassphrase makes and keeps the record of a passphrase, and turns joining by it on.
func (r *Remote) keepPassphrase(passphrase string) error {
	rec, err := secure.NewPassRecord([]byte(passphrase))
	if err != nil {
		return err
	}
	r.mu.Lock()
	if _, err := r.identityLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	if err := r.secrets.Set(networkSecret, rec.Encode()); err != nil {
		r.mu.Unlock()
		return err
	}
	r.net.record = &rec
	n := &r.saved.Network
	n.Passphrase, n.On, n.Locked, n.Wrong = true, true, "", 0
	err = r.saved.write(r.path)
	rc := r.relay
	r.mu.Unlock()
	if rc != nil {
		rc.network(true)
	}
	r.changed()
	return err
}

// SetNetworkOn turns joining by passphrase on or off. On starts the count of wrong passphrases over.
func (r *Remote) SetNetworkOn(on bool) error {
	r.mu.Lock()
	n := &r.saved.Network
	if on && !n.Passphrase {
		r.mu.Unlock()
		return errors.New("Set a passphrase first.")
	}
	n.On = on
	if on {
		n.Locked, n.Wrong = "", 0
	}
	err := r.saved.write(r.path)
	rc := r.relay
	r.mu.Unlock()
	if rc != nil {
		rc.network(on)
	}
	r.changed()
	return err
}

// SetAutoAllow says whether a device that proves the passphrase is let in without asking.
func (r *Remote) SetAutoAllow(on bool) error {
	r.mu.Lock()
	r.saved.Network.AutoAllow = on
	err := r.saved.write(r.path)
	r.mu.Unlock()
	r.audit.event(fmt.Sprintf("auto-allow=%v", on), "", "")
	r.changed()
	return err
}

// recordLocked returns the passphrase's record, reading it from the keyring the first time. Call
// with r.mu held.
func (r *Remote) recordLocked() (*secure.PassRecord, error) {
	if r.net.record != nil {
		return r.net.record, nil
	}
	if _, err := r.identityLocked(); err != nil {
		return nil, err
	}
	v, err := r.secrets.Get(networkSecret)
	if err != nil {
		return nil, err
	}
	rec, err := secure.DecodePassRecord(v)
	if err != nil {
		return nil, err
	}
	r.net.record = &rec
	return &rec, nil
}

// guess counts a guess of the passphrase, which answering a first message lets the asker test, and
// returns the record to answer with; nil when joining is off, or has had too many wrong guesses. The
// guess is taken back (right) if it turns out right. The last guess allowed turns joining off.
func (r *Remote) guess() *secure.PassRecord {
	r.mu.Lock()
	n := &r.saved.Network
	if !n.Passphrase || !n.On || n.Locked != "" || n.Wrong >= maxWrong {
		r.mu.Unlock()
		return nil
	}
	rec, err := r.recordLocked()
	if err != nil {
		r.mu.Unlock()
		r.log.Warn("remote: reading the passphrase's record", "err", err)
		return nil
	}
	n.Wrong++
	locked := n.Wrong >= maxWrong
	if locked {
		n.Locked = fmt.Sprintf("%d wrong passphrases were tried, so joining by passphrase turned off. Turn it on when you're sure it was you.", n.Wrong)
	}
	if err := r.saved.write(r.path); err != nil {
		r.log.Warn("remote: saving the count of wrong passphrases", "err", err)
	}
	rc := r.relay
	r.mu.Unlock()
	if locked {
		if rc != nil {
			rc.network(false)
		}
		r.audit.event("passphrase locked", "", "")
		if r.cfg.Notify != nil {
			r.cfg.Notify("Joining by passphrase turned off", "Too many wrong passphrases were tried on this machine.")
		}
		r.changed()
	}
	return rec
}

// right takes back a guess that was right.
func (r *Remote) right() {
	r.mu.Lock()
	if r.saved.Network.Wrong > 0 {
		r.saved.Network.Wrong--
		if err := r.saved.write(r.path); err != nil {
			r.log.Warn("remote: saving the count of wrong passphrases", "err", err)
		}
	}
	r.mu.Unlock()
}

// answerPass runs the machine's side of a passphrase handshake on a connection whose first frame is
// first. It counts the guess, and returns the channel once the passphrase is proven.
func (r *Remote) answerPass(rc *relayConn, c *muxConn, first []byte) (*secure.Channel, bool) {
	rec := r.guess()
	if rec == nil {
		return nil, false
	}
	s, f2, err := secure.RespondPass(*rec, first)
	if err != nil || c.Write(f2) != nil {
		return nil, false
	}
	f3, err := c.Read()
	if err != nil {
		return nil, false
	}
	f4, err := s.Verify(f3, rc.origin, rc.machine.ID, rc.machine.Noise, rand.Reader)
	if err != nil {
		return nil, false // a wrong passphrase stays counted
	}
	r.right()
	if c.Write(f4) != nil {
		return nil, false
	}
	f5, err := c.Read()
	if err != nil {
		return nil, false
	}
	ch, err := s.Finish(f5)
	if err != nil {
		return nil, false
	}
	return ch, true
}

// servePassPairing runs one connection of a device that asks to join by passphrase: the handshake,
// who is asking, and then the wait for the person, unless a member let it in already or devices are
// let in without asking.
func (r *Remote) servePassPairing(rc *relayConn, c *muxConn) {
	defer c.Close()
	late := time.AfterFunc(2*handshakeWithin, func() { c.Close() })
	defer late.Stop()
	first, err := c.Read()
	if err != nil {
		return
	}
	ch, ok := r.answerPass(rc, c, first)
	if !ok {
		return
	}
	link := wire.NewLink(c, ch)
	req, ok := request(link)
	if !ok {
		return
	}
	late.Stop()
	c.timeout.Store(0)
	name := device.CleanName(req.Name)
	if name == "" {
		name = "A device"
	}
	key := append([]byte(nil), ch.Peer()...)

	r.mu.Lock()
	a, allowed := r.net.allowed[hex.EncodeToString(key)]
	allowed = allowed && time.Now().Before(a.until)
	auto := r.saved.Network.AutoAllow
	r.mu.Unlock()
	if allowed || auto {
		d, err := r.admit(rc, key, name, a.readOnly)
		if err != nil {
			r.log.Warn("remote: letting a device in", "err", err)
			d = device.Decision{}
		}
		link.Send(decision(d))
		return
	}

	q := &joinRequest{id: randomID(), name: name, at: time.Now(), key: key, decide: make(chan device.Decision, 2)}
	r.mu.Lock()
	if len(r.net.joins) >= maxJoins {
		r.mu.Unlock()
		link.Send(decision(device.Decision{}))
		return
	}
	r.net.joins = append(r.net.joins, q)
	r.mu.Unlock()
	r.changed()
	if r.cfg.Notify != nil {
		r.cfg.Notify("A device wants to join", "“"+name+"” knows the passphrase. Allow it in Settings › Remote.")
	}
	leave := func() {
		r.mu.Lock()
		r.net.joins = slices.DeleteFunc(r.net.joins, func(x *joinRequest) bool { return x == q })
		r.mu.Unlock()
		r.changed()
	}
	gone := make(chan struct{})
	go func() {
		link.Receive()
		close(gone)
	}()
	wait := time.NewTimer(joinFor)
	defer wait.Stop()
	select {
	case d := <-q.decide:
		link.Send(decision(d))
	case <-gone:
		leave()
	case <-wait.C:
		leave()
		link.Send(decision(device.Decision{}))
	}
}

// admit pairs a device that proved the passphrase and was let in, and returns what it is told.
func (r *Remote) admit(rc *relayConn, key []byte, name string, readOnly bool) (device.Decision, error) {
	psk := make([]byte, secure.KeySize)
	if _, err := rand.Read(psk); err != nil {
		return device.Decision{}, err
	}
	id := secure.DeviceID(key)
	now := time.Now()
	d := savedDevice{ID: id.String(), Key: key, PSK: psk, Name: name, ReadOnly: readOnly, Added: now}
	r.mu.Lock()
	devices := slices.DeleteFunc(slices.Clone(r.saved.Devices), func(x savedDevice) bool { return x.ID == d.ID })
	if len(devices) >= MaxDevices {
		r.mu.Unlock()
		return device.Decision{}, errors.New("this machine has as many devices as it can pair with")
	}
	r.saved.Devices = append(devices, d)
	if err := r.saved.write(r.path); err != nil {
		r.saved.Devices = devices
		r.mu.Unlock()
		return device.Decision{}, err
	}
	r.mu.Unlock()
	r.cut(d.ID)
	r.audit.event("joined", d.ID, d.Name)
	r.changed()
	return device.Decision{OK: true, PSK: psk, Voucher: rc.machine.Voucher(rc.origin, id, now), Name: r.cfg.Name,
		ReadOnly: readOnly, Machine: rc.machine.ID.String()}, nil
}

// AllowJoin lets in a device that proved the passphrase, here and, on this machine's word, at every
// other member it reaches.
func (r *Remote) AllowJoin(requestID string, readOnly bool) error {
	r.mu.Lock()
	i := slices.IndexFunc(r.net.joins, func(q *joinRequest) bool { return q.id == requestID })
	rc := r.relay
	if i < 0 || rc == nil {
		r.mu.Unlock()
		return errors.New("That device isn't waiting any more. Ask it to try again.")
	}
	q := r.net.joins[i]
	r.net.joins = slices.Delete(slices.Clone(r.net.joins), i, i+1)
	r.mu.Unlock()
	d, err := r.admit(rc, q.key, q.name, readOnly)
	if err != nil {
		q.decide <- device.Decision{}
		r.changed()
		return err
	}
	q.decide <- d
	go r.allowEverywhere(q.key, readOnly)
	if r.cfg.Notify != nil {
		r.cfg.Notify("A device joined", "“"+q.name+"” can now use Solo on your machines.")
	}
	return nil
}

// DenyJoin turns a waiting device down.
func (r *Remote) DenyJoin(requestID string) {
	r.mu.Lock()
	i := slices.IndexFunc(r.net.joins, func(q *joinRequest) bool { return q.id == requestID })
	var q *joinRequest
	if i >= 0 {
		q = r.net.joins[i]
		r.net.joins = slices.Delete(slices.Clone(r.net.joins), i, i+1)
	}
	r.mu.Unlock()
	if q != nil {
		q.decide <- device.Decision{}
		r.audit.event("join denied", "", q.name)
	}
	r.changed()
}

// allowed lets in, on a member's word, a device that proved the passphrase (rpc.Options.Allow): the one
// waiting here, or one that comes in the next few minutes. Only a device that proves the passphrase
// here too is let in: a member's word alone opens nothing.
func (r *Remote) allowed(d rpc.Device, key []byte, readOnly bool) error {
	k := hex.EncodeToString(key)
	r.mu.Lock()
	if r.net.allowed == nil {
		r.net.allowed = map[string]allowedKey{}
	}
	for x, a := range r.net.allowed {
		if time.Now().After(a.until) {
			delete(r.net.allowed, x)
		}
	}
	r.net.allowed[k] = allowedKey{until: time.Now().Add(allowedFor), readOnly: readOnly}
	i := slices.IndexFunc(r.net.joins, func(q *joinRequest) bool { return hex.EncodeToString(q.key) == k })
	var q *joinRequest
	if i >= 0 {
		q = r.net.joins[i]
		r.net.joins = slices.Delete(slices.Clone(r.net.joins), i, i+1)
	}
	rc := r.relay
	r.mu.Unlock()
	r.audit.event("allowed by a member", "", d.Name)
	if q == nil || rc == nil {
		return nil
	}
	dec, err := r.admit(rc, q.key, q.name, readOnly)
	if err != nil {
		q.decide <- device.Decision{}
		return err
	}
	q.decide <- dec
	return nil
}

// allowEverywhere tells every member a device was let in here.
func (r *Remote) allowEverywhere(key []byte, readOnly bool) {
	r.eachMember(func(ctx context.Context, m savedMember, p *rpc.Peer) error {
		return p.Call(ctx, "allow", map[string]any{"key": key, "readOnly": readOnly}, nil)
	})
}

// sharePassphrase gives every member a new passphrase.
func (r *Remote) sharePassphrase(passphrase string) {
	r.eachMember(func(ctx context.Context, m savedMember, p *rpc.Peer) error {
		return p.Call(ctx, "passphrase", map[string]any{"passphrase": passphrase}, nil)
	})
}

// takePassphrase takes a new passphrase a member set (rpc.Options.Passphrase).
func (r *Remote) takePassphrase(d rpc.Device, passphrase string) error {
	if err := api.CheckPassphrase(passphrase); err != nil {
		return err
	}
	if err := r.keepPassphrase(passphrase); err != nil {
		return err
	}
	r.audit.event("passphrase set by a member", "", d.Name)
	return nil
}

// eachMember runs f with a session to every member at once, and logs what fails.
func (r *Remote) eachMember(f func(context.Context, savedMember, *rpc.Peer) error) {
	r.mu.Lock()
	members := slices.Clone(r.saved.Network.Members)
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), peerFor)
			defer cancel()
			p, err := r.memberSession(ctx, m)
			if err == nil {
				err = f(ctx, m, p)
				p.Close()
			}
			if err != nil {
				r.log.Warn("remote: a request to a member", "member", m.Name, "err", err)
			}
		}()
	}
	wg.Wait()
}

// RemoveMember forgets a member: its sessions end here. It can join again with the passphrase.
func (r *Remote) RemoveMember(machineID string) error {
	r.mu.Lock()
	i := slices.IndexFunc(r.saved.Network.Members, func(m savedMember) bool { return m.ID == machineID })
	if i < 0 {
		r.mu.Unlock()
		return errors.New("no such member")
	}
	m := r.saved.Network.Members[i]
	r.saved.Network.Members = slices.Delete(slices.Clone(r.saved.Network.Members), i, i+1)
	delete(r.net.usage, m.ID)
	err := r.saved.write(r.path)
	r.mu.Unlock()
	r.rpc.Drop(memberDevice(m.ID))
	r.audit.event("member removed", m.ID, m.Name)
	r.changed()
	return err
}

// memberDevice is how a member's sessions are known to rpc.Host.
func memberDevice(id string) string { return "member:" + id }

// keepMember saves a member, in place of an earlier record of the same machine.
func (r *Remote) keepMember(m savedMember) error {
	r.mu.Lock()
	members := slices.DeleteFunc(slices.Clone(r.saved.Network.Members), func(x savedMember) bool { return x.ID == m.ID })
	if len(members) >= maxMembers {
		r.mu.Unlock()
		return errors.New("the network has as many machines as it can")
	}
	prev := r.saved.Network.Members
	r.saved.Network.Members = append(members, m)
	err := r.saved.write(r.path)
	if err != nil {
		r.saved.Network.Members = prev
	}
	r.mu.Unlock()
	r.rpc.Drop(memberDevice(m.ID)) // sessions under an earlier key end
	r.changed()
	return err
}

// servePeer runs a connection from another machine on the relay, which the relay says is from (with
// the relay key it connected under): a member's session, or a machine joining the network.
func (r *Remote) servePeer(rc *relayConn, c *muxConn, from secure.ID, relayKey []byte) {
	defer c.Close()
	late := time.AfterFunc(2*handshakeWithin, func() { c.Close() })
	defer late.Stop()
	first, err := c.Read()
	if err != nil || len(first) < 2 {
		return
	}
	switch first[0] {
	case peerSession:
		r.mu.Lock()
		i := slices.IndexFunc(r.saved.Network.Members, func(m savedMember) bool { return m.ID == from.String() })
		var m savedMember
		if i >= 0 {
			m = r.saved.Network.Members[i]
		}
		r.mu.Unlock()
		if i < 0 {
			return
		}
		p := secure.Params{Purpose: secure.Peer, Origin: rc.origin, Machine: rc.machine.ID}
		ch, answer, err := secure.Respond(p, rc.machine.Noise, m.PSK, first[1:], rand.Reader)
		if err != nil || !constantEqual(ch.Peer(), m.Key) || c.Write(answer) != nil {
			return
		}
		late.Stop()
		c.timeout.Store(0)
		link := wire.NewLink(c, ch)
		link.Compress = true
		expire := time.AfterFunc(sessionFor, func() { c.Close() })
		defer expire.Stop()
		r.rpc.Serve(link, rpc.Device{ID: memberDevice(m.ID), Name: m.Name, Member: true})
	case peerJoin:
		ch, ok := r.answerPass(rc, c, first[1:])
		if !ok {
			return
		}
		// The machine the relay says this is: its key must be the one it proved the passphrase with.
		if secure.MachineID(ch.Peer(), relayKey) != from {
			return
		}
		link := wire.NewLink(c, ch)
		var hello struct {
			Name string `json:"name"`
		}
		msg, err := link.ReceiveSmall(1024)
		if err != nil || json.Unmarshal(msg, &hello) != nil {
			return
		}
		mine, _ := json.Marshal(map[string]string{"name": r.cfg.Name})
		if link.Send(mine) != nil {
			return
		}
		m := savedMember{ID: from.String(), Name: cmpOr(device.CleanName(hello.Name), "Solo"), Key: append([]byte(nil), ch.Peer()...),
			PSK: ch.Derive("member"), Added: time.Now()}
		if err := r.keepMember(m); err != nil {
			r.log.Warn("remote: keeping a member", "err", err)
			return
		}
		r.audit.event("member joined", m.ID, m.Name)
		if r.cfg.Notify != nil {
			r.cfg.Notify("A machine joined your network", "Solo on “"+m.Name+"” now shares usage with this machine.")
		}
	}
}

// dialPeer opens a connection to another machine on the relay, as this machine.
func (r *Remote) dialPeer(ctx context.Context, target secure.ID) (wire.Conn, *relayConn, error) {
	r.mu.Lock()
	rc, address := r.relay, r.saved.Relay
	r.mu.Unlock()
	if rc == nil {
		return nil, nil, errors.New("this machine isn't connected to its relay")
	}
	token, err := rc.machine.PeerAuth(rc.origin, target, time.Now().Add(rc.skew), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	conn, err := device.Native(address, r.cfg.HTTP)(ctx, device.PathPeer, token)
	if err != nil {
		var closed *device.CloseError
		if errors.As(err, &closed) && closed.Code == device.CloseOffline {
			return nil, nil, errors.New("it's offline")
		}
		return nil, nil, err
	}
	return conn, rc, nil
}

// memberSession opens a session with a member.
func (r *Remote) memberSession(ctx context.Context, m savedMember) (*rpc.Peer, error) {
	target, err := secure.ParseID(m.ID)
	if err != nil {
		return nil, err
	}
	conn, rc, err := r.dialPeer(ctx, target)
	if err != nil {
		return nil, err
	}
	p := secure.Params{Purpose: secure.Peer, Origin: rc.origin, Machine: target}
	ini, first, err := secure.Initiate(p, rc.machine.Noise, m.Key, m.PSK, rand.Reader)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Write(append([]byte{peerSession}, first...)); err != nil {
		conn.Close()
		return nil, err
	}
	answer, err := conn.Read()
	if err != nil {
		conn.Close()
		return nil, err
	}
	ch, err := ini.Finish(answer)
	if err != nil {
		conn.Close()
		return nil, errors.New("it no longer knows this machine")
	}
	return rpc.OpenPeer(ctx, wire.NewLink(conn, ch))
}

// firstPrefixed puts a byte before the first frame written on a connection: what a peer connection
// is for.
type firstPrefixed struct {
	wire.Conn
	prefix byte
	done   bool
}

func (c *firstPrefixed) Write(b []byte) error {
	if !c.done {
		c.done = true
		return c.Conn.Write(append([]byte{c.prefix}, b...))
	}
	return c.Conn.Write(b)
}

// join joins the machine on the relay with this ID: it proves the passphrase there, and the two keep
// each other as members.
func (r *Remote) join(ctx context.Context, target secure.ID, passphrase string) (savedMember, error) {
	conn, rc, err := r.dialPeer(ctx, target)
	if err != nil {
		return savedMember{}, err
	}
	link, err := device.PassHandshake(ctx, &firstPrefixed{Conn: conn, prefix: peerJoin}, rc.origin, target,
		[]byte(passphrase), rc.machine.Noise, rand.Reader)
	if err != nil {
		if errors.Is(err, secure.ErrWrongPassphrase) {
			return savedMember{}, errors.New("its passphrase is a different one")
		}
		return savedMember{}, err
	}
	defer link.Close()
	hello, _ := json.Marshal(map[string]string{"name": r.cfg.Name})
	if err := link.Send(hello); err != nil {
		return savedMember{}, err
	}
	msg, err := link.ReceiveSmall(1024)
	var theirs struct {
		Name string `json:"name"`
	}
	if err != nil || json.Unmarshal(msg, &theirs) != nil {
		return savedMember{}, errors.New("it didn't answer")
	}
	ch := link.Channel()
	m := savedMember{ID: target.String(), Name: cmpOr(device.CleanName(theirs.Name), "Solo"), Key: append([]byte(nil), ch.Peer()...),
		PSK: ch.Derive("member"), Added: time.Now()}
	return m, r.keepMember(m)
}

// joinAll joins every machine on the relay whose passphrase pairing is on and that isn't a member
// yet, and says how it went.
func (r *Remote) joinAll(passphrase string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r.mu.Lock()
	rc, address := r.relay, r.saved.Relay
	members := map[string]bool{}
	for _, m := range r.saved.Network.Members {
		members[m.ID] = true
	}
	r.mu.Unlock()
	status := func(s string) {
		r.mu.Lock()
		r.net.status = s
		r.mu.Unlock()
		r.changed()
	}
	if rc == nil {
		status("This machine isn't connected to its relay, so it hasn't joined the others yet. It will when you set the passphrase again.")
		return
	}
	ids, err := device.Network(ctx, device.WebSocket(address, r.cfg.HTTP))
	if err != nil {
		status("Couldn't ask the relay which machines are in the network: " + err.Error())
		return
	}
	var joined, failed []string
	for _, id := range ids {
		if id == rc.machine.ID || members[id.String()] {
			continue
		}
		m, err := r.join(ctx, id, passphrase)
		if err != nil {
			failed = append(failed, fmt.Sprintf("a machine (%s…): %v", id.String()[:6], err))
			continue
		}
		joined = append(joined, m.Name)
		r.audit.event("joined a member", m.ID, m.Name)
	}
	switch {
	case len(failed) > 0:
		status("Couldn't join " + strings.Join(failed, "; ") + ".")
	case len(joined) > 0:
		status("Joined " + strings.Join(joined, ", ") + ".")
	default:
		status("")
	}
}

// MemberUsage fetches every member's usage at once. A member that can't be reached comes with its last
// copy.
func (r *Remote) MemberUsage(ctx context.Context) []api.MemberUsage {
	r.mu.Lock()
	members := slices.Clone(r.saved.Network.Members)
	r.mu.Unlock()
	out := make([]api.MemberUsage, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, peerFor)
			defer cancel()
			var rep core.UsageReport
			p, err := r.memberSession(ctx, m)
			if err == nil {
				err = p.Call(ctx, "usage", struct{}{}, &rep)
				p.Close()
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.net.usage == nil {
				r.net.usage = map[string]api.MemberUsage{}
			}
			got := r.net.usage[m.ID]
			got.ID, got.Name, got.Err = m.ID, m.Name, ""
			if err != nil {
				got.Err = err.Error()
			} else {
				got.Report, got.At = rep, time.Now()
			}
			r.net.usage[m.ID] = got
			out[i] = got
		}()
	}
	wg.Wait()
	return out
}

// network tells the relay whether this machine takes passphrases: whether it is listed in the
// network, and devices may ask to join by passphrase here.
func (rc *relayConn) network(on bool) {
	frame := make([]byte, 0, 43)
	op := byte(6)
	if on {
		op = 5
	}
	frame = append(frame, frameTicket, op)
	frame = append(frame, make([]byte, 32+8+1)...)
	rc.send(frame)
}

// checkNetwork asks the relay whether it can carry a network, and says so when it can't.
func (r *Remote) checkNetwork(address string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/v1/info", nil)
	if err != nil {
		return
	}
	resp, err := r.cfg.HTTP.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var info struct {
		Network int `json:"network"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&info) != nil {
		return
	}
	problem := ""
	if info.Network < 1 {
		problem = "The relay is older than networks. Update it (Settings › Remote › The relay in Cloudflare › Update) to join by passphrase."
	}
	r.mu.Lock()
	changed := r.net.problem != problem
	r.net.problem = problem
	r.mu.Unlock()
	if changed {
		r.changed()
	}
}

func randomID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func cmpOr(s, orElse string) string {
	if s == "" {
		return orElse
	}
	return s
}
