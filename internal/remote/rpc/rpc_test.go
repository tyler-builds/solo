package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/core"
)

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The client's mirror matches the machine, and follows it.
func TestMirrorFollowsTheMachine(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusRunning)
	m.addThread("b", core.StatusIdle)
	m.repos, m.plain = []string{"/code/x"}, []string{"/code/notes"}
	m.todos = []core.Todo{{ID: "t1", Text: "ship"}}
	r := newRig(t, m, Device{ID: "d1", Name: "phone"})

	if got, want := asJSON(r.c.Threads()), asJSON(m.Threads()); got != want {
		t.Fatalf("threads:\n got %s\nwant %s", got, want)
	}
	if st := r.c.Status(); !st.Connected || st.Host.Name != "box" || st.ReadOnly {
		t.Errorf("status %+v", st)
	}
	if v := r.c.HostVersion(); v != "1.2.3" {
		t.Errorf("the machine's version came as %q", v)
	}
	if !reflect.DeepEqual(r.c.RepoPaths(), m.repos) || !reflect.DeepEqual(r.c.PlainFolders(), m.plain) ||
		asJSON(r.c.Todos(true)) != asJSON(m.todos) || asJSON(r.c.Settings()) != asJSON(m.Settings()) {
		t.Error("the lists differ")
	}

	// A change is announced after the state it speaks of, with the machine's own details.
	var mu sync.Mutex
	var seen []core.Change
	var statusAtChange string
	r.c.Subscribe(func(ch core.Change) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ch)
		if ch.Kind == core.ChangeThread && ch.ThreadID == "a" && ch.PrevStatus != ch.Status {
			v, _ := r.c.Thread("a")
			statusAtChange = v.Status
		}
	})
	m.update("a", func(v *core.ThreadView) { v.Status, v.PendingAsk = core.StatusWaiting, true })
	r.wait("the status change", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return statusAtChange != ""
	})
	mu.Lock()
	edge := slices.ContainsFunc(seen, func(ch core.Change) bool {
		return ch.Kind == core.ChangeThread && ch.ThreadID == "a" && ch.PrevStatus == core.StatusRunning && ch.Status == core.StatusWaiting
	})
	mu.Unlock()
	if statusAtChange != core.StatusWaiting || !edge {
		t.Errorf("the change wasn't announced as the machine made it: status %q, edge %v", statusAtChange, edge)
	}
	r.wait("the question", func() bool { return len(r.c.PendingQuestions("a")) == 1 })

	m.addThread("c", core.StatusQueued)
	r.wait("the new thread", func() bool { return asJSON(r.c.Threads()) == asJSON(m.Threads()) })
}

// A watched thread's transcript arrives, and follows items that are added, changed and removed.
func TestTranscript(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusRunning)
	m.setItems("a", item(1, "one"), item(2, "two"), item(3, "three"))
	r := newRig(t, m, Device{ID: "d1"})

	if items, _ := r.c.Items(context.Background(), "a", 100); len(items) != 0 {
		t.Fatalf("items before they were asked for: %v", items)
	}
	same := func() bool {
		got, _ := r.c.Items(context.Background(), "a", 100)
		want, _ := m.Items(context.Background(), "a", 100)
		return asJSON(got) == asJSON(want) && len(got) == len(want)
	}
	r.wait("the transcript", same)

	sent := r.host.sent.Load()
	m.setItems("a", item(1, "one"), item(2, "two, changed"), item(4, "four"))
	r.wait("the changed transcript", same)
	if n := r.host.sent.Load() - sent; n > 1500 {
		t.Errorf("a change to two items sent %d bytes", n)
	}

	// A large transcript comes in several messages and shows whole.
	var big []core.Item
	for i := int64(1); i <= 300; i++ {
		big = append(big, item(i, strings.Repeat("x", 2000)))
	}
	m.setItems("a", big...)
	r.wait("the large transcript", same)
}

