package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
)

// LeaseEvery is how often a device renews its watch.
const LeaseEvery = 25 * time.Second

const (
	// itemsKept is how many threads' transcripts a client keeps; the ones shown longest ago go first.
	itemsKept = 6
	// touchedFor is how long a thread whose transcript the UI read stays watched without being named
	// by Watch.
	touchedFor = 2 * time.Minute
	// callFor bounds a request made without a context.
	callFor = time.Minute
	// connectWait is how long a request waits for a connection before it fails.
	connectWait = 15 * time.Second
	// maxTries is how often a request is sent while connections drop under it.
	maxTries = 3
	// openPRSpare is how long opening pull requests is waited for beyond the longest the machine takes
	// (Settings.OpenPRTimeout), for what its steps do around their limits.
	openPRSpare = 5 * time.Minute
)

// uploadPace is the wait after each part of a large upload. A part is four of the relay's frames, so
// this is 32 frames a second, under the 50 the relay allows a device: about 1.5 MB a second.
const uploadPace = 125 * time.Millisecond

// ErrOffline is returned by a request made while the machine can't be reached.
var ErrOffline = errors.New("Not connected to the machine. Try again in a moment.")

// EndError is why the machine ended a session (Bye).
type EndError struct{ Code, Message string }

func (e *EndError) Error() string { return e.Message }

// Status is where a client stands with its machine.
type Status struct {
	Connected bool // a session is up
	Synced    bool // and the client shows the machine as it is
	ReadOnly  bool // this device may look and not change
	Host      HostInfo
}

// ClientOptions configure a Client.
type ClientOptions struct {
	// Random makes request IDs (crypto/rand.Reader).
	Random io.Reader
	// OnStatus is called, off the caller's goroutine, when Status changes.
	OnStatus func(Status)
	// Limit is how many of a transcript's newest items to hold when the UI hasn't said (default 200).
	Limit int
	// Now is this device's clock (time.Now), which Client.Now measures the machine's time against.
	Now func() time.Time
}

// Client is an api.Backend over a connection to a machine. It keeps a mirror of the machine's state,
// which the UI reads without waiting, and outlives its connections: Run serves one, and the next
// resumes where the last left off.
type Client struct {
	opts ClientOptions

	mu       sync.Mutex
	epoch    string
	rev      uint64
	threads  map[string]core.ThreadView // without their live text
	sorted   []core.ThreadView          // threads in order, live text included; nil when stale
	live     map[string][]core.LiveBlock
	items    map[string]*transcript
	settings core.Settings
	todos    []core.Todo
	repos    []string
	plain    []string
	rate     []agent.Event
	pending  map[string][]core.PendingQuestion
	subs     map[int]func(core.Change)
	nextSub  int

	s       *session
	up      chan struct{} // closed while a session is up; replaced when it ends
	calls   map[string]chan Envelope
	status  Status
	watch   []string // the threads the UI named
	open    string   // the thread the UI has open (Open)
	touched map[string]time.Time
	limit   int
	lease   chan struct{} // wakes the session's lease keeper
	clock   machineClock
}

// machineClock tells the machine's time by this device's clock. The machine's times (when a turn
// began) are stamped by its clock, and a device's clock can be off from it by minutes either way. In
// a browser it can also fall behind while the page runs: under TinyGo a page's clock is Date.now() as
// the page loaded plus performance.now(), which never takes up a correction to the device's clock and,
// in browsers that pause it while the device sleeps, loses the time the page was asleep.
//
// So the machine says its time in the welcome and in each sync, and the client keeps the latest
// reading with this device's clock at the time: the machine's time now is that reading plus how long
// this device has counted since. A reading is how the machine's clock stood when it sent the message,
// so it can only be behind the machine's time when it arrives: within a session the estimate only
// moves forward to a later reading (a slow message leaves it where it is, a clock that lost time while
// the device slept catches up), and a new session starts it over.
type machineClock struct {
	machine time.Time // the machine's time at the reading, zero before the first
	local   time.Time // this device's clock then
}

// now is the machine's time when this device's clock reads local.
func (k machineClock) now(local time.Time) time.Time {
	if k.machine.IsZero() {
		return local
	}
	return k.machine.Add(local.Sub(k.local))
}

