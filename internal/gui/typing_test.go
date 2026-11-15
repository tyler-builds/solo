package gui

import "testing"

// A key typed to Solo re-renders the composer, not the conversation above it: the conversation's
// rows render their markdown, which on a phone's web page took most of each key's frame.
func TestTypingRendersOnlyTheComposer(t *testing.T) {
	_, u := newUI(t, demoBackend())
	area := u.Get("orch-draft-area")
	area.Type("h") // the first character folds the pinned question, which re-renders around it
	u.Frame()
	whole := 0
	for _, c := range "ello" {
		u.Type(string(c))
		whole = max(whole, u.Runtime().Stats().Rendered)
	}
	t.Logf("components rendered for a key: %d", whole)
	if whole > 12 {
		t.Errorf("a key typed to Solo rendered %d components; want the composer's few", whole)
	}
}
