//go:build !js

package host

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/rpc"
	"github.com/tyler-builds/solo/internal/remote/secure"
)

// Linked Solos share their usage with each other. A Solo links to another the way a device pairs: with
// the pairing code the other shows and the six digits typed there, but as a device that may only read
// usage (savedDevice.Linked), under a key of its own (secure.Machine.Link). Once paired it links back
// over that session: it adds the other machine as such a device here, and hands it what it needs to
// reach this machine (rpc.LinkGrant). Each end trusts the other for the reason pairing does: the device
// had the machine's key from the code, and the machine had the device's digits from the person.

const (
	// maxLinks is how many Solos one links to.
	maxLinks = 16
	// linkFor is how long making a link may take, the person typing the digits included.
	linkFor = 5 * time.Minute
	// fetchFor is how long fetching a linked machine's usage may take.
	fetchFor = 20 * time.Second
)

// savedLink is a Solo on another machine that this one reads usage from: what a device keeps of a
// machine (device.Machine), and the relay that reaches it.
type savedLink struct {
	ID      string    `json:"id"` // the other machine's ID
	Name    string    `json:"name"`
	Relay   string    `json:"relay"`
	Key     []byte    `json:"key"`
	PSK     []byte    `json:"psk"`
	Voucher []byte    `json:"voucher"`
	Added   time.Time `json:"added"`
	// Device is the device on this machine that the other reaches it by: the link's way back. "" when
	// there is none (this machine had no relay when they were linked).
	Device string `json:"device,omitempty"`
}

// linking is a link being made from here.
type linking struct {
	digits string
	done   bool
	err    string
	note   string
	cancel func()
}

// LinkTo links this Solo to the one whose pairing link this is: it asks to pair as a linked Solo, and
// returns once the six digits to type there are known (State.Linking); the rest runs on its own.
func (r *Remote) LinkTo(link string) error {
	address, code, err := device.ParseLink(link)
	if err != nil {
		return err
	}
	if !time.Now().Before(code.Expires) {
		return device.ErrExpired
	}
	u, _ := url.Parse(address)
	origin := device.Origin(u.Host)
	r.mu.Lock()
	m, err := r.identityLocked()
	if err != nil {
		r.mu.Unlock()
		return err
	}
	if code.Machine == m.ID {
		r.mu.Unlock()
		return errors.New("That link is this machine's own. Copy the one the other machine shows.")
	}
	if len(r.saved.Links) >= maxLinks && !slices.ContainsFunc(r.saved.Links, func(l savedLink) bool { return l.ID == code.Machine.String() }) {
		r.mu.Unlock()
		return errors.New("This machine is linked to as many Solos as it can be. Remove one first.")
	}
	old := r.linking
	r.linking = nil
	key := m.Link
	name := r.cfg.Name
	r.mu.Unlock()
	if old != nil && old.cancel != nil {
		old.cancel()
	}

	ctx, cancel := context.WithTimeout(context.Background(), linkFor)
	dial := device.WebSocket(address, r.cfg.HTTP)
	p, err := device.PairAs(ctx, dial, origin, code, key, device.Request{Name: name, Solo: true}, rand.Reader)
	if err != nil {
		cancel()
		var refused *device.RefusedError
		if errors.As(err, &refused) {
			return errors.New("The other machine's relay didn't let this one in. The link may have expired: make a new one there.")
		}
		return errors.New("Couldn't reach the other machine: " + err.Error())
	}
	l := &linking{digits: p.Digits}
	l.cancel = func() { cancel(); p.Cancel() }
	r.mu.Lock()
	r.linking = l
	r.mu.Unlock()
	r.changed()
	go func() {
		defer cancel()
		note, err := r.finishLink(ctx, p, address)
		r.mu.Lock()
		if r.linking == l {
			l.done = true
			l.note = note
			if err != nil {
				l.err = err.Error()
			}
		}
		r.mu.Unlock()
		r.changed()
	}()
	return nil
}

// CancelLink gives up the link being made.
func (r *Remote) CancelLink() {
	r.mu.Lock()
	l := r.linking
	r.linking = nil
	r.mu.Unlock()
	if l != nil && l.cancel != nil {
		l.cancel()
	}
	r.changed()
}

