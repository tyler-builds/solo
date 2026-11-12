package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
)

// Device is the paired device a session serves.
type Device struct {
	ID   string
	Name string
	// ReadOnly: it may look, and every request that changes something is refused.
	ReadOnly bool
}

// Options configure a Host. The zero value works.
type Options struct {
	// Info describes the machine to a device.
	Info func() HostInfo
	// IsDir reports whether a path is a folder on the machine, for folders a device adds to the
	// settings. Nil refuses them all.
	IsDir func(path string) bool
	// Audit records a request that changes something, and how it ended. It must not block.
	Audit func(d Device, method, detail string, err error)
	// FlushEvery is the shortest time between two messages of state to a device (default 300 ms):
	// streaming text changes many times a second, and the relay's free quota counts messages.
	FlushEvery time.Duration
	// Lease is how long a device's watch lasts unrenewed (default 45 s).
	Lease time.Duration
	// Idle is how long a silent session is kept (default IdleTimeout).
	Idle time.Duration
	Log  *slog.Logger
}

// Host serves paired devices over a backend. One Host lasts for the machine's run: it numbers the
// backend's changes, so a device that reconnects is sent only what changed while it was away.
type Host struct {
	b     api.Backend
	opts  Options
	epoch string
	unsub func()

	mu       sync.Mutex
	rev      uint64
	threads  map[string]uint64 // the revision at which each thread last changed
	items    map[string]uint64 // and each transcript
	conns    map[*conn]struct{}
	answered map[string]*answer // answers to requests, kept so that one sent twice runs once
}

// answer is a request's outcome, or one still being worked out.
type answer struct {
	done chan struct{}
	res  Envelope
	at   time.Time
}

const (
	answersKept = 1024
	answersFor  = 10 * time.Minute
	// maxRequests is how many requests of one device run at a time.
	maxRequests = 16
	// maxChanges is how many announcements wait for a device that has stopped reading before its
	// session is ended; it resumes from its revision when it connects again.
	maxChanges = 4096
)

// NewHost starts serving a backend. Close it when done.
func NewHost(b api.Backend, opts Options) *Host {
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = 300 * time.Millisecond
	}
	if opts.Lease <= 0 {
		opts.Lease = 45 * time.Second
	}
	if opts.Idle <= 0 {
		opts.Idle = IdleTimeout
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	// Revisions start at 1: 0 is a device that holds nothing.
	h := &Host{b: b, opts: opts, epoch: hex.EncodeToString(id[:]), rev: 1, threads: map[string]uint64{},
		items: map[string]uint64{}, conns: map[*conn]struct{}{}, answered: map[string]*answer{}}
	h.unsub = b.Subscribe(h.onChange)
	return h
}

// Close stops listening to the backend and ends every session.
func (h *Host) Close() {
	h.unsub()
	h.mu.Lock()
	conns := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.s.close(ErrClosed)
	}
}

// Drop ends the sessions of a device, such as one that was just revoked.
func (h *Host) Drop(deviceID string) {
	h.mu.Lock()
	var conns []*conn
	for c := range h.conns {
		if c.dev.ID == deviceID {
			conns = append(conns, c)
		}
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.bye(CodeDenied, "This device's access was removed.")
	}
}

// Sessions returns the IDs of the devices connected now.
func (h *Host) Sessions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for c := range h.conns {
		if !slices.Contains(out, c.dev.ID) {
			out = append(out, c.dev.ID)
		}
	}
	slices.Sort(out)
	return out
}

// onChange runs on the backend's goroutines: it only notes what changed.
func (h *Host) onChange(ch core.Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch ch.Kind {
	case core.ChangeThread:
		h.rev++
		h.threads[ch.ThreadID] = h.rev
	case core.ChangeItems:
		h.rev++
		h.items[ch.ThreadID] = h.rev
	case core.ChangeStream:
	default:
		h.rev++
	}
	for c := range h.conns {
		c.note(ch)
	}
}

