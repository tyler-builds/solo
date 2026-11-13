package gui

import (
	"testing"

	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"
)

// newPhoneUI is the UI on a phone: a narrow window, used with fingers.
func newPhoneUI(t *testing.T, f *fakeBackend) (*App, *uitest.UI) {
	t.Helper()
	_, dark := Themes()
	a := New(f)
	a.Touch = true
	u := uitest.New(t, a.Root, uitest.Size(390, 844), uitest.Theme(dark),
		uitest.Settings(ui.SystemSettings{Dark: true, CaptionButtons: ":", ReducedMotion: true, CaretWidth: 1}))
	t.Cleanup(u.Close)
	started(a, u)
	u.Settle()
	return a, u
}

// A compact window shows one conversation, Solo's or the open thread's, and keeps the threads in a
// drawer, which closes when a thread is picked from it.
func TestCompactWindowShowsOneConversation(t *testing.T) {
	const thread = "c3d4e5f6-forms"
	a, u := newPhoneUI(t, demoBackend())
	if !u.Has("orch-draft-area") || u.Has("rail-filter") {
		t.Fatalf("a compact window opens on Solo's conversation alone: composer %v, rail %v",
			u.Has("orch-draft-area"), u.Has("rail-filter"))
	}
	u.Click("open-drawer")
	u.Settle()
	if !a.drawer.Peek() || !u.Has("rail-filter") || !u.Has("card-"+thread) {
		t.Fatalf("the drawer lists the threads: open %v, rail %v, the thread's row %v",
			a.drawer.Peek(), u.Has("rail-filter"), u.Has("card-"+thread))
	}
	u.Click("card-" + thread)
	u.Settle()
	if a.drawer.Peek() || !u.Has("thread-panel-"+thread) || u.Has("rail-filter") || u.Has("orch-draft-area") {
		t.Fatalf("a thread picked in the drawer fills the window: drawer %v, panel %v, rail %v, Solo's composer %v",
			a.drawer.Peek(), u.Has("thread-panel-"+thread), u.Has("rail-filter"), u.Has("orch-draft-area"))
	}
	// Closing the thread shows Solo again at once, without the desktop's slide.
	u.Click("thread-close")
	u.Frame()
	if a.openID.Peek() != "" || u.Has("thread-panel-"+thread) || !u.Has("orch-draft-area") {
		t.Fatalf("Close left the thread %q: panel %v, Solo's composer %v", a.openID.Peek(), u.Has("thread-panel-"+thread), u.Has("orch-draft-area"))
	}
	// The drawer's Solo row closes an open thread too.
	a.openThread(thread, "conversation")
	u.Settle()
	u.Click("open-drawer")
	u.Settle()
	u.Click("drawer-solo")
	u.Settle()
	if a.openID.Peek() != "" || a.drawer.Peek() || !u.Has("orch-draft-area") {
		t.Fatalf("the drawer's Solo row shows Solo's conversation: thread %q, drawer %v, composer %v",
			a.openID.Peek(), a.drawer.Peek(), u.Has("orch-draft-area"))
	}
}

// With fingers, Enter in a message makes a new line, since a phone's keyboard has no Shift+Enter, and
// the UI is drawn larger.
func TestTouchEnterMakesANewLine(t *testing.T) {
	f := demoBackend()
	a, u := newPhoneUI(t, f)
	if z := u.Runtime().Zoom(); z != touchZoom {
		t.Errorf("the UI is drawn at %v, want %v with fingers", z, touchZoom)
	}
	u.Get("orch-draft-area").Type("line one")
	u.Press("enter")
	u.Settle()
	f.mu.Lock()
	sent := len(f.sent)
	f.mu.Unlock()
	if sent != 0 || a.orchDraft.Peek() != "line one\n" {
		t.Errorf("Enter sent %d messages and left the draft %q, want a new line", sent, a.orchDraft.Peek())
	}
}

// A window with room shows the rail, Solo's conversation and the open thread side by side, without
// the compact window's drawer.
func TestWideWindowShowsThePanesTogether(t *testing.T) {
	const thread = "c3d4e5f6-forms"
	a, u := newUI(t, demoBackend())
	a.openThread(thread, "conversation")
	u.Settle()
	if !u.Has("orch-draft-area") || !u.Has("rail-filter") || !u.Has("thread-panel-"+thread) {
		t.Fatalf("a wide window shows all three: composer %v, rail %v, panel %v",
			u.Has("orch-draft-area"), u.Has("rail-filter"), u.Has("thread-panel-"+thread))
	}
	if u.Has("open-drawer") || u.Has("thread-close") {
		t.Error("a wide window shows the compact window's drawer button or its Close")
	}
}