// read takes in the machine's time, in Unix milliseconds, from a message that arrived when this
// device's clock read local. With fresh set it replaces what was known (a new session); otherwise it
// is kept only if it is later than the current estimate.
func (k *machineClock) read(ms int64, local time.Time, fresh bool) {
	if ms <= 0 {
		return
	}
	m := time.UnixMilli(ms)
	if fresh || k.machine.IsZero() || m.After(k.now(local)) {
		k.machine, k.local = m, local
	}
}

// transcript is a thread's items as the client holds them.
type transcript struct {
	rev   uint64
	items []core.Item         // in order; replaced, never changed in place
	build map[int64]core.Item // an update in progress
	used  time.Time
}

// NewClient makes a client with nothing in it. Run gives it a connection.
func NewClient(opts ClientOptions) *Client {
	if opts.Limit <= 0 {
		opts.Limit = 200
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Client{opts: opts, threads: map[string]core.ThreadView{}, live: map[string][]core.LiveBlock{},
		items: map[string]*transcript{}, settings: core.DefaultSettings(), pending: map[string][]core.PendingQuestion{},
		subs: map[int]func(core.Change){}, up: make(chan struct{}), calls: map[string]chan Envelope{},
		touched: map[string]time.Time{}, limit: opts.Limit, lease: make(chan struct{}, 1)}
}

var _ api.Backend = (*Client)(nil)
var _ api.Watcher = (*Client)(nil)
var _ api.Opener = (*Client)(nil)
var _ api.Clock = (*Client)(nil)

// Status returns where the client stands with its machine.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Now is the machine's time, as well as this device can tell it: see machineClock. Before the first
// session it is this device's own.
func (c *Client) Now() time.Time {
	local := c.opts.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clock.now(local)
}

// HostVersion is the version of Solo on the machine, as it last said: "" before the first session, and
// from a Solo too old to say.
func (c *Client) HostVersion() string { return c.Status().Host.Version }

func (c *Client) setStatus(f func(*Status)) {
	c.mu.Lock()
	before := c.status
	f(&c.status)
	after := c.status
	c.mu.Unlock()
	if after != before && c.opts.OnStatus != nil {
		c.opts.OnStatus(after)
	}
}

// Run serves one connection until it ends, and returns why: an *EndError when the machine said so.
// The mirror is kept, and the next connection resumes from it.
func (c *Client) Run(link Link) error {
	s := newSession(link, maxFromMachine)
	s.pace = uploadPace
	c.mu.Lock()
	hello := Hello{Protocol: Protocol, Epoch: c.epoch, Rev: c.rev}
	c.mu.Unlock()
	p, _ := json.Marshal(hello)
	if err := s.send(Envelope{T: kindHello, P: p}); err != nil {
		return err
	}
	var ended error
	stop := make(chan struct{})
	go func() { // say we are here, and notice when the machine isn't
		t := time.NewTicker(PingEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if s.silent() > IdleTimeout {
					s.close(errors.New("rpc: the machine went silent"))
					return
				}
				s.ping()
			}
		}
	}()
	greeted := false
	err := s.run(func(e Envelope) error {
		switch {
		case e.T == kindBye:
			var b Bye
			if json.Unmarshal(e.P, &b) != nil {
				return errProtocol
			}
			ended = &EndError{Code: b.Code, Message: clip(b.Message, 500)}
			return ended
		case !greeted:
			if e.T != kindWelcome {
				return errProtocol
			}
			var w Welcome
			if json.Unmarshal(e.P, &w) != nil {
				return errProtocol
			}
			if w.Protocol != Protocol {
				ended = &EndError{Code: CodeVersion, Message: "This web app and Solo on the machine are different versions."}
				return ended
			}
			greeted = true
			c.welcome(s, w)
			go c.keepLease(s, stop)
			return nil
		case e.T == kindSync:
			var sy Sync
			if json.Unmarshal(e.P, &sy) != nil {
				return errProtocol
			}
			local := c.opts.Now()
			c.mu.Lock()
			c.clock.read(sy.Time, local, false)
			c.mu.Unlock()
			return c.apply(sy)
		case e.T == kindRes:
			c.mu.Lock()
			ch := c.calls[e.ID]
			delete(c.calls, e.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- e
			}
			return nil
		}
		return errProtocol
	})
	close(stop)
	c.mu.Lock()
	if c.s == s {
		c.s = nil
		c.up = make(chan struct{})
	}
	c.mu.Unlock()
	c.setStatus(func(st *Status) { st.Connected, st.Synced = false, false })
	if ended != nil {
		return ended
	}
	return err
}

