package claudesdk

import (
	"encoding/json"
	"strings"
)

// Message is one message the CLI emitted, other than control frames. Type and Subtype say what it
// is (system/init, assistant, user, stream_event, result/success, …); Raw is the whole message for
// the caller to decode.
type Message struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype,omitempty"`
	UUID            string          `json:"uuid,omitempty"`
	SessionID       string          `json:"session_id,omitempty"`
	ParentToolUseID *string         `json:"parent_tool_use_id,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

// Decode unmarshals the whole message into v.
func (m Message) Decode(v any) error { return json.Unmarshal(m.Raw, v) }

// IsRoot reports whether the message comes from the main agent rather than a subagent.
func (m Message) IsRoot() bool { return m.ParentToolUseID == nil || *m.ParentToolUseID == "" }

// Usage is a turn's token usage.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

// ResultMessage is the result message that ends a turn.
type ResultMessage struct {
	Subtype          string   `json:"subtype"` // success, error_max_turns, error_during_execution, …
	Result           string   `json:"result"`
	IsError          bool     `json:"is_error"`
	Errors           []string `json:"errors"`
	TerminalReason   string   `json:"terminal_reason"`
	APIErrorStatus   int      `json:"api_error_status"`
	NumTurns         int      `json:"num_turns"`
	DurationMS       int      `json:"duration_ms"`
	TotalCostUSD     float64  `json:"total_cost_usd"`
	SessionID        string   `json:"session_id"`
	UserMessageUUIDs []string `json:"user_message_uuids"`
	Usage            Usage    `json:"usage"`
}

// Result decodes m as a result message; ok is false when it is not one.
func (m Message) Result() (r ResultMessage, ok bool) {
	if m.Type != "result" {
		return r, false
	}
	return r, m.Decode(&r) == nil
}

// Turn statuses, from TurnStatus.
const (
	TurnCompleted   = "completed"
	TurnInterrupted = "interrupted"
	TurnFailed      = "failed"
	TurnCancelled   = "cancelled"
)

// failureReasons are terminal reasons that make a "success" result a failure.
var failureReasons = map[string]bool{
	"blocking_limit": true, "rapid_refill_breaker": true, "prompt_too_long": true, "image_error": true,
	"model_error": true, "api_error": true, "malformed_tool_use_exhausted": true, "budget_exhausted": true,
	"max_turns": true, "turn_setup_failed": true, "structured_output_retry_exhausted": true,
	"tool_deferred_unavailable": true,
}

// Status says how the turn ended. A "success" result can still be a failure (an API error, a rate
// limit, a full context), and an abort reason wins over success. Ported from t3code's
// terminalStatusFromResult.
func (r ResultMessage) Status() string {
	if r.TerminalReason == "aborted_tools" || r.TerminalReason == "aborted_streaming" {
		return TurnInterrupted
	}
	if r.Subtype == "success" {
		if r.IsError || r.APIErrorStatus == 429 || r.APIErrorStatus == 529 || failureReasons[r.TerminalReason] {
			return TurnFailed
		}
		return TurnCompleted
	}
	text := strings.ToLower(strings.Join(r.Errors, "\n"))
	switch {
	case strings.Contains(text, "interrupt"):
		return TurnInterrupted
	case strings.Contains(text, "cancel"):
		return TurnCancelled
	}
	return TurnFailed
}

// ErrorText is why the turn failed, for a result whose Status is not completed.
func (r ResultMessage) ErrorText() string {
	if len(r.Errors) > 0 {
		return strings.Join(r.Errors, "\n")
	}
	if r.Result != "" {
		return r.Result
	}
	switch {
	case r.APIErrorStatus == 429:
		return "Claude is rate limited."
	case r.APIErrorStatus == 529:
		return "Claude is overloaded."
	case r.TerminalReason != "":
		return r.TerminalReason
	case r.Subtype != "success":
		return r.Subtype
	}
	return "Claude failed."
}

// UserMessage is a message to send into the session.
type UserMessage struct {
	// UUID identifies the message; the CLI echoes it back on the turn that answers it. Empty sends none.
	UUID   string
	Text   string
	Images []Image
	// Priority "now" steers a running turn: Claude stops what it is doing and reads this message.
	Priority string
	// ClientComposed marks text the app wrote rather than the user typed.
	ClientComposed bool
}

// Image is an image sent with a user message.
type Image struct {
	MediaType string // image/png, image/jpeg, image/gif or image/webp
	Data      string // base64
}

func (u UserMessage) wire() map[string]any {
	var content any = u.Text
	if len(u.Images) > 0 {
		blocks := make([]map[string]any, 0, len(u.Images)+1)
		// Images first: Claude reads them best before the words about them.
		for _, im := range u.Images {
			blocks = append(blocks, map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "base64", "media_type": im.MediaType, "data": im.Data},
			})
		}
		if u.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": u.Text})
		}
		content = blocks
	}
	m := map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
		"session_id":         "",
	}
	if u.UUID != "" {
		m["uuid"] = u.UUID
	}
	if u.Priority != "" {
		m["priority"] = u.Priority
	}
	if u.ClientComposed {
		m["client_composed"] = true
	}
	return m
}
