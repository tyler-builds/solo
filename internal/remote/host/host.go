//go:build !js

// Package host is a machine's side of remote access: it keeps the machine's identity and the devices
// paired with it, holds the connection to the relay, pairs new devices with the approval of the person
// at the machine, and serves each device's session over the app's backend (internal/remote/rpc).
//
// Nothing here trusts the relay. A device is let in by the handshake it runs with the machine, keyed
// with a secret the relay never had; what the relay says of a connection only picks which key to try.
package host

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/remote/bundle"
	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/rpc"
	"github.com/tyler-builds/solo/internal/remote/secure"
)

// Config is what a Remote needs.
type Config struct {
	// Dir is Solo's data folder: remote.json and the audit log live there.
	Dir     string
	Backend api.Backend
	// Secrets keeps the machine's seed; nil uses the OS keyring (NewSecrets(Dir)).
	Secrets Secrets
	// Name is what devices call this machine; "" uses the host name.
	Name string
	Log  *slog.Logger
	// Notify tells the person at the machine that something happened, such as a device being paired.
	Notify func(title, body string)
	// HTTP reaches the relay and Cloudflare; nil uses http.DefaultClient. Tests set it to trust a local
	// certificate.
	HTTP *http.Client
	// CloudflareAPI is Cloudflare's API ("" for the real one), and Bundle what is deployed there (nil
	// for what is built into the app); tests set both.
	CloudflareAPI string
	Bundle        func() (*bundle.Bundle, error)
	// Timing, for tests; zero values use the defaults.
	PairingFor time.Duration // how long a pairing code lasts (5 minutes)
	FlushEvery time.Duration // rpc.Options.FlushEvery
	Retry      time.Duration // the first wait before connecting to the relay again (1 second)
}

// The state Settings shows is defined with the interface the UI drives (api.Remote).
type (
	Connection = api.RemoteConnection
	State      = api.RemoteState
	Device     = api.RemoteDevice
	Pairing    = api.RemotePairing
	Request    = api.RemoteRequest
)

const (
	Off        = api.RemoteOff
	Connecting = api.RemoteConnecting
	Online     = api.RemoteOnline
	Refused    = api.RemoteRefused
)

var _ api.Remote = (*Remote)(nil)

// Remote is a machine's remote access.
type Remote struct {
	cfg    Config
	log    *slog.Logger
	rpc    *rpc.Host
	path   string
	audit  *auditLog
	random func([]byte) (int, error)

	mu       sync.Mutex
	saved    saved
	machine  *secure.Machine // loaded when first needed: reading the keyring may ask the user
	secrets  Secrets
	conn     Connection
	problem  string
	relay    *relayConn // the connection to the relay, when there is one
	stop     chan struct{}
	pairing  *pairing
	subs     map[int]func()
	nextSub  int
	lastSave time.Time
	closed   bool
	done     chan struct{} // closed by Close

	hasToken  bool   // a Cloudflare token is kept, or set in the environment
	cloudBusy string // what a deploy is doing now
}

// New loads the machine's remote access, and connects to its relay if it is set up and on.
func New(cfg Config) (*Remote, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	cfg.Name = device.CleanName(cfg.Name)
	if cfg.Name == "" {
		cfg.Name = "Solo"
	}
	if cfg.PairingFor <= 0 {
		cfg.PairingFor = 5 * time.Minute
	}
	if cfg.Retry <= 0 {
		cfg.Retry = time.Second
	}
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	r := &Remote{cfg: cfg, log: cfg.Log, path: filepath.Join(cfg.Dir, "remote.json"), secrets: cfg.Secrets,
		conn: Off, subs: map[int]func(){}, random: rand.Read, done: make(chan struct{})}
	var err error
	if r.saved, err = loadSaved(r.path); err != nil {
		return nil, err
	}
	// Devices unused for a long time are forgotten: a phone sold or lost long ago shouldn't still open
	// this machine.
	cutoff := time.Now().Add(-unusedFor)
	kept := r.saved.Devices[:0]
	for _, d := range r.saved.Devices {
		last := d.LastSeen
		if last.IsZero() {
			last = d.Added
		}
		if last.After(cutoff) {
			kept = append(kept, d)
		}
	}
	if len(kept) != len(r.saved.Devices) {
		r.saved.Devices = kept
		if err := r.saved.write(r.path); err != nil {
			return nil, err
		}
	}
	r.audit = newAuditLog(filepath.Join(cfg.Dir, "remote-audit.log"), cfg.Log)
	r.rpc = rpc.NewHost(cfg.Backend, rpc.Options{
		FlushEvery: cfg.FlushEvery,
		Log:        cfg.Log,
		Info: func() rpc.HostInfo {
			home, _ := os.UserHomeDir()
			return rpc.HostInfo{Name: cfg.Name, OS: goos, Home: home}
		},
		IsDir: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.IsDir() && filepath.IsAbs(p)
		},
		Audit: func(d rpc.Device, method, detail string, err error) { r.audit.request(d, method, detail, err) },
	})
	// Whether a Cloudflare token is kept is only looked up once remote access is in use: reading the
	// keyring can ask the user to unlock it, and most starts of Solo have no need.
	if r.saved.Relay != "" || r.saved.Cloud.Account != "" {
		if t, err := r.token(); err == nil {
			r.hasToken = t != ""
		}
	} else {
		r.hasToken = strings.TrimSpace(os.Getenv(tokenEnv)) != ""
	}
	if r.saved.Enabled && r.saved.Relay != "" && !r.saved.Cloud.Paused {
		r.start()
	}
	if r.saved.Cloud.Script != "" {
		go r.watchRelay()
	}
	return r, nil
}

