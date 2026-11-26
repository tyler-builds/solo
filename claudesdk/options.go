package claudesdk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultEntrypoint is what claude processes started by this package report as their entrypoint
// (CLAUDE_CODE_ENTRYPOINT) unless Options.Entrypoint says otherwise.
const DefaultEntrypoint = "sdk-go"

// Options configure a claude process. The zero value runs Claude Code with its own defaults: its
// system prompt, its tools, and the user's settings.
type Options struct {
	// CLIPath is the claude executable. Empty finds it with FindCLI.
	CLIPath string
	Cwd     string
	// Env is added to this process's environment. CLAUDECODE, which would make claude act as part of
	// a parent Claude Code session, is always removed first, and so is every variable in UnsetEnv.
	Env      map[string]string
	UnsetEnv []string
	// Entrypoint is CLAUDE_CODE_ENTRYPOINT; empty means DefaultEntrypoint.
	Entrypoint string

	Model  string
	Effort string // low, medium, high, xhigh, max
	// Name is the session's display name (--name).
	Name string

	// SystemPrompt replaces Claude Code's system prompt when set. AppendSystemPrompt keeps Claude
	// Code's and adds to it.
	SystemPrompt       string
	AppendSystemPrompt string

	// Tools is the built-in tool set: nil for Claude Code's default, an empty non-nil slice for none.
	Tools           []string
	AllowedTools    []string
	DisallowedTools []string
	PermissionMode  string // default, acceptEdits, bypassPermissions, plan, …

	// SessionID starts a new session with this id. Resume continues a saved session instead.
	SessionID string
	Resume    string
	// NoSessionPersistence keeps no transcript: the session can't be resumed.
	NoSessionPersistence bool

	AddDirs []string
	// SettingSources limits which settings load (user, project, local): nil for Claude Code's default,
	// an empty non-nil slice for none.
	SettingSources []string
	// MCPConfig is a JSON object of the form {"mcpServers": {...}}. StrictMCPConfig ignores every other
	// MCP server, the user's and claude.ai's included.
	MCPConfig       string
	StrictMCPConfig bool
	// MCPServers run in this process (see MCPServer); they are added to MCPConfig.
	MCPServers []*MCPServer
	// Hooks are callbacks for hook events, by event name (PreToolUse, PostToolUse, …), registered
	// with initialize.
	Hooks map[string][]HookMatcher
	// Skills limits the skills the session may use, sent with initialize; nil leaves the default.
	Skills []string

	IncludePartialMessages bool
	Thinking               string // adaptive or disabled; empty leaves the default
	ThinkingDisplay        string // summarized, …
	MaxTurns               int

	// ExtraArgs are more CLI flags, by name without the dashes; an empty value is a bare flag.
	ExtraArgs map[string]string

	// CanUseTool answers the CLI's permission requests. When set, the CLI is told to ask over the
	// control protocol (--permission-prompt-tool stdio). It runs on its own goroutine; ctx is cancelled
	// when the CLI withdraws the request.
	CanUseTool func(ctx context.Context, req PermissionRequest) PermissionResult

	// Stderr receives the CLI's stderr, a line at a time.
	Stderr func(line string)
	// InitializeTimeout bounds the initialize handshake; 0 means 60 seconds.
	InitializeTimeout time.Duration
}

