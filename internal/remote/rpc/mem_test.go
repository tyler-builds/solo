package rpc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
)

// pipe is a pair of links joined in memory. Each end counts what it sent.
type pipeEnd struct {
	in, out chan []byte
	closed  chan struct{}
	once    *sync.Once
	sent    *atomic.Int64 // bytes this end sent
	frames  *atomic.Int64
}

func pipe() (*pipeEnd, *pipeEnd) {
	a, b := make(chan []byte, 64), make(chan []byte, 64)
	closed, once := make(chan struct{}), &sync.Once{}
	return &pipeEnd{in: a, out: b, closed: closed, once: once, sent: &atomic.Int64{}, frames: &atomic.Int64{}},
		&pipeEnd{in: b, out: a, closed: closed, once: once, sent: &atomic.Int64{}, frames: &atomic.Int64{}}
}

func (p *pipeEnd) Send(b []byte) error {
	if len(b) > 1<<20 {
		return errors.New("pipe: over a megabyte")
	}
	select {
	case <-p.closed:
		return ErrClosed
	default:
	}
	select {
	case p.out <- slices.Clone(b):
		p.sent.Add(int64(len(b)))
		p.frames.Add(1)
		return nil
	case <-p.closed:
		return ErrClosed
	}
}

func (p *pipeEnd) Receive() ([]byte, error) {
	select {
	case b := <-p.in:
		return b, nil
	case <-p.closed:
		return nil, ErrClosed
	}
}

func (p *pipeEnd) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

// mem is a backend held in memory, which a test changes and announces.
type mem struct {
	api.Backend // the methods a test doesn't use panic, which the server answers as a failure

	mu       sync.Mutex
	threads  map[string]core.ThreadView
	items    map[string][]core.Item
	settings core.Settings
	todos    []core.Todo
	repos    []string
	plain    []string
	subs     map[int]func(core.Change)
	next     int
	sent     []string // "thread: text"
	merges   int
	images   map[string][]byte
	onMerge  func() (string, error)

	claudeChecks atomic.Int32
}

func newMem() *mem {
	m := &mem{threads: map[string]core.ThreadView{}, items: map[string][]core.Item{}, settings: core.DefaultSettings(),
		subs: map[int]func(core.Change){}, images: map[string][]byte{}}
	m.addThread(core.OrchestratorID, core.StatusIdle)
	return m
}

func (m *mem) addThread(id, status string) {
	m.mu.Lock()
	v := core.ThreadView{IsWorker: id != core.OrchestratorID}
	v.ID, v.Title, v.Status = id, "Thread "+id, status
	v.CreatedAt = time.Unix(int64(1_700_000_000+len(m.threads)), 0).UTC()
	m.threads[id] = v
	m.mu.Unlock()
	m.emit(core.Change{Kind: core.ChangeThread, ThreadID: id, Status: status})
}

// update changes a thread and announces it.
func (m *mem) update(id string, f func(*core.ThreadView)) {
	m.mu.Lock()
	v := m.threads[id]
	prev := v.Status
	f(&v)
	m.threads[id] = v
	m.mu.Unlock()
	m.emit(core.Change{Kind: core.ChangeThread, ThreadID: id, PrevStatus: prev, Status: v.Status})
}

// stream sets a thread's live text and announces it as streaming.
func (m *mem) stream(id string, blocks ...core.LiveBlock) {
	m.mu.Lock()
	v := m.threads[id]
	v.Live = blocks
	m.threads[id] = v
	m.mu.Unlock()
	m.emit(core.Change{Kind: core.ChangeStream, ThreadID: id})
}

func (m *mem) setItems(id string, items ...core.Item) {
	m.mu.Lock()
	m.items[id] = items
	m.mu.Unlock()
	m.emit(core.Change{Kind: core.ChangeItems, ThreadID: id})
}

func item(id int64, text string) core.Item {
	return core.Item{ID: id, Kind: core.ItemAssistant, Data: []byte(fmt.Sprintf(`{"text":%q}`, text)),
		CreatedAt: time.Unix(1_700_000_000+id, 0).UTC()}
}

func (m *mem) emit(ch core.Change) {
	m.mu.Lock()
	subs := make([]func(core.Change), 0, len(m.subs))
	for _, f := range m.subs {
		subs = append(subs, f)
	}
	m.mu.Unlock()
	for _, f := range subs {
		f(ch)
	}
}

func (m *mem) Threads() []core.ThreadView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]core.ThreadView, 0, len(m.threads))
	for _, v := range m.threads {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b core.ThreadView) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

func (m *mem) Thread(id string) (core.ThreadView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.threads[id]
	return v, ok
}

