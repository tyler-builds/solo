package claudesdk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned for calls on a client whose claude process has exited.
var ErrClosed = errors.New("claudesdk: claude process has exited")

const (
	stderrTailMax = 2048
	closeWait     = 5 * time.Second
	maxLine       = 64 << 20 // one message; a huge tool result can be several MB
)

// Client is one running claude process: one session.
type Client struct {
	opts  Options
	cmd   *exec.Cmd
	stdin io.WriteCloser

	wmu sync.Mutex // serializes writes to stdin

	mu       sync.Mutex
	pending  map[string]chan controlReply
	inflight map[string]context.CancelFunc
	stderr   string // tail of stderr written before the first message
	err      error  // why the process ended
	reqN     atomic.Int64
	answered atomic.Bool
	initRes  json.RawMessage
	hooks    map[string]func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) // by callback id

	queue     *queue
	msgs      chan Message
	done      chan struct{} // closed once the process exited and stdout is drained
	abandon   chan struct{} // closed by Close
	closeOnce sync.Once
	inputOnce sync.Once
}

type controlReply struct {
	resp json.RawMessage
	err  error
}

// Start starts a claude process and performs the initialize handshake. ctx bounds the start only;
// the process runs until Close, or until it exits.
func Start(ctx context.Context, opts Options) (*Client, error) {
	if _, err := opts.mcpConfig(); err != nil {
		return nil, err
	}
	path := opts.CLIPath
	if path == "" {
		if path = FindCLI(); path == "" {
			return nil, errors.New("claudesdk: Claude Code not found; install it or set Options.CLIPath")
		}
	}
	name, args := path, opts.args()
	if strings.HasSuffix(strings.ToLower(path), ".js") {
		name, args = "node", append([]string{path}, args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = opts.Cwd
	cmd.Env = opts.env()
	setProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claudesdk: starting %s: %w", path, err)
	}
	c := &Client{
		opts:     opts,
		cmd:      cmd,
		stdin:    stdin,
		pending:  map[string]chan controlReply{},
		inflight: map[string]context.CancelFunc{},
		queue:    newQueue(),
		msgs:     make(chan Message),
		done:     make(chan struct{}),
		abandon:  make(chan struct{}),
	}
	stderrDone := make(chan struct{})
	go c.readStderr(stderr, stderrDone)
	go c.readStdout(stdout, stderrDone)
	go c.pump()

	timeout := opts.InitializeTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ictx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req := map[string]any{"subtype": "initialize", "hooks": nil}
	if len(opts.Hooks) > 0 {
		c.hooks = map[string]func(context.Context, json.RawMessage) (json.RawMessage, error){}
		cfg := map[string]any{}
		n := 0
		for event, matchers := range opts.Hooks {
			var list []map[string]any
			for _, m := range matchers {
				n++
				id := fmt.Sprintf("hook_%d", n)
				c.hooks[id] = m.Callback
				var matcher any
				if m.Matcher != "" {
					matcher = m.Matcher
				}
				list = append(list, map[string]any{"matcher": matcher, "hookCallbackIds": []string{id}})
			}
			cfg[event] = list
		}
		req["hooks"] = cfg
	}
	if opts.Skills != nil {
		req["skills"] = opts.Skills
	}
	res, err := c.Control(ictx, req)
	if err != nil {
		c.Close()
		if tail := c.stderrTail(); tail != "" {
			err = fmt.Errorf("%w\n%s", err, tail)
		}
		return nil, fmt.Errorf("claudesdk: initialize: %w", err)
	}
	c.initRes = res
	return c, nil
}

// InitResult is the CLI's answer to initialize: its commands, models, account and so on.
func (c *Client) InitResult() json.RawMessage { return c.initRes }

// Messages delivers the CLI's messages in order. It is closed when the process exits or the client is
// closed. Reading is not required for control calls to work: messages queue up without bound.
func (c *Client) Messages() <-chan Message { return c.msgs }

// Done is closed once the process has exited.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the process exited, once Done is closed: nil for a clean exit.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Send sends a user message: a new turn, or, while a turn runs, one queued after it (or steering it,
// with Priority "now").
func (c *Client) Send(u UserMessage) error { return c.write(u.wire()) }

// Interrupt stops the running turn. Its result message still arrives, with terminal_reason
// aborted_streaming or aborted_tools.
func (c *Client) Interrupt(ctx context.Context) (json.RawMessage, error) {
	return c.Control(ctx, map[string]any{"subtype": "interrupt"})
}

// SetModel changes the model for the next turns; "" goes back to the default.
func (c *Client) SetModel(ctx context.Context, model string) error {
	var m any
	if model != "" {
		m = model
	}
	_, err := c.Control(ctx, map[string]any{"subtype": "set_model", "model": m})
	return err
}

// SetPermissionMode changes the permission mode.
func (c *Client) SetPermissionMode(ctx context.Context, mode string) error {
	_, err := c.Control(ctx, map[string]any{"subtype": "set_permission_mode", "mode": mode})
	return err
}

// StopTask stops a background task (a command or subagent) by its task id.
func (c *Client) StopTask(ctx context.Context, taskID string) error {
	_, err := c.Control(ctx, map[string]any{"subtype": "stop_task", "task_id": taskID})
	return err
}

// BackgroundTasks moves the running subagent that the tool call started (an Agent tool call) to the
// background, so a steer stops the turn without losing its work; backgrounded says it moved.
func (c *Client) BackgroundTasks(ctx context.Context, toolUseID string) (backgrounded bool, err error) {
	res, err := c.Control(ctx, map[string]any{"subtype": "background_tasks", "tool_use_id": toolUseID})
	if err != nil {
		return false, err
	}
	var r struct {
		Backgrounded *bool `json:"backgrounded"`
	}
	if json.Unmarshal(res, &r) != nil || r.Backgrounded == nil {
		return true, nil // the TypeScript SDK reads a missing answer as moved
	}
	return *r.Backgrounded, nil
}

// Control sends a control request (req must have a "subtype") and waits for its response.
func (c *Client) Control(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	var b [4]byte
	_, _ = rand.Read(b[:])
	id := fmt.Sprintf("req_%d_%s", c.reqN.Add(1), hex.EncodeToString(b[:]))
	ch := make(chan controlReply, 1)
	c.mu.Lock()
	if c.isDone() {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.write(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("control request %v: %w", req["subtype"], ctx.Err())
	case <-c.done:
		return nil, ErrClosed
	}
}

// CloseInput closes the CLI's stdin: it finishes the work it has and exits. Wait for Done after.
func (c *Client) CloseInput() {
	c.inputOnce.Do(func() { _ = c.stdin.Close() })
}

// Close ends the process: it closes stdin, waits a few seconds, then kills the process group.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.abandon) })
	c.CloseInput()
	select {
	case <-c.done:
	case <-time.After(closeWait):
		killProcessGroup(c.cmd)
		<-c.done
	}
	return nil
}

func (c *Client) isDone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *Client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.isDone() {
		return ErrClosed
	}
	if _, err := c.stdin.Write(b); err != nil {
		return fmt.Errorf("claudesdk: writing to claude: %w", err)
	}
	return nil
}

