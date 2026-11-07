package core

import (
	"encoding/json"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
)

// Item kinds: one transcript entry each.
const (
	ItemUser      = "user"      // UserData
	ItemAssistant = "assistant" // TextData
	ItemThinking  = "thinking"  // TextData
	ItemTool      = "tool"      // ToolData; Ref is the tool_use id
	ItemTask      = "task"      // TaskData; Ref is the task id
	ItemQuestion  = "question"  // QuestionData; Ref is the tool_use id
	ItemTurn      = "turn"      // TurnData: the end of a turn
	ItemNotice    = "notice"    // NoticeData: app messages (worktree created, merged, errors)
)

// Item is one transcript entry. Data holds the kind's payload; decode it with the matching type.
type Item struct {
	ID        int64
	ThreadID  string
	Kind      string
	TurnUUID  string
	Ref       string
	Data      json.RawMessage
	CreatedAt time.Time
}

// Decode decodes the item's data into v.
func (it *Item) Decode(v any) error { return json.Unmarshal(it.Data, v) }

type UserData struct {
	Text string `json:"text"`
	// Synthetic marks text the app sent, such as a thread update to the orchestrator.
	Synthetic bool `json:"synthetic,omitempty"`
}

type TextData struct {
	Text string `json:"text"`
}

type ToolData struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input,omitempty"`
	Result  string          `json:"result,omitempty"`
	IsError bool            `json:"isError,omitempty"`
	Done    bool            `json:"done,omitempty"`
}

type TaskData struct {
	TaskID      string `json:"taskId"`
	Description string `json:"description,omitempty"`
	TaskType    string `json:"taskType,omitempty"`
	Status      string `json:"status,omitempty"` // running, completed, failed, stopped
	Summary     string `json:"summary,omitempty"`
	ToolUses    int    `json:"toolUses,omitempty"`
}

type QuestionData struct {
	ToolUseID string            `json:"toolUseId"`
	Questions []agent.Question  `json:"questions"`
	Answers   map[string]string `json:"answers,omitempty"`
	Cancelled bool              `json:"cancelled,omitempty"`
}

type TurnData struct {
	Status     string  `json:"status"`
	Reason     string  `json:"reason,omitempty"`
	Error      string  `json:"error,omitempty"`
	DurationMs int     `json:"durationMs,omitempty"`
	CostUSD    float64 `json:"costUsd,omitempty"` // running total for the session's process
}

type NoticeData struct {
	Level string `json:"level"` // info, warning, error
	Text  string `json:"text"`
}

// LiveBlock is assistant text still streaming.
type LiveBlock struct {
	Thinking bool
	Text     string
}