// conn is one device's session.
type conn struct {
	h      *Host
	s      *session
	dev    Device
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	mu       sync.Mutex
	threads  map[string]bool // threads to send
	items    map[string]bool // watched transcripts that changed
	live     map[string]bool // watched threads whose streaming text changed
	misc     bool            // the settings, todos, repositories, limits or questions may have changed
	changes  []core.Change
	stalled  bool
	watch    map[string]*watched
	limit    int
	lease    time.Time
	requests int
	greeted  bool

	// What was last sent, for sending only what differs. Used by the flusher alone.
	sent map[string]uint64
}

// watched is a thread the device shows, and what it was last sent of it. Its fields are the flusher's,
// except reset and held, which are set under conn.mu.
type watched struct {
	reset bool   // send the whole window again
	held  uint64 // the transcript revision the device says it holds
	began bool
	items map[int64]uint64 // each item sent, by its hash
	live  []core.LiveBlock
	// liveSent: the device was sent this thread's streaming text in this session.
	liveSent bool
	view     uint64 // the thread's view as last sent, hashed
}

// Serve serves a device over a link until the session ends, and returns why it ended.
func (h *Host) Serve(link Link, dev Device) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &conn{h: h, s: newSession(link, maxFromDevice), dev: dev, ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), threads: map[string]bool{}, items: map[string]bool{}, live: map[string]bool{},
		watch: map[string]*watched{}, limit: 1, sent: map[string]uint64{}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.flusher()
	}()
	go func() { // a session that falls silent is over: the device, or the path to it, is gone
		t := time.NewTicker(h.opts.Idle / 3)
		defer t.Stop()
		for {
			select {
			case <-c.s.done:
				return
			case <-t.C:
				if c.s.silent() > h.opts.Idle {
					c.s.close(errors.New("rpc: the device went silent"))
					return
				}
			}
		}
	}()
	err := c.s.run(c.handle)
	cancel()
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
	wg.Wait()
	return err
}

// bye tells the device why the session ends, then ends it.
func (c *conn) bye(code, msg string) {
	p, _ := json.Marshal(Bye{Code: code, Message: msg})
	c.s.send(Envelope{T: kindBye, P: p})
	c.s.close(errors.New("rpc: " + msg))
}

// note records a change for the device. It runs under Host.mu and must not block.
func (c *conn) note(ch core.Change) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ch.Kind {
	case core.ChangeThread:
		c.threads[ch.ThreadID] = true
		c.misc = true
	case core.ChangeItems:
		if c.watch[ch.ThreadID] == nil {
			return // the device doesn't show it; it asks when it does
		}
		c.items[ch.ThreadID] = true
	case core.ChangeStream:
		if c.watch[ch.ThreadID] != nil {
			c.live[ch.ThreadID] = true
		}
		c.poke()
		return // not announced: the device announces the text it is sent
	default:
		c.misc = true
	}
	if len(c.changes) >= maxChanges {
		c.stalled = true
	} else {
		c.changes = append(c.changes, ch)
	}
	c.poke()
}

func (c *conn) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// handle takes one envelope from the device.
func (c *conn) handle(e Envelope) error {
	if !c.greeted {
		if e.T != kindHello {
			return errProtocol
		}
		return c.hello(e)
	}
	if e.T != kindReq {
		return errProtocol
	}
	if !validID(e.ID) || len(e.M) > 64 {
		return errProtocol
	}
	c.mu.Lock()
	if c.requests >= maxRequests {
		c.mu.Unlock()
		go c.s.send(Envelope{T: kindRes, ID: e.ID, E: &Error{Code: CodeBusy, Message: "Too many requests at once."}})
		return nil
	}
	c.requests++
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			c.requests--
			c.mu.Unlock()
		}()
		c.serve(e)
	}()
	return nil
}

