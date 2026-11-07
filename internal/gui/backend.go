package gui

import (
	"context"
	"image"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/orchestrator"
)

// Backend is what the UI drives: the core and the orchestrator. Tests and screenshots use a fake.
type Backend interface {
	Threads() []core.ThreadView
	Thread(id string) (core.ThreadView, bool)
	Items(ctx context.Context, threadID string, limit int) ([]core.Item, error)
	PendingQuestions(threadID string) []core.PendingQuestion
	Subscribe(f func(core.Change)) func()

	SendToOrchestrator(ctx context.Context, text string, images ...string) error
	Send(ctx context.Context, threadID, text string, images ...string) error
	SaveImage(img image.Image) (string, error) // a pasted image, stored as an attachment
	ImportImage(path string) (string, error)   // a copy of an image file, stored as an attachment
	Interrupt(ctx context.Context, threadID string) error
	Answer(threadID, toolUseID string, answers map[string]string) error
	Dismiss(threadID, toolUseID string) error

	Changes(ctx context.Context, threadID string) ([]git.FileChange, error)
	Diff(ctx context.Context, threadID string) (string, error)
	Merge(ctx context.Context, threadID string) (string, error)
	Discard(ctx context.Context, threadID string) error
	Archive(ctx context.Context, threadID string) error

	Settings() core.Settings
	UpdateSettings(f func(*core.Settings)) error
	RepoPaths() []string
	RateLimit() agent.Event
	CheckClaude(ctx context.Context, override string) core.ClaudeStatus
}

// Live is the real backend.
type Live struct {
	*core.Core
	O *orchestrator.Orchestrator
}

func (l Live) SendToOrchestrator(ctx context.Context, text string, images ...string) error {
	return l.O.Send(ctx, text, images...)
}