// Streaming text is sent as what was added, for watched threads only.
func TestStreamingText(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusRunning)
	m.addThread("b", core.StatusRunning)
	r := newRig(t, m, Device{ID: "d1"})
	r.c.Watch([]string{"a"})

	text := strings.Repeat("The quick brown fox. ", 400) // 8 KB
	m.stream("a", core.LiveBlock{Text: text})
	live := func(id string) string {
		v, _ := r.c.Thread(id)
		return asJSON(v.Live)
	}
	r.wait("the text", func() bool { return live("a") == asJSON([]core.LiveBlock{{Text: text}}) })

	sent := r.host.sent.Load()
	for i := 0; i < 20; i++ {
		text += "more "
		m.stream("a", core.LiveBlock{Text: text})
		time.Sleep(2 * time.Millisecond)
	}
	r.wait("the added text", func() bool { return live("a") == asJSON([]core.LiveBlock{{Text: text}}) })
	if n := r.host.sent.Load() - sent; n > 4000 {
		t.Errorf("100 added bytes cost %d on the wire: the text was sent again", n)
	}

	// A block that is replaced, and blocks that go, are followed too.
	m.stream("a", core.LiveBlock{Thinking: true, Text: "hmm"}, core.LiveBlock{Text: "new"})
	r.wait("replaced blocks", func() bool {
		return live("a") == asJSON([]core.LiveBlock{{Thinking: true, Text: "hmm"}, {Text: "new"}})
	})
	m.stream("a")
	r.wait("no blocks", func() bool { return live("a") == "null" })

	// A thread that isn't watched streams nothing.
	sent = r.host.sent.Load()
	m.stream("b", core.LiveBlock{Text: text})
	time.Sleep(40 * time.Millisecond)
	if n := r.host.sent.Load() - sent; n != 0 {
		t.Errorf("an unwatched thread's text cost %d bytes", n)
	}
	if live("b") != "null" {
		t.Error("an unwatched thread's text arrived")
	}
}

// A device that reconnects is sent what changed while it was away, not everything again.
func TestResume(t *testing.T) {
	m := newMem()
	for i := 0; i < 200; i++ {
		m.addThread(fmt.Sprintf("t%03d", i), core.StatusIdle)
	}
	m.setItems("t001", item(1, "one"), item(2, "two"))
	r := newRig(t, m, Device{ID: "d1"})
	r.c.Watch([]string{"t001"})
	r.wait("the transcript", func() bool {
		items, _ := r.c.Items(context.Background(), "t001", 100)
		return len(items) == 2
	})
	cold := r.host.sent.Load()

	for round := 0; round < 3; round++ {
		r.disconnect()
		if round == 1 {
			m.update("t007", func(v *core.ThreadView) { v.Title = "renamed while away" })
		}
		r.connect()
		r.wait("the mirror", func() bool { return asJSON(r.c.Threads()) == asJSON(m.Threads()) })
		// The lease is renewed with the transcript the device holds, which is still current.
		time.Sleep(30 * time.Millisecond)
		if n := r.host.sent.Load(); n > 3000 {
			t.Errorf("round %d: a resume sent %d bytes (a cold start sent %d)", round, n, cold)
		}
	}
	if items, _ := r.c.Items(context.Background(), "t001", 100); len(items) != 2 {
		t.Errorf("the transcript was lost over a resume: %d items", len(items))
	}

	// A machine that restarted is a new epoch: the device starts over.
	r.disconnect()
	r.h.Close()
	r.h = NewHost(m, Options{FlushEvery: 5 * time.Millisecond})
	r.connect()
	if n := r.host.sent.Load(); n < cold/2 {
		t.Errorf("a new machine run sent %d bytes, less than a cold start's %d", n, cold)
	}
	if asJSON(r.c.Threads()) != asJSON(m.Threads()) {
		t.Error("the mirror differs after a new epoch")
	}
}

