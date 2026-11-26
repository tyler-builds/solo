//go:build !js

package agent

// Direct runs Solo's Claude sessions on claudesdk, in this process: one claude process per session,
// reporting through Callbacks as Events (normalize.go). A session whose process exited opens again,
// with resume, on its next message. It also probes the install (Probe) and makes one-shot model calls
// (Complete).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/tyler-builds/solo/claudesdk"
)

// NewUUID returns a random version 4 UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ErrUnknownSession is returned for a session Direct doesn't know: never opened, or closed. Open it
// again (with Resume if it has a transcript) to use it.
var ErrUnknownSession = errors.New("agent: unknown session")

// ErrClosed is returned for a session whose process has exited.
var ErrClosed = claudesdk.ErrClosed

// Callbacks are how sessions reach the app.
type Callbacks struct {
	OnEvent    func(Event)
	OnToolCall func(ctx context.Context, p ToolCallParams) (ToolCallResult, error)
	OnAsk      func(ctx context.Context, p AskParams) (AskResult, error)
	// OnGuard checks a tool call an agent is about to make (a PreToolUse hook).
	OnGuard func(ctx context.Context, p GuardParams) (GuardResult, error)
}

// backgroundMoveWait is how long a steer waits for running subagents to move to the background
// (t3code's BACKGROUND_MOVE_TIMEOUT_MS).
const backgroundMoveWait = 5 * time.Second

// entrypoint is what Solo's claude processes report as their entrypoint. Left unset, claudesdk reports
// "sdk-go", and Claude Code keeps off tools it doesn't offer SDK sessions by default, the Artifact tools
// among them.
const entrypoint = "solo"

// hostSessionVariables are what a Claude Code session that started Solo exports; a claude started with
// them would act as part of that session.
var hostSessionVariables = []string{
	"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_HOST_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
	"CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_DESKTOP_APP_VERSION", "CLAUDE_EFFORT", "CLAUDE_PID", "CLAUDE_AGENT_SDK_VERSION",
}

// guardedTools are the tools the secrets guard looks at.
const guardedTools = "Bash|PowerShell|Read|Grep|Glob"

// readTools are the built-in tools a read-only session may use without asking.
var readTools = map[string]bool{"Read": true, "Grep": true, "Glob": true, "LS": true, "WebFetch": true,
	"WebSearch": true, "TodoWrite": true, "ToolSearch": true}

// Direct runs sessions on claudesdk.
type Direct struct {
	cb  Callbacks
	log *slog.Logger
	// LogDir, when set, gets each session's raw messages, one per line, in <session id>.jsonl, for
	// debugging and replay fixtures. Set it before the first Open.
	LogDir string

	mu       sync.Mutex
	sessions map[string]*directSession
	closed   bool
}

type directSession struct {
	d             *Direct
	params        OpenParams
	hasTranscript bool

	mu      sync.Mutex // serializes opening and closing the process
	client  *claudesdk.Client
	norm    normalizer
	seq     int64
	steers  int  // steers sent into the running turn whose abort hasn't come back
	stopped bool // interrupt() was asked, until the turn ends
	closing bool
	// foreground are the running turn's subagents (Agent tool calls) still in the foreground, by tool
	// use id: a steer moves them to the background first, so their work isn't lost.
	foreground map[string]bool
}

// NewDirect returns a Direct that reports to cb.
func NewDirect(cb Callbacks, log *slog.Logger) *Direct {
	if log == nil {
		log = slog.Default()
	}
	return &Direct{cb: cb, log: log, sessions: map[string]*directSession{}}
}

func (d *Direct) session(id string) (*directSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.sessions[id]
	if s == nil {
		return nil, fmt.Errorf("%w %s", ErrUnknownSession, id)
	}
	return s, nil
}

// Open registers a session and starts its process.
func (d *Direct) Open(ctx context.Context, p OpenParams) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return errors.New("agent: shut down")
	}
	if old := d.sessions[p.SessionID]; old != nil {
		d.mu.Unlock()
		old.close()
		d.mu.Lock()
	}
	s := &directSession{d: d, params: p, hasTranscript: p.Resume}
	d.sessions[p.SessionID] = s
	d.mu.Unlock()
	return s.ensure(ctx)
}

