package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyler-builds/solo/internal/store"
)

// Memory is one thing Solo remembers across conversations: a fact, decision or preference.
type Memory = store.Memory

// MaxMemoryLen is the longest memory Solo may save, in characters: one fact, not a document.
const MaxMemoryLen = 400

// Remember saves a memory, replacing the memories in replaces (they are kept, but no longer shown or
// found). repo names the repository it is about, or "" when it holds everywhere. It returns the new
// memory and up to two current ones that look alike, so a near duplicate can be replaced.
func (c *Core) Remember(ctx context.Context, text, keywords, repo string, replaces []int64) (Memory, []Memory, error) {
	text = strings.Join(strings.Fields(text), " ")
	switch {
	case text == "":
		return Memory{}, nil, errors.New("a memory needs text")
	case len(text) > MaxMemoryLen:
		return Memory{}, nil, fmt.Errorf("a memory is one fact in at most %d characters; this is %d. Split it or say it shorter", MaxMemoryLen, len(text))
	}
	if repo = strings.TrimSpace(repo); repo != "" {
		if p, err := c.ResolveRepo(repo); err == nil {
			repo = filepath.Base(p)
		}
	}
	m := Memory{Text: text, Keywords: strings.Join(strings.Fields(keywords), " "), Repo: repo}
	if err := c.db.AddMemory(&m, replaces...); err != nil {
		return Memory{}, nil, err
	}
	var similar []Memory
	if found, err := c.db.SearchMemories(ctx, text+" "+m.Keywords, 3); err == nil {
		for _, f := range found {
			if f.ID != m.ID && len(similar) < 2 && overlap(f.Text, text) >= 0.5 {
				similar = append(similar, f)
			}
		}
	}
	return m, similar, nil
}

// overlap is the share of a's search terms that b also has.
func overlap(a, b string) float64 {
	ta, tb := terms(a), terms(b)
	if len(ta) == 0 {
		return 0
	}
	n := 0
	for w := range ta {
		if tb[w] {
			n++
		}
	}
	return float64(n) / float64(len(ta))
}

func terms(s string) map[string]bool {
	q, err := store.MatchQuery(s)
	out := map[string]bool{}
	if err != nil {
		return out
	}
	for _, t := range strings.Split(q, " OR ") {
		out[strings.Trim(t, `"`)] = true
	}
	return out
}

// Forget deletes a memory for good.
func (c *Core) Forget(id int64) error { return c.db.DeleteMemory(id) }

// Memories returns the current memories, newest first.
func (c *Core) Memories(ctx context.Context) ([]Memory, error) { return c.db.Memories(ctx) }

// Recalled is what a recall found: memories, earlier messages with Solo, and threads.
type Recalled struct {
	Memories []Memory
	Messages []store.MessageHit
	Threads  []store.ThreadHit
}

// How many of each a recall returns: few enough to read at a glance, which keeps Solo's turns short.
const (
	recallMemories = 6
	recallMessages = 4
	recallThreads  = 3
)

// Recall searches memories, Solo's earlier conversation and the threads with each query, and merges
// the rankings (reciprocal rank fusion), so what several phrasings agree on comes first.
func (c *Core) Recall(ctx context.Context, queries []string) (Recalled, error) {
	var out Recalled
	var mems [][]Memory
	var msgs [][]store.MessageHit
	var ths [][]store.ThreadHit
	searched := false
	for _, q := range queries {
		m, err := c.db.SearchMemories(ctx, q, 10)
		if errors.Is(err, store.ErrNoTerms) {
			continue
		}
		if err != nil {
			return out, err
		}
		searched = true
		mems = append(mems, m)
		if h, err := c.db.SearchMessages(ctx, OrchestratorID, q, 8); err == nil {
			msgs = append(msgs, h)
		}
		if h, err := c.db.SearchThreads(ctx, q, 6); err == nil {
			ths = append(ths, h)
		}
	}
	if !searched {
		return out, errors.New("the queries have no words to search for; use the names and terms the memory would contain")
	}
	out.Memories = fuse(mems, func(m Memory) int64 { return m.ID }, recallMemories)
	out.Messages = fuse(msgs, func(h store.MessageHit) int64 { return h.ItemID }, recallMessages)
	out.Threads = fuse(ths, func(h store.ThreadHit) string { return h.ThreadID }, recallThreads)
	return out, nil
}

// fuse merges ranked lists by reciprocal rank and keeps the best n.
func fuse[T any, K comparable](lists [][]T, key func(T) K, n int) []T {
	score := map[K]float64{}
	first := map[K]T{}
	var order []K
	for _, l := range lists {
		for rank, x := range l {
			k := key(x)
			if _, ok := first[k]; !ok {
				first[k] = x
				order = append(order, k)
			}
			score[k] += 1 / float64(60+rank)
		}
	}
	slices.SortStableFunc(order, func(a, b K) int {
		switch {
		case score[a] > score[b]:
			return -1
		case score[a] < score[b]:
			return 1
		}
		return 0
	})
	out := make([]T, 0, min(n, len(order)))
	for _, k := range order[:min(n, len(order))] {
		out = append(out, first[k])
	}
	return out
}

// SetState saves a small value the app keeps between runs, such as a recap waiting for Solo's next
// message. A nil v clears it.
func (c *Core) SetState(key string, v any) error { return c.db.SetSetting("state."+key, v) }

// State reads a value saved with SetState into v, reporting whether there was one.
func (c *Core) State(key string, v any) bool {
	ok, err := c.db.GetSetting("state."+key, v)
	return ok && err == nil
}