func TestRequests(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusIdle)
	m.plain = []string{"/code/notes"}
	r := newRig(t, m, Device{ID: "d1", Name: "phone"})
	ctx := context.Background()

	if err := r.c.Send(ctx, "a", "hello", "/att/0.png"); err != nil {
		t.Fatal(err)
	}
	if err := r.c.SendToOrchestrator(ctx, "to solo"); err != nil {
		t.Fatal(err)
	}
	if got := m.sent; len(got) != 2 || got[0] != "a: hello [/att/0.png]" || got[1] != "orchestrator: to solo []" {
		t.Errorf("sent %q", got)
	}
	if commit, err := r.c.Merge(ctx, "a"); err != nil || commit != "abc123" {
		t.Errorf("merge: %q %v", commit, err)
	}

	// The errors the UI knows keep their kind.
	m.onMerge = func() (string, error) { return "", &core.ResolvingError{Title: "Thread a"} }
	var re *core.ResolvingError
	if _, err := r.c.Merge(ctx, "a"); !errors.As(err, &re) || re.Title != "Thread a" {
		t.Errorf("a resolving merge: %v", err)
	}
	m.onMerge = func() (string, error) { return "", core.ErrBusy }
	if _, err := r.c.Merge(ctx, "a"); !errors.Is(err, core.ErrBusy) {
		t.Errorf("a busy merge: %v", err)
	}
	m.onMerge = func() (string, error) { return "", errors.New("nothing to merge") }
	if _, err := r.c.Merge(ctx, "a"); err == nil || err.Error() != "nothing to merge" {
		t.Errorf("a failed merge: %v", err)
	}
	// A backend that panics answers with a failure, and the session lives.
	if err := r.c.Interrupt(ctx, "a"); err == nil {
		t.Error("a request the backend panicked on succeeded")
	}

	// Settings: the fields changed are sent, and the machine's programs can't be set from a device.
	if err := r.c.UpdateSettings(func(s *core.Settings) { s.WorkerModel = "opus"; s.Folders = []string{"/code"} }); err != nil {
		t.Fatal(err)
	}
	if s := m.Settings(); s.WorkerModel != "opus" || !slices.Equal(s.Folders, []string{"/code"}) {
		t.Errorf("settings on the machine: %+v", s)
	}
	if s := r.c.Settings(); s.WorkerModel != "opus" {
		t.Errorf("settings on the device: %+v", s)
	}
	for name, f := range map[string]func(*core.Settings){
		"claude path": func(s *core.Settings) { s.ClaudePath = "/tmp/evil" },
		"editor":      func(s *core.Settings) { s.EditorCommand = "sh -c evil" },
		"terminal":    func(s *core.Settings) { s.TerminalCommand = "sh -c evil" },
		"folder":      func(s *core.Settings) { s.Folders = []string{"/etc/nowhere"} },
	} {
		before := asJSON(m.Settings())
		if err := r.c.UpdateSettings(f); err == nil {
			t.Errorf("%s: changed from a device", name)
		}
		if asJSON(m.Settings()) != before {
			t.Errorf("%s: the machine's settings changed", name)
		}
	}
	// The Claude checked is the machine's, whatever path the device names.
	if st := r.c.CheckClaude(ctx, "/tmp/evil"); st.Path != "checked:" {
		t.Errorf("CheckClaude ran %q", st.Path)
	}
	if _, err := r.c.InitRepo(ctx, "/etc"); err == nil {
		t.Error("put a folder the machine didn't find in git")
	}
	if p, err := r.c.InitRepo(ctx, "/code/notes"); err != nil || p != "/code/notes" {
		t.Errorf("InitRepo: %q %v", p, err)
	}
	if _, err := r.c.ImportImage("/etc/passwd"); err == nil {
		t.Error("imported a path from a device")
	}

	r.amu.Lock()
	audit := strings.Join(r.audit, "\n")
	r.amu.Unlock()
	for _, want := range []string{"phone send a <nil>", "phone merge a <nil>", "phone merge a nothing to merge",
		"phone updateSettings folders,workerModel <nil>", "phone initRepo /code/notes <nil>"} {
		if !strings.Contains(audit, want) {
			t.Errorf("the audit log lacks %q:\n%s", want, audit)
		}
	}
	if strings.Contains(audit, "checkClaude") {
		t.Error("a read was audited")
	}
}

// A read-only device looks, and changes nothing.
func TestReadOnlyDevice(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusIdle)
	r := newRig(t, m, Device{ID: "d1", ReadOnly: true})
	ctx := context.Background()
	if !r.c.Status().ReadOnly {
		t.Error("the device wasn't told it is read-only")
	}
	if err := r.c.Send(ctx, "a", "hello"); err == nil {
		t.Error("sent a message")
	}
	if _, err := r.c.Merge(ctx, "a"); err == nil {
		t.Error("merged")
	}
	if err := r.c.UpdateSettings(func(s *core.Settings) { s.ShowCosts = true }); err == nil {
		t.Error("changed settings")
	}
	if len(m.sent) != 0 || m.merges != 0 || m.Settings().ShowCosts {
		t.Error("a read-only device changed the machine")
	}
	if st := r.c.CheckClaude(ctx, ""); st.Err != "" {
		t.Errorf("a read was refused: %s", st.Err)
	}
}