// SendUUID sends a user message, reopening the session's process (with resume) if it exited.
func (d *Direct) SendUUID(ctx context.Context, p SendParams) error {
	s, err := d.session(p.SessionID)
	if err != nil {
		return err
	}
	if err := s.ensure(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	c := s.client
	steer := p.Steer && s.norm.turnActive
	var subagents []string
	if steer {
		for id := range s.foreground {
			subagents = append(subagents, id)
		}
	}
	s.mu.Unlock()
	if c == nil {
		return ErrClosed
	}
	if len(subagents) > 0 {
		// A steer stops what the turn is running: subagents move to the background first.
		mctx, cancel := context.WithTimeout(ctx, backgroundMoveWait)
		var wg sync.WaitGroup
		for _, id := range subagents {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := c.BackgroundTasks(mctx, id); err != nil {
					d.log.Warn("steer: backgrounding a subagent", "err", err)
				}
			}()
		}
		wg.Wait()
		cancel()
	}
	s.mu.Lock()
	steer = steer && s.norm.turnActive
	if steer {
		s.steers++
	}
	s.mu.Unlock()
	u := claudesdk.UserMessage{UUID: p.UUID, Text: p.Text, ClientComposed: p.Synthetic}
	for _, im := range p.Images {
		u.Images = append(u.Images, claudesdk.Image{MediaType: im.MediaType, Data: im.Data})
	}
	if steer {
		u.Priority = "now"
	}
	return c.Send(u)
}

// Interrupt stops the session's running turn.
func (d *Direct) Interrupt(ctx context.Context, sessionID string) error {
	s, err := d.session(sessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	c := s.client
	s.stopped = true
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	_, err = c.Interrupt(ctx)
	return err
}

// StopTask stops a background task of the session.
func (d *Direct) StopTask(ctx context.Context, sessionID, taskID string) error {
	s, err := d.session(sessionID)
	if err != nil {
		return err
	}
	if c := s.current(); c != nil {
		return c.StopTask(ctx, taskID)
	}
	return nil
}

// SetModel changes the session's model for its next turns.
func (d *Direct) SetModel(ctx context.Context, sessionID, model string) error {
	s, err := d.session(sessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.params.Model = model
	c := s.client
	s.mu.Unlock()
	if c != nil {
		return c.SetModel(ctx, model)
	}
	return nil
}

// Close ends the session's process and forgets it.
func (d *Direct) Close(ctx context.Context, sessionID string) error {
	d.mu.Lock()
	s := d.sessions[sessionID]
	delete(d.sessions, sessionID)
	d.mu.Unlock()
	if s != nil {
		s.close()
	}
	return nil
}

// Send sends a user message and returns its uuid.
func (d *Direct) Send(ctx context.Context, sessionID, text string, synthetic bool) (string, error) {
	uuid := NewUUID()
	return uuid, d.SendUUID(ctx, SendParams{SessionID: sessionID, UUID: uuid, Text: text, Synthetic: synthetic})
}

// Has reports whether the session is one of Direct's.
func (d *Direct) Has(sessionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions[sessionID] != nil
}

// Shutdown ends every session's process.
func (d *Direct) Shutdown() {
	d.mu.Lock()
	d.closed = true
	all := make([]*directSession, 0, len(d.sessions))
	for _, s := range d.sessions {
		all = append(all, s)
	}
	d.sessions = map[string]*directSession{}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func() { defer wg.Done(); s.close() }()
	}
	wg.Wait()
}

func (s *directSession) current() *claudesdk.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *directSession) close() {
	s.mu.Lock()
	c := s.client
	s.closing = true
	s.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// ensure starts the session's process if it isn't running.
func (s *directSession) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return nil
	}
	s.closing = false
	c, err := claudesdk.Start(ctx, s.options())
	if err != nil {
		return err
	}
	s.client = c
	s.norm = normalizer{contextTokens: s.norm.contextTokens}
	s.foreground = map[string]bool{}
	go s.consume(c)
	return nil
}

