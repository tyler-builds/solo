//go:build !js

package agent

// Raw Claude Code messages (claudesdk.Message) into the compact events Core consumes (Event). Ported
// from the TypeScript normalizer Solo's sidecar had, after t3code's ClaudeAdapterV2.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tyler-builds/solo/claudesdk"
)

const toolResultLimit = 16_000

type normalizer struct {
	turnActive    bool   // between a turn's first root frame and its result
	streamMessage string // the root agent's streaming message id
	initModel     string
	contextTokens int
}

// abortTurn closes a turn whose process is gone; ok is false when no turn was open.
func (n *normalizer) abortTurn(errText string) (Event, bool) {
	if !n.turnActive {
		return Event{}, false
	}
	n.turnActive, n.streamMessage = false, ""
	return Event{Kind: EventTurnEnd, UUIDs: []string{}, Status: TurnFailed, Reason: "process_exited", Error: errText,
		Usage: &Usage{}, ContextTokens: n.contextTokens}, true
}

// raw is the parts of a message the normalizer reads.
type raw struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	ParentToolUseID  *string         `json:"parent_tool_use_id"`
	IsReplay         bool            `json:"isReplay"`
	UserMessageUUID  string          `json:"user_message_uuid"`
	UserMessageUUIDs []string        `json:"user_message_uuids"`
	Message          json.RawMessage `json:"message"`
	Event            json.RawMessage `json:"event"`
	Error            string          `json:"error"`
}

func (n *normalizer) push(m claudesdk.Message) []Event {
	var r raw
	if json.Unmarshal(m.Raw, &r) != nil {
		return nil
	}
	var out []Event
	if !n.turnActive && isTurnFrame(r) {
		n.turnActive = true
		out = append(out, Event{Kind: EventTurnStart, UUIDs: stampedUUIDs(r)})
	}
	switch r.Type {
	case "system":
		out = n.system(m.Raw, r.Subtype, out)
	case "stream_event":
		out = n.stream(r, out)
	case "assistant":
		var msg struct {
			ID      string            `json:"id"`
			Content []json.RawMessage `json:"content"`
			Usage   *apiUsage         `json:"usage"`
		}
		_ = json.Unmarshal(r.Message, &msg)
		if r.ParentToolUseID == nil && msg.Usage != nil {
			n.contextTokens = msg.Usage.inputSide()
		}
		var blocks []Block
		for _, raw := range msg.Content {
			var b struct {
				Type     string          `json:"type"`
				Text     string          `json:"text"`
				Thinking string          `json:"thinking"`
				ID       string          `json:"id"`
				Name     string          `json:"name"`
				Input    json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(raw, &b)
			switch b.Type {
			case "text":
				blocks = append(blocks, Block{Type: "text", Text: b.Text})
			case "thinking":
				blocks = append(blocks, Block{Type: "thinking", Text: b.Thinking})
			case "tool_use", "server_tool_use", "mcp_tool_use":
				blocks = append(blocks, Block{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input})
			}
		}
		out = append(out, Event{Kind: EventAssistant, MessageID: msg.ID, Blocks: blocks, ParentToolUseID: r.ParentToolUseID, Error: r.Error})
	case "user":
		if r.IsReplay {
			break
		}
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(r.Message, &msg)
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			break // plain text: the echo of a message sent
		}
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			out = append(out, Event{Kind: EventToolResult, ToolUseID: b.ToolUseID, IsError: b.IsError,
				Text: truncate(toolResultText(b.Content)), ParentToolUseID: r.ParentToolUseID})
		}
	case "result":
		out = append(out, n.result(m))
	case "rate_limit_event":
		var e struct {
			Info struct {
				Status        string                     `json:"status"`
				ResetsAt      float64                    `json:"resetsAt"`
				RateLimitType string                     `json:"rateLimitType"`
				Utilization   *float64                   `json:"utilization"`
				Windows       map[string]json.RawMessage `json:"unifiedWindows"`
			} `json:"rate_limit_info"`
		}
		_ = json.Unmarshal(m.Raw, &e)
		ev := Event{Kind: EventRateLimit, Status: e.Info.Status, ResetsAt: e.Info.ResetsAt, RateLimitType: e.Info.RateLimitType, Utilization: e.Info.Utilization}
		for name, w := range e.Info.Windows {
			var v struct {
				Utilization *float64 `json:"utilization"`
				ResetsAt    float64  `json:"resetsAt"`
				Status      string   `json:"status"`
			}
			if json.Unmarshal(w, &v) == nil && v.Utilization != nil {
				ev.Windows = append(ev.Windows, RateLimitWindow{RateLimitType: name, Utilization: *v.Utilization, ResetsAt: v.ResetsAt, Status: v.Status})
			}
		}
		out = append(out, ev)
	}
	return out
}

type apiUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

func (u apiUsage) inputSide() int { return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens }

func (n *normalizer) system(rawMsg json.RawMessage, subtype string, out []Event) []Event {
	var m struct {
		Model          string   `json:"model"`
		Cwd            string   `json:"cwd"`
		Tools          []string `json:"tools"`
		Version        string   `json:"claude_code_version"`
		PermissionMode string   `json:"permissionMode"`
		APIKeySource   string   `json:"apiKeySource"`
		Compact        struct {
			Trigger    string `json:"trigger"`
			PreTokens  int    `json:"pre_tokens"`
			PostTokens int    `json:"post_tokens"`
		} `json:"compact_metadata"`
		State        string          `json:"state"`
		Attempt      int             `json:"attempt"`
		MaxRetries   int             `json:"max_retries"`
		DelayMs      int             `json:"retry_delay_ms"`
		Error        json.RawMessage `json:"error"`
		ErrorStatus  json.RawMessage `json:"error_status"`
		TaskID       string          `json:"task_id"`
		ToolUseID    string          `json:"tool_use_id"`
		TaskType     string          `json:"task_type"`
		SubagentType string          `json:"subagent_type"`
		Description  string          `json:"description"`
		Summary      string          `json:"summary"`
		LastToolName string          `json:"last_tool_name"`
		Status       string          `json:"status"`
		Background   bool            `json:"is_backgrounded"`
		Ambient      bool            `json:"ambient"`
		Skip         bool            `json:"skip_transcript"`
		Usage        struct {
			TotalTokens int `json:"total_tokens"`
			ToolUses    int `json:"tool_uses"`
		} `json:"usage"`
		Patch struct {
			Status      string `json:"status"`
			Description string `json:"description"`
			Background  bool   `json:"is_backgrounded"`
		} `json:"patch"`
		Tasks []struct {
			TaskID       string `json:"task_id"`
			TaskType     string `json:"task_type"`
			Description  string `json:"description"`
			ParentTaskID string `json:"parent_task_id"`
			Ambient      bool   `json:"ambient"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal(rawMsg, &m)
	switch subtype {
	case "init":
		// The CLI repeats init at the start of every turn; forward the first and any model change.
		if n.initModel == m.Model {
			return out
		}
		n.initModel = m.Model
		return append(out, Event{Kind: EventInit, Model: m.Model, Cwd: m.Cwd, Tools: m.Tools, ClaudeVersion: m.Version,
			PermissionMode: m.PermissionMode, APIKeySource: m.APIKeySource})
	case "compact_boundary":
		return append(out, Event{Kind: EventCompact, Trigger: m.Compact.Trigger, PreTokens: m.Compact.PreTokens, PostTokens: m.Compact.PostTokens})
	case "session_state_changed":
		return append(out, Event{Kind: EventState, State: m.State})
	case "api_retry":
		errText := strings.Trim(string(m.Error), `"`)
		if errText == "" || errText == "null" {
			errText = strings.Trim(string(m.ErrorStatus), `"`)
		}
		if errText == "" || errText == "null" {
			errText = "unknown"
		}
		return append(out, Event{Kind: EventAPIRetry, Attempt: m.Attempt, MaxRetries: m.MaxRetries, DelayMs: m.DelayMs, Error: errText})
	case "task_started":
		return append(out, Event{Kind: EventTask, Phase: "started", TaskID: m.TaskID, ToolUseID: m.ToolUseID,
			TaskType: firstOf(m.TaskType, m.SubagentType), Description: m.Description, Background: m.Background, Ambient: m.Ambient || m.Skip})
	case "task_progress":
		return append(out, Event{Kind: EventTask, Phase: "progress", TaskID: m.TaskID, ToolUseID: m.ToolUseID, Description: m.Description,
			Summary: m.Summary, LastToolName: m.LastToolName, TotalTokens: m.Usage.TotalTokens, ToolUses: m.Usage.ToolUses})
	case "task_updated":
		return append(out, Event{Kind: EventTask, Phase: "updated", TaskID: m.TaskID, Status: m.Patch.Status,
			Description: m.Patch.Description, Background: m.Patch.Background})
	case "background_tasks_changed":
		var tasks []BackgroundTask
		for _, t := range m.Tasks {
			if t.TaskID == "" || t.Ambient {
				continue
			}
			tasks = append(tasks, BackgroundTask{TaskID: t.TaskID, TaskType: t.TaskType, Description: t.Description, ParentTaskID: t.ParentTaskID})
		}
		return append(out, Event{Kind: EventBackground, Tasks: tasks})
	case "task_notification":
		return append(out, Event{Kind: EventTask, Phase: "ended", TaskID: m.TaskID, ToolUseID: m.ToolUseID, Status: m.Status,
			Summary: m.Summary, TotalTokens: m.Usage.TotalTokens, ToolUses: m.Usage.ToolUses, Ambient: m.Ambient || m.Skip})
	}
	return out
}

func (n *normalizer) stream(r raw, out []Event) []Event {
	if r.ParentToolUseID != nil {
		return out // subagent text isn't streamed; its complete messages and task progress are enough
	}
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string    `json:"id"`
			Usage *apiUsage `json:"usage"`
		} `json:"message"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	_ = json.Unmarshal(r.Event, &ev)
	switch ev.Type {
	case "message_start":
		n.streamMessage = ev.Message.ID
		if ev.Message.Usage != nil {
			n.contextTokens = ev.Message.Usage.inputSide()
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			out = append(out, Event{Kind: EventTextDelta, MessageID: n.streamMessage, Index: ev.Index, Text: ev.Delta.Text})
		case "thinking_delta":
			out = append(out, Event{Kind: EventThinkingDelta, MessageID: n.streamMessage, Index: ev.Index, Text: ev.Delta.Thinking})
		}
	}
	return out
}

func (n *normalizer) result(m claudesdk.Message) Event {
	n.turnActive, n.streamMessage = false, ""
	r, _ := m.Result()
	var extra struct {
		UUID   string `json:"user_message_uuid"`
		Queued int    `json:"queued_turn_count"`
	}
	_ = json.Unmarshal(m.Raw, &extra)
	uuids := r.UserMessageUUIDs
	if uuids == nil && extra.UUID != "" {
		uuids = []string{extra.UUID}
	}
	if uuids == nil {
		uuids = []string{}
	}
	e := Event{Kind: EventTurnEnd, UUIDs: uuids, Status: r.Status(), NumTurns: r.NumTurns, DurationMs: r.DurationMS,
		TotalCostUSD: r.TotalCostUSD, ContextTokens: n.contextTokens, QueuedTurns: extra.Queued,
		Usage: &Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens,
			CacheReadTokens: r.Usage.CacheReadTokens, CacheCreationTokens: r.Usage.CacheCreationTokens}}
	if e.Status == TurnCompleted {
		e.Result = r.Result
	} else {
		e.Reason = firstOf(r.TerminalReason, r.Subtype)
		e.Error = r.ErrorText()
	}
	return e
}

// isTurnFrame reports whether a message belongs to a turn, as opposed to session housekeeping.
func isTurnFrame(r raw) bool {
	switch r.Type {
	case "assistant", "result":
		return true
	case "stream_event":
		var ev struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(r.Event, &ev)
		return ev.Type != "ping"
	case "user":
		return !r.IsReplay && r.ParentToolUseID == nil
	}
	return false
}

func stampedUUIDs(r raw) []string {
	if r.UserMessageUUIDs != nil {
		return r.UserMessageUUIDs
	}
	if r.UserMessageUUID != "" {
		return []string{r.UserMessageUUID}
	}
	if r.Type == "user" && r.UUID != "" {
		// The CLI echoes the user message itself; its uuid is the client's.
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(r.Message, &msg)
		var s string
		if json.Unmarshal(msg.Content, &s) == nil {
			return []string{r.UUID}
		}
	}
	return []string{}
}

func toolResultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

func truncate(s string) string {
	if len(s) <= toolResultLimit {
		return s
	}
	return s[:toolResultLimit] + fmt.Sprintf("\n… (%d more characters)", len(s)-toolResultLimit)
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