// Images cross whole, in parts, in both directions, while other requests go on.
func TestImages(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusIdle)
	r := newRig(t, m, Device{ID: "d1"})
	ctx := context.Background()
	big := bytes.Repeat([]byte("0123456789abcdef"), 200_000) // 3.2 MB
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := r.c.Send(ctx, "a", "meanwhile"); err != nil {
				t.Error(err)
			}
		}
	}()
	ref, err := r.c.SaveImageData(ctx, big)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.images[ref], big) {
		t.Fatal("the upload arrived changed")
	}
	back, err := r.c.ReadImage(ctx, ref, 0)
	if err != nil || !bytes.Equal(back, big) {
		t.Fatalf("the download: %d bytes, %v", len(back), err)
	}
	wg.Wait()
	if _, err := r.c.ReadImage(ctx, "/etc/passwd", 0); err == nil {
		t.Error("read a file that isn't an attachment")
	}
	if _, err := r.c.SaveImageData(ctx, make([]byte, 17<<20)); err == nil {
		t.Error("sent an image over the limit")
	}
}

// A request whose connection drops before its answer is sent again, and runs once.
func TestRetriedRequestRunsOnce(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusIdle)
	r := newRig(t, m, Device{ID: "d1"})
	started, release := make(chan struct{}), make(chan struct{})
	m.onMerge = func() (string, error) {
		close(started)
		<-release
		return "once", nil
	}
	type result struct {
		commit string
		err    error
	}
	got := make(chan result, 1)
	go func() {
		commit, err := r.c.Merge(context.Background(), "a")
		got <- result{commit, err}
	}()
	<-started
	r.disconnect() // the answer has nowhere to go
	r.connect()
	close(release)
	res := <-got
	if res.err != nil || res.commit != "once" {
		t.Fatalf("the retried merge: %q %v", res.commit, res.err)
	}
	if m.merges != 1 {
		t.Errorf("the merge ran %d times", m.merges)
	}
}

// A request made while offline waits for the connection.
func TestOfflineRequest(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusIdle)
	r := newRig(t, m, Device{ID: "d1"})
	r.disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.c.Send(ctx, "a", "x"); !errors.Is(err, ErrOffline) {
		t.Errorf("a request while offline: %v", err)
	}
	got := make(chan error, 1)
	go func() { got <- r.c.Send(context.Background(), "a", "queued") }()
	time.Sleep(20 * time.Millisecond)
	r.connect()
	if err := <-got; err != nil || len(m.sent) != 1 {
		t.Errorf("a request that waited for the connection: %v, sent %v", err, m.sent)
	}
}

// When the lease runs out the machine stops sending a watched thread, and starts again on renewal.
func TestLease(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusRunning)
	h := NewHost(m, Options{FlushEvery: 5 * time.Millisecond, Lease: 60 * time.Millisecond})
	defer h.Close()
	host, device := pipe()
	go h.Serve(host, Device{ID: "d1"})
	s := newSession(device, maxFromMachine)
	defer s.close(nil)
	var mu sync.Mutex
	var lives int
	go s.run(func(e Envelope) error {
		if e.T == kindSync {
			var sy Sync
			json.Unmarshal(e.P, &sy)
			mu.Lock()
			lives += len(sy.Live)
			mu.Unlock()
		}
		return nil
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return lives
	}
	send := func(t string, m string, p any) {
		raw, _ := json.Marshal(p)
		s.send(Envelope{T: t, ID: fmt.Sprint(time.Now().UnixNano()), M: m, P: raw})
	}
	send(kindHello, "", Hello{Protocol: Protocol})
	send(kindReq, "watch", Watch{Threads: []WatchThread{{ID: "a"}}, Limit: 10})
	time.Sleep(20 * time.Millisecond)
	m.stream("a", core.LiveBlock{Text: "one"})
	time.Sleep(30 * time.Millisecond)
	if count() == 0 {
		t.Fatal("a watched thread's text didn't arrive")
	}
	time.Sleep(80 * time.Millisecond) // the lease runs out
	before := count()
	m.stream("a", core.LiveBlock{Text: "one two"})
	time.Sleep(40 * time.Millisecond)
	if count() != before {
		t.Error("text was sent after the lease ran out")
	}
	send(kindReq, "watch", Watch{Threads: []WatchThread{{ID: "a"}}, Limit: 10})
	time.Sleep(40 * time.Millisecond)
	if count() == before {
		t.Error("text didn't resume when the lease was renewed")
	}
}