// options are the claudesdk options for the session.
func (s *directSession) options() claudesdk.Options {
	p := s.params
	o := claudesdk.Options{
		CLIPath:                p.ClaudePath,
		Cwd:                    p.Cwd,
		Env:                    p.Env,
		UnsetEnv:               hostSessionVariables,
		Entrypoint:             sessionEntrypoint(),
		Model:                  p.Model,
		Effort:                 p.Effort,
		Name:                   p.Title,
		AppendSystemPrompt:     p.SystemAppend,
		Tools:                  p.Tools,
		DisallowedTools:        p.DisallowedTools,
		PermissionMode:         p.PermissionMode,
		AddDirs:                p.AdditionalDirectories,
		SettingSources:         []string{"user", "project", "local"},
		Skills:                 p.Skills,
		NoSessionPersistence:   p.Ephemeral,
		IncludePartialMessages: true,
		Thinking:               "adaptive",
		ThinkingDisplay:        "summarized",
		CanUseTool:             s.canUseTool,
		Hooks: map[string][]claudesdk.HookMatcher{
			"PreToolUse": {{Matcher: guardedTools, Callback: s.guard}},
		},
	}
	if s.hasTranscript {
		o.Resume = p.SessionID
	} else {
		o.SessionID = p.SessionID
	}
	if p.PermissionMode == PermissionBypass {
		o.ExtraArgs = map[string]string{"allow-dangerously-skip-permissions": ""}
	}
	if len(p.MCPTools) > 0 {
		name := p.MCPServerName
		if name == "" {
			name = "solo"
		}
		srv := &claudesdk.MCPServer{Name: name}
		for _, spec := range p.MCPTools {
			spec := spec
			srv.Tools = append(srv.Tools, claudesdk.Tool{
				Name: spec.Name, Description: spec.Description, InputSchema: spec.InputSchema, ReadOnly: spec.ReadOnly,
				AlwaysLoad: true, // the session's main tools, not worth a tool search round trip
				Handler: func(ctx context.Context, args json.RawMessage) (claudesdk.ToolResult, error) {
					if s.d.cb.OnToolCall == nil {
						return claudesdk.ToolResult{Text: "This tool is not available here.", IsError: true}, nil
					}
					res, err := s.d.cb.OnToolCall(ctx, ToolCallParams{SessionID: p.SessionID, Name: spec.Name, Args: args})
					if err != nil {
						return claudesdk.ToolResult{Text: "Tool failed: " + err.Error(), IsError: true}, nil
					}
					return claudesdk.ToolResult{Text: res.Text, IsError: res.IsError}, nil
				},
			})
		}
		o.MCPServers = []*claudesdk.MCPServer{srv}
	}
	return o
}

// consume turns the process's messages into events until it exits.
func (s *directSession) consume(c *claudesdk.Client) {
	for m := range c.Messages() {
		s.record(m.Raw)
		s.mu.Lock()
		s.hasTranscript = true
		events := s.norm.push(m)
		for _, e := range events {
			if e.Kind == EventInit {
				s.checkArtifact(e)
			}
		}
		for i := range events {
			s.track(&events[i])
		}
		s.mu.Unlock()
		for _, e := range events {
			s.emit(e)
		}
	}
	<-c.Done()
	s.mu.Lock()
	failure := ""
	if err := c.Err(); err != nil && !s.closing {
		failure = err.Error()
	}
	if s.client == c {
		s.client = nil
	}
	s.steers, s.stopped = 0, false
	why := failure
	if why == "" {
		why = "Claude exited before the turn finished."
	}
	end, ended := s.norm.abortTurn(why)
	s.foreground = map[string]bool{}
	s.mu.Unlock()
	exit, _ := json.Marshal(map[string]string{"type": "$exit", "error": failure})
	s.record(exit)
	if ended {
		s.emit(end)
	}
	s.emit(Event{Kind: EventExited, Error: failure})
}