// welcome starts a session: it keeps the mirror when the machine resumes it, and drops it otherwise.
func (c *Client) welcome(s *session, w Welcome) {
	local := c.opts.Now()
	c.mu.Lock()
	c.clock.read(w.Host.Time, local, true)
	if !w.Resumed || w.Epoch != c.epoch {
		c.epoch, c.rev = w.Epoch, 0
		c.threads, c.sorted = map[string]core.ThreadView{}, nil
		c.live = map[string][]core.LiveBlock{}
		c.items = map[string]*transcript{}
	}
	c.s = s
	close(c.up)
	c.mu.Unlock()
	c.setStatus(func(st *Status) {
		st.Connected, st.Synced, st.ReadOnly, st.Host = true, false, w.ReadOnly, w.Host
	})
}

// keepLease tells the machine which threads the UI shows, when that changes and often enough that the
// machine keeps sending them.
func (c *Client) keepLease(s *session, stop chan struct{}) {
	t := time.NewTicker(LeaseEvery)
	defer t.Stop()
	for {
		c.mu.Lock()
		w := Watch{Limit: c.limit, Open: c.open}
		for _, id := range c.watchedLocked() {
			wt := WatchThread{ID: id}
			if tr := c.items[id]; tr != nil {
				wt.Rev = tr.rev
			}
			w.Threads = append(w.Threads, wt)
		}
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), IdleTimeout)
		c.callOn(ctx, s, "watch", w, nil)
		cancel()
		select {
		case <-stop:
			return
		case <-s.done:
			return
		case <-t.C:
		case <-c.lease:
		}
	}
}

// watchedLocked returns the threads to watch: those the UI named, then those whose transcripts it read
// lately. Call with c.mu held.
func (c *Client) watchedLocked() []string {
	out := slices.Clone(c.watch)
	type recent struct {
		id string
		at time.Time
	}
	var rs []recent
	cutoff := time.Now().Add(-touchedFor)
	for id, at := range c.touched {
		if at.Before(cutoff) {
			delete(c.touched, id)
		} else if !slices.Contains(out, id) {
			rs = append(rs, recent{id, at})
		}
	}
	slices.SortFunc(rs, func(a, b recent) int { return b.at.Compare(a.at) })
	for _, r := range rs {
		out = append(out, r.id)
	}
	if len(out) > maxWatched {
		out = out[:maxWatched]
	}
	return out
}

func (c *Client) pokeLease() {
	select {
	case c.lease <- struct{}{}:
	default:
	}
}

// Watch names the threads the UI shows now (api.Watcher).
func (c *Client) Watch(threadIDs []string) {
	c.mu.Lock()
	same := slices.Equal(c.watch, threadIDs)
	c.watch = slices.Clone(threadIDs)
	c.mu.Unlock()
	if !same {
		c.pokeLease()
	}
}

// Open names the thread the UI has open (api.Opener). The machine hears it with the watch, and forgets
// it when the session ends or the watch lapses.
func (c *Client) Open(threadID string) {
	c.mu.Lock()
	same := c.open == threadID
	c.open = threadID
	c.mu.Unlock()
	if !same {
		c.pokeLease()
	}
}