// What a device sends that breaks the rules ends its session and reaches nothing.
func TestHostileDevice(t *testing.T) {
	hello, _ := json.Marshal(Hello{Protocol: Protocol})
	helloFrame := func() []byte {
		b, _ := Envelope{T: kindHello, P: hello}.encode()
		return append([]byte{frameWhole}, b...)
	}
	req := func(m string, p string) []byte {
		b, _ := Envelope{T: kindReq, ID: "r1", M: m, P: json.RawMessage(p)}.encode()
		return append([]byte{frameWhole}, b...)
	}
	part := func(stream, seq uint32, last byte, n int) []byte {
		b := make([]byte, 10+n)
		b[0] = framePart
		b[4], b[8], b[9] = byte(stream), byte(seq), last
		return b
	}
	cases := map[string][][]byte{
		"empty frame":          {{}},
		"unknown frame":        {{99, 1, 2}},
		"request before hello": {req("send", `{"thread":"a","text":"x"}`)},
		"not an envelope":      {{frameWhole, 0, 0}},
		"length past the end":  {{frameWhole, 0, 0, 0, 9, '{', '}'}},
		"not JSON":             {{frameWhole, 0, 0, 0, 2, '{', 'x'}},
		"two hellos":           {helloFrame(), helloFrame()},
		"unknown kind":         {helloFrame(), func() []byte { b, _ := Envelope{T: "sync"}.encode(); return append([]byte{frameWhole}, b...) }()},
		"no request id": {helloFrame(), func() []byte {
			b, _ := Envelope{T: kindReq, M: "send"}.encode()
			return append([]byte{frameWhole}, b...)
		}()},
		"part out of order":     {helloFrame(), part(1, 2, 0, partSize)},
		"short part not last":   {helloFrame(), part(1, 1, 0, 10)},
		"empty part":            {helloFrame(), part(1, 1, 1, 0)},
		"ping with a body":      {{framePing, 1}},
		"ack of the wrong size": {{frameAck, 1}},
		"old protocol": {func() []byte {
			p, _ := json.Marshal(Hello{Protocol: 0})
			b, _ := Envelope{T: kindHello, P: p}.encode()
			return append([]byte{frameWhole}, b...)
		}()},
	}
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMem()
			m.addThread("a", core.StatusIdle)
			h := NewHost(m, Options{FlushEvery: time.Millisecond})
			defer h.Close()
			host, device := pipe()
			done := make(chan error, 1)
			go func() { done <- h.Serve(host, Device{ID: "d1"}) }()
			go func() {
				for {
					if _, err := device.Receive(); err != nil {
						return
					}
				}
			}()
			for _, f := range frames {
				device.Send(f)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the session went on")
			}
			if len(m.sent) != 0 {
				t.Errorf("the backend was reached: %v", m.sent)
			}
		})
	}

	// Bad requests are answered as failures; the session goes on, and nothing runs.
	m := newMem()
	m.addThread("a", core.StatusIdle)
	r := newRig(t, m, Device{ID: "d1"})
	for name, e := range map[string]Envelope{
		"unknown method":   {M: "exec", P: json.RawMessage(`{}`)},
		"no parameters":    {M: "send"},
		"bad parameters":   {M: "send", P: json.RawMessage(`[1]`)},
		"control in id":    {M: "merge", P: json.RawMessage(`{"thread":"a\u0000b"}`)},
		"long thread id":   {M: "merge", P: json.RawMessage(`{"thread":"` + strings.Repeat("a", 200) + `"}`)},
		"too many images":  {M: "send", P: json.RawMessage(`{"thread":"a","text":"x","images":[` + strings.Repeat(`"i",`, 20) + `"i"]}`)},
		"too many watched": {M: "watch", P: json.RawMessage(`{"threads":[` + strings.Repeat(`{"id":"a"},`, 8) + `{"id":"a"}]}`)},
		"huge preview":     {M: "readImage", P: json.RawMessage(`{"ref":"/att/0.png","maxSide":100000}`)},
		"long search":      {M: "search", P: json.RawMessage(`{"query":"` + strings.Repeat("q", 300) + `"}`)},
		"empty image":      {M: "saveImage", P: json.RawMessage(`{}`)},
	} {
		e.T, e.ID = kindReq, "bad-"+name
		r.c.mu.Lock()
		s := r.c.s
		r.c.mu.Unlock()
		res, err := r.c.send(context.Background(), s, e)
		if err != nil || res.E == nil {
			t.Errorf("%s: answered %+v, %v", name, res, err)
		}
	}
	if len(m.sent) != 0 || m.merges != 0 {
		t.Error("a bad request ran")
	}
	if err := r.c.Send(context.Background(), "a", "still here"); err != nil {
		t.Errorf("the session ended over a bad request: %v", err)
	}
}

