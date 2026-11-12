// Package agent runs the solo-agent sidecar (sidecar/) and talks to it: it opens Claude sessions, sends
// turns, and receives their events. The wire types here mirror sidecar/src/protocol.ts; keep both in sync.
package agent

import "encoding/json"

// ProtocolVersion must match PROTOCOL_VERSION in sidecar/src/protocol.ts.
const ProtocolVersion = 1

// Permission modes.
const (
	PermissionDefault = "default"
	PermissionBypass  = "bypassPermissions"
)

// Tool policies: how the sidecar answers tool requests the CLI forwards.
const (
	ToolPolicyAllow    = "allow"
	ToolPolicyReadOnly = "readonly"
)

// ToolSpec is a tool the app implements and exposes to a session over an in-process MCP server. The
// model sees it as mcp__<server>__<name>.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	ReadOnly    bool            `json:"readOnly,omitempty"`
}

// OpenParams start a session's claude process.
type OpenParams struct {
	SessionID             string   `json:"sessionId"`
	Resume                bool     `json:"resume"`
	Cwd                   string   `json:"cwd"`
	Model                 string   `json:"model,omitempty"`
	Effort                string   `json:"effort,omitempty"`
	Title                 string   `json:"title,omitempty"`
	SystemAppend          string   `json:"systemAppend,omitempty"`
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	PermissionMode        string   `json:"permissionMode"`
	Tools                 []string `json:"tools,omitempty"`
	DisallowedTools       []string `json:"disallowedTools,omitempty"`
	// Skills limits the skills the session may use (names, or "plugin:skill"); nil leaves Claude Code's
	// defaults.
	Skills []string `json:"skills,omitempty"`
	// Ephemeral keeps no transcript: for a one-off job that is never resumed.
	Ephemeral     bool              `json:"ephemeral,omitempty"`
	MCPTools      []ToolSpec        `json:"mcpTools,omitempty"`
	MCPServerName string            `json:"mcpServerName,omitempty"`
	ToolPolicy    string            `json:"toolPolicy"`
	BashAllow     []string          `json:"bashAllow,omitempty"`
	ClaudePath    string            `json:"claudePath,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
}

// SendParams is one user message.
type SendParams struct {
	SessionID string  `json:"sessionId"`
	UUID      string  `json:"uuid"`
	Text      string  `json:"text"`
	Images    []Image `json:"images,omitempty"`
	Synthetic bool    `json:"synthetic,omitempty"`
	// Steer stops what the running turn is doing so Claude reads this message now (running subagents
	// move to the background first). Without a running turn it is an ordinary send.
	Steer bool `json:"steer,omitempty"`
}

// Image is an image sent with a user message, as Claude takes it.
type Image struct {
	MediaType string `json:"mediaType"` // image/png, image/jpeg, image/gif or image/webp
	Data      string `json:"data"`      // base64
}

// QuestionOption is one choice of an AskUserQuestion question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

// Question is one AskUserQuestion question.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	MultiSelect bool             `json:"multiSelect"`
	Options     []QuestionOption `json:"options"`
}

// AskParams is the sidecar's question.ask request.
type AskParams struct {
	SessionID string     `json:"sessionId"`
	ToolUseID string     `json:"toolUseId"`
	Questions []Question `json:"questions"`
}

// AskResult answers question.ask, keyed by question text. Set Cancelled to dismiss.
type AskResult struct {
	Answers   map[string]string `json:"answers,omitempty"`
	Cancelled bool              `json:"cancelled,omitempty"`
	// Message is what the agent hears when Cancelled; empty means the user dismissed it.
	Message string `json:"message,omitempty"`
}

// ToolCallParams is the sidecar's tool.call request.
type ToolCallParams struct {
	SessionID string          `json:"sessionId"`
	ToolUseID string          `json:"toolUseId,omitempty"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
}

// ToolCallResult answers tool.call.
type ToolCallResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
}

// ModelInfo is a model Claude Code offers.
type ModelInfo struct {
	Value        string   `json:"value"`
	DisplayName  string   `json:"displayName"`
	Description  string   `json:"description"`
	EffortLevels []string `json:"effortLevels,omitempty"`
}

// ProbeResult describes the Claude install: its models and account.
type ProbeResult struct {
	ClaudeVersion string      `json:"claudeVersion"`
	Models        []ModelInfo `json:"models"`
	Account       struct {
		Email            string `json:"email,omitempty"`
		Organization     string `json:"organization,omitempty"`
		SubscriptionType string `json:"subscriptionType,omitempty"`
		APIKeySource     string `json:"apiKeySource,omitempty"`
	} `json:"account"`
}

// Event kinds.
const (
	EventInit          = "init"
	EventTurnStart     = "turn_start"
	EventTextDelta     = "text_delta"
	EventThinkingDelta = "thinking_delta"
	EventAssistant     = "assistant"
	EventToolResult    = "tool_result"
	EventTask          = "task"
	EventBackground    = "background"
	EventCompact       = "compact"
	EventState         = "state"
	EventAPIRetry      = "api_retry"
	EventRateLimit     = "rate_limit"
	EventTurnEnd       = "turn_end"
	EventNotice        = "notice"
	EventExited        = "exited"
)