// HookMatcher is a hook callback for the tools whose names Matcher matches (a regular expression, as
// in Claude Code's settings; "" matches every tool).
type HookMatcher struct {
	Matcher string
	// Callback gets the hook's input (hook_event_name, tool_name, tool_input, …) and returns its output,
	// a JSON object such as {"hookSpecificOutput": {...}}; nil means {}. It runs on its own goroutine.
	Callback func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

// PermissionRequest is a can_use_tool request: the CLI asks whether a tool call may run.
type PermissionRequest struct {
	ToolName       string          `json:"tool_name"`
	Input          json.RawMessage `json:"input"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	AgentID        string          `json:"agent_id,omitempty"`
	BlockedPath    string          `json:"blocked_path,omitempty"`
	DecisionReason string          `json:"decision_reason,omitempty"`
	Title          string          `json:"title,omitempty"`
	DisplayName    string          `json:"display_name,omitempty"`
	Description    string          `json:"description,omitempty"`
	Suggestions    json.RawMessage `json:"permission_suggestions,omitempty"`
}

// PermissionResult answers a PermissionRequest. Allow runs the tool with UpdatedInput (the request's
// input when nil); otherwise the tool is refused with Message, and Interrupt also ends the turn.
type PermissionResult struct {
	Allow        bool
	UpdatedInput json.RawMessage
	Message      string
	Interrupt    bool
}

// args are the CLI flags for o, after the executable.
func (o *Options) args() []string {
	a := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json"}
	switch {
	case o.SystemPrompt != "":
		a = append(a, "--system-prompt", o.SystemPrompt)
	case o.AppendSystemPrompt != "":
		a = append(a, "--append-system-prompt", o.AppendSystemPrompt)
	}
	if o.Tools != nil {
		a = append(a, "--tools", strings.Join(o.Tools, ","))
	}
	if len(o.AllowedTools) > 0 {
		a = append(a, "--allowedTools", strings.Join(o.AllowedTools, ","))
	}
	if len(o.DisallowedTools) > 0 {
		a = append(a, "--disallowedTools", strings.Join(o.DisallowedTools, ","))
	}
	if o.MaxTurns > 0 {
		a = append(a, "--max-turns", strconv.Itoa(o.MaxTurns))
	}
	if o.Model != "" {
		a = append(a, "--model", o.Model)
	}
	if o.Name != "" {
		a = append(a, "--name="+o.Name)
	}
	if o.CanUseTool != nil {
		a = append(a, "--permission-prompt-tool", "stdio")
	}
	if o.PermissionMode != "" {
		a = append(a, "--permission-mode", o.PermissionMode)
	}
	// The equals form, so a value can never be read as a flag of its own.
	if o.Resume != "" {
		a = append(a, "--resume="+o.Resume)
	} else if o.SessionID != "" {
		a = append(a, "--session-id="+o.SessionID)
	}
	if o.NoSessionPersistence {
		a = append(a, "--no-session-persistence")
	}
	for _, d := range o.AddDirs {
		a = append(a, "--add-dir", d)
	}
	if cfg, _ := o.mcpConfig(); cfg != "" {
		a = append(a, "--mcp-config", cfg)
	}
	if o.StrictMCPConfig {
		a = append(a, "--strict-mcp-config")
	}
	if o.IncludePartialMessages {
		a = append(a, "--include-partial-messages")
	}
	if o.SettingSources != nil {
		a = append(a, "--setting-sources="+strings.Join(o.SettingSources, ","))
	}
	if o.Thinking != "" {
		a = append(a, "--thinking", o.Thinking)
	}
	if o.ThinkingDisplay != "" && o.Thinking != "disabled" {
		a = append(a, "--thinking-display", o.ThinkingDisplay)
	}
	if o.Effort != "" {
		a = append(a, "--effort", o.Effort)
	}
	keys := make([]string, 0, len(o.ExtraArgs))
	for k := range o.ExtraArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := o.ExtraArgs[k]; v == "" {
			a = append(a, "--"+k)
		} else {
			a = append(a, "--"+k+"="+v)
		}
	}
	return a
}

// env is the claude process's environment.
func (o *Options) env() []string {
	entry := o.Entrypoint
	if entry == "" {
		entry = DefaultEntrypoint
	}
	m := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = v
	}
	unset := map[string]bool{"CLAUDECODE": true}
	for _, k := range o.UnsetEnv {
		unset[k] = true
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if unset[k] {
			continue
		}
		set(k, v)
	}
	set("CLAUDE_CODE_ENTRYPOINT", entry)
	for k, v := range o.Env {
		set(k, v)
	}
	if o.Cwd != "" {
		set("PWD", o.Cwd)
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out
}

// FindCLI looks for the claude executable on PATH, then where Claude Code's installers put it. It
// returns "" when there is none.
func FindCLI() string {
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	if p, err := lookPath(name); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".claude", "local", name),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