// A machine that sends what breaks the rules ends the session too, and the mirror isn't corrupted.
func TestHostileMachine(t *testing.T) {
	for name, sy := range map[string]Sync{
		"live index past count": {Live: []LiveUpdate{{ThreadID: "a", Count: 1, Blocks: []LiveDelta{{Index: 5, Text: "x"}}}}},
		"negative count":        {Live: []LiveUpdate{{ThreadID: "a", Count: -1}}},
		"huge count":            {Live: []LiveUpdate{{ThreadID: "a", Count: 1 << 30}}},
	} {
		c := NewClient(ClientOptions{Random: countingReader{}})
		if err := c.apply(sy); err == nil {
			t.Errorf("%s: applied", name)
		}
	}
	// A machine of another version is refused, and one that never welcomes isn't taken for one.
	c := NewClient(ClientOptions{Random: countingReader{}})
	host, device := pipe()
	done := make(chan error, 1)
	go func() { done <- c.Run(device) }()
	s := newSession(host, maxFromDevice)
	go s.run(func(Envelope) error { return nil })
	p, _ := json.Marshal(Sync{Threads: []core.ThreadView{{}}})
	s.send(Envelope{T: kindSync, P: p})
	if err := <-done; err == nil || len(c.Threads()) != 0 {
		t.Errorf("state before a welcome was taken: %v", err)
	}
	host, device = pipe()
	go func() { done <- c.Run(device) }()
	s = newSession(host, maxFromDevice)
	go s.run(func(Envelope) error { return nil })
	p, _ = json.Marshal(Welcome{Protocol: 99})
	s.send(Envelope{T: kindWelcome, P: p})
	var end *EndError
	if err := <-done; !errors.As(err, &end) || end.Code != CodeVersion {
		t.Errorf("a machine of another version: %v", err)
	}
}

// Dropping a device ends its sessions and tells it why.
func TestDrop(t *testing.T) {
	m := newMem()
	r := newRig(t, m, Device{ID: "d1"})
	r.h.Drop("d1")
	var end *EndError
	if err := <-r.done; !errors.As(err, &end) || end.Code != CodeDenied {
		t.Errorf("a dropped device was told %v", err)
	}
}