// finishLink waits for the other machine's person to approve, keeps the link, and links back.
func (r *Remote) finishLink(ctx context.Context, p *device.Pairing, address string) (string, error) {
	m, err := p.Wait()
	if err != nil {
		if errors.Is(err, device.ErrDenied) {
			return "", errors.New("The other machine turned the link down.")
		}
		return "", err
	}
	l := savedLink{ID: m.ID, Name: cmpOr(m.Name, "Solo"), Relay: address, Key: m.Key, PSK: m.PSK, Voucher: m.Voucher,
		Added: time.Now()}
	note, back := r.linkBack(ctx, l)
	l.Device = back
	if err := r.keepLink(l); err != nil {
		return "", err
	}
	r.audit.event("linked", l.ID, l.Name)
	if note == "" {
		note = "Linked with " + l.Name + ". Each machine's Usage page now shows the other's."
	}
	return note, nil
}

// linkBack lets the machine just linked to reach this one: it is added here as a linked Solo, by the
// key it says it links with, and handed what reaching this machine takes. It returns a note when it
// couldn't, and the device it added when it could.
func (r *Remote) linkBack(ctx context.Context, l savedLink) (note, deviceID string) {
	r.mu.Lock()
	rc, online, relay := r.relay, r.conn == Online, r.saved.Relay
	r.mu.Unlock()
	if rc == nil || !online {
		return "Linked: this machine sees " + l.Name + "'s usage. For " + l.Name + " to see this machine's, set up remote access here and link again.", ""
	}
	peer, err := r.openPeer(ctx, l)
	if err != nil {
		return "Linked one way: " + l.Name + " couldn't be reached to link back (" + err.Error() + ").", ""
	}
	defer peer.Close()
	var lk struct {
		Key []byte `json:"key"`
	}
	if err := peer.Call(ctx, "linkKey", struct{}{}, &lk); err != nil || len(lk.Key) != secure.KeySize {
		return "Linked one way: " + l.Name + " can't link back. Update Solo there.", ""
	}
	psk := make([]byte, secure.KeySize)
	if _, err := rand.Read(psk); err != nil {
		return "Linked one way: " + err.Error(), ""
	}
	id := secure.DeviceID(lk.Key)
	now := time.Now()
	d := savedDevice{ID: id.String(), Key: lk.Key, PSK: psk, Name: l.Name, Linked: true, Added: now}
	r.mu.Lock()
	devices := slices.DeleteFunc(slices.Clone(r.saved.Devices), func(x savedDevice) bool { return x.ID == d.ID })
	if len(devices) >= MaxDevices {
		r.mu.Unlock()
		return "Linked one way: this machine has as many devices as it can pair with. Remove one, then link again.", ""
	}
	prev := r.saved.Devices
	r.saved.Devices = append(devices, d)
	if err := r.saved.write(r.path); err != nil {
		r.saved.Devices = prev
		r.mu.Unlock()
		return "Linked one way: " + err.Error(), ""
	}
	grant := rpc.LinkGrant{Relay: relay, ID: rc.machine.ID.String(), Key: rc.machine.Noise.Public, PSK: psk,
		Voucher: rc.machine.Voucher(rc.origin, id, now), Name: r.cfg.Name}
	r.mu.Unlock()
	r.cut(d.ID) // whatever it had open under an earlier record ends
	if err := peer.Call(ctx, "link", grant, nil); err != nil {
		r.dropDevice(d.ID)
		return "Linked one way: " + l.Name + " didn't take the link back (" + err.Error() + ").", ""
	}
	r.changed()
	return "", d.ID
}

// acceptLink keeps what a linked Solo granted this machine to reach it (rpc.Options.Link): the device
// that sends it is the Solo it reaches. The grant must hang together: its voucher is the other
// machine's, for its relay, and for the key this machine links with.
func (r *Remote) acceptLink(d rpc.Device, g rpc.LinkGrant) error {
	u, err := url.Parse(g.Relay)
	if err != nil || u.Host == "" || u.Path != "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
		return errors.New("the link's relay isn't valid")
	}
	id, err := secure.ParseID(g.ID)
	if err != nil {
		return errors.New("the link's machine isn't valid")
	}
	relayKey, machine, dev, err := secure.VerifyVoucher(device.Origin(u.Host), g.Voucher)
	r.mu.Lock()
	m, ierr := r.identityLocked()
	r.mu.Unlock()
	if ierr != nil {
		return ierr
	}
	if err != nil || machine != id || dev != secure.DeviceID(m.Link.Public) || secure.MachineID(g.Key, relayKey) != id {
		return errors.New("the link doesn't hold together")
	}
	l := savedLink{ID: g.ID, Name: cmpOr(device.CleanName(g.Name), cmpOr(d.Name, "Solo")), Relay: u.Scheme + "://" + u.Host,
		Key: g.Key, PSK: g.PSK, Voucher: g.Voucher, Added: time.Now(), Device: d.ID}
	// A linked Solo grants one way back to itself: a later grant takes the place of its earlier one,
	// whatever machine it names.
	r.mu.Lock()
	r.saved.Links = slices.DeleteFunc(slices.Clone(r.saved.Links), func(x savedLink) bool { return x.Device == d.ID && x.ID != l.ID })
	r.mu.Unlock()
	if err := r.keepLink(l); err != nil {
		return err
	}
	r.audit.event("linked back", l.ID, l.Name)
	return nil
}

