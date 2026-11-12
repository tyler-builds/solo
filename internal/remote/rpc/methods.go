package rpc

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
)

// Limits on what a device may ask for. A paired device is authenticated, but it may have been taken
// over, so the machine bounds every request rather than trusting the UI that sent it.
const (
	maxText       = 512 << 10 // a message's text
	maxImages     = 20        // images named in one message
	maxItems      = 400       // transcript items in a window
	maxHits       = 50        // search results
	maxQuery      = 256       // a search's length
	maxDiff       = 4 << 20   // a diff's size; more is cut, and said to be
	maxFiles      = 5000      // changed files listed
	maxPreview    = 4096      // an image preview's longest side
	maxWatched    = 8         // threads watched at once
	maxAnswers    = 64        // answers in one reply to a question
	maxAnswerText = 16 << 10
)

// method is a request the machine takes.
type method struct {
	// write: it changes something. A read-only device may not send it, it is recorded in the audit log,
	// and it runs once however often it is sent.
	write bool
	// run returns the result, any bytes to send with it, and what the audit log says it was about.
	run func(ctx context.Context, c *conn, p json.RawMessage, blob []byte) (result any, out []byte, detail string, err error)
}

// Parameters. A thread is named by its ID.
type (
	threadParams struct {
		Thread string `json:"thread"`
	}
	sendParams struct {
		Thread string   `json:"thread"` // "" for Solo
		Text   string   `json:"text"`
		Images []string `json:"images,omitempty"`
	}
	imageParams struct {
		Ref     string `json:"ref"`
		MaxSide int    `json:"maxSide,omitempty"`
	}
	imageResult struct {
		Ref string `json:"ref"`
	}
	answerParams struct {
		Thread  string            `json:"thread"`
		ToolUse string            `json:"toolUse"`
		Answers map[string]string `json:"answers,omitempty"`
	}
	repoParams struct {
		Thread string `json:"thread"`
		Repo   string `json:"repo,omitempty"`
		Draft  bool   `json:"draft,omitempty"`
	}
	modelParams struct {
		Thread string `json:"thread"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
	}
	searchParams struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	diffResult struct {
		Diff      string `json:"diff"`
		Truncated bool   `json:"truncated,omitempty"`
	}
	textResult struct {
		Text string `json:"text"`
	}
	settingsParams struct {
		Patch api.SettingsPatch `json:"patch"`
	}
	folderParams struct {
		Folder string `json:"folder"`
	}
	todoParams struct {
		ID     string          `json:"id"`
		Change core.TodoChange `json:"change"`
	}
)

// decode reads a request's parameters, and refuses a thread or other ID that isn't one.
func decode[T any](p json.RawMessage) (T, error) {
	var v T
	if len(p) == 0 {
		return v, invalid("the request has no parameters")
	}
	if err := json.Unmarshal(p, &v); err != nil {
		return v, invalid("the request's parameters aren't valid")
	}
	return v, nil
}

// checkPath is checkID for a folder or a repository, which may be named by its whole path.
func checkPath(what, p string) error {
	if len(p) == 0 || len(p) > 4096 || !utf8.ValidString(p) || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return invalid("that isn't a %s", what)
	}
	return nil
}

func checkID(what, id string) error {
	if !validID(id) || !utf8.ValidString(id) {
		return invalid("that isn't a %s", what)
	}
	return nil
}

// thread makes a method that takes a thread and returns nothing.
func thread(write bool, f func(ctx context.Context, b api.Backend, id string) error) method {
	return method{write: write, run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
		a, err := decode[threadParams](p)
		if err == nil {
			err = checkID("thread", a.Thread)
		}
		if err != nil {
			return nil, nil, "", err
		}
		return nil, nil, a.Thread, f(ctx, c.h.b, a.Thread)
	}}
}

// commit makes a method that takes a thread and returns text, such as the commit a merge made.
func commit(f func(ctx context.Context, b api.Backend, id string) (string, error)) method {
	return method{write: true, run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
		a, err := decode[threadParams](p)
		if err == nil {
			err = checkID("thread", a.Thread)
		}
		if err != nil {
			return nil, nil, "", err
		}
		text, err := f(ctx, c.h.b, a.Thread)
		return textResult{Text: text}, nil, a.Thread, err
	}}
}

var methods map[string]method

func init() {
	methods = map[string]method{
		"watch": {run: func(_ context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			w, err := decode[Watch](p)
			if err != nil {
				return nil, nil, "", err
			}
			if len(w.Threads) > maxWatched {
				return nil, nil, "", invalid("too many threads to watch")
			}
			for _, t := range w.Threads {
				if err := checkID("thread", t.ID); err != nil {
					return nil, nil, "", err
				}
			}
			limit := min(max(w.Limit, 1), maxItems)
			c.mu.Lock()
			// A device that now wants more of each transcript is sent the windows again: what it
			// holds is current, and too short.
			grew := limit > c.limit && len(c.watch) > 0
			next := make(map[string]*watched, len(w.Threads))
			for _, t := range w.Threads {
				st := c.watch[t.ID]
				if st == nil {
					st = &watched{reset: true}
				}
				st.held = t.Rev
				if grew {
					st.reset, st.held = true, 0
				}
				next[t.ID] = st
			}
			c.watch = next
			c.limit = limit
			c.lease = time.Now().Add(c.h.opts.Lease)
			c.mu.Unlock()
			c.poke()
			return nil, nil, "", nil
		}},

		"send": {write: true, run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[sendParams](p)
			if err != nil {
				return nil, nil, "", err
			}
			if len(a.Text) > maxText || !utf8.ValidString(a.Text) {
				return nil, nil, "", invalid("the message is too long")
			}
			if len(a.Images) > maxImages {
				return nil, nil, "", invalid("at most %d images in a message", maxImages)
			}
			for _, ref := range a.Images {
				if len(ref) == 0 || len(ref) > 4096 {
					return nil, nil, "", invalid("that isn't an image")
				}
			}
			if a.Thread == "" {
				return nil, nil, core.OrchestratorID, c.h.b.SendToOrchestrator(ctx, a.Text, a.Images...)
			}
			if err := checkID("thread", a.Thread); err != nil {
				return nil, nil, "", err
			}
			return nil, nil, a.Thread, c.h.b.Send(ctx, a.Thread, a.Text, a.Images...)
		}},
		"saveImage": {write: true, run: func(ctx context.Context, c *conn, _ json.RawMessage, blob []byte) (any, []byte, string, error) {
			if len(blob) == 0 || len(blob) > api.MaxImageUpload {
				return nil, nil, "", invalid("an image is at most %d MB", api.MaxImageUpload>>20)
			}
			ref, err := c.h.b.SaveImageData(ctx, blob)
			return imageResult{Ref: ref}, nil, "", err
		}},
		"readImage": {run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[imageParams](p)
			if err != nil {
				return nil, nil, "", err
			}
			if len(a.Ref) == 0 || len(a.Ref) > 4096 || a.MaxSide < 0 || a.MaxSide > maxPreview {
				return nil, nil, "", invalid("that isn't an image")
			}
			data, err := c.h.b.ReadImage(ctx, a.Ref, a.MaxSide)
			if err != nil {
				return nil, nil, "", err
			}
			return nil, data, "", nil
		}},
		"interrupt": thread(true, func(ctx context.Context, b api.Backend, id string) error { return b.Interrupt(ctx, id) }),
		"answer": {write: true, run: func(_ context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[answerParams](p)
			if err == nil {
				err = checkID("thread", a.Thread)
			}
			if err == nil {
				err = checkID("question", a.ToolUse)
			}
			if err != nil {
				return nil, nil, "", err
			}
			if len(a.Answers) > maxAnswers {
				return nil, nil, "", invalid("too many answers")
			}
			for q, v := range a.Answers {
				if len(q) > maxAnswerText || len(v) > maxAnswerText {
					return nil, nil, "", invalid("an answer is too long")
				}
			}
			return nil, nil, a.Thread, c.h.b.Answer(a.Thread, a.ToolUse, a.Answers)
		}},
		"dismiss": {write: true, run: func(_ context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[answerParams](p)
			if err == nil {
				err = checkID("thread", a.Thread)
			}
			if err == nil {
				err = checkID("question", a.ToolUse)
			}
			if err != nil {
				return nil, nil, "", err
			}
			return nil, nil, a.Thread, c.h.b.Dismiss(a.Thread, a.ToolUse)
		}},

		"changes": {run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[threadParams](p)
			if err == nil {
				err = checkID("thread", a.Thread)
			}
			if err != nil {
				return nil, nil, "", err
			}
			files, err := c.h.b.Changes(ctx, a.Thread)
			if len(files) > maxFiles {
				files = files[:maxFiles]
			}
			if files == nil {
				files = []git.FileChange{}
			}
			return files, nil, "", err
		}},
		"diff": {run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[threadParams](p)
			if err == nil {
				err = checkID("thread", a.Thread)
			}
			if err != nil {
				return nil, nil, "", err
			}
			diff, err := c.h.b.Diff(ctx, a.Thread)
			if err != nil {
				return nil, nil, "", err
			}
			res := diffResult{Diff: diff}
			if len(diff) > maxDiff {
				cut := strings.LastIndexByte(diff[:maxDiff], '\n') + 1
				res = diffResult{Diff: diff[:cut], Truncated: true}
			}
			return res, nil, "", nil
		}},
		"merge":      commit(func(ctx context.Context, b api.Backend, id string) (string, error) { return b.Merge(ctx, id) }),
		"mergeLocal": commit(func(ctx context.Context, b api.Backend, id string) (string, error) { return b.MergeLocal(ctx, id) }),
		"mergeRepo": {write: true, run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[repoParams](p)
			if err == nil {
				err = checkID("thread", a.Thread)
			}
			if err == nil {
				err = checkPath("repository", a.Repo)
			}
			if err != nil {
				return nil, nil, "", err
			}
			text, err := c.h.b.MergeRepo(ctx, a.Thread, a.Repo)
			return textResult{Text: text}, nil, a.Thread, err
		}},
		"openPR": {write: true, run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[repoParams](p)
			if err == nil {
				err = checkID("thread", a.Thread)
			}
			if err == nil && a.Repo != "" {
				err = checkPath("repository", a.Repo)
			}
			if err != nil {
				return nil, nil, "", err
			}
			prs, err := c.h.b.OpenPRFor(ctx, a.Thread, a.Repo, a.Draft)
			if prs == nil {
				prs = []forge.PR{}
			}
			return prs, nil, a.Thread, err
		}},
		"markReady": thread(true, func(ctx context.Context, b api.Backend, id string) error { return b.MarkReady(ctx, id) }),
		"setModel": {write: true, run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[modelParams](p)
			if err == nil {
				err = checkID("thread", a.Thread)
			}
			if err == nil && (len(a.Model) > 256 || len(a.Effort) > 256) {
				err = invalid("that isn't a model")
			}
			if err != nil {
				return nil, nil, "", err
			}
			return nil, nil, a.Thread, c.h.b.SetThreadModel(ctx, a.Thread, a.Model, a.Effort)
		}},
		"search": {run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[searchParams](p)
			if err != nil {
				return nil, nil, "", err
			}
			if len(a.Query) > maxQuery {
				return nil, nil, "", invalid("the search is too long")
			}
			hits, err := c.h.b.SearchMessages(ctx, a.Query, min(max(a.Limit, 1), maxHits))
			if hits == nil {
				hits = []core.SearchHit{}
			}
			return hits, nil, "", err
		}},
		"discard": thread(true, func(ctx context.Context, b api.Backend, id string) error { return b.Discard(ctx, id) }),
		"done":    thread(true, func(ctx context.Context, b api.Backend, id string) error { return b.Done(ctx, id) }),
		"restore": thread(true, func(ctx context.Context, b api.Backend, id string) error { return b.Restore(ctx, id) }),

		"updateSettings": {write: true, run: func(_ context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[settingsParams](p)
			if err != nil {
				return nil, nil, "", err
			}
			isDir := c.h.opts.IsDir
			if isDir == nil {
				isDir = func(string) bool { return false }
			}
			if err := a.Patch.CheckRemote(isDir); err != nil {
				return nil, nil, "", denied("%s", err.Error())
			}
			names := make([]string, 0, len(a.Patch))
			for name := range a.Patch {
				names = append(names, name)
			}
			sort.Strings(names) // the audit log names them in one order
			var applyErr error
			err = c.h.b.UpdateSettings(func(s *core.Settings) { applyErr = a.Patch.Apply(s) })
			if err == nil {
				err = applyErr
			}
			if err != nil {
				return nil, nil, strings.Join(names, ","), err
			}
			return c.h.b.Settings(), nil, strings.Join(names, ","), nil
		}},
		"initRepo": {write: true, run: func(ctx context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[folderParams](p)
			if err == nil {
				err = checkPath("folder", a.Folder)
			}
			if err != nil {
				return nil, nil, "", err
			}
			// Only a folder the machine itself found outside git, named as the machine names it.
			known := false
			for _, f := range c.h.b.PlainFolders() {
				known = known || f == a.Folder
			}
			if !known {
				return nil, nil, "", denied("That isn't one of the folders Solo found outside git.")
			}
			path, err := c.h.b.InitRepo(ctx, a.Folder)
			return textResult{Text: path}, nil, a.Folder, err
		}},
		// The path a device gives is never run: the check is of the Claude the machine is set to use.
		// Each check starts Claude Code, so devices share one: a check is made at most every claudeFor,
		// one at a time, and a device that may only look can't start programs without end.
		"checkClaude": {run: func(_ context.Context, c *conn, _ json.RawMessage, _ []byte) (any, []byte, string, error) {
			h := c.h
			h.claudeMu.Lock()
			defer h.claudeMu.Unlock()
			if h.claudeAt.IsZero() || time.Since(h.claudeAt) > claudeFor {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				h.claude = h.b.CheckClaude(ctx, h.b.Settings().ClaudePath)
				cancel()
				h.claudeAt = time.Now()
			}
			return h.claude, nil, "", nil
		}},
		"completeTodo": {write: true, run: func(_ context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[todoParams](p)
			if err == nil {
				err = checkID("todo", a.ID)
			}
			if err != nil {
				return nil, nil, "", err
			}
			return nil, nil, a.ID, c.h.b.CompleteTodo(a.ID)
		}},
		"updateTodo": {write: true, run: func(_ context.Context, c *conn, p json.RawMessage, _ []byte) (any, []byte, string, error) {
			a, err := decode[todoParams](p)
			if err == nil {
				err = checkID("todo", a.ID)
			}
			if err != nil {
				return nil, nil, "", err
			}
			ch := a.Change
			if ch.Text != nil && len(*ch.Text) > maxAnswerText {
				return nil, nil, "", invalid("the to-do is too long")
			}
			if ch.Threads != nil && len(*ch.Threads) > 64 {
				return nil, nil, "", invalid("too many threads for a to-do")
			}
			ch.By = core.TodoByUser // a device is the user's
			todo, err := c.h.b.UpdateTodo(a.ID, ch)
			return todo, nil, a.ID, err
		}},
	}
}