// What a session costs on the wire, before compression. These are ceilings: a change that sends much
// more than it did fails here rather than on a phone.
func TestSizeBudgets(t *testing.T) {
	m := newMem()
	for i := 0; i < 300; i++ {
		m.addThread(fmt.Sprintf("t%03d", i), core.StatusIdle)
	}
	var items []core.Item
	for i := int64(1); i <= 400; i++ {
		items = append(items, item(i, strings.Repeat("word ", 100)))
	}
	m.setItems("t000", items...)
	r := newRig(t, m, Device{ID: "d1"})
	cold := r.host.sent.Load()
	if cold > 400_000 {
		t.Errorf("a cold start with 300 threads sent %d bytes", cold)
	}
	r.c.Watch([]string{"t000"})
	r.wait("the transcript", func() bool {
		got, _ := r.c.Items(context.Background(), "t000", 400)
		return len(got) == 400
	})
	if n := r.host.sent.Load() - cold; n > 350_000 {
		t.Errorf("a 400-item transcript sent %d bytes", n)
	}
	frames := r.host.frames.Load()
	for i := 0; i < 100; i++ {
		m.stream("t000", core.LiveBlock{Text: strings.Repeat("a", i)})
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if n := r.host.frames.Load() - frames; n > 40 {
		t.Errorf("100 streaming changes in 100 ms at a 5 ms flush sent %d messages", n)
	}
}

// A device that asks for more of a transcript than it holds is sent the longer window.
func TestTranscriptWindowGrows(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusIdle)
	m.setItems("a", item(1, "one"), item(2, "two"), item(3, "three"), item(4, "four"))
	r := newRig(t, m, Device{ID: "d1"})
	r.c.mu.Lock()
	r.c.limit = 2
	r.c.mu.Unlock()
	ctx := context.Background()
	r.c.Watch([]string{"a"})
	r.wait("two items", func() bool {
		items, _ := r.c.Items(ctx, "a", 2)
		return len(items) == 2 && items[0].ID == 3
	})
	r.wait("four items", func() bool {
		items, _ := r.c.Items(ctx, "a", 100)
		return len(items) == 4
	})
}

// Whatever arrives, a session answers with an error or carries on: it never panics, which in a page
// would end the app.
func FuzzFrames(f *testing.F) {
	hello, _ := Envelope{T: kindHello, P: json.RawMessage(`{"protocol":1}`)}.encode()
	f.Add(append([]byte{frameWhole}, hello...))
	f.Add([]byte{framePart, 0, 0, 0, 1, 0, 0, 0, 1, 1, 'x'})
	f.Add([]byte{frameAck, 0, 0, 0, 1, 0, 0, 0, 1})
	f.Add([]byte{framePing})
	sync, _ := Envelope{T: kindSync, P: json.RawMessage(`{"rev":3,"live":[{"thread":"a","count":2,"blocks":[{"i":1,"text":"x"}]}],"items":[{"thread":"a","rev":2,"reset":true,"upsert":[{"ID":1}],"done":true}]}`)}.encode()
	f.Add(append([]byte{frameWhole}, sync...))
	f.Fuzz(func(t *testing.T, frame []byte) {
		a, b := pipe()
		defer a.Close()
		go func() {
			for {
				if _, err := b.Receive(); err != nil {
					return
				}
			}
		}()
		c := NewClient(ClientOptions{Random: countingReader{}})
		s := newSession(a, maxFromMachine)
		s.frame(frame, func(e Envelope) error {
			if e.T == kindSync {
				var sy Sync
				if json.Unmarshal(e.P, &sy) == nil {
					c.apply(sy)
					c.Threads()
				}
			}
			return nil
		})
	})
}

// A connection that has finished its handshake and doesn't say hello is not kept: it would be a
// session nobody can see, and so nobody can end.
func TestHelloIsDue(t *testing.T) {
	m := newMem()
	h := NewHost(m, Options{HelloFor: 40 * time.Millisecond})
	defer h.Close()
	host, device := pipe()
	done := make(chan error, 1)
	go func() { done <- h.Serve(host, Device{ID: "d1"}) }()
	s := newSession(device, maxFromMachine)
	go s.run(func(Envelope) error { return nil })
	stop := make(chan struct{})
	defer close(stop)
	go func() { // it says it is there, and nothing else
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				s.ping()
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a connection that never said hello was kept")
	}
}

