package gui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/dialog"
	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
)

func newUI(t *testing.T, f *fakeBackend) (*App, *uitest.UI) {
	t.Helper()
	return newUIWith(t, f, nil)
}

// newUIWith is newUI with the app set up before its window opens.
func newUIWith(t *testing.T, f *fakeBackend, setup func(*App)) (*App, *uitest.UI) {
	t.Helper()
	_, dark := Themes()
	a := New(f)
	if setup != nil {
		setup(a)
	}
	u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(dark))
	t.Cleanup(u.Close)
	return a, u
}

func waitSent(t *testing.T, f *fakeBackend, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		sent := append([]string(nil), f.sent...)
		f.mu.Unlock()
		for _, s := range sent {
			if s == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%q was not sent", want)
}

func TestEnterSendsToTheOrchestrator(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	u.Get("orch-draft-area").Type("Fix the login bug")
	u.Press("enter")
	waitSent(t, f, "Fix the login bug")
}

func TestShiftEnterDoesNotSend(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	u.Get("orch-draft-area").Type("line one")
	u.Press("shift+enter")
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 0 {
		t.Fatalf("sent %v", f.sent)
	}
}

func TestCardOpensThreadAndOpenPRButton(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	u.Click("card-c3d4e5f6-forms")
	u.Frame()
	if a.openID.Peek() != "c3d4e5f6-forms" {
		t.Fatalf("drawer shows %q", a.openID.Peek())
	}
	u.ByRole(access.RoleButton, "Open PR").Click()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.opened)
		f.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pull request was opened")
}

func TestQuestionOptionAnswers(t *testing.T) {
	f := demoBackend()
	_, u := newUI(t, f)
	if !u.HasRole(access.RoleButton, "Answer: Redis") {
		t.Fatal("the worker's question is not shown")
	}
}

