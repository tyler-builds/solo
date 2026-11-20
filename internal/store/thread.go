package store

import "time"

// Thread kinds.
const (
	KindOrchestrator = "orchestrator"
	KindWorker       = "worker"
)

// Thread is one row of threads.
type Thread struct {
	ID            string
	Kind          string
	SessionID     string
	HasTranscript bool
	Title         string
	RepoPath      string
	WorktreePath  string
	Branch        string
	BaseBranch    string
	BaseCommit    string
	Model         string
	Status        string
	Summary       string
	Error         string
	Prompt        string
	PromptImages  []string  // attachments sent with the prompt: images, and files (core.IsFileAttachment)
	DoneAt        time.Time // when the user marked it done (it leaves the rail for History); zero while open
	PR            string    // its pull request, as JSON (see forge.PR); "" when none
	Parts         string    // every repository it works in, as JSON (see core.Part); "" for old rows
	Effort        string    // its effort level; "" for the default (Settings)
	// Scratch: the user started it only to try or test something and doesn't mean to land it, so
	// nothing about landing it asks for their attention (see core.Core.SetScratch).
	Scratch bool
	CostUSD float64
	// CostBase is what earlier Claude sessions cost; CostUSD adds the current session's running total.
	CostBase      float64
	ContextTokens int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