func (c *Client) readStdout(r io.Reader, stderrDone <-chan struct{}) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			break
		}
	}
	<-stderrDone
	werr := c.cmd.Wait()
	// MCP servers and shells the CLI started share its group; don't let them outlive it.
	killProcessGroup(c.cmd)
	c.mu.Lock()
	if werr != nil {
		if tail := c.stderr; tail != "" && !c.answered.Load() {
			werr = fmt.Errorf("%w\n%s", werr, strings.TrimSpace(tail))
		}
		c.err = werr
	}
	for id, cancel := range c.inflight {
		cancel()
		delete(c.inflight, id)
	}
	close(c.done)
	c.mu.Unlock()
	c.queue.close()
}

// readLine reads one line without its newline, refusing lines over maxLine.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, fmt.Errorf("claudesdk: message over %d bytes", maxLine)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

func (c *Client) dispatch(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return // not a message (a stray print); stderr is where the CLI explains itself
	}
	var head struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
		Response  struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
			Error     string          `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return
	}
	c.answered.Store(true)
	switch head.Type {
	case "control_response":
		r := head.Response
		reply := controlReply{resp: r.Response}
		if r.Subtype == "error" {
			msg := r.Error
			if msg == "" {
				msg = "unknown error"
			}
			reply = controlReply{err: errors.New(msg)}
		}
		c.mu.Lock()
		ch := c.pending[r.RequestID]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- reply:
			default: // a duplicate response
			}
		}
	case "control_request":
		ctx, cancel := context.WithCancel(context.Background())
		c.mu.Lock()
		c.inflight[head.RequestID] = cancel
		c.mu.Unlock()
		go c.answer(ctx, head.RequestID, head.Request)
	case "control_cancel_request":
		c.mu.Lock()
		cancel := c.inflight[head.RequestID]
		delete(c.inflight, head.RequestID)
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	default:
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return
		}
		m.Raw = append(json.RawMessage(nil), line...)
		c.queue.push(m)
	}
}

// answer handles a control request from the CLI.
func (c *Client) answer(ctx context.Context, id string, raw json.RawMessage) {
	defer func() {
		c.mu.Lock()
		if cancel := c.inflight[id]; cancel != nil {
			cancel()
			delete(c.inflight, id)
		}
		c.mu.Unlock()
	}()
	var req struct {
		Subtype string `json:"subtype"`
	}
	_ = json.Unmarshal(raw, &req)
	var resp any
	var err error
	switch req.Subtype {
	case "can_use_tool":
		resp, err = c.permission(ctx, raw)
	case "mcp_message":
		resp, err = c.mcpMessage(ctx, raw)
	case "hook_callback":
		resp, err = c.hookCallback(ctx, raw)
	default:
		err = fmt.Errorf("claudesdk does not support control request %q", req.Subtype)
	}
	if ctx.Err() != nil {
		return // withdrawn by control_cancel_request: the CLI expects no answer
	}
	body := map[string]any{"subtype": "success", "request_id": id, "response": resp}
	if err != nil {
		body = map[string]any{"subtype": "error", "request_id": id, "error": err.Error()}
	}
	_ = c.write(map[string]any{"type": "control_response", "response": body})
}

func (c *Client) hookCallback(ctx context.Context, raw json.RawMessage) (any, error) {
	var req struct {
		CallbackID string          `json:"callback_id"`
		Input      json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cb := c.hooks[req.CallbackID]
	if cb == nil {
		return nil, fmt.Errorf("no hook callback %q", req.CallbackID)
	}
	out, err := cb(ctx, req.Input)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return map[string]any{}, nil
	}
	return out, nil
}

func (c *Client) permission(ctx context.Context, raw json.RawMessage) (any, error) {
	if c.opts.CanUseTool == nil {
		return nil, errors.New("no CanUseTool callback")
	}
	var req PermissionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	res := c.opts.CanUseTool(ctx, req)
	if res.Allow {
		in := res.UpdatedInput
		if in == nil {
			in = req.Input
		}
		return map[string]any{"behavior": "allow", "updatedInput": in}, nil
	}
	out := map[string]any{"behavior": "deny", "message": res.Message}
	if res.Interrupt {
		out["interrupt"] = true
	}
	return out, nil
}

func (c *Client) readStderr(r io.Reader, done chan<- struct{}) {
	defer close(done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !c.answered.Load() {
			c.mu.Lock()
			c.stderr += line + "\n"
			if len(c.stderr) > stderrTailMax {
				c.stderr = c.stderr[len(c.stderr)-stderrTailMax:]
			}
			c.mu.Unlock()
		}
		if c.opts.Stderr != nil {
			c.opts.Stderr(line)
		}
	}
	// A line too long for the scanner stops it; keep draining so the CLI never blocks on stderr.
	_, _ = io.Copy(io.Discard, r)
}

func (c *Client) stderrTail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.stderr)
}

// pump moves queued messages to the Messages channel.
func (c *Client) pump() {
	defer close(c.msgs)
	for {
		m, ok := c.queue.pop()
		if !ok {
			return
		}
		select {
		case c.msgs <- m:
		case <-c.abandon:
			return
		}
	}
}

// queue is an unbounded FIFO, so reading stdout never waits on the consumer (control responses must
// get through while nobody reads Messages).
type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []Message
	closed bool
}

func newQueue() *queue {
	q := &queue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue) push(m Message) {
	q.mu.Lock()
	q.items = append(q.items, m)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// pop waits for the next message; ok is false once the queue is closed and empty.
func (q *queue) pop() (Message, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return Message{}, false
	}
	m := q.items[0]
	q.items[0] = Message{}
	q.items = q.items[1:]
	return m, true
}