func TestRailDoneButtonMarksDone(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.railQuery.Set("uploads are logged") // keep its row on screen
	u.Frame()
	u.ByRole(access.RoleButton, "Mark “Find where uploads are logged” done").Click()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if th, _ := f.Thread("a0b1c2d3-audit"); !th.DoneAt.IsZero() {
			if a.openID.Peek() != "" {
				t.Fatalf("the click also opened %q", a.openID.Peek())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("not marked done")
}

func TestNotifiesWhenAThreadNeedsYou(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	got := make(chan Notification, 4)
	a.Notify = func(n Notification) { got <- n }
	a.focused = func() bool { return false }
	a.notifyChange(core.Change{Kind: core.ChangeThread, ThreadID: "b7e21a90-limit", PrevStatus: core.StatusRunning, Status: core.StatusWaiting})
	u.Frame()
	select {
	case n := <-got:
		if n.Thread != "b7e21a90-limit" || n.Body != "Which store should back the upload rate limiter?" {
			t.Fatalf("notification %+v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no notification")
	}

	// Not while Solo is in front, and not when Settings turns them off.
	a.focused = func() bool { return true }
	a.notifyChange(core.Change{Kind: core.ChangeThread, ThreadID: "d9a8b7c6-flaky", PrevStatus: core.StatusRunning, Status: core.StatusFailed})
	u.Frame() // the check runs on the UI thread
	a.focused = func() bool { return false }
	f.settings.NoNotifications = true
	a.notifyChange(core.Change{Kind: core.ChangeThread, ThreadID: "d9a8b7c6-flaky", PrevStatus: core.StatusRunning, Status: core.StatusFailed})
	u.Frame()
	select {
	case n := <-got:
		t.Fatalf("unexpected notification %+v", n)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRailFilter(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.railQuery.Set("api rate")
	u.Frame()
	if got := a.openThreads(false); len(got) != 1 || got[0].ID != "b7e21a90-limit" {
		var titles []string
		for _, th := range got {
			titles = append(titles, th.Title)
		}
		t.Fatalf("filtered to %v", titles)
	}
}

func TestSearchFindsMessages(t *testing.T) {
	f := demoBackend()
	a, _ := newUI(t, f)
	var found bool
	for _, it := range a.paletteItems("retry once") {
		if strings.HasPrefix(it.Key, "msg-") && strings.Contains(it.Label, "Retry once") && strings.Contains(it.Detail, "Fix OAuth token refresh") {
			found = true
		}
	}
	if !found {
		t.Fatal("no message result")
	}
}

// pickerWindow answers the folder picker with a fixed choice.
type pickerWindow struct {
	ui.NopWindow
	folder string
	asked  int
}

func (w *pickerWindow) Focused() bool { return true }
func (w *pickerWindow) PickFolder(_ dialog.FileOptions, done func(dialog.FileResult)) {
	w.asked++
	done(dialog.FileResult{Paths: []string{w.folder}})
}

// Choose… takes the folder from the system's picker, shown and saved as ~/… inside home.
func TestSettingsPicksFolder(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	f := demoBackend()
	a, u := newUI(t, f)
	w := &pickerWindow{folder: filepath.Join(home, "work", "repos")}
	u.Runtime().SetWindow(w)
	a.settingsTab = "repos"
	a.settings.Set(true)
	u.Frame()
	u.ByRole(access.RoleButton, "Choose a folder").Click()
	u.Frame()
	if w.asked != 1 {
		t.Fatalf("picker asked %d times, want 1", w.asked)
	}
	want := filepath.Join("~", "work", "repos")
	u.ByRole(access.RoleButton, "Remove "+want) // fails the test when the row is missing
	// Saved a moment after the change, in the background.
	u.WaitFor("the folder to be saved", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.settings.Folders, want)
	})
}

// A folder that isn't in git is listed in Settings with a button that puts it in git, after which it
// is a repository.
func TestSettingsPutsFolderInGit(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.settingsTab = "repos"
	a.settings.Set(true)
	u.Frame()
	u.ByRole(access.RoleButton, "Put vfl in git").Click()
	u.WaitFor("vfl to leave the list", func() bool { return !u.HasRole(access.RoleButton, "Put vfl in git") })
	f.mu.Lock()
	inited, repos := f.inited, f.repos
	f.mu.Unlock()
	if !slices.Equal(inited, []string{"/home/dev/code/vfl"}) || !slices.Contains(repos, "/home/dev/code/vfl") {
		t.Fatalf("inited %v, repos %v", inited, repos)
	}
}

// Coming to the front clears Solo's notifications, once each time; losing focus doesn't.
func TestFocusClearsNotifications(t *testing.T) {
	a, _ := newUI(t, demoBackend())
	cleared := make(chan struct{}, 4)
	a.ClearNotifications = func() { cleared <- struct{}{} }
	a.wasFocused = false
	for _, f := range []bool{true, true, false, false, true} {
		a.windowFocus(f)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-cleared:
		case <-time.After(2 * time.Second):
			t.Fatalf("cleared %d times, want 2", i)
		}
	}
	select {
	case <-cleared:
		t.Fatal("cleared a third time")
	case <-time.After(50 * time.Millisecond):
	}
}

// A working thread's panel says it is working once, in the line pinned above its composer, not again
// at the end of its transcript.
func TestThreadPanelShowsWorkingOnce(t *testing.T) {
	f := demoBackend()
	for i := range f.threads {
		if f.threads[i].ID == "a1f3c9d2-oauth" {
			f.threads[i].Live, f.threads[i].Step = nil, nil // between steps, nothing streaming
		}
	}
	a, u := newUI(t, f)
	a.openThread("a1f3c9d2-oauth", "conversation")
	u.Frame()
	if u.HasRole(access.RoleProgressBar, "Working") {
		t.Fatal("the transcript repeats the working line")
	}
}

// A command that Claude Code also tracks as a task shows once, as the command; a task that runs on in
// the background after its command returned is a step of its own, which the line of steps names.
func TestForegroundTaskShowsAsItsCommand(t *testing.T) {
	f := demoBackend()
	id := "a7a7a7a7-subs"
	run := tool("b1", "Bash", map[string]any{"command": "node harness.js", "description": "Measure baseline interlude durations"}, "")
	run.Done = false
	f.add(id, core.ItemTool, run)
	f.add(id, core.ItemTask, core.TaskData{TaskID: "t1", Description: "Measure baseline interlude durations", TaskType: "local_bash", Status: "running", ToolUseID: "b1"})
	f.add(id, core.ItemTask, core.TaskData{TaskID: "t2", Description: "Serve the demo game", TaskType: "local_bash", Status: "running", ToolUseID: "b2", Background: true})
	a, u := newUI(t, f)
	a.openThread(id, "conversation")
	u.Frame()
	if u.HasName("Measure baseline interlude durations") {
		t.Fatal("the foreground task has a row besides its command")
	}
	if line := u.Get("thread-" + id + "-transcript").Text(); !strings.Contains(line, "Serve the demo game") {
		t.Fatalf("the background task is not named: %q", line)
	}
}

// A command Claude described leads with what it is for; the command follows, and the working line
// says only what it is for.
func TestBashRowLeadsWithDescription(t *testing.T) {
	a, _ := newUI(t, demoBackend())
	d := tool("b1", "Bash", map[string]any{"command": "node harness.js --plays 40", "description": "Measure baseline interlude durations"}, "")
	if _, label, detail := a.describeTool(d); label != "Measure baseline interlude durations" || detail != "node harness.js --plays 40" {
		t.Fatalf("row %q · %q", label, detail)
	}
	if got := a.stepLabel(d); got != "Measure baseline interlude durations" {
		t.Fatalf("working line %q", got)
	}
	plain := tool("b2", "Bash", map[string]any{"command": "pnpm test"}, "")
	if got := a.stepLabel(plain); got != "Running pnpm test" {
		t.Fatalf("working line without a description %q", got)
	}
}

func TestWorkingComposerStopsThenSteers(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	a.openThread("a1f3c9d2-oauth", "conversation")
	u.Frame()
	if !u.HasRole(access.RoleButton, "Stop") || u.HasRole(access.RoleButton, "Steer") {
		t.Fatal("a working thread's empty composer should offer Stop, not Steer")
	}
	u.Get("draft-a1f3c9d2-oauth-area").Type("Use the v2 endpoint instead")
	u.Frame()
	if u.HasRole(access.RoleButton, "Stop") || !u.HasRole(access.RoleButton, "Steer") {
		t.Fatal("typing should turn Stop into Steer")
	}
	u.Press("enter")
	waitSent(t, f, "Use the v2 endpoint instead")
}

// The round button above the composer shows only while the chat is scrolled up, and takes it back down.
func TestToLatestButton(t *testing.T) {
	f := demoBackend()
	longChat(f)
	_, u := newUI(t, f)
	settle := func() {
		for i := 0; i < 10; i++ {
			u.Advance(100 * time.Millisecond)
		}
	}
	settle()
	if u.Has("to-latest") {
		t.Fatal("the button shows while the chat is at its latest message")
	}
	u.Get("orch-transcript").Scroll(0, -3000)
	settle()
	if !u.Has("to-latest") {
		t.Fatal("no button once the chat is scrolled up")
	}
	u.Click("to-latest")
	settle()
	if u.Has("to-latest") {
		t.Fatal("the button stayed after it scrolled to the latest message")
	}
}