// checkArtifact warns when a session that should have the Artifact tool started without it. Solo gets
// the tool only through its entrypoint, which Claude Code doesn't document, so an update could take it
// away. Call with s.mu held.
func (s *directSession) checkArtifact(init Event) {
	if !slices.Contains(s.params.Tools, "Artifact") || slices.Contains(s.params.DisallowedTools, "Artifact") ||
		slices.Contains(init.Tools, "Artifact") {
		return
	}
	s.d.log.Warn("a session started without the Artifact tool: artifacts are off for this account or its settings, or Claude Code no longer offers them to this entrypoint",
		"session", s.params.SessionID, "claude", init.ClaudeVersion, "entrypoint", sessionEntrypoint())
}

// record appends a raw message to the session's log, when Direct keeps one.
func (s *directSession) record(raw []byte) {
	dir := s.d.LogDir
	if dir == "" {
		return
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, s.params.SessionID+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Write(append(append([]byte(nil), raw...), '\n'))
	f.Close()
}

// track follows the turn's foreground subagents, and marks a turn end that only made way for a steer.
// Call with s.mu held.
func (s *directSession) track(e *Event) {
	switch e.Kind {
	case EventAssistant:
		if e.IsRoot() {
			for _, b := range e.Blocks {
				if b.Type == "tool_use" && (b.Name == "Agent" || b.Name == "Task") {
					s.foreground[b.ID] = true
				}
			}
		}
		return
	case EventToolResult:
		if e.IsRoot() {
			delete(s.foreground, e.ToolUseID)
		}
		return
	case EventTask:
		if e.ToolUseID != "" && (e.Background || e.Phase == "ended") {
			delete(s.foreground, e.ToolUseID)
		}
		return
	case EventTurnEnd:
	default:
		return
	}
	clear(s.foreground)
	if s.steers > 0 && !s.stopped && e.Status == TurnInterrupted && (e.Reason == "aborted_streaming" || e.Reason == "aborted_tools") {
		s.steers--
		e.Steered = true
		return
	}
	s.steers, s.stopped = 0, false
}

func (s *directSession) emit(e Event) {
	if s.d.cb.OnEvent == nil {
		return
	}
	s.mu.Lock()
	s.seq++
	e.SessionID, e.Seq = s.params.SessionID, s.seq
	s.mu.Unlock()
	s.d.cb.OnEvent(e)
}

// canUseTool answers the CLI's permission requests: AskUserQuestion goes to the app, and the tool
// policy decides the rest.
func (s *directSession) canUseTool(ctx context.Context, req claudesdk.PermissionRequest) claudesdk.PermissionResult {
	if req.ToolName == "AskUserQuestion" {
		return s.ask(ctx, req)
	}
	if why := toolDenial(s.params, req.ToolName, req.Input); why != "" {
		return claudesdk.PermissionResult{Message: why}
	}
	return claudesdk.PermissionResult{Allow: true}
}

func (s *directSession) ask(ctx context.Context, req claudesdk.PermissionRequest) claudesdk.PermissionResult {
	var in struct {
		Questions []Question `json:"questions"`
	}
	_ = json.Unmarshal(req.Input, &in)
	if s.d.cb.OnAsk == nil {
		return claudesdk.PermissionResult{Message: "The question could not be shown."}
	}
	res, err := s.d.cb.OnAsk(ctx, AskParams{SessionID: s.params.SessionID, ToolUseID: req.ToolUseID, Questions: in.Questions})
	switch {
	case err != nil:
		return claudesdk.PermissionResult{Message: "The question could not be shown: " + err.Error()}
	case res.Cancelled || res.Answers == nil:
		msg := res.Message
		if msg == "" {
			msg = "The user dismissed the question."
		}
		return claudesdk.PermissionResult{Message: msg}
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(req.Input, &raw)
	answers, _ := json.Marshal(res.Answers)
	updated, _ := json.Marshal(map[string]json.RawMessage{"questions": raw["questions"], "answers": answers})
	return claudesdk.PermissionResult{Allow: true, UpdatedInput: updated}
}

// guard is the PreToolUse hook of the secrets guard: the app may deny the call.
func (s *directSession) guard(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Event     string          `json:"hook_event_name"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if json.Unmarshal(input, &in) != nil || in.Event != "PreToolUse" || s.d.cb.OnGuard == nil {
		return nil, nil
	}
	if len(in.ToolInput) == 0 {
		in.ToolInput = json.RawMessage(`{}`)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := s.d.cb.OnGuard(ctx, GuardParams{SessionID: s.params.SessionID, ToolName: in.ToolName, Input: in.ToolInput})
	if err != nil {
		s.d.log.Warn("guard", "err", err)
		return nil, nil // if the app can't be asked, the call goes through
	}
	if res.Deny == "" {
		return nil, nil
	}
	return json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": res.Deny,
	}})
}

// toolDenial says why a session's tool policy refuses a tool, or "" when it allows it.
func toolDenial(p OpenParams, toolName string, input json.RawMessage) string {
	if p.ToolPolicy != ToolPolicyReadOnly {
		return ""
	}
	if readTools[toolName] {
		return ""
	}
	if toolName == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(input, &in) == nil && bashAllowed(p.BashAllow, in.Command) {
			return ""
		}
	}
	return toolName + " is not available to this session: it only reads."
}

var smuggle = regexp.MustCompile("[;&|`$<>]|\n")

func bashAllowed(allow []string, command string) bool {
	if smuggle.MatchString(command) {
		return false // chaining or redirection could smuggle a write past an allowed prefix
	}
	for _, re := range allow {
		if r, err := regexp.Compile(`^(?:` + re + `)$`); err == nil && r.MatchString(trimSpace(command)) {
			return true
		}
	}
	return false
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// CompleteParams asks a model for one short answer, with no tools or session.
type CompleteParams struct {
	Model      string
	Effort     string
	System     string
	Prompt     string
	ClaudePath string
	TimeoutMs  int // 0 means 60 seconds
}

// Complete asks a model for one short answer, for small jobs such as naming a branch.
func (d *Direct) Complete(ctx context.Context, p CompleteParams) (string, error) {
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return claudesdk.Complete(ctx, claudesdk.CompleteParams{CLIPath: p.ClaudePath, Model: p.Model, Effort: p.Effort,
		System: p.System, Prompt: p.Prompt, Entrypoint: entrypoint})
}

// Probe starts claude at path just long enough to read its models and account.
func (d *Direct) Probe(ctx context.Context, path string) (ProbeResult, error) {
	c, err := claudesdk.Start(ctx, claudesdk.Options{CLIPath: path, Entrypoint: sessionEntrypoint(), UnsetEnv: hostSessionVariables,
		SettingSources: []string{"user", "project", "local"}, NoSessionPersistence: true})
	if err != nil {
		return ProbeResult{}, err
	}
	defer c.Close()
	var init struct {
		Models []struct {
			Value                 string   `json:"value"`
			DisplayName           string   `json:"displayName"`
			Description           string   `json:"description"`
			SupportedEffortLevels []string `json:"supportedEffortLevels"`
		} `json:"models"`
		Account struct {
			Email            string `json:"email"`
			Organization     string `json:"organization"`
			SubscriptionType string `json:"subscriptionType"`
			APIKeySource     string `json:"apiKeySource"`
		} `json:"account"`
	}
	if err := json.Unmarshal(c.InitResult(), &init); err != nil {
		return ProbeResult{}, fmt.Errorf("reading what Claude Code reported: %w", err)
	}
	var res ProbeResult
	for _, m := range init.Models {
		res.Models = append(res.Models, ModelInfo{Value: m.Value, DisplayName: m.DisplayName, Description: m.Description, EffortLevels: m.SupportedEffortLevels})
	}
	res.Account.Email, res.Account.Organization = init.Account.Email, init.Account.Organization
	res.Account.SubscriptionType, res.Account.APIKeySource = init.Account.SubscriptionType, init.Account.APIKeySource
	return res, nil
}
