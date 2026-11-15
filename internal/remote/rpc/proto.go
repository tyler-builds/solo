package rpc

import (
	"errors"
	"fmt"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

// Envelope kinds.
const (
	kindHello   = "hello"   // device: the first message, with what it already has
	kindWelcome = "welcome" // machine: the answer to hello
	kindReq     = "req"     // device: a request
	kindRes     = "res"     // machine: a request's answer
	kindSync    = "sync"    // machine: state that changed, and the changes announced
	kindBye     = "bye"     // machine: the session is over, and why
)

// Limits on what each end takes in one envelope.
const (
	// maxFromDevice holds an image upload and its request.
	maxFromDevice = 17 << 20
	// maxFromMachine holds an image, or a transcript window.
	maxFromMachine = 32 << 20
)

// Error codes.
const (
	CodeError     = "error"     // the request failed; Message says why
	CodeResolving = "resolving" // a merge went back to its thread to resolve conflicts (core.ResolvingError)
	CodeBusy      = "busy"      // another landing action is under way (core.ErrBusy)
	CodeDenied    = "denied"    // this device may not do that
	CodeInvalid   = "invalid"   // the request is not one the machine takes
	CodeVersion   = "version"   // the two ends speak different versions
)

// Error is why a request failed.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Title   string `json:"title,omitempty"` // CodeResolving: the thread's title
}

func (e *Error) Error() string { return e.Message }

// toError turns a backend's error into what crosses.
func toError(err error) *Error {
	var re *core.ResolvingError
	var own *Error
	switch {
	case errors.As(err, &own):
		return own
	case errors.As(err, &re):
		return &Error{Code: CodeResolving, Message: err.Error(), Title: re.Title}
	case errors.Is(err, core.ErrBusy):
		return &Error{Code: CodeBusy, Message: err.Error()}
	}
	return &Error{Code: CodeError, Message: clip(err.Error(), 2000)}
}

// fromError turns what crossed back into the error the UI knows.
func fromError(e *Error) error {
	switch e.Code {
	case CodeResolving:
		return &core.ResolvingError{Title: e.Title}
	case CodeBusy:
		return core.ErrBusy
	}
	return e
}

func denied(format string, args ...any) *Error {
	return &Error{Code: CodeDenied, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *Error {
	return &Error{Code: CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Hello is a device's first message.
type Hello struct {
	Protocol int `json:"protocol"`
	// Epoch and Rev say what the device holds from an earlier session: the machine's run it came from,
	// and the last revision it applied. The machine then sends only what is newer.
	Epoch string `json:"epoch,omitempty"`
	Rev   uint64 `json:"rev,omitempty"`
}

// Welcome is the machine's answer.
type Welcome struct {
	Protocol int      `json:"protocol"`
	Epoch    string   `json:"epoch"`
	ReadOnly bool     `json:"readOnly,omitempty"` // this device may look and not change
	Host     HostInfo `json:"host"`
	// Resumed: the machine sends what changed since the device's revision; otherwise everything, and
	// the device drops what it held.
	Resumed bool `json:"resumed,omitempty"`
}

// HostInfo describes the machine, for showing its paths and times.
type HostInfo struct {
	Name string `json:"name"`
	OS   string `json:"os"`
	Home string `json:"home"`
	Time int64  `json:"time"` // the machine's clock, in Unix milliseconds
	// Version is the machine's Solo (version.String); empty from one too old to say.
	Version string `json:"version,omitempty"`
}

// Bye ends a session.
type Bye struct {
	Code    string `json:"code"` // CodeVersion, CodeDenied (revoked), CodeError
	Message string `json:"message"`
}

// Sync is state that changed on the machine. A device applies the state, then announces the changes.
type Sync struct {
	Rev uint64 `json:"rev"` // the machine's revision once this is applied
	// Time is the machine's clock as it sent this, in Unix milliseconds, so a device keeps measuring
	// the machine's times against it (see Client.Now); 0 from a machine too old to say.
	Time int64 `json:"time,omitempty"`

	Threads []core.ThreadView `json:"threads,omitempty"` // new or changed, without their live text
	Live    []LiveUpdate      `json:"live,omitempty"`
	Items   []ItemsUpdate     `json:"items,omitempty"`

	Settings *core.Settings                     `json:"settings,omitempty"`
	Todos    *[]core.Todo                       `json:"todos,omitempty"` // all of them, done or not
	Repos    *[]string                          `json:"repos,omitempty"`
	Plain    *[]string                          `json:"plain,omitempty"`
	Rate     *[]agent.Event                     `json:"rate,omitempty"`
	Pending  *map[string][]core.PendingQuestion `json:"pending,omitempty"`

	// Changes are the machine's announcements, as it made them: notifications depend on their details.
	Changes []core.Change `json:"changes,omitempty"`
	// Synced ends the catch-up after hello: the device now shows the machine as it is.
	Synced bool `json:"synced,omitempty"`
}

// LiveUpdate is a watched thread's streaming text: for each block, the text added since the last
// update, or the whole text when Reset says to start over.
type LiveUpdate struct {
	ThreadID string      `json:"thread"`
	Reset    bool        `json:"reset,omitempty"`
	Blocks   []LiveDelta `json:"blocks,omitempty"` // the blocks that changed
	Count    int         `json:"count"`            // how many blocks there are now
}

// LiveDelta is the change to one block.
type LiveDelta struct {
	Index    int    `json:"i"`
	Thinking bool   `json:"thinking,omitempty"`
	Reset    bool   `json:"reset,omitempty"` // Text replaces the block's text
	Text     string `json:"text"`
}

// ItemsUpdate is a watched thread's transcript: the items that are new or changed, and the IDs of those
// that are gone. A window too large for one message comes in several; the last has Done.
type ItemsUpdate struct {
	ThreadID string      `json:"thread"`
	Rev      uint64      `json:"rev"`             // the transcript's revision
	Reset    bool        `json:"reset,omitempty"` // drop the items held before applying
	Upsert   []core.Item `json:"upsert,omitempty"`
	Remove   []int64     `json:"remove,omitempty"`
	Done     bool        `json:"done,omitempty"`
}

// Watch (method "watch") says which threads the device shows, so the machine sends their transcripts
// and streaming text. It lasts for LeaseFor; a device that stays renews it.
type Watch struct {
	Threads []WatchThread `json:"threads"`
	Limit   int           `json:"limit"` // how many of each transcript's newest items it wants
}

// WatchThread is a thread to watch and the revision of its transcript the device holds, 0 for none.
type WatchThread struct {
	ID  string `json:"id"`
	Rev uint64 `json:"rev,omitempty"`
}
