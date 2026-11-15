// Package api is the interface between Solo's UI and the engine it drives. The desktop app implements
// it over the engine in the same process; a web page implements it over an encrypted connection to a
// machine that runs Solo (internal/remote).
package api

import (
	"context"
	"image"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
)

// MaxImageUpload is the largest image file SaveImageData takes. A larger image than Claude takes is
// scaled down, so this bounds what is read, not what is sent on.
const MaxImageUpload = 16 << 20

// Backend is what the UI drives: the engine and the orchestrator.
//
// The methods that only read state (Threads, Thread, Items, PendingQuestions, Todos, Settings,
// RepoPaths, PlainFolders, RateLimits) are called on the UI thread and must not wait on anything slow. A
// backend that fetches its state returns what it has and reports a change when more arrives.
type Backend interface {
	Threads() []core.ThreadView
	Thread(id string) (core.ThreadView, bool)
	Items(ctx context.Context, threadID string, limit int) ([]core.Item, error)
	PendingQuestions(threadID string) []core.PendingQuestion
	Subscribe(f func(core.Change)) func()
	Todos(done bool) []core.Todo
	CompleteTodo(id string) error
	UpdateTodo(id string, ch core.TodoChange) (core.Todo, error)

	// Images in a message are named by the references SaveImage, SaveImageData and ImportImage return.
	SendToOrchestrator(ctx context.Context, text string, images ...string) error
	Send(ctx context.Context, threadID, text string, images ...string) error
	// SaveImage stores a decoded image, such as one pasted from the clipboard, as an attachment.
	SaveImage(img image.Image) (string, error)
	// SaveImageData stores an image file's contents (PNG, JPEG, GIF or WebP, at most MaxImageUpload) as
	// an attachment.
	SaveImageData(ctx context.Context, data []byte) (string, error)
	// ImportImage stores a copy of an image file on the machine that runs Solo as an attachment. A web
	// page has no such files and uses SaveImageData.
	ImportImage(path string) (string, error)
	// ReadImage returns an attachment's contents. With maxSide above zero, an image with a longer side
	// comes scaled down to it.
	ReadImage(ctx context.Context, ref string, maxSide int) ([]byte, error)
	Interrupt(ctx context.Context, threadID string) error
	Answer(threadID, toolUseID string, answers map[string]string) error
	Dismiss(threadID, toolUseID string) error

	Changes(ctx context.Context, threadID string) ([]git.FileChange, error)
	Diff(ctx context.Context, threadID string) (string, error)
	Merge(ctx context.Context, threadID string) (string, error)
	MergeLocal(ctx context.Context, threadID string) (string, error)
	OpenPR(ctx context.Context, threadID string, draft bool) (forge.PR, error)
	MarkReady(ctx context.Context, threadID string) error
	OpenPRFor(ctx context.Context, threadID, repo string, draft bool) ([]forge.PR, error)
	MergeRepo(ctx context.Context, threadID, repo string) (string, error)
	SetThreadModel(ctx context.Context, threadID, model, effort string) error
	SearchMessages(ctx context.Context, query string, limit int) ([]core.SearchHit, error)
	// Skills lists the skills and commands a conversation can invoke with "/"; with threadID "", the
	// skills Settings offers for pull request text (see core.Core.Skills).
	Skills(ctx context.Context, threadID string) ([]core.Skill, error)
	// SearchPaths finds files and folders in a thread's working folder, for an "@" mention.
	SearchPaths(ctx context.Context, threadID, query string, limit int) ([]core.PathHit, error)
	Discard(ctx context.Context, threadID string) error
	Done(ctx context.Context, threadID string) error
	Restore(ctx context.Context, threadID string) error

	Settings() core.Settings
	UpdateSettings(f func(*core.Settings)) error
	RepoPaths() []string
	PlainFolders() []string
	InitRepo(ctx context.Context, folder string) (string, error)
	RateLimits() []agent.Event
	CheckClaude(ctx context.Context, override string) core.ClaudeStatus
}

// Watcher is a backend that wants to know which threads the UI shows, such as one that fetches their
// transcripts from another machine. The UI calls Watch when the threads on screen change; it must not
// block.
type Watcher interface {
	Watch(threadIDs []string)
}

// Clock is a backend on another machine, whose clock may differ from this one's. The engine's times
// (when a turn or background work began, when a thread changed) are the machine's, so the UI measures
// them against Now, the machine's time, rather than its own clock. Now is called on the UI thread and
// must not block. A backend that isn't a Clock shares this process's clock.
type Clock interface {
	Now() time.Time
}
