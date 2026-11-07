package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Manager keeps one sidecar running, restarting it if it dies, and keeps track of the sessions opened
// on it so they can be reopened (with resume) on a new sidecar the next time they are used.
type Manager struct {
	command Command
	cb      Callbacks
	log     *slog.Logger

	mu       sync.Mutex
	sc       *Sidecar
	gen      int // increments with each sidecar started
	starting chan struct{}
	sessions map[string]*session
	closed   bool
}

type session struct {
	params        OpenParams
	gen           int  // the sidecar generation it is open on; 0 when not open
	hasTranscript bool // a saved transcript exists, so reopening must resume
	inTurn        bool
}

// NewManager returns a manager that starts the sidecar on first use.
func NewManager(command Command, cb Callbacks, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{command: command, cb: cb, log: log, sessions: map[string]*session{}}
}

// NewUUID returns a random version 4 UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sidecar returns the running sidecar, starting one if needed.
func (m *Manager) sidecar(ctx context.Context) (*Sidecar, int, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, 0, errors.New("agent: manager is shut down")
		}
		if m.sc != nil {
			sc, gen := m.sc, m.gen
			m.mu.Unlock()
			return sc, gen, nil
		}
		if wait := m.starting; wait != nil {
			m.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
		}
		m.starting = make(chan struct{})
		m.mu.Unlock()

		sc, err := StartSidecar(ctx, m.command, Callbacks{
			OnEvent:    m.onEvent,
			OnToolCall: m.cb.OnToolCall,
			OnAsk:      m.cb.OnAsk,
		}, m.log)

		m.mu.Lock()
		close(m.starting)
		m.starting = nil
		if err != nil {
			m.mu.Unlock()
			return nil, 0, err
		}
		m.gen++
		m.sc = sc
		gen := m.gen
		m.mu.Unlock()
		go m.watch(sc, gen)
		return sc, gen, nil
	}
}

// watch handles a sidecar's death: every session open on it ends (its turn fails, then it exits), and
// is reopened on the next sidecar when next used. A new sidecar starts lazily, on the next call.
func (m *Manager) watch(sc *Sidecar, gen int) {
	<-sc.Exited()
	m.mu.Lock()
	if m.sc == sc {
		m.sc = nil
	}
	closed := m.closed
	var ended []Event
	for id, s := range m.sessions {
		if s.gen != gen {
			continue
		}
		s.gen = 0
		if s.inTurn {
			s.inTurn = false
			ended = append(ended, Event{SessionID: id, Kind: EventTurnEnd, Status: TurnFailed,
				Reason: "process_exited", Error: "The agent process stopped unexpectedly."})
		}
		ended = append(ended, Event{SessionID: id, Kind: EventExited, Error: "The agent process stopped unexpectedly."})
	}
	m.mu.Unlock()
	if closed {
		return
	}
	m.log.Error("agent: sidecar exited", "err", sc.Err())
	for _, e := range ended {
		if m.cb.OnEvent != nil {
			m.cb.OnEvent(e)
		}
	}
}

func (m *Manager) onEvent(e Event) {
	m.mu.Lock()
	if s := m.sessions[e.SessionID]; s != nil {
		switch e.Kind {
		case EventTurnStart:
			s.inTurn = true
			s.hasTranscript = true
		case EventTurnEnd:
			s.inTurn = false
		case EventExited:
			s.gen = 0
			s.inTurn = false
		case EventInit:
			s.hasTranscript = true
		}
	}
	m.mu.Unlock()
	if m.cb.OnEvent != nil {
		m.cb.OnEvent(e)
	}
}

// Open starts a session. Use Resume to continue a session saved earlier.
func (m *Manager) Open(ctx context.Context, p OpenParams) error {
	m.mu.Lock()
	m.sessions[p.SessionID] = &session{params: p, hasTranscript: p.Resume}
	m.mu.Unlock()
	return m.ensureOpen(ctx, p.SessionID)
}

func (m *Manager) ensureOpen(ctx context.Context, id string) error {
	sc, gen, err := m.sidecar(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return fmt.Errorf("agent: unknown session %s", id)
	}
	if s.gen == gen {
		m.mu.Unlock()
		return nil
	}
	p := s.params
	p.Resume = s.hasTranscript
	m.mu.Unlock()

	if err := sc.call(ctx, "session.open", p, nil); err != nil {
		return err
	}
	m.mu.Lock()
	if s := m.sessions[id]; s != nil {
		s.gen = gen
	}
	m.mu.Unlock()
	return nil
}

