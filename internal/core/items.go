package core

import (
	"encoding/json"
	"strings"
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
	// From is who wrote it: FromUser (empty), FromOrchestrator, or FromSolo.
	From string `json:"from,omitempty"`
	// Images are the image files sent with it.
	Images []string `json:"images,omitempty"`
	// Steer marks a message sent into a running turn, which Claude read at once.
	Steer bool `json:"steer,omitempty"`
}

// Message authors.
const (
	FromUser         = ""
	FromOrchestrator = "orchestrator"
	FromSolo         = "solo"
)

// Noted is an agent's whole reply when its turn has nothing to tell: Solo's to a thread update that
// needs no word, or an agent's when work it ran in the background ends and adds nothing new. The app
// shows no message for it, and a thread's summary stays what its last turn with news said.
const Noted = "(noted)"

// IsNoted reports whether an agent's message is Noted.
func IsNoted(text string) bool { return strings.TrimSpace(text) == Noted }

type TextData struct {
	Text string `json:"text"`
	// Images are the attachments an assistant message shows inline, as markdown images in Text
	// (see embedImages): the app draws only these.
	Images []string `json:"images,omitempty"`
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
	// ToolUseID is the tool call that started it (a Bash command, a subagent). Background: it runs on
	// after that call returned, so the call's own row no longer shows it working.
	ToolUseID  string `json:"toolUseId,omitempty"`
	Background bool   `json:"background,omitempty"`
}

type QuestionData struct {
	ToolUseID string            `json:"toolUseId"`
	Questions []agent.Question  `json:"questions"`
	Answers   map[string]string `json:"answers,omitempty"`
	Cancelled bool              `json:"cancelled,omitempty"`
	// Provided: the user entered the secret this asked for (Questions[0].Secret). Its value is in the
	// keyring and nowhere here.
	Provided bool `json:"provided,omitempty"`
}

// Waiting reports whether the question still waits for the user.
func (d QuestionData) Waiting() bool { return !d.Cancelled && !d.Provided && len(d.Answers) == 0 }

// Secret is the secret the question asks for, or nil for an ordinary question.
func (d QuestionData) Secret() *agent.SecretRequest { return SecretOf(d.Questions) }

// SecretOf is the secret questions ask for (request_secret), or nil.
func SecretOf(qs []agent.Question) *agent.SecretRequest {
	if len(qs) > 0 {
		return qs[0].Secret
	}
	return nil
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