// Turn statuses.
const (
	TurnCompleted   = "completed"
	TurnInterrupted = "interrupted"
	TurnFailed      = "failed"
	TurnCancelled   = "cancelled"
)

// Block is one block of an assistant message.
type Block struct {
	Type  string          `json:"type"` // text, thinking, tool_use
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Usage is token usage.
type Usage struct {
	InputTokens         int `json:"inputTokens"`
	OutputTokens        int `json:"outputTokens"`
	CacheReadTokens     int `json:"cacheReadTokens"`
	CacheCreationTokens int `json:"cacheCreationTokens"`
}

// Event is one session event. Kind says which fields are set; see AgentEvent in protocol.ts.
type Event struct {
	SessionID string `json:"sessionId"`
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`

	// init
	Model          string   `json:"model,omitempty"`
	Cwd            string   `json:"cwd,omitempty"`
	Tools          []string `json:"tools,omitempty"`
	ClaudeVersion  string   `json:"claudeVersion,omitempty"`
	PermissionMode string   `json:"permissionMode,omitempty"`
	APIKeySource   string   `json:"apiKeySource,omitempty"`

	// turn_start, turn_end
	UUIDs []string `json:"uuids,omitempty"`

	// text_delta, thinking_delta, assistant, tool_result
	MessageID       string  `json:"messageId,omitempty"`
	Index           int     `json:"index,omitempty"`
	Text            string  `json:"text,omitempty"`
	ParentToolUseID *string `json:"parentToolUseId,omitempty"`
	Blocks          []Block `json:"blocks,omitempty"`
	ToolUseID       string  `json:"toolUseId,omitempty"`
	IsError         bool    `json:"isError,omitempty"`

	// task
	Phase        string `json:"phase,omitempty"`
	TaskID       string `json:"taskId,omitempty"`
	TaskType     string `json:"taskType,omitempty"`
	Description  string `json:"description,omitempty"`
	Summary      string `json:"summary,omitempty"`
	LastToolName string `json:"lastToolName,omitempty"`
	TotalTokens  int    `json:"totalTokens,omitempty"`
	ToolUses     int    `json:"toolUses,omitempty"`
	Background   bool   `json:"background,omitempty"`
	Ambient      bool   `json:"ambient,omitempty"`

	// background: every task still running in the background, replacing the previous list
	Tasks []BackgroundTask `json:"tasks,omitempty"`

	// compact
	Trigger    string `json:"trigger,omitempty"`
	PreTokens  int    `json:"preTokens,omitempty"`
	PostTokens int    `json:"postTokens,omitempty"`

	// state, rate_limit, task (status), turn_end (status)
	State  string `json:"state,omitempty"`
	Status string `json:"status,omitempty"`

	// api_retry
	Attempt    int `json:"attempt,omitempty"`
	MaxRetries int `json:"maxRetries,omitempty"`
	DelayMs    int `json:"delayMs,omitempty"`

	// rate_limit. Status, ResetsAt, RateLimitType and Utilization describe only the limit that binds
	// now; Windows has every subscription window Claude Code tracks.
	ResetsAt      float64 `json:"resetsAt,omitempty"`      // Unix seconds
	RateLimitType string  `json:"rateLimitType,omitempty"` // five_hour, seven_day, seven_day_opus, …
	// Utilization is how much of the limit is used, as Claude Code reports it: a fraction, 0 to 1. Nil
	// when the event doesn't say.
	Utilization *float64          `json:"utilization,omitempty"`
	Windows     []RateLimitWindow `json:"windows,omitempty"`

	// turn_end
	Reason        string  `json:"reason,omitempty"`
	Error         string  `json:"error,omitempty"`
	Result        string  `json:"result,omitempty"`
	NumTurns      int     `json:"numTurns,omitempty"`
	DurationMs    int     `json:"durationMs,omitempty"`
	TotalCostUSD  float64 `json:"totalCostUsd,omitempty"`
	Usage         *Usage  `json:"usage,omitempty"`
	ContextTokens int     `json:"contextTokens,omitempty"`
	QueuedTurns   int     `json:"queuedTurns,omitempty"`
	// Steered marks a turn end that only made way for a steer: the turn carries on at the next turn_start.
	Steered bool `json:"steered,omitempty"`

	// notice
	Level string `json:"level,omitempty"`
}

// BackgroundTask is work a session runs in the background (a command, a subagent), from a background
// event.
type BackgroundTask struct {
	TaskID       string `json:"taskId"`
	TaskType     string `json:"taskType"` // local_bash, local_agent, …
	Description  string `json:"description"`
	ParentTaskID string `json:"parentTaskId,omitempty"`
}

// RateLimitWindow is one subscription rate-limit window of a rate_limit event (five_hour, seven_day, …).
type RateLimitWindow struct {
	RateLimitType string  `json:"rateLimitType"`
	Utilization   float64 `json:"utilization"`        // fraction used; can pass 1
	ResetsAt      float64 `json:"resetsAt,omitempty"` // Unix seconds
	Status        string  `json:"status,omitempty"`   // allowed, allowed_warning, rejected
}

// IsRoot reports whether the event comes from the main agent rather than a subagent.
func (e *Event) IsRoot() bool { return e.ParentToolUseID == nil || *e.ParentToolUseID == "" }