// apply takes state from the machine into the mirror, then announces what changed.
func (c *Client) apply(sy Sync) error {
	type key struct {
		kind core.ChangeKind
		id   string
	}
	covered := map[key]bool{}
	for _, ch := range sy.Changes {
		covered[key{ch.Kind, ch.ThreadID}] = true
	}
	var extra []core.Change
	add := func(ch core.Change) {
		if !covered[key{ch.Kind, ch.ThreadID}] {
			covered[key{ch.Kind, ch.ThreadID}] = true
			extra = append(extra, ch)
		}
	}

	c.mu.Lock()
	for _, v := range sy.Threads {
		v.Live = nil
		c.threads[v.ID] = v
		c.sorted = nil
		add(core.Change{Kind: core.ChangeThread, ThreadID: v.ID, PrevStatus: v.Status, Status: v.Status})
	}
	for _, u := range sy.Live {
		if u.Count < 0 || u.Count > 4096 {
			c.mu.Unlock()
			return errProtocol
		}
		var blocks []core.LiveBlock
		if !u.Reset {
			blocks = c.live[u.ThreadID]
		}
		next := make([]core.LiveBlock, u.Count)
		copy(next, blocks)
		for _, d := range u.Blocks {
			if d.Index < 0 || d.Index >= u.Count {
				c.mu.Unlock()
				return errProtocol
			}
			if d.Reset {
				next[d.Index] = core.LiveBlock{Thinking: d.Thinking, Text: d.Text}
			} else {
				next[d.Index].Thinking = d.Thinking
				next[d.Index].Text += d.Text
			}
		}
		if len(next) == 0 {
			delete(c.live, u.ThreadID)
		} else {
			c.live[u.ThreadID] = next
		}
		c.sorted = nil
		add(core.Change{Kind: core.ChangeStream, ThreadID: u.ThreadID})
	}
	for _, u := range sy.Items {
		tr := c.items[u.ThreadID]
		if tr == nil {
			tr = &transcript{used: time.Now()}
			c.items[u.ThreadID] = tr
		}
		switch {
		case u.Reset:
			tr.build = map[int64]core.Item{}
		case tr.build == nil:
			tr.build = make(map[int64]core.Item, len(tr.items)+len(u.Upsert))
			for _, it := range tr.items {
				tr.build[it.ID] = it
			}
		}
		for _, it := range u.Upsert {
			it.ThreadID = u.ThreadID
			tr.build[it.ID] = it
		}
		for _, id := range u.Remove {
			delete(tr.build, id)
		}
		if !u.Done {
			continue
		}
		items := make([]core.Item, 0, len(tr.build))
		for _, it := range tr.build {
			items = append(items, it)
		}
		slices.SortFunc(items, func(a, b core.Item) int {
			switch {
			case a.ID < b.ID:
				return -1
			case a.ID > b.ID:
				return 1
			}
			return 0
		})
		tr.items, tr.build, tr.rev = items, nil, u.Rev
		add(core.Change{Kind: core.ChangeItems, ThreadID: u.ThreadID})
	}
	c.forgetItemsLocked()
	other := false
	if sy.Settings != nil {
		c.settings = *sy.Settings
		add(core.Change{Kind: core.ChangeSettings})
	}
	if sy.Todos != nil {
		c.todos = *sy.Todos
		add(core.Change{Kind: core.ChangeTodos})
	}
	if sy.Repos != nil {
		c.repos = *sy.Repos
		add(core.Change{Kind: core.ChangeRepos})
	}
	if sy.Plain != nil {
		c.plain = *sy.Plain
		add(core.Change{Kind: core.ChangeRepos})
	}
	if sy.Rate != nil {
		c.rate, other = *sy.Rate, true
	}
	if sy.Pending != nil {
		c.pending, other = *sy.Pending, true
		if c.pending == nil {
			c.pending = map[string][]core.PendingQuestion{}
		}
	}
	if sy.Rev > c.rev {
		c.rev = sy.Rev
	}
	subs := make([]func(core.Change), 0, len(c.subs))
	for _, f := range c.subs {
		subs = append(subs, f)
	}
	c.mu.Unlock()

	if other && len(extra) == 0 && len(sy.Changes) == 0 {
		extra = append(extra, core.Change{Kind: core.ChangeStream}) // "look again", with nothing to notify of
	}
	if sy.Synced {
		c.setStatus(func(st *Status) { st.Synced = true })
	}
	// State first, as the machine's own listeners see it: what they are told has already happened.
	for _, ch := range append(extra, sy.Changes...) {
		for _, f := range subs {
			f(ch)
		}
	}
	return nil
}