// Large messages are for a device that has said hello and may change things: before hello, and from a
// device that may only look, a message in parts ends the session, and its bytes are never kept.
func TestLargeMessagesNeedASession(t *testing.T) {
	part := func(seq uint32, last byte) []byte {
		b := make([]byte, 10+partSize)
		b[0] = framePart
		b[4], b[8], b[9] = 1, byte(seq), last
		return b
	}
	hello, _ := json.Marshal(Hello{Protocol: Protocol})
	helloFrame, _ := Envelope{T: kindHello, P: hello}.encode()
	for name, tc := range map[string]struct {
		dev    Device
		frames [][]byte
	}{
		"before hello":       {Device{ID: "d1"}, [][]byte{part(1, 0)}},
		"a read-only device": {Device{ID: "d1", ReadOnly: true}, [][]byte{append([]byte{frameWhole}, helloFrame...), part(1, 0)}},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMem()
			h := NewHost(m, Options{FlushEvery: time.Millisecond})
			defer h.Close()
			host, device := pipe()
			done := make(chan error, 1)
			go func() { done <- h.Serve(host, tc.dev) }()
			go func() {
				for {
					if _, err := device.Receive(); err != nil {
						return
					}
				}
			}()
			for _, f := range tc.frames {
				device.Send(f)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the session took a message in parts")
			}
		})
	}

	// Only an image upload carries bytes: anything else that does is refused, unread.
	m := newMem()
	m.addThread("a", core.StatusIdle)
	r := newRig(t, m, Device{ID: "d1"})
	r.c.mu.Lock()
	s := r.c.s
	r.c.mu.Unlock()
	res, err := r.c.send(context.Background(), s, Envelope{T: kindReq, ID: "big", M: "send",
		P: json.RawMessage(`{"thread":"a","text":"x"}`), Blob: make([]byte, 300_000)})
	if err != nil || res.E == nil || len(m.sent) != 0 {
		t.Errorf("a message with bytes attached: %+v, %v, sent %v", res.E, err, m.sent)
	}
}

// A device's requests are counted over all its connections, and those still running after their
// connection went count too: reconnecting doesn't start sixteen more.
func TestRequestsAreCountedPerDevice(t *testing.T) {
	m := newMem()
	m.addThread("a", core.StatusIdle)
	release := make(chan struct{})
	var started atomic.Int32
	m.onMerge = func() (string, error) {
		started.Add(1)
		<-release
		return "done", nil
	}
	r := newRig(t, m, Device{ID: "d1"})
	defer close(release)
	for round := 0; round < 3; round++ {
		for i := 0; i < maxRequests; i++ {
			go r.c.Merge(context.Background(), "a")
		}
		r.wait("the requests to start", func() bool { return int(started.Load()) >= maxRequests })
		time.Sleep(30 * time.Millisecond)
		r.disconnect()
		r.connect()
	}
	if n := started.Load(); n != maxRequests {
		t.Errorf("%d requests ran at once for one device, over three connections; the limit is %d", n, maxRequests)
	}
}

// The answers kept are dropped oldest first, and stay within their count and their bytes.
func TestAnswersAreForgottenOldestFirst(t *testing.T) {
	h := NewHost(newMem(), Options{})
	defer h.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	add := func(key string, size int) {
		a := &answer{done: make(chan struct{}), at: time.Now(), size: size}
		close(a.done)
		h.answered[key] = a
		h.order = append(h.order, key)
		h.kept += size
		h.forgetAnswersLocked()
	}
	for i := 0; i < answersKept+10; i++ {
		add(fmt.Sprint("k", i), 10)
	}
	if _, ok := h.answered["k0"]; ok || len(h.answered) != answersKept {
		t.Errorf("%d answers kept, the oldest among them: %v", len(h.answered), ok)
	}
	if _, ok := h.answered[fmt.Sprint("k", answersKept+9)]; !ok {
		t.Error("the newest answer was dropped")
	}
	add("big", answersBytes)
	if h.kept > answersBytes+10 || len(h.answered) > 2 {
		t.Errorf("after a large answer: %d bytes in %d answers", h.kept, len(h.answered))
	}
	if _, ok := h.answered["big"]; !ok {
		t.Error("the answer just given was dropped")
	}
}

// Checking Claude starts Claude: devices share one check rather than each start their own.
func TestClaudeIsCheckedOnce(t *testing.T) {
	m := newMem()
	r := newRig(t, m, Device{ID: "d1", ReadOnly: true})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.c.CheckClaude(context.Background(), "")
		}()
	}
	wg.Wait()
	if n := m.claudeChecks.Load(); n != 1 {
		t.Errorf("Claude was started %d times for 12 requests", n)
	}
}