// keepLink saves a link, in place of an earlier one to the same machine.
func (r *Remote) keepLink(l savedLink) error {
	r.mu.Lock()
	links := slices.DeleteFunc(slices.Clone(r.saved.Links), func(x savedLink) bool { return x.ID == l.ID })
	if len(links) >= maxLinks {
		r.mu.Unlock()
		return errors.New("This machine is linked to as many Solos as it can be. Remove one first.")
	}
	prev := r.saved.Links
	r.saved.Links = append(links, l)
	err := r.saved.write(r.path)
	if err != nil {
		r.saved.Links = prev
	}
	r.mu.Unlock()
	r.changed()
	return err
}

// dropDevice removes a device without a word in the audit log: one this machine added itself.
func (r *Remote) dropDevice(id string) {
	r.mu.Lock()
	r.saved.Devices = slices.DeleteFunc(slices.Clone(r.saved.Devices), func(x savedDevice) bool { return x.ID == id })
	if err := r.saved.write(r.path); err != nil {
		r.log.Warn("remote: saving the devices", "err", err)
	}
	r.mu.Unlock()
	r.cut(id)
}

// Unlink removes a link, and the other machine's way back here: it can no longer read this machine's
// usage. The other machine keeps its side until it next finds this one won't answer.
func (r *Remote) Unlink(machineID string) error {
	r.mu.Lock()
	i := slices.IndexFunc(r.saved.Links, func(l savedLink) bool { return l.ID == machineID })
	if i < 0 {
		r.mu.Unlock()
		return errors.New("no such link")
	}
	l := r.saved.Links[i]
	r.saved.Links = slices.Delete(slices.Clone(r.saved.Links), i, i+1)
	if l.Device != "" {
		r.saved.Devices = slices.DeleteFunc(slices.Clone(r.saved.Devices), func(x savedDevice) bool { return x.ID == l.Device })
	}
	delete(r.usage, l.ID)
	err := r.saved.write(r.path)
	r.mu.Unlock()
	if l.Device != "" {
		r.cut(l.Device)
	}
	r.audit.event("unlinked", l.ID, l.Name)
	r.changed()
	return err
}

// linkKey is the key this machine links to others with (rpc.Options.LinkKey).
func (r *Remote) linkKey() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.identityLocked()
	if err != nil {
		return nil
	}
	return m.Link.Public
}

// openPeer opens a session with a linked machine.
func (r *Remote) openPeer(ctx context.Context, l savedLink) (*rpc.Peer, error) {
	u, err := url.Parse(l.Relay)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	m, err := r.identityLocked()
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	link, err := device.Connect(ctx, device.WebSocket(l.Relay, r.cfg.HTTP), device.Origin(u.Host),
		device.Machine{ID: l.ID, Key: l.Key, PSK: l.PSK, Voucher: l.Voucher}, m.Link, rand.Reader)
	if err != nil {
		var closed *device.CloseError
		if errors.As(err, &closed) && closed.Code == device.CloseOffline {
			return nil, errors.New("it's offline")
		}
		return nil, err
	}
	return rpc.OpenPeer(ctx, link)
}

// LinkedUsage fetches each linked machine's usage, all at once. A machine that can't be reached
// comes with its last copy.
func (r *Remote) LinkedUsage(ctx context.Context) []api.LinkedUsage {
	r.mu.Lock()
	links := slices.Clone(r.saved.Links)
	r.mu.Unlock()
	out := make([]api.LinkedUsage, len(links))
	var wg sync.WaitGroup
	for i, l := range links {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, fetchFor)
			defer cancel()
			var rep core.UsageReport
			peer, err := r.openPeer(ctx, l)
			if err == nil {
				err = peer.Call(ctx, "usage", struct{}{}, &rep)
				peer.Close()
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.usage == nil {
				r.usage = map[string]api.LinkedUsage{}
			}
			got := r.usage[l.ID]
			got.ID, got.Name, got.Err = l.ID, l.Name, ""
			if err != nil {
				got.Err = err.Error()
			} else {
				got.Report, got.At = rep, time.Now()
			}
			r.usage[l.ID] = got
			out[i] = got
		}()
	}
	wg.Wait()
	return out
}

func cmpOr(s, orElse string) string {
	if s == "" {
		return orElse
	}
	return s
}