// forgetItemsLocked drops the transcripts shown longest ago. Call with c.mu held.
func (c *Client) forgetItemsLocked() {
	if len(c.items) <= itemsKept {
		return
	}
	keep := c.watchedLocked()
	for len(c.items) > itemsKept {
		oldest := ""
		for id, tr := range c.items {
			if slices.Contains(keep, id) {
				continue
			}
			if oldest == "" || tr.used.Before(c.items[oldest].used) {
				oldest = id
			}
		}
		if oldest == "" {
			return
		}
		delete(c.items, oldest)
	}
}

// call sends a request and waits for its answer. A request whose connection drops is sent again on the
// next, under the same ID, so the machine runs it once.
func (c *Client) call(ctx context.Context, m string, params any, blob []byte) (Envelope, error) {
	var raw [16]byte
	if _, err := io.ReadFull(c.opts.Random, raw[:]); err != nil {
		return Envelope{}, err
	}
	p, err := json.Marshal(params)
	if err != nil {
		return Envelope{}, err
	}
	e := Envelope{T: kindReq, ID: hex.EncodeToString(raw[:]), M: m, P: p, Blob: blob}
	for try := 1; ; try++ {
		c.mu.Lock()
		s, up := c.s, c.up
		c.mu.Unlock()
		if s == nil {
			wait := time.NewTimer(connectWait)
			select {
			case <-up:
				wait.Stop()
				continue
			case <-wait.C:
				return Envelope{}, ErrOffline
			case <-ctx.Done():
				wait.Stop()
				return Envelope{}, ErrOffline
			}
		}
		res, err := c.send(ctx, s, e)
		if err == nil {
			if res.E != nil {
				return res, fromError(res.E)
			}
			return res, nil
		}
		if ctx.Err() != nil || try == maxTries {
			return Envelope{}, ErrOffline
		}
	}
}

// callOn is call on one session, with no second try.
func (c *Client) callOn(ctx context.Context, s *session, m string, params any, blob []byte) error {
	var raw [16]byte
	if _, err := io.ReadFull(c.opts.Random, raw[:]); err != nil {
		return err
	}
	p, _ := json.Marshal(params)
	_, err := c.send(ctx, s, Envelope{T: kindReq, ID: hex.EncodeToString(raw[:]), M: m, P: p, Blob: blob})
	return err
}

func (c *Client) send(ctx context.Context, s *session, e Envelope) (Envelope, error) {
	ch := make(chan Envelope, 1)
	c.mu.Lock()
	c.calls[e.ID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.calls, e.ID)
		c.mu.Unlock()
	}()
	if err := s.send(e); err != nil {
		return Envelope{}, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-s.done:
		return Envelope{}, ErrClosed
	case <-ctx.Done():
		return Envelope{}, ctx.Err()
	}
}

// do is call for a request with a JSON result, decoded into out when out isn't nil.
func (c *Client) do(ctx context.Context, m string, params, out any) error {
	res, err := c.call(ctx, m, params, nil)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(res.P, out); err != nil {
			return errors.New("The machine's answer couldn't be read.")
		}
	}
	return nil
}

// soon is do for a request the UI makes without a context.
func (c *Client) soon(m string, params, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), callFor)
	defer cancel()
	return c.do(ctx, m, params, out)
}

// --- api.Backend: what the UI reads, from the mirror ---

func (c *Client) Threads() []core.ThreadView {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sorted == nil {
		out := make([]core.ThreadView, 0, len(c.threads))
		for id, v := range c.threads {
			v.Live = c.live[id]
			out = append(out, v)
		}
		slices.SortFunc(out, func(a, b core.ThreadView) int {
			if d := a.CreatedAt.Compare(b.CreatedAt); d != 0 {
				return d
			}
			return cmpString(a.ID, b.ID)
		})
		c.sorted = out
	}
	return slices.Clone(c.sorted)
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (c *Client) Thread(id string) (core.ThreadView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.threads[id]
	if ok {
		v.Live = c.live[id]
	}
	return v, ok
}

// Items returns what the client holds of a thread's transcript, and has the machine send it if it
// doesn't yet: a change is announced when it arrives.
func (c *Client) Items(_ context.Context, threadID string, limit int) ([]core.Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	_, was := c.touched[threadID]
	c.touched[threadID] = now
	grew := limit > c.limit && limit <= maxItems
	if grew {
		c.limit = limit
	}
	if grew || (!was && !slices.Contains(c.watch, threadID)) {
		c.pokeLease() // the machine is told what the UI now wants
	}
	tr := c.items[threadID]
	if tr == nil {
		return nil, nil
	}
	tr.used = now
	items := tr.items
	if limit > 0 && len(items) > limit {
		items = items[len(items)-limit:]
	}
	return items, nil
}

func (c *Client) PendingQuestions(threadID string) []core.PendingQuestion {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending[threadID]
}

func (c *Client) Subscribe(f func(core.Change)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextSub
	c.nextSub++
	c.subs[id] = f
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

func (c *Client) Todos(done bool) []core.Todo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]core.Todo, 0, len(c.todos))
	for _, t := range c.todos {
		if done || !t.Done() {
			out = append(out, t)
		}
	}
	return out
}

