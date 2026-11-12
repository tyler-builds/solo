//go:build !js

package core

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// SearchMessages finds messages, the user's, Solo's and the threads', whose text contains every word
// of query, ignoring case; newest first, at most limit.
func (c *Core) SearchMessages(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil, nil
	}
	// The database matches the JSON; ask for more and keep the ones whose text matches.
	items, err := c.db.SearchItems(ctx, []string{ItemUser, ItemAssistant}, words, limit*3)
	if err != nil {
		return nil, err
	}
	var out []SearchHit
	for _, it := range items {
		var d struct {
			Text      string `json:"text"`
			Synthetic bool   `json:"synthetic"`
		}
		if json.Unmarshal(it.Data, &d) != nil || d.Synthetic {
			continue
		}
		lower := strings.ToLower(d.Text)
		ok := true
		for _, w := range words {
			ok = ok && strings.Contains(lower, w)
		}
		if !ok {
			continue
		}
		out = append(out, SearchHit{ThreadID: it.ThreadID, ItemID: it.ID, Kind: it.Kind,
			Snippet: snippet(d.Text, strings.Index(lower, words[0]), len(words[0])), At: it.CreatedAt})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// snippet is about 120 characters of text around [at, at+n), on one line, with "…" where it was cut.
func snippet(text string, at, n int) string {
	const before, after = 40, 80
	start, end := max(at-before, 0), min(at+n+after, len(text))
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}
