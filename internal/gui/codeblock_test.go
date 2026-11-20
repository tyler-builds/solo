package gui

import (
	"testing"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

const longCode = "```\nLinux has great apps, scattered across Flathub, Homebrew, Snap, GitHub releases, AppImages and " +
	"your distro's repos. Finding the right one is a chore, so I built one app store for every source.\n```"

// A code block with a line longer than the chat is wide stays inside the chat and scrolls sideways to
// the rest of the line, in Solo's message and in the user's bubble, which keeps to its column.
func TestLongCodeLineScrolls(t *testing.T) {
	f := demoBackend()
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "Shorten this:\n\n" + longCode})
	f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: "Here it is:\n\n" + longCode})
	_, u := newUIWith(t, f, func(a *App) { a.items = map[string]*ui.Signal[[]core.Item]{} })
	u.Settle()
	var blocks []*access.Node
	u.Tree().Walk(func(n *access.Node) bool {
		if n.Role == access.RoleGroup && n.Name == "Code" {
			blocks = append(blocks, n)
		}
		return true
	})
	if len(blocks) < 2 {
		t.Fatalf("found %d code blocks; want the user's and Solo's", len(blocks))
	}
	chat := u.Get("orch-transcript").Bounds()
	for _, b := range blocks[len(blocks)-2:] {
		if b.Bounds.X < chat.X || b.Bounds.X+b.Bounds.W > chat.Right() {
			t.Errorf("a code block at %v runs out of the chat at %v", b.Bounds, chat)
		}
		if !b.Scroll.CanX {
			t.Errorf("the code block at %v can't scroll sideways to the rest of its line", b.Bounds)
		}
	}
}

// Each code block in a message has a copy button, which copies the block's code alone: in Solo's
// reply (several lines, one line, with a language, inside a list) and in the user's bubble. Inline code
// has none.
func TestCodeBlockCopyButton(t *testing.T) {
	f := demoBackend()
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "Why does this fail?\n\n```\nmake: *** No rule to make target 'all'.\n```"})
	f.add(core.OrchestratorID, core.ItemAssistant, core.TextData{Text: shotCode})
	_, u := newUIWith(t, f, func(a *App) { a.items = map[string]*ui.Signal[[]core.Item]{} })
	u.Settle()
	want := []string{
		"make: *** No rule to make target 'all'.",
		"SOLO_SHOTS=/tmp/shots go test ./internal/gui -run TestShots",
		"func (a *App) md(ctx *ui.Context, text string) ui.Element {\n\treturn markdown.View(text).Code(code).CopyCode(true)\n}",
		"go install ./cmd/solo",
		"shots: wrote 64 files to /tmp/shots (dark-main.png, dark-thread.png, dark-changes.png, dark-split.png, light-main.png, light-changes.png and 58 more)",
	}
	var got []string
	for i := 0; ; i++ {
		b := codeCopyButton(u, i)
		if b == nil {
			break
		}
		u.SetClipboard("")
		u.ClickAt(b.Bounds.X+b.Bounds.W/2, b.Bounds.Y+b.Bounds.H/2)
		got = append(got, u.Clipboard())
	}
	if len(got) < len(want) {
		t.Fatalf("%d code blocks with a copy button; want at least %d", len(got), len(want))
	}
	got = got[len(got)-len(want):] // the demo's earlier messages may have code too
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("button %d copied %q; want %q", i, got[i], want[i])
		}
	}
}