func (c *Client) Settings() core.Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.settings
	s.Folders = slices.Clone(s.Folders)
	return s
}

func (c *Client) RepoPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.repos)
}

func (c *Client) PlainFolders() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.plain)
}

func (c *Client) RateLimits() []agent.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.rate)
}

// --- api.Backend: requests ---

func (c *Client) CompleteTodo(id string) error {
	return c.soon("completeTodo", todoParams{ID: id}, nil)
}

func (c *Client) UpdateTodo(id string, ch core.TodoChange) (core.Todo, error) {
	var t core.Todo
	err := c.soon("updateTodo", todoParams{ID: id, Change: ch}, &t)
	return t, err
}

func (c *Client) SendToOrchestrator(ctx context.Context, text string, images ...string) error {
	return c.do(ctx, "send", sendParams{Text: text, Images: images}, nil)
}

func (c *Client) Send(ctx context.Context, threadID, text string, images ...string) error {
	return c.do(ctx, "send", sendParams{Thread: threadID, Text: text, Images: images}, nil)
}

// SaveImage sends a decoded image as a PNG. The machine scales it down if it is larger than Claude
// takes.
func (c *Client) SaveImage(img image.Image) (string, error) {
	if img == nil {
		return "", errors.New("no image")
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return c.SaveImageData(ctx, buf.Bytes())
}

func (c *Client) SaveImageData(ctx context.Context, data []byte) (string, error) {
	if len(data) > api.MaxImageUpload {
		return "", errors.New("The image is too large to attach.")
	}
	res, err := c.call(ctx, "saveImage", struct{}{}, data)
	if err != nil {
		return "", err
	}
	var out imageResult
	if json.Unmarshal(res.P, &out) != nil || out.Ref == "" {
		return "", errors.New("The machine's answer couldn't be read.")
	}
	return out.Ref, nil
}

func (c *Client) ImportImage(string) (string, error) {
	return "", errors.New("A file on this device is attached by its contents.")
}

func (c *Client) ImportFile(string) (string, error) {
	return "", errors.New("A file on this device is attached by its contents.")
}

// errUploadDropped is why an upload stopped when the connection to the machine was lost.
var errUploadDropped = errors.New("The connection to the machine dropped, so the file didn't arrive. Attach it again.")

// SaveFile sends a file to the machine in parts (see upload.go), with the SHA-256 of the whole, which
// the machine checks. A cancelled ctx stops it and has the machine remove what arrived.
func (c *Client) SaveFile(ctx context.Context, name string, data []byte, progress func(int64)) (string, error) {
	if len(data) > api.MaxFileUpload {
		return "", fmt.Errorf("%s is larger than %d MB.", name, api.MaxFileUpload>>20)
	}
	var begun uploadResult
	if err := c.do(ctx, "uploadBegin", uploadBeginParams{Name: name, Size: int64(len(data))}, &begun); err != nil {
		return "", uploadError(ctx, err)
	}
	id := begun.Upload
	sum := sha256.New()
	for off := 0; off < len(data); {
		part := data[off:min(off+maxUploadPart, len(data))]
		if _, err := c.call(ctx, "uploadPart", uploadParams{Upload: id, Offset: int64(off)}, part); err != nil {
			c.cancelUpload(id)
			return "", uploadError(ctx, err)
		}
		sum.Write(part) // a part at a time, between requests: a page's goroutines don't take turns otherwise
		off += len(part)
		if progress != nil {
			progress(int64(off))
		}
	}
	var out imageResult
	if err := c.do(ctx, "uploadEnd", uploadParams{Upload: id, Sum: hex.EncodeToString(sum.Sum(nil))}, &out); err != nil {
		c.cancelUpload(id)
		return "", uploadError(ctx, err)
	}
	if out.Ref == "" {
		return "", errors.New("The machine's answer couldn't be read.")
	}
	return out.Ref, nil
}

// cancelUpload asks the machine to remove what arrived of an upload, without waiting. A machine that
// can't be reached removes it itself when no part comes (Options.UploadIdle).
func (c *Client) cancelUpload(id string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.do(ctx, "uploadCancel", uploadParams{Upload: id}, nil)
	}()
}

