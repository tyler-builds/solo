package gui

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
)

func TestFindTrigger(t *testing.T) {
	for _, c := range []struct {
		text  string
		caret int
		want  trigger
	}{
		{"/rev", 4, trigger{'/', 0, "rev"}},
		{"please /rev now", 11, trigger{'/', 7, "rev"}},
		{"please /rev now", 9, trigger{'/', 7, "r"}},
		{"see @internal/au", 16, trigger{'@', 4, "internal/au"}},
		{"line\n@", 6, trigger{'@', 5, ""}},
		{"/usr/bin", 4, trigger{}},    // a path, not a command
		{"mail a@b.c", 10, trigger{}}, // "@" inside a word
		{"/rev", -1, trigger{}},       // the caret's place isn't known
		{"/rev ", 5, trigger{}},       // past the word
		{"x/y", 3, trigger{}},         // "/" inside a word
		{"/日本", len("/日本"), trigger{'/', 0, "日本"}},
	} {
		if got := findTrigger(c.text, c.caret); got != c.want {
			t.Errorf("findTrigger(%q, %d) = %+v, want %+v", c.text, c.caret, got, c.want)
		}
	}
}

func TestEditCaret(t *testing.T) {
	for _, c := range []struct {
		old, new  string
		prev, out int
	}{
		{"", "a", 0, 1},
		{"ab", "aXb", 1, 2},             // typed at the caret
		{"aa", "aaa", 0, 1},             // typed at the start: the known caret settles which "a" is new
		{"aa", "aaa", -1, 3},            // not known: the end of the change
		{"abc", "ac", 2, 1},             // Backspace
		{"abc", "ac", 1, 1},             // Delete
		{"hello", "hello there", 5, 11}, // a paste
		{"fix it", "/fix it", -1, 1},
	} {
		if got := editCaret(c.old, c.new, c.prev); got != c.out {
			t.Errorf("editCaret(%q, %q, %d) = %d, want %d", c.old, c.new, c.prev, got, c.out)
		}
	}
}

func TestWrapLines(t *testing.T) {
	measure := func(s string) float32 { return float32(len(s)) } // one unit a byte
	got := wrapLines("aaa bbb ccc\nd", 7, measure)               // "aaa bbb" fits in 7
	want := [][2]int{{0, 8}, {8, 11}, {12, 13}}
	if !slices.Equal(got, want) {
		t.Errorf("wrapLines = %v, want %v", got, want)
	}
	got = wrapLines("aaa bbb ccc\nd", 6, measure)
	want = [][2]int{{0, 4}, {4, 8}, {8, 11}, {12, 13}}
	if !slices.Equal(got, want) {
		t.Errorf("wrapLines = %v, want %v", got, want)
	}
}

// menuShows reports whether the composer's list shows a row named label.
func menuShows(u *uitest.UI, label string) bool { return u.HasRole(access.RoleListItem, label) }

// Typing "/" lists the skills and commands; Enter types the one picked in, with the caret after it,
// and the message goes as typed. Both composers do it.
func TestSlashMenu(t *testing.T) {
	for _, tc := range []struct{ name, key, thread string }{
		{"Solo", "orch-draft-area", ""},
		{"thread", "draft-a1f3c9d2-oauth-area", "a1f3c9d2-oauth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := demoBackend()
			a, u := newUI(t, f)
			if tc.thread != "" {
				a.openThread(tc.thread, "conversation")
				u.Frame()
			}
			area := u.Get(tc.key)
			area.Type("Please /rev")
			u.WaitFor("the list", func() bool { return menuShows(u, "/review") })
			if menuShows(u, "/pr-style") {
				t.Error("the list shows a skill that doesn't match")
			}
			u.Press("enter")
			u.Frame()
			if menuShows(u, "/review") {
				t.Error("the list stays open after picking")
			}
			u.Type("now")
			if got := u.Get(tc.key).Value(); got != "Please /review now" {
				t.Fatalf("draft %q", got)
			}
			u.Press("enter")
			waitSent(t, f, "Please /review now")
		})
	}
}

// Arrow keys move through the list, Tab picks, and a word picked in the middle of the text leaves the
// caret after it.
func TestSlashMenuKeys(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	area := u.Get("orch-draft-area")
	area.Type("/")
	u.WaitFor("the list", func() bool { return menuShows(u, "/review") && menuShows(u, "/release-notes") })
	u.Press("down")
	u.Press("tab")
	if got := area.Value(); got != "/release-notes " {
		t.Fatalf("after Down and Tab: %q", got)
	}

	// Escape closes the list, and Enter then sends.
	area.Type("and /zz")
	u.Frame()
	u.Press("backspace")
	u.Press("backspace")
	area.Type("pdf") // matches the organization's pdf skill
	u.WaitFor("the list", func() bool { return menuShows(u, "/anthropic-skills:pdf") })
	u.Press("escape")
	u.Frame()
	if menuShows(u, "/anthropic-skills:pdf") {
		t.Fatal("Escape left the list open")
	}
	area.Type("x") // the same word: the list stays closed
	if menuShows(u, "/anthropic-skills:pdf") || menuShows(u, "/review") {
		t.Fatal("the list opened again for the word Escape closed it on")
	}
	if !area.Focused() {
		t.Fatalf("Escape moved focus to %s", focusedName(u))
	}
}

// A word picked in the middle of the text is typed in where the caret is, which stays after it.
func TestSlashMenuMidText(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	area := u.Get("orch-draft-area")
	area.Type("then ship it")
	u.Press("home")
	u.Type("/pr-s")
	u.WaitFor("the list", func() bool { return menuShows(u, "/pr-style") })
	u.Press("enter")
	u.Type("X")
	if got := area.Value(); got != "/pr-style Xthen ship it" {
		t.Fatalf("draft %q", got)
	}
}

