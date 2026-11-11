package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/forge"
)

// Todo is one entry of the user's to-do list, which Solo keeps as they talk: an outcome the user
// wants, not a thread. Most have no thread, and Solo ticks them off when it sees they're done. Solo
// links threads only when their work finishes the todo; it is then done once that work lands: a pull
// request opened or merged, a local merge, or the user marking the thread done.
type Todo struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Threads   []string  `json:"threads,omitempty"` // threads whose work landing finishes it; most todos have none
	CreatedAt time.Time `json:"created_at"`
	// DoneAt is when it was completed, and DoneBy who completed it: TodoByUser, TodoBySolo or
	// TodoByThread (its threads landed).
	DoneAt time.Time `json:"done_at,omitzero"`
	DoneBy string    `json:"done_by,omitempty"`
}

// Who completed a todo (Todo.DoneBy).
const (
	TodoByUser   = "user"
	TodoBySolo   = "solo"
	TodoByThread = "thread"
)

// Done reports whether the todo is completed.
func (t Todo) Done() bool { return !t.DoneAt.IsZero() }

// keepDone is how many completed todos are kept, newest first, so Solo can see what was done lately.
const keepDone = 30

// todosKey is the setting the list is saved under.
const todosKey = "todos"

// TodoChange changes a todo; nil fields stay as they are.
type TodoChange struct {
	Text    *string
	Threads *[]string // replaces the linked threads
	Done    *bool
	By      string // who completes it, when Done is true; default TodoBySolo
}

func (c *Core) loadTodos() {
	if _, err := c.db.GetSetting(todosKey, &c.todos); err != nil {
		c.log.Warn("core: todos unreadable", "err", err)
		c.todos = nil
	}
}

// saveTodosLocked trims old completed todos and saves the list. Call with c.mu held.
func (c *Core) saveTodosLocked() error {
	done := 0
	kept := make([]Todo, 0, len(c.todos))
	for i := len(c.todos) - 1; i >= 0; i-- { // newest last, so count from the end
		if c.todos[i].Done() {
			done++
			if done > keepDone {
				continue
			}
		}
		kept = append(kept, c.todos[i])
	}
	slices.Reverse(kept)
	c.todos = kept
	return c.db.SetSetting(todosKey, c.todos)
}

// Todos returns the to-do list, oldest first: the open todos, and with done the recently completed too.
func (c *Core) Todos(done bool) []Todo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Todo, 0, len(c.todos))
	for _, t := range c.todos {
		if done || !t.Done() {
			t.Threads = slices.Clone(t.Threads)
			out = append(out, t)
		}
	}
	return out
}

// AddTodo adds a todo, linked to the threads (IDs or short IDs) doing it.
func (c *Core) AddTodo(text string, threads []string) (Todo, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Todo{}, fmt.Errorf("a todo needs text")
	}
	ids, err := c.resolveThreads(threads)
	if err != nil {
		return Todo{}, err
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	td := Todo{ID: hex.EncodeToString(b), Text: text, Threads: ids, CreatedAt: time.Now()}
	c.mu.Lock()
	c.todos = append(c.todos, td)
	err = c.saveTodosLocked()
	c.mu.Unlock()
	if err != nil {
		return Todo{}, err
	}
	c.notify(Change{Kind: ChangeTodos})
	c.checkTodos() // linked to work that already landed
	return td, nil
}

// UpdateTodo changes a todo. Completing one that is done, or reopening one that is open, does nothing.
func (c *Core) UpdateTodo(id string, ch TodoChange) (Todo, error) {
	var ids []string
	if ch.Threads != nil {
		var err error
		if ids, err = c.resolveThreads(*ch.Threads); err != nil {
			return Todo{}, err
		}
	}
	c.mu.Lock()
	i := slices.IndexFunc(c.todos, func(t Todo) bool { return t.ID == id })
	if i < 0 {
		c.mu.Unlock()
		return Todo{}, fmt.Errorf("no todo %q", id)
	}
	td := &c.todos[i]
	if ch.Text != nil && strings.TrimSpace(*ch.Text) != "" {
		td.Text = strings.TrimSpace(*ch.Text)
	}
	if ch.Threads != nil {
		td.Threads = ids
	}
	if ch.Done != nil && *ch.Done != td.Done() {
		if *ch.Done {
			td.DoneAt, td.DoneBy = time.Now(), ch.By
			if td.DoneBy == "" {
				td.DoneBy = TodoBySolo
			}
		} else {
			td.DoneAt, td.DoneBy = time.Time{}, ""
		}
	}
	out := *td
	err := c.saveTodosLocked()
	c.mu.Unlock()
	if err != nil {
		return Todo{}, err
	}
	c.notify(Change{Kind: ChangeTodos})
	if ch.Threads != nil {
		c.checkTodos()
	}
	return out, nil
}

// CompleteTodo marks a todo done by the user.
func (c *Core) CompleteTodo(id string) error {
	done := true
	_, err := c.UpdateTodo(id, TodoChange{Done: &done, By: TodoByUser})
	return err
}

// RemoveTodo deletes a todo that is no longer wanted.
func (c *Core) RemoveTodo(id string) error {
	c.mu.Lock()
	n := len(c.todos)
	c.todos = slices.DeleteFunc(c.todos, func(t Todo) bool { return t.ID == id })
	if len(c.todos) == n {
		c.mu.Unlock()
		return fmt.Errorf("no todo %q", id)
	}
	err := c.saveTodosLocked()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	c.notify(Change{Kind: ChangeTodos})
	return nil
}

// resolveThreads turns thread IDs or short IDs into full worker IDs.
func (c *Core) resolveThreads(refs []string) ([]string, error) {
	var ids []string
	for _, r := range refs {
		v, err := c.FindThread(r)
		if err != nil {
			return nil, err
		}
		if !v.IsWorker {
			return nil, fmt.Errorf("%s is not a worker thread", r)
		}
		if !slices.Contains(ids, v.ID) {
			ids = append(ids, v.ID)
		}
	}
	return ids, nil
}

// Landed reports whether a thread's work is in: merged, a pull request opened or merged, or marked
// done by the user.
func Landed(v ThreadView) bool {
	if v.Status == StatusMerged || v.Status == StatusArchived || !v.DoneAt.IsZero() {
		return true
	}
	for _, p := range v.Parts {
		if p.PR != nil && (p.PR.State == forge.StateOpen || p.PR.State == forge.StateMerged) {
			return true
		}
	}
	return false
}

// checkTodos completes the open todos whose threads have all landed. Discarded threads don't count,
// so a todo whose only thread was discarded stays open for another try.
func (c *Core) checkTodos() {
	c.mu.Lock()
	changed := false
	for i := range c.todos {
		td := &c.todos[i]
		if td.Done() || len(td.Threads) == 0 {
			continue
		}
		landed, pending := 0, 0
		for _, id := range td.Threads {
			t := c.threads[id]
			if t == nil {
				continue
			}
			switch v := c.viewLocked(t); {
			case v.Status == StatusDiscarded:
			case Landed(v):
				landed++
			default:
				pending++
			}
		}
		if landed > 0 && pending == 0 {
			td.DoneAt, td.DoneBy = time.Now(), TodoByThread
			changed = true
		}
	}
	var err error
	if changed {
		err = c.saveTodosLocked()
	}
	c.mu.Unlock()
	if err != nil {
		c.log.Warn("core: saving todos", "err", err)
	}
	if changed {
		c.notify(Change{Kind: ChangeTodos})
	}
}