// uploadError says why an upload stopped.
func uploadError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, ErrOffline):
		return errUploadDropped
	}
	return err
}

func (c *Client) ReadImage(ctx context.Context, ref string, maxSide int) ([]byte, error) {
	res, err := c.call(ctx, "readImage", imageParams{Ref: ref, MaxSide: maxSide}, nil)
	if err != nil {
		return nil, err
	}
	return res.Blob, nil
}

func (c *Client) Interrupt(ctx context.Context, threadID string) error {
	return c.do(ctx, "interrupt", threadParams{Thread: threadID}, nil)
}

func (c *Client) Answer(threadID, toolUseID string, answers map[string]string) error {
	return c.soon("answer", answerParams{Thread: threadID, ToolUse: toolUseID, Answers: answers}, nil)
}

func (c *Client) Dismiss(threadID, toolUseID string) error {
	return c.soon("dismiss", answerParams{Thread: threadID, ToolUse: toolUseID}, nil)
}

func (c *Client) Changes(ctx context.Context, threadID string) ([]git.FileChange, error) {
	var out []git.FileChange
	err := c.do(ctx, "changes", threadParams{Thread: threadID}, &out)
	return out, err
}

func (c *Client) Diff(ctx context.Context, threadID string) (string, error) {
	var out diffResult
	if err := c.do(ctx, "diff", threadParams{Thread: threadID}, &out); err != nil {
		return "", err
	}
	if out.Truncated {
		out.Diff += "\n… The rest of this diff is too long to show here.\n"
	}
	return out.Diff, nil
}

func (c *Client) text(ctx context.Context, m string, params any) (string, error) {
	var out textResult
	err := c.do(ctx, m, params, &out)
	return out.Text, err
}

func (c *Client) Merge(ctx context.Context, threadID string) (string, error) {
	return c.text(ctx, "merge", threadParams{Thread: threadID})
}

func (c *Client) MergeLocal(ctx context.Context, threadID string) (string, error) {
	return c.text(ctx, "mergeLocal", threadParams{Thread: threadID})
}

func (c *Client) MergeRepo(ctx context.Context, threadID, repo string) (string, error) {
	if repo == "" {
		return c.Merge(ctx, threadID)
	}
	return c.text(ctx, "mergeRepo", repoParams{Thread: threadID, Repo: repo})
}

func (c *Client) OpenPR(ctx context.Context, threadID string, draft bool) (forge.PR, error) {
	prs, err := c.OpenPRFor(ctx, threadID, "", draft)
	if err != nil {
		return forge.PR{}, err
	}
	if len(prs) == 0 {
		return forge.PR{}, errors.New("No pull request was opened.")
	}
	return prs[0], nil
}

// OpenPRFor waits for the machine's answer however long the UI waits for a click: the machine runs it
// to its end, each step with its own limit, and the PR text can take minutes per repository. Its
// own limit only keeps an answer that never comes from leaving the UI waiting for good.
func (c *Client) OpenPRFor(ctx context.Context, threadID, repo string, draft bool) ([]forge.PR, error) {
	v, _ := c.Thread(threadID)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.Settings().OpenPRTimeout(len(v.Parts))+openPRSpare)
	defer cancel()
	var out []forge.PR
	err := c.do(ctx, "openPR", repoParams{Thread: threadID, Repo: repo, Draft: draft}, &out)
	return out, err
}

