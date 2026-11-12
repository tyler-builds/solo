//go:build !js

package rpc

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/agent/agenttest"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/remote/secure"
	"github.com/tyler-builds/solo/internal/remote/wire"
)

func TestMain(m *testing.M) {
	if agenttest.Main() {
		return
	}
	os.Exit(m.Run())
}

// engine is a real core as the desktop's backend wraps it.
type engine struct{ *core.Core }

func (e engine) SendToOrchestrator(ctx context.Context, text string, images ...string) error {
	return e.Send(ctx, core.OrchestratorID, text, images...)
}

// frames is one end of an in-memory connection that carries frames, as the relay does.
type frames struct {
	in, out chan []byte
	closed  chan struct{}
	once    *sync.Once
}

func (f *frames) Read() ([]byte, error) {
	select {
	case b := <-f.in:
		return b, nil
	case <-f.closed:
		return nil, errors.New("closed")
	}
}

func (f *frames) Write(b []byte) error {
	select {
	case f.out <- slices.Clone(b):
		return nil
	case <-f.closed:
		return errors.New("closed")
	}
}

func (f *frames) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// secureLinks returns a device's and a machine's link after a real handshake, as in production.
func secureLinks(t *testing.T) (device, machine *wire.Link) {
	t.Helper()
	seed, _ := secure.NewSeed(rand.Reader)
	m, _ := secure.MachineFromSeed(seed)
	d, _ := secure.NewKey(rand.Reader)
	var psk [secure.KeySize]byte
	rand.Read(psk[:])
	p := secure.Params{Purpose: secure.Session, Origin: "https://relay.example", Machine: m.ID}
	ini, first, err := secure.Initiate(p, d, m.Noise.Public, psk[:], rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mch, answer, err := secure.Respond(p, m.Noise, psk[:], first, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dch, err := ini.Finish(answer)
	if err != nil {
		t.Fatal(err)
	}
	a, b := make(chan []byte, 64), make(chan []byte, 64)
	closed, once := make(chan struct{}), &sync.Once{}
	device = wire.NewLink(&frames{in: a, out: b, closed: closed, once: once}, dch)
	machine = wire.NewLink(&frames{in: b, out: a, closed: closed, once: once}, mch)
	machine.Compress = true
	return device, machine
}

// Over the real engine, the real encryption and the real framing: a device's mirror matches the
// machine after threads run, and it is told of every status change the machine announced, in order.
func TestMirrorOfTheRealEngine(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	os.MkdirAll(repo, 0o755)
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		if _, err := git.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	exe, vars := agenttest.Command()
	c, err := core.New(core.Config{DataDir: t.TempDir(), Agent: agent.Command{Path: exe, Env: vars},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.UpdateSettings(func(s *core.Settings) { s.Folders = []string{root} }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}

	h := NewHost(engine{c}, Options{FlushEvery: 10 * time.Millisecond, IsDir: func(string) bool { return true }})
	defer h.Close()
	client := NewClient(ClientOptions{Random: rand.Reader})
	device, machine := secureLinks(t)
	defer device.Close()
	go h.Serve(machine, Device{ID: "d1", Name: "phone"})
	go client.Run(device)

	type edge struct{ id, from, to string }
	var mu sync.Mutex
	var onMachine, onDevice []edge
	record := func(into *[]edge) func(core.Change) {
		return func(ch core.Change) {
			if ch.Kind == core.ChangeThread && ch.PrevStatus != ch.Status && ch.PrevStatus != "" {
				mu.Lock()
				*into = append(*into, edge{ch.ThreadID, ch.PrevStatus, ch.Status})
				mu.Unlock()
			}
		}
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("the first sync", func() bool { return client.Status().Synced })
	defer c.Subscribe(record(&onMachine))()
	defer client.Subscribe(record(&onDevice))()

	// Threads are started and messaged from the device, as the UI would.
	v1, err := c.CreateWorker(ctx, core.WorkerSpec{Repo: "proj", Title: "One", Prompt: "write a.txt hello"})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := c.CreateWorker(ctx, core.WorkerSpec{Repo: "proj", Title: "Two", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	client.Watch([]string{v1.ID, v2.ID})
	idle := func(id string) func() bool {
		return func() bool {
			v, _ := c.Thread(id)
			return v.Status == core.StatusIdle && !v.InTurn
		}
	}
	wait("thread one", idle(v1.ID))
	wait("thread two", idle(v2.ID))
	wait("thread one on the device", func() bool { _, ok := client.Thread(v1.ID); return ok })
	for i := 0; i < 3; i++ {
		if err := client.Send(ctx, v2.ID, "again"); err != nil {
			t.Fatal(err)
		}
		wait("the turn to start", func() bool {
			items, _ := c.Items(ctx, v2.ID, 0)
			n := 0
			for _, it := range items {
				if it.Kind == core.ItemUser {
					n++
				}
			}
			return n == i+2
		})
		wait("the turn to end", idle(v2.ID))
	}
	if err := client.UpdateSettings(func(s *core.Settings) { s.ShowCosts = true }); err != nil {
		t.Fatal(err)
	}
	if files, err := client.Changes(ctx, v1.ID); err != nil || len(files) != 1 || files[0].Path != "a.txt" {
		t.Errorf("changes: %v %v", files, err)
	}
	if hits, err := client.SearchPaths(ctx, v1.ID, "a.t", 5); err != nil || len(hits) == 0 || hits[0].Path != "a.txt" {
		t.Errorf("searchPaths: %v %v", hits, err)
	}
	if _, err := client.Skills(ctx, v1.ID); err != nil {
		t.Errorf("skills: %v", err)
	}
	if _, err := client.SearchPaths(ctx, "../etc", "x", 5); err == nil {
		t.Error("searchPaths took a bad thread")
	}
	if diff, err := client.Diff(ctx, v1.ID); err != nil || len(diff) == 0 {
		t.Errorf("diff: %q %v", diff, err)
	}
	if _, err := client.MergeLocal(ctx, v1.ID); err != nil {
		t.Errorf("merge: %v", err)
	}
	wait("thread one to merge", func() bool {
		v, _ := c.Thread(v1.ID)
		return v.Status == core.StatusMerged
	})

	// When the machine is quiet, the device shows it as it is.
	same := func() bool {
		if asJSON(client.Threads()) != asJSON(c.Threads()) || asJSON(client.Settings()) != asJSON(c.Settings()) ||
			asJSON(client.RepoPaths()) != asJSON(c.RepoPaths()) {
			return false
		}
		for _, id := range []string{v1.ID, v2.ID} {
			got, _ := client.Items(ctx, id, 200)
			want, _ := c.Items(ctx, id, 200)
			if asJSON(got) != asJSON(want) {
				return false
			}
		}
		return true
	}
	wait("the mirror to match", same)
	time.Sleep(50 * time.Millisecond)
	if !same() {
		t.Error("the mirror drifted after it matched")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(onMachine) == 0 || !slices.Equal(onMachine, onDevice) {
		t.Errorf("status changes:\n on the machine %v\n on the device  %v", onMachine, onDevice)
	}
}
