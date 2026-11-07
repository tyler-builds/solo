package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// RPCError is an error response from the other side.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// ErrClosed is returned for calls on, or pending when, the connection closes.
var ErrClosed = errors.New("agent: connection closed")

type wireMsg struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// Handler serves a request from the other side. A non-nil error becomes an error response. ctx is
// cancelled when the other side cancels the request ($/cancel) or the connection closes.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// conn is a bidirectional NDJSON JSON-RPC connection, the Go twin of sidecar/src/rpc.ts.
type conn struct {
	w  io.Writer
	wm sync.Mutex // serializes writes so lines never interleave

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan wireMsg
	incoming map[int64]context.CancelFunc
	closed   bool
	err      error
	done     chan struct{}

	handlers      map[string]Handler
	notifications map[string]func(json.RawMessage)
}

func newConn(w io.Writer) *conn {
	return &conn{
		w:             w,
		pending:       map[int64]chan wireMsg{},
		incoming:      map[int64]context.CancelFunc{},
		done:          make(chan struct{}),
		handlers:      map[string]Handler{},
		notifications: map[string]func(json.RawMessage){},
	}
}

// handle registers a request handler. Register before serve starts.
func (c *conn) handle(method string, h Handler) { c.handlers[method] = h }

// onNotify registers a notification handler, called on the read goroutine in arrival order. Register
// before serve starts.
func (c *conn) onNotify(method string, f func(json.RawMessage)) { c.notifications[method] = f }

// serve reads messages until r ends, then closes the connection with the read error (nil at EOF).
func (c *conn) serve(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m wireMsg
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		c.dispatch(m)
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.close(err)
}

func (c *conn) dispatch(m wireMsg) {
	switch {
	case m.Method != "" && m.ID == nil:
		if m.Method == "$/cancel" {
			var p struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(m.Params, &p) == nil {
				c.mu.Lock()
				cancel := c.incoming[p.ID]
				c.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
			return
		}
		if f := c.notifications[m.Method]; f != nil {
			f(m.Params)
		}
	case m.Method != "":
		id := *m.ID
		h := c.handlers[m.Method]
		if h == nil {
			c.write(wireMsg{ID: &id, Error: &RPCError{Code: -32601, Message: "unknown method " + m.Method}})
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			cancel()
			return
		}
		c.incoming[id] = cancel
		c.mu.Unlock()
		// Handlers can block (a question waits for the user), so each runs on its own goroutine.
		go func() {
			defer func() {
				c.mu.Lock()
				delete(c.incoming, id)
				c.mu.Unlock()
				cancel()
			}()
			res, err := h(ctx, m.Params)
			if ctx.Err() != nil {
				return // cancelled by the other side, or closed: nobody waits for an answer
			}
			if err != nil {
				c.write(wireMsg{ID: &id, Error: &RPCError{Code: -32000, Message: err.Error()}})
				return
			}
			raw, merr := json.Marshal(res)
			if merr != nil {
				c.write(wireMsg{ID: &id, Error: &RPCError{Code: -32603, Message: merr.Error()}})
				return
			}
			c.write(wireMsg{ID: &id, Result: raw})
		}()
	case m.ID != nil:
		c.mu.Lock()
		ch := c.pending[*m.ID]
		delete(c.pending, *m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// call sends a request and decodes its result into out (which may be nil).
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan wireMsg, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(wireMsg{ID: &id, Method: method, Params: raw}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out == nil || len(m.Result) == 0 {
			return nil
		}
		return json.Unmarshal(m.Result, out)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("%w: %v", ErrClosed, c.err)
	}
}

func (c *conn) write(m wireMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wm.Lock()
	defer c.wm.Unlock()
	_, err = c.w.Write(b)
	return err
}

func (c *conn) close(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	for _, cancel := range c.incoming {
		cancel()
	}
	c.mu.Unlock()
	close(c.done)
}