// Close ends remote access for this run.
func (r *Remote) Close() {
	r.mu.Lock()
	if !r.closed {
		close(r.done)
	}
	r.closed = true
	r.stopLocked()
	r.mu.Unlock()
	r.rpc.Close()
	r.audit.close()
}

// Subscribe calls f, off the caller's goroutine, whenever State changes.
func (r *Remote) Subscribe(f func()) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = f
	return func() {
		r.mu.Lock()
		delete(r.subs, id)
		r.mu.Unlock()
	}
}

// changed tells subscribers. Call without r.mu held.
func (r *Remote) changed() {
	r.mu.Lock()
	subs := make([]func(), 0, len(r.subs))
	for _, f := range r.subs {
		subs = append(subs, f)
	}
	r.mu.Unlock()
	for _, f := range subs {
		go f()
	}
}

// identity returns the machine's keys, reading them from the keyring the first time. Call with r.mu
// held.
func (r *Remote) identityLocked() (*secure.Machine, error) {
	if r.machine != nil {
		return r.machine, nil
	}
	if r.secrets == nil {
		r.secrets = NewSecrets(r.cfg.Dir)
	}
	m, err := loadMachine(r.secrets)
	if err != nil {
		return nil, err
	}
	r.machine = &m
	return r.machine, nil
}

// RelayKey returns the key the relay must list for this machine to connect (HOST_KEYS), as text.
func (r *Remote) RelayKey() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.identityLocked()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(m.RelayKey()), nil
}

// State returns remote access as it stands.
func (r *Remote) State() State {
	online := r.rpc.Sessions()
	r.mu.Lock()
	defer r.mu.Unlock()
	st := State{Relay: r.saved.Relay, Enabled: r.saved.Enabled, Connection: r.conn, Problem: r.problem, Name: r.cfg.Name,
		Cloud: r.cloudStateLocked()}
	if r.machine != nil {
		st.MachineID = r.machine.ID.String()
	}
	if r.secrets != nil {
		st.SecretsInFiles = r.secrets.InFiles()
	}
	for _, d := range r.saved.Devices {
		st.Devices = append(st.Devices, Device{ID: d.ID, Name: d.Name, ReadOnly: d.ReadOnly, Added: d.Added,
			LastSeen: d.LastSeen, Online: slices.Contains(online, d.ID)})
	}
	if p := r.pairing; p != nil && time.Now().Before(p.code.Expires) {
		st.Pairing = &Pairing{Link: p.link, Expires: p.code.Expires}
		for _, q := range p.requests {
			st.Requests = append(st.Requests, Request{Name: q.name, At: q.at})
		}
	}
	return st
}