func (m *mem) Items(_ context.Context, id string, limit int) ([]core.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := m.items[id]
	if limit > 0 && len(items) > limit {
		items = items[len(items)-limit:]
	}
	out := slices.Clone(items)
	for i := range out {
		out[i].ThreadID = id
	}
	return out, nil
}

func (m *mem) PendingQuestions(id string) []core.PendingQuestion {
	if v, _ := m.Thread(id); v.PendingAsk {
		return []core.PendingQuestion{{ToolUseID: "ask-" + id, Questions: []agent.Question{{Question: "Which?"}}}}
	}
	return nil
}

func (m *mem) Subscribe(f func(core.Change)) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.next
	m.next++
	m.subs[id] = f
	return func() {
		m.mu.Lock()
		delete(m.subs, id)
		m.mu.Unlock()
	}
}

func (m *mem) Todos(bool) []core.Todo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.todos)
}

func (m *mem) Settings() core.Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

func (m *mem) UpdateSettings(f func(*core.Settings)) error {
	m.mu.Lock()
	f(&m.settings)
	m.mu.Unlock()
	m.emit(core.Change{Kind: core.ChangeSettings})
	return nil
}

func (m *mem) RepoPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.repos)
}

func (m *mem) PlainFolders() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.plain)
}

func (m *mem) RateLimits() []agent.Event { return nil }

func (m *mem) Send(_ context.Context, id, text string, images ...string) error {
	m.mu.Lock()
	m.sent = append(m.sent, fmt.Sprintf("%s: %s %v", id, text, images))
	m.mu.Unlock()
	return nil
}

func (m *mem) SendToOrchestrator(ctx context.Context, text string, images ...string) error {
	return m.Send(ctx, core.OrchestratorID, text, images...)
}

func (m *mem) Merge(context.Context, string) (string, error) {
	m.mu.Lock()
	m.merges++
	f := m.onMerge
	m.mu.Unlock()
	if f != nil {
		return f()
	}
	return "abc123", nil
}

func (m *mem) SaveImageData(_ context.Context, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref := fmt.Sprintf("/att/%d.png", len(m.images))
	m.images[ref] = slices.Clone(data)
	return ref, nil
}

func (m *mem) ReadImage(_ context.Context, ref string, _ int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.images[ref]
	if !ok {
		return nil, errors.New("not an attachment")
	}
	return data, nil
}

func (m *mem) CheckClaude(_ context.Context, override string) core.ClaudeStatus {
	m.claudeChecks.Add(1)
	time.Sleep(5 * time.Millisecond)
	return core.ClaudeStatus{Path: "checked:" + override}
}

func (m *mem) InitRepo(_ context.Context, folder string) (string, error) { return folder, nil }

// rig is a host and a client joined by a pipe.
type rig struct {
	t      *testing.T
	m      *mem
	h      *Host
	c      *Client
	dev    Device
	host   *pipeEnd // the host's end of the current connection
	device *pipeEnd
	done   chan error // the client's Run
	audit  []string
	amu    sync.Mutex
}

func newRig(t *testing.T, m *mem, dev Device) *rig {
	t.Helper()
	r := &rig{t: t, m: m, dev: dev}
	r.h = NewHost(m, Options{FlushEvery: 5 * time.Millisecond, Lease: time.Second,
		IsDir: func(p string) bool { return p == "/code" },
		Info:  func() HostInfo { return HostInfo{Name: "box", OS: "linux", Home: "/home/u"} },
		Audit: func(d Device, method, detail string, err error) {
			r.amu.Lock()
			r.audit = append(r.audit, fmt.Sprintf("%s %s %s %v", d.Name, method, detail, err))
			r.amu.Unlock()
		}})
	r.c = NewClient(ClientOptions{Random: countingReader{}})
	t.Cleanup(func() {
		r.h.Close()
		if r.device != nil {
			r.device.Close()
		}
	})
	r.connect()
	return r
}

// countingReader makes distinct request IDs without real randomness.
type countingReader struct{}

var counter atomic.Uint64

func (countingReader) Read(p []byte) (int, error) {
	n := counter.Add(1)
	for i := range p {
		p[i] = byte(n >> (8 * (i % 8)))
	}
	return len(p), nil
}

func (r *rig) connect() {
	r.host, r.device = pipe()
	r.done = make(chan error, 1)
	host, device, done := r.host, r.device, r.done
	go r.h.Serve(host, r.dev)
	go func() { done <- r.c.Run(device) }()
	r.wait("the client to sync", func() bool { return r.c.Status().Synced })
}

func (r *rig) disconnect() {
	r.device.Close()
	<-r.done
	r.wait("the host to drop the session", func() bool { return len(r.h.Sessions()) == 0 })
}

func (r *rig) wait(what string, ok func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