// Send sends a user message and returns its uuid; the turn that answers it carries the uuid in its
// turn_start and turn_end events. Messages sent during a turn are queued by Claude, or folded into the
// running turn.
func (m *Manager) Send(ctx context.Context, sessionID, text string, synthetic bool) (string, error) {
	uuid := NewUUID()
	return uuid, m.SendUUID(ctx, sessionID, uuid, text, nil, synthetic)
}

// SendUUID is Send with a uuid the caller chose, so it can record the message before Claude answers.
func (m *Manager) SendUUID(ctx context.Context, sessionID, uuid, text string, images []Image, synthetic bool) error {
	// A session whose process exited (or whose sidecar died) reopens here, with resume. Retry once if the
	// sidecar dies between the open and the send.
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = m.ensureOpen(ctx, sessionID); err != nil {
			continue
		}
		var sc *Sidecar
		if sc, _, err = m.sidecar(ctx); err != nil {
			continue
		}
		err = sc.call(ctx, "session.send", SendParams{SessionID: sessionID, UUID: uuid, Text: text, Images: images, Synthetic: synthetic}, nil)
		if err == nil {
			return nil
		}
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == -32001 { // the sidecar lost the session: reopen it
			m.mu.Lock()
			if s := m.sessions[sessionID]; s != nil {
				s.gen = 0
			}
			m.mu.Unlock()
			continue
		}
		if !errors.Is(err, ErrClosed) {
			return err
		}
	}
	return err
}

// Interrupt stops the session's running turn. The turn ends with status "interrupted".
func (m *Manager) Interrupt(ctx context.Context, sessionID string) error {
	sc, gen, err := m.sidecar(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	s := m.sessions[sessionID]
	open := s != nil && s.gen == gen
	m.mu.Unlock()
	if !open {
		return nil
	}
	return sc.call(ctx, "session.interrupt", map[string]string{"sessionId": sessionID}, nil)
}

// SetModel switches the session's model for its next turns.
func (m *Manager) SetModel(ctx context.Context, sessionID, model string) error {
	m.mu.Lock()
	s := m.sessions[sessionID]
	if s != nil {
		s.params.Model = model
	}
	open := s != nil && s.gen == m.gen && m.sc != nil
	sc := m.sc
	m.mu.Unlock()
	if !open {
		return nil
	}
	return sc.call(ctx, "session.setModel", map[string]string{"sessionId": sessionID, "model": model}, nil)
}

// Close ends the session's process and forgets it. Its transcript stays on disk, so it can be opened
// again later with Resume.
func (m *Manager) Close(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	s := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	sc := m.sc
	open := s != nil && s.gen == m.gen && sc != nil
	m.mu.Unlock()
	if !open {
		return nil
	}
	return sc.call(ctx, "session.close", map[string]string{"sessionId": sessionID}, nil)
}

// Probe asks Claude for its models and account.
func (m *Manager) Probe(ctx context.Context, claudePath string) (ProbeResult, error) {
	var res ProbeResult
	sc, _, err := m.sidecar(ctx)
	if err != nil {
		return res, err
	}
	err = sc.call(ctx, "probe", map[string]string{"claudePath": claudePath}, &res)
	return res, err
}

// CompleteParams asks a model for one short answer, with no tools or session.
type CompleteParams struct {
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	System     string `json:"system"`
	Prompt     string `json:"prompt"`
	ClaudePath string `json:"claudePath,omitempty"`
	TimeoutMs  int    `json:"timeoutMs,omitempty"`
}

// Complete runs a one-shot prompt, for small jobs such as naming a branch.
func (m *Manager) Complete(ctx context.Context, p CompleteParams) (string, error) {
	sc, _, err := m.sidecar(ctx)
	if err != nil {
		return "", err
	}
	var res struct {
		Text string `json:"text"`
	}
	err = sc.call(ctx, "complete", p, &res)
	return res.Text, err
}

// Shutdown closes every session and stops the sidecar.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closed = true
	sc := m.sc
	m.mu.Unlock()
	if sc != nil {
		sc.Shutdown(15 * time.Second)
	}
}