// SetRelay names the relay this machine connects to, by the address its web client is served from, and
// turns remote access on. An empty address forgets the relay and turns it off.
func (r *Remote) SetRelay(address string) error {
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	if address != "" {
		u, err := url.Parse(address)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("the relay's address looks like https://name.account.workers.dev")
		}
		if u.Scheme == "http" && !isLoopback(u.Hostname()) {
			return errors.New("a relay that isn't on this machine must use https")
		}
	}
	r.mu.Lock()
	if _, err := r.identityLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	r.stopLocked()
	r.saved.Relay, r.saved.Enabled = address, address != ""
	err := r.saved.write(r.path)
	if err == nil && r.saved.Enabled {
		r.start()
	}
	r.mu.Unlock()
	r.changed()
	return err
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// SetEnabled turns remote access on or off. Off closes the connection to the relay and every device's
// session; the paired devices are kept.
func (r *Remote) SetEnabled(on bool) error {
	r.mu.Lock()
	if on && r.saved.Relay == "" {
		r.mu.Unlock()
		return errors.New("remote access has no relay yet")
	}
	if on {
		if _, err := r.identityLocked(); err != nil {
			r.mu.Unlock()
			return err
		}
	}
	r.stopLocked()
	r.saved.Enabled = on
	err := r.saved.write(r.path)
	if err == nil && on && !r.saved.Cloud.Paused {
		r.start()
	}
	r.mu.Unlock()
	r.changed()
	return err
}

// Revoke removes a device: its session ends now, and its key opens nothing from here on.
func (r *Remote) Revoke(deviceID string) error {
	r.mu.Lock()
	i := slices.IndexFunc(r.saved.Devices, func(d savedDevice) bool { return d.ID == deviceID })
	if i < 0 {
		r.mu.Unlock()
		return errors.New("no such device")
	}
	name := r.saved.Devices[i].Name
	r.saved.Devices = slices.Delete(slices.Clone(r.saved.Devices), i, i+1)
	err := r.saved.write(r.path)
	r.mu.Unlock()
	r.rpc.Drop(deviceID)
	r.audit.event("revoked", deviceID, name)
	r.changed()
	return err
}

// RevokeAll removes every device and withdraws any pairing code.
func (r *Remote) RevokeAll() error {
	r.mu.Lock()
	devices := r.saved.Devices
	r.saved.Devices = nil
	err := r.saved.write(r.path)
	r.mu.Unlock()
	r.CancelPairing()
	for _, d := range devices {
		r.rpc.Drop(d.ID)
		r.audit.event("revoked", d.ID, d.Name)
	}
	r.changed()
	return err
}

// SetReadOnly changes whether a device may only look. Its session ends, and its next one has the new
// access.
func (r *Remote) SetReadOnly(deviceID string, readOnly bool) error {
	r.mu.Lock()
	i := slices.IndexFunc(r.saved.Devices, func(d savedDevice) bool { return d.ID == deviceID })
	if i < 0 {
		r.mu.Unlock()
		return errors.New("no such device")
	}
	devices := slices.Clone(r.saved.Devices)
	devices[i].ReadOnly = readOnly
	r.saved.Devices = devices
	name := devices[i].Name
	err := r.saved.write(r.path)
	r.mu.Unlock()
	r.rpc.Drop(deviceID)
	r.audit.event(fmt.Sprintf("read-only=%v", readOnly), deviceID, name)
	r.changed()
	return err
}

// device returns a paired device by the ID the relay gave. Call with r.mu held.
func (r *Remote) deviceLocked(id secure.ID) (savedDevice, bool) {
	want := id.String()
	for _, d := range r.saved.Devices {
		if d.ID == want {
			return d, true
		}
	}
	return savedDevice{}, false
}

// seen records that a device connected. The file is written at most once a minute for it.
func (r *Remote) seen(id string) {
	r.mu.Lock()
	i := slices.IndexFunc(r.saved.Devices, func(d savedDevice) bool { return d.ID == id })
	if i < 0 {
		r.mu.Unlock()
		return
	}
	devices := slices.Clone(r.saved.Devices)
	devices[i].LastSeen = time.Now()
	r.saved.Devices = devices
	if time.Since(r.lastSave) > time.Minute {
		r.lastSave = time.Now()
		if err := r.saved.write(r.path); err != nil {
			r.log.Warn("remote: saving the devices", "err", err)
		}
	}
	r.mu.Unlock()
	r.changed()
}

// setConn records where the connection to the relay stands.
func (r *Remote) setConn(c Connection, problem string) {
	r.mu.Lock()
	same := r.conn == c && r.problem == problem
	r.conn, r.problem = c, problem
	r.mu.Unlock()
	if !same {
		r.changed()
	}
}

// decision is the machine's answer to a pairing request, as the device reads it.
func decision(d device.Decision) []byte {
	b, _ := json.Marshal(d)
	return b
}
