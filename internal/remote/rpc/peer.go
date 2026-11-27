package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Peer is a short session with a machine, for a linked Solo: it says hello, makes a few requests (the
// machine's usage, and the requests that link the two both ways), and closes. A machine sends a linked
// Solo none of its state, so there is nothing to mirror.
type Peer struct {
	s     *session
	mu    sync.Mutex
	calls map[string]chan Envelope
	err   error
	ended chan struct{}
}

// OpenPeer says hello on a link and waits for the machine's welcome.
func OpenPeer(ctx context.Context, link Link) (*Peer, error) {
	p := &Peer{s: newSession(link, maxFromMachine), calls: map[string]chan Envelope{}, ended: make(chan struct{})}
	hello, _ := json.Marshal(Hello{Protocol: Protocol})
	if err := p.s.send(Envelope{T: kindHello, P: hello}); err != nil {
		link.Close()
		return nil, err
	}
	welcomed := make(chan struct{})
	go func() {
		greeted := false
		err := p.s.run(func(e Envelope) error {
			switch {
			case e.T == kindBye:
				var b Bye
				json.Unmarshal(e.P, &b)
				return &EndError{Code: b.Code, Message: clip(b.Message, 500)}
			case !greeted:
				var w Welcome
				if e.T != kindWelcome || json.Unmarshal(e.P, &w) != nil {
					return errProtocol
				}
				if w.Protocol != Protocol {
					return &EndError{Code: CodeVersion, Message: "Solo on the other machine is a different version. Update both."}
				}
				greeted = true
				close(welcomed)
			case e.T == kindRes:
				p.mu.Lock()
				ch := p.calls[e.ID]
				delete(p.calls, e.ID)
				p.mu.Unlock()
				if ch != nil {
					ch <- e
				}
			case e.T == kindSync:
				// Not sent to a linked Solo; nothing to keep if it were.
			default:
				return errProtocol
			}
			return nil
		})
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.ended)
	}()
	stop := make(chan struct{})
	defer close(stop)
	go func() { // say we are here while requests run
		t := time.NewTicker(PingEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
			case <-p.ended:
				return
			case <-t.C:
				p.s.ping()
				continue
			}
			return
		}
	}()
	select {
	case <-welcomed:
		return p, nil
	case <-p.ended:
		return nil, p.failure()
	case <-ctx.Done():
		p.Close()
		return nil, ctx.Err()
	}
}

func (p *Peer) failure() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		return ErrOffline
	}
	return p.err
}

// Call makes a request and decodes its answer into out (when not nil).
func (p *Peer) Call(ctx context.Context, method string, params, out any) error {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := hex.EncodeToString(raw[:])
	ch := make(chan Envelope, 1)
	p.mu.Lock()
	p.calls[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.calls, id)
		p.mu.Unlock()
	}()
	if err := p.s.send(Envelope{T: kindReq, ID: id, M: method, P: b}); err != nil {
		return err
	}
	select {
	case e := <-ch:
		if e.E != nil {
			return fromError(e.E)
		}
		if out != nil && json.Unmarshal(e.P, out) != nil {
			return errors.New("The other machine's answer couldn't be read.")
		}
		return nil
	case <-p.ended:
		return p.failure()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close ends the session.
func (p *Peer) Close() { p.s.close(ErrClosed) }