func (c *conn) hello(e Envelope) error {
	var hi Hello
	if json.Unmarshal(e.P, &hi) != nil {
		return errProtocol
	}
	if hi.Protocol != Protocol {
		msg := "Update Solo on this machine to use it from this web app."
		if hi.Protocol < Protocol {
			msg = "This web app is out of date. Reload it."
		}
		c.bye(CodeVersion, msg)
		return errors.New("rpc: protocol mismatch")
	}
	h := c.h
	w := Welcome{Protocol: Protocol, Epoch: h.epoch, ReadOnly: c.dev.ReadOnly}
	if h.opts.Info != nil {
		w.Host = h.opts.Info()
	}
	w.Host.Time = time.Now().UnixMilli()

	// From here the device is told of every change; what it missed is worked out against the same
	// revision, so nothing falls between the two.
	h.mu.Lock()
	w.Resumed = hi.Epoch == h.epoch && hi.Rev > 0 && hi.Rev <= h.rev
	c.mu.Lock()
	if w.Resumed {
		for id, rev := range h.threads {
			if rev > hi.Rev {
				c.threads[id] = true
			}
		}
	} else {
		c.threads[allThreads] = true
	}
	c.misc = true
	c.greeted = true
	c.mu.Unlock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()

	p, _ := json.Marshal(w)
	if err := c.s.send(Envelope{T: kindWelcome, P: p}); err != nil {
		return err
	}
	c.poke()
	return nil
}

// allThreads, in conn.threads, stands for every thread.
const allThreads = "\x00all"

// flusher sends the device what changed, no more often than Options.FlushEvery.
func (c *conn) flusher() {
	first := true
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.s.done:
			return
		case <-c.wake:
		}
		if err := c.flush(first); err != nil {
			c.s.close(err)
			return
		}
		first = false
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(c.h.opts.FlushEvery):
		}
	}
}

// flush reads what changed and sends it. first marks the catch-up after hello.
func (c *conn) flush(first bool) error {
	h := c.h
	c.mu.Lock()
	if !c.greeted {
		c.mu.Unlock()
		return nil
	}
	if c.stalled {
		c.mu.Unlock()
		return errors.New("rpc: the device fell too far behind")
	}
	threads, items, live, misc, changes := c.threads, c.items, c.live, c.misc, c.changes
	c.threads, c.items, c.live, c.misc, c.changes = map[string]bool{}, map[string]bool{}, map[string]bool{}, false, nil
	watching := time.Now().Before(c.lease)
	watch := make(map[string]*watched, len(c.watch))
	for id, w := range c.watch {
		watch[id] = w
		if w.reset || !w.began {
			items[id], live[id] = true, true
		}
	}
	limit := c.limit
	c.mu.Unlock()

	// The revision is read before the state: what changes in between is sent again after a resume,
	// which costs a little and loses nothing.
	h.mu.Lock()
	rev := h.rev
	itemRevs := make(map[string]uint64, len(items))
	for id := range items {
		itemRevs[id] = h.items[id] + 1
	}
	h.mu.Unlock()

	var out []Sync
	cur := &Sync{}
	next := func() {
		out = append(out, *cur)
		cur = &Sync{}
	}

	// Threads, a few to a message.
	var views []core.ThreadView
	if threads[allThreads] {
		views = h.b.Threads()
	} else {
		ids := make([]string, 0, len(threads))
		for id := range threads {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			if v, ok := h.b.Thread(id); ok {
				views = append(views, v)
			}
		}
	}
	sentView := map[string]bool{}
	for _, v := range views {
		v.Live = nil
		sentView[v.ID] = true
		if w := watch[v.ID]; w != nil {
			w.view = hashOf(v)
		}
		cur.Threads = append(cur.Threads, v)
		if len(cur.Threads) == 48 {
			next()
		}
	}

	if misc {
		c.misc2(cur)
	}

	if watching {
		for _, id := range sortedKeys(live) {
			w := watch[id]
			if w == nil {
				continue
			}
			v, ok := h.b.Thread(id)
			if !ok {
				continue
			}
			if u, changed := liveUpdate(id, w, v.Live); changed {
				cur.Live = append(cur.Live, u)
			}
			// Streaming also moves parts of the thread's view (its thinking, its step in progress).
			if !sentView[id] {
				v.Live = nil
				if hv := hashOf(v); hv != w.view {
					w.view = hv
					cur.Threads = append(cur.Threads, v)
				}
			}
		}
		for _, id := range sortedKeys(items) {
			w := watch[id]
			if w == nil {
				continue
			}
			all, err := h.b.Items(c.ctx, id, limit)
			if err != nil {
				continue
			}
			c.mu.Lock()
			reset, held := w.reset || !w.began, w.held
			w.reset, w.held = false, 0
			c.mu.Unlock()
			seen := make(map[int64]uint64, len(all))
			u := ItemsUpdate{ThreadID: id, Rev: itemRevs[id], Reset: reset}
			size := 0
			quiet := reset && held != 0 && held == itemRevs[id] // the device holds this very window
			if quiet {
				u.Reset = false
			}
			for _, it := range all {
				hv := hashItem(it)
				seen[it.ID] = hv
				if quiet || (!reset && w.items[it.ID] == hv) {
					continue
				}
				u.Upsert = append(u.Upsert, it)
				if size += len(it.Data) + 128; size > partSize/2 {
					cur.Items = append(cur.Items, u)
					next()
					u, size = ItemsUpdate{ThreadID: id, Rev: itemRevs[id]}, 0
				}
			}
			if !reset {
				for old := range w.items {
					if _, ok := seen[old]; !ok {
						u.Remove = append(u.Remove, old)
					}
				}
				slices.Sort(u.Remove)
			}
			w.items, w.began = seen, true
			if reset || len(u.Upsert) > 0 || len(u.Remove) > 0 {
				u.Done = true
				cur.Items = append(cur.Items, u)
			}
		}
	} else {
		// The lease ran out: the device is hidden or gone. What it watched is sent again, whole,
		// when it renews.
		c.mu.Lock()
		for _, w := range c.watch {
			w.reset = true
		}
		c.mu.Unlock()
	}

	cur.Changes = changes
	cur.Rev = rev
	cur.Synced = first
	next()
	for _, sy := range out {
		if !first && sy.empty() {
			continue
		}
		p, err := json.Marshal(sy)
		if err != nil {
			return err
		}
		if err := c.s.send(Envelope{T: kindSync, P: p}); err != nil {
			return err
		}
	}
	return nil
}

