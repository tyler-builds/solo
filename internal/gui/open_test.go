package gui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
)

// longThread gives a thread a long transcript of rows of mixed heights, ending with a message that
// says it is the last.
func longThread(f *fakeBackend, id string) string {
	for i := range 40 {
		f.add(id, core.ItemUser, core.UserData{Text: fmt.Sprintf("How is it going? (%d)", i+1)})
		f.add(id, core.ItemTool, tool(fmt.Sprintf("%s-read%d", id, i), "Read", map[string]any{"file_path": "/wt/auth.go"}, "ok"))
		f.add(id, core.ItemAssistant, core.TextData{Text: strings.Repeat("The refresh path now retries once on a 401. ", 3*(1+i%4)) +
			"\n\n```go\nfunc refresh() error {\n\treturn retry(once)\n}\n```\n\n- the tests\n- the docs"})
	}
	last := "The last message of " + id + "."
	f.add(id, core.ItemAssistant, core.TextData{Text: last})
	return last
}

// openRig opens threads as the user does, in a window of the given width, and checks where their
// conversations land.
type openRig struct {
	t *testing.T
	a *App
	f *fakeBackend
	u *uitest.UI
}

func newOpenRig(t *testing.T, width float32, f *fakeBackend) *openRig {
	a := New(f)
	u := uitest.New(t, a.Root, uitest.Size(width, 900))
	t.Cleanup(u.Close)
	started(a, u)
	return &openRig{t, a, f, u}
}

// open opens a thread and waits for the panel to finish sliding in, in real time: the slide steps on
// timers, as in a window.
func (r *openRig) open(id string) {
	r.a.openThread(id, "conversation")
	r.u.WaitFor("the panel to slide in", func() bool { return r.a.panelAnim.Peek() == 1 && r.a.animStop == nil })
	r.settle()
}

func (r *openRig) close() {
	r.a.closeThread()
	r.u.WaitFor("the panel to slide out", func() bool { return r.a.openID.Peek() == "" })
	r.settle()
}

func (r *openRig) settle() {
	for range 10 {
		r.u.Advance(50 * time.Millisecond)
	}
}

// mustShowLatest fails unless the thread's conversation is at its end, with no way back to it
// offered, and shows the text its transcript ends with.
func (r *openRig) mustShowLatest(what, id, last string) {
	r.t.Helper()
	h := r.a.listHandle("thread-" + id)
	if !h.AtEnd() || r.u.Has("to-latest") {
		r.t.Fatalf("%s: the conversation is not at its latest message (at end %v, button %v)", what, h.AtEnd(), r.u.Has("to-latest"))
	}
	if last == "" {
		return
	}
	list := r.u.Get("thread-" + id + "-transcript").Bounds()
	if !r.u.HasName(last) {
		r.t.Fatalf("%s: the last message isn't built", what)
	}
	if b := r.u.ByName(last).Bounds(); b.Y < list.Y || b.Y+b.H > list.Y+list.H {
		r.t.Fatalf("%s: the last message (%v) isn't in view (%v)", what, b, list)
	}
}

// Every way a thread opens shows its latest message: the first thread, another while one is open,
// one already seen while the panel stayed open, one opened again after closing, one mid-turn with
// streaming text, and one shorter than the panel. Desktop's animated panel and a compact window.
func TestThreadsOpenAtTheLatestMessage(t *testing.T) {
	const forms, paginate, oauth, short = "c3d4e5f6-forms", "b1b2b3b4-paginate", "a1f3c9d2-oauth", "d1d2d3d4-export"
	for _, width := range []float32{1440, 600} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			f := demoBackend()
			last := map[string]string{}
			for _, id := range []string{forms, paginate, oauth} {
				last[id] = longThread(f, id)
			}
			// Mid-turn: streaming text ends the conversation.
			for i := range f.threads {
				if f.threads[i].ID == oauth {
					f.threads[i].InTurn = true
					f.threads[i].Live = []core.LiveBlock{{Text: "Streaming the next part of the reply."}}
				}
			}
			r := newOpenRig(t, width, f)

			r.open(forms)
			r.mustShowLatest("the first thread", forms, last[forms])
			r.open(paginate)
			r.mustShowLatest("another thread while one is open", paginate, last[paginate])
			r.open(forms)
			r.mustShowLatest("a thread seen before, the panel still open", forms, last[forms])
			r.open(oauth)
			r.mustShowLatest("a thread mid-turn", oauth, "Streaming the next part of the reply.")
			r.open(paginate)
			r.mustShowLatest("back to another thread seen before", paginate, last[paginate])
			r.open(oauth)
			r.mustShowLatest("back to the thread mid-turn", oauth, "Streaming the next part of the reply.")
			r.open(short)
			r.mustShowLatest("a thread shorter than the panel", short, "")
			r.close()
			r.open(forms)
			r.mustShowLatest("a thread opened again after closing", forms, last[forms])
		})
	}
}

// A thread opened again still follows what arrives while it is at its end, and stays put once the
// user has scrolled up.
func TestReopenedThreadFollowsAndStaysPut(t *testing.T) {
	const forms, paginate = "c3d4e5f6-forms", "b1b2b3b4-paginate"
	f := demoBackend()
	longThread(f, forms)
	longThread(f, paginate)
	r := newOpenRig(t, 1440, f)
	r.open(forms)
	r.open(paginate)
	r.open(forms)
	add := func(text string) {
		r.f.add(forms, core.ItemAssistant, core.TextData{Text: text})
		items, _ := r.f.Items(t.Context(), forms, 0)
		r.a.itemsOf(forms).Set(items)
		r.settle()
	}
	add("A new message while at the end.")
	r.mustShowLatest("after a new message", forms, "A new message while at the end.")

	r.u.Get("thread-"+forms+"-transcript").Scroll(0, -3000)
	r.settle()
	if r.a.listHandle("thread-"+forms).AtEnd() || !r.u.Has("to-latest") {
		t.Fatal("still at the latest message after scrolling up")
	}
	add("A message that arrives while scrolled up.")
	if r.a.listHandle("thread-"+forms).AtEnd() || !r.u.Has("to-latest") {
		t.Fatal("a new message scrolled a conversation the user had scrolled up")
	}
}