// A click in the text moves the caret, so the list closes until the next edit.
func TestMenuClosesOnClick(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	area := u.Get("orch-draft-area")
	area.Type("/rev")
	u.WaitFor("the list", func() bool { return menuShows(u, "/review") })
	area.Click()
	u.Frame()
	if menuShows(u, "/review") {
		t.Fatal("the list stays open after a click in the text")
	}
	area.Type("i")
	u.WaitFor("the list again", func() bool { return menuShows(u, "/review") })
}

// "@" in a thread lists its files and folders; in Solo's conversation, Solo's repositories.
func TestAtMenu(t *testing.T) {
	t.Run("thread", func(t *testing.T) {
		f := demoBackend()
		a, u := newUI(t, f)
		a.openThread("a1f3c9d2-oauth", "conversation")
		u.Frame()
		area := u.Get("draft-a1f3c9d2-oauth-area")
		area.Type("look at @tok")
		u.WaitFor("the files", func() bool { return menuShows(u, "token.go") && menuShows(u, "token_test.go") })
		u.Press("enter")
		if got := area.Value(); got != "look at @internal/auth/token.go " {
			t.Fatalf("draft %q", got)
		}
		area.Type("@inte")
		u.WaitFor("the folder", func() bool { return menuShows(u, "internal/") })
		u.Press("enter")
		if got := area.Value(); got != "look at @internal/auth/token.go @internal/ " {
			t.Fatalf("draft %q", got)
		}
	})
	t.Run("Solo", func(t *testing.T) {
		f := demoBackend()
		_, u := newUI(t, f)
		area := u.Get("orch-draft-area")
		area.Type("compare @inf")
		u.WaitFor("the repositories", func() bool { return menuShows(u, "infra") })
		u.Press("enter")
		want := "compare @" + repoMention("/home/dev/code/infra") + " "
		if got := area.Value(); got != want {
			t.Fatalf("draft %q, want %q", got, want)
		}
	})
}

// Up in an empty composer brings back the messages sent there, newest first; Down goes back.
func TestPromptHistory(t *testing.T) {
	f := demoBackend()
	f.items[core.OrchestratorID] = nil
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "first"})
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "a thread finished", Synthetic: true})
	f.add(core.OrchestratorID, core.ItemUser, core.UserData{Text: "second"})
	_, u := newUI(t, f)
	area := u.Get("orch-draft-area")
	area.Focus()
	for _, step := range []struct{ key, want string }{
		{"up", "second"}, {"up", "first"}, {"up", "first"}, {"down", "second"}, {"down", ""},
	} {
		u.Press(step.key)
		if got := area.Value(); got != step.want {
			t.Fatalf("after %s: %q, want %q", step.key, got, step.want)
		}
	}
	// A recalled message, once edited, is the user's: Up moves in it rather than replacing it.
	u.Press("up")
	u.Type("!")
	u.Press("up")
	if got := area.Value(); got != "second!" {
		t.Fatalf("an edited message was replaced: %q", got)
	}
}

// A file or folder dropped on the composer is mentioned by its path; an image is still attached.
func TestDropMentionsFiles(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.openThread("a1f3c9d2-oauth", "conversation")
	// The drop goes where the composer is: wait until the panel has slid in, and it is all on screen.
	u.WaitFor("the panel to slide in", func() bool { return a.panelAnim.Peek() == 1 })
	u.Settle()
	th, _ := a.thread("a1f3c9d2-oauth")
	dir := th.WorktreePath
	if dir == "" {
		dir = th.RepoPath
	}
	area := u.Get("draft-a1f3c9d2-oauth-area")
	area.Type("read")
	b := area.Bounds()
	outside := filepath.Join(t.TempDir(), "spec notes.md")
	u.Runtime().DropFiles(b.X+b.W/2, b.Y+b.H/2, []string{filepath.Join(dir, "docs", "api.md"), outside})
	u.Frame()
	want := `read @docs/api.md @` + repoMention(outside) + " "
	if got := area.Value(); got != want {
		t.Fatalf("draft %q, want %q", got, want)
	}
	if !strings.Contains(want, `"`) {
		t.Fatalf("a path with a space isn't quoted: %s", want)
	}
	u.Type("first")
	if got := area.Value(); got != want+"first" {
		t.Fatalf("the caret isn't after the mentions: %q", got)
	}
}

// Settings' pull request page picks skills, saves them, and says when one is gone.
func TestPRSkillsPicker(t *testing.T) {
	f := demoBackend()
	f.settings.PRSkills = []string{"gone-skill"}
	a, u := newUI(t, f)
	a.settingsTab = "prs"
	a.settings.Set(true)
	u.Frame()
	u.WaitFor("the skills", func() bool { return u.HasRole(access.RoleButton, "Add a skill") })
	if !u.HasName("Not found: pull requests skip it.") {
		t.Error("a missing skill isn't marked")
	}
	u.ByRole(access.RoleButton, "Add a skill").Click()
	u.Frame()
	if u.HasRole(access.RoleMenuItem, "release-notes") {
		t.Error("the menu offers a command")
	}
	u.ByRole(access.RoleMenuItem, "pr-style").Click()
	u.WaitFor("the skill to be saved", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Equal(f.settings.PRSkills, []string{"gone-skill", "pr-style"})
	})
	u.ByRole(access.RoleButton, "Stop following gone-skill").Click()
	u.WaitFor("the removal to be saved", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Equal(f.settings.PRSkills, []string{"pr-style"})
	})
}