func (s Sync) empty() bool {
	return len(s.Threads) == 0 && len(s.Live) == 0 && len(s.Items) == 0 && s.Settings == nil && s.Todos == nil &&
		s.Repos == nil && s.Plain == nil && s.Rate == nil && s.Pending == nil && len(s.Changes) == 0 && !s.Synced
}

// misc2 adds the small lists that differ from what the device was last sent.
func (c *conn) misc2(sy *Sync) {
	b := c.h.b
	differs := func(name string, v any) bool {
		hv := hashOf(v)
		if old, ok := c.sent[name]; ok && old == hv {
			return false
		}
		c.sent[name] = hv
		return true
	}
	if v := b.Settings(); differs("settings", v) {
		sy.Settings = &v
	}
	if v := orEmpty(b.Todos(true)); differs("todos", v) {
		sy.Todos = &v
	}
	if v := orEmpty(b.RepoPaths()); differs("repos", v) {
		sy.Repos = &v
	}
	if v := orEmpty(b.PlainFolders()); differs("plain", v) {
		sy.Plain = &v
	}
	if v := orEmpty(b.RateLimits()); differs("rate", v) {
		sy.Rate = &v
	}
	pending := map[string][]core.PendingQuestion{}
	for _, t := range b.Threads() {
		if t.PendingAsk {
			if qs := b.PendingQuestions(t.ID); len(qs) > 0 {
				pending[t.ID] = qs
			}
		}
	}
	if differs("pending", pending) {
		sy.Pending = &pending
	}
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// liveUpdate works out what to send of a thread's streaming text, given what was sent before.
func liveUpdate(id string, w *watched, now []core.LiveBlock) (LiveUpdate, bool) {
	u := LiveUpdate{ThreadID: id, Count: len(now)}
	changed := len(now) != len(w.live)
	if !w.liveSent {
		// The first update of a session starts the device over: it may hold text from an earlier one.
		w.liveSent, w.live = true, nil
		u.Reset, changed = true, true
	}
	for i, b := range now {
		if i < len(w.live) {
			old := w.live[i]
			if old == b {
				continue
			}
			if old.Thinking == b.Thinking && strings.HasPrefix(b.Text, old.Text) {
				u.Blocks = append(u.Blocks, LiveDelta{Index: i, Thinking: b.Thinking, Text: b.Text[len(old.Text):]})
				changed = true
				continue
			}
		}
		u.Blocks = append(u.Blocks, LiveDelta{Index: i, Thinking: b.Thinking, Reset: true, Text: b.Text})
		changed = true
	}
	w.live = slices.Clone(now)
	return u, changed
}

func hashOf(v any) uint64 {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	h := fnv.New64a()
	h.Write(data)
	return h.Sum64() | 1 // never zero, which stands for "not sent"
}

func hashItem(it core.Item) uint64 {
	h := fnv.New64a()
	for _, s := range []string{it.Kind, it.TurnUUID, it.Ref} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	h.Write(it.Data)
	return h.Sum64() | 1
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// serve runs one request and sends its answer. A request that changes something runs once however
// often it is sent: a device that lost its connection sends it again under the same ID.
func (c *conn) serve(e Envelope) {
	m, ok := methods[e.M]
	if !ok {
		c.s.send(Envelope{T: kindRes, ID: e.ID, E: invalid("unknown request %q", clip(e.M, 40))})
		return
	}
	if m.write && c.dev.ReadOnly {
		c.s.send(Envelope{T: kindRes, ID: e.ID, E: denied("This device can look but not change anything.")})
		return
	}
	if !m.write {
		ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
		defer cancel()
		c.s.send(c.run(ctx, m, e))
		return
	}
	h := c.h
	key := c.dev.ID + "\x00" + e.ID
	h.mu.Lock()
	a := h.answered[key]
	mine := a == nil
	if mine {
		a = &answer{done: make(chan struct{}), at: time.Now()}
		h.answered[key] = a
		h.forgetAnswersLocked()
	}
	h.mu.Unlock()
	if mine {
		// It runs to its end even if the device goes: a merge half done helps nobody.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		a.res = c.run(ctx, m, e)
		cancel()
		close(a.done)
	}
	select {
	case <-a.done:
		c.s.send(a.res)
	case <-c.ctx.Done():
	}
}

// forgetAnswersLocked drops old answers. Call with h.mu held.
func (h *Host) forgetAnswersLocked() {
	if len(h.answered) <= answersKept {
		cutoff := time.Now().Add(-answersFor)
		for k, a := range h.answered {
			if a.at.Before(cutoff) && isDone(a) {
				delete(h.answered, k)
			}
		}
		return
	}
	for k, a := range h.answered {
		if isDone(a) {
			delete(h.answered, k)
			if len(h.answered) <= answersKept*3/4 {
				return
			}
		}
	}
}

func isDone(a *answer) bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}

func (c *conn) run(ctx context.Context, m method, e Envelope) (res Envelope) {
	res = Envelope{T: kindRes, ID: e.ID}
	var detail string
	var err error
	defer func() {
		if r := recover(); r != nil {
			c.h.opts.Log.Error("rpc: a request panicked", "method", e.M, "panic", r)
			res = Envelope{T: kindRes, ID: e.ID, E: &Error{Code: CodeError, Message: "The request failed on the machine."}}
			err = errors.New("panic")
		}
		if m.write && c.h.opts.Audit != nil {
			c.h.opts.Audit(c.dev, e.M, detail, err)
		}
	}()
	var out any
	out, res.Blob, detail, err = m.run(ctx, c, e.P, e.Blob)
	if err != nil {
		res.E, res.Blob = toError(err), nil
		return res
	}
	if out != nil {
		if res.P, err = json.Marshal(out); err != nil {
			res.E = toError(err)
		}
	}
	return res
}