func (c *Client) MarkReady(ctx context.Context, threadID string) error {
	return c.do(ctx, "markReady", threadParams{Thread: threadID}, nil)
}

func (c *Client) SetThreadModel(ctx context.Context, threadID, model, effort string) error {
	return c.do(ctx, "setModel", modelParams{Thread: threadID, Model: model, Effort: effort}, nil)
}

// CreateWorker starts a thread from a card the user wrote (spec.FromUser): only its repository, prompt,
// attachments and scratch mode go to the machine.
func (c *Client) CreateWorker(ctx context.Context, spec core.WorkerSpec) (core.ThreadView, error) {
	var out core.ThreadView
	err := c.do(ctx, "createWorker", createParams{Repo: spec.Repo, Prompt: spec.Prompt,
		Attachments: append(slices.Clone(spec.Images), spec.Files...), Scratch: spec.Scratch}, &out)
	return out, err
}

func (c *Client) SetScratch(ctx context.Context, threadID string, on bool) error {
	return c.do(ctx, "setScratch", scratchParams{Thread: threadID, On: on}, nil)
}

func (c *Client) SearchMessages(ctx context.Context, query string, limit int) ([]core.SearchHit, error) {
	var out []core.SearchHit
	err := c.do(ctx, "search", searchParams{Query: query, Limit: limit}, &out)
	return out, err
}

func (c *Client) Usage(ctx context.Context) (core.UsageReport, error) {
	var out core.UsageReport
	err := c.do(ctx, "usage", struct{}{}, &out)
	return out, err
}

func (c *Client) Skills(ctx context.Context, threadID string) ([]core.Skill, error) {
	var out []core.Skill
	err := c.do(ctx, "skills", threadParams{Thread: threadID}, &out)
	return out, err
}

func (c *Client) SearchPaths(ctx context.Context, threadID, query string, limit int) ([]core.PathHit, error) {
	var out []core.PathHit
	err := c.do(ctx, "searchPaths", pathParams{Thread: threadID, Query: query, Limit: limit}, &out)
	return out, err
}

func (c *Client) Discard(ctx context.Context, threadID string) error {
	return c.do(ctx, "discard", threadParams{Thread: threadID}, nil)
}

func (c *Client) Done(ctx context.Context, threadID string) error {
	return c.do(ctx, "done", threadParams{Thread: threadID}, nil)
}

func (c *Client) Restore(ctx context.Context, threadID string) error {
	return c.do(ctx, "restore", threadParams{Thread: threadID}, nil)
}

// UpdateSettings sends the fields f changes, not the settings this device holds, so that a setting
// changed elsewhere meanwhile is left as it is.
func (c *Client) UpdateSettings(f func(*core.Settings)) error {
	before := c.Settings()
	after := before
	after.Folders = slices.Clone(before.Folders)
	if before.RepoBranchRules != nil {
		after.RepoBranchRules = make(map[string]string, len(before.RepoBranchRules))
		for k, v := range before.RepoBranchRules {
			after.RepoBranchRules[k] = v
		}
	}
	f(&after)
	patch := api.DiffSettings(before, after)
	if len(patch) == 0 {
		return nil
	}
	var now core.Settings
	if err := c.soon("updateSettings", settingsParams{Patch: patch}, &now); err != nil {
		return err
	}
	c.mu.Lock()
	c.settings = now
	subs := make([]func(core.Change), 0, len(c.subs))
	for _, f := range c.subs {
		subs = append(subs, f)
	}
	c.mu.Unlock()
	for _, f := range subs {
		f(core.Change{Kind: core.ChangeSettings})
	}
	return nil
}

func (c *Client) InitRepo(ctx context.Context, folder string) (string, error) {
	return c.text(ctx, "initRepo", folderParams{Folder: folder})
}

// CheckClaude checks the Claude the machine is set to use. A device can't have the machine try
// another path, so override is not sent.
func (c *Client) CheckClaude(ctx context.Context, _ string) core.ClaudeStatus {
	var out core.ClaudeStatus
	if err := c.do(ctx, "checkClaude", struct{}{}, &out); err != nil {
		return core.ClaudeStatus{Err: err.Error()}
	}
	return out
}
