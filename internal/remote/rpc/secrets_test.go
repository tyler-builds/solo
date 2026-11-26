package rpc

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent/agenttest"
	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/secrets"
)

// A web page never takes a secret's value: its backend isn't api.Secrets, a request it mirrors names the
// secret and why but can't be answered from it, and it can only decline one.
func TestSecretRequestFromADevice(t *testing.T) {
	if _, ok := any(NewClient(ClientOptions{Random: rand.Reader})).(api.Secrets); ok {
		t.Fatal("the web client's backend takes secrets")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	os.MkdirAll(repo, 0o755)
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		if _, err := git.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	store := &secrets.Mem{}
	c, err := core.New(core.Config{DataDir: t.TempDir(), ClaudePath: agenttest.ClaudePath(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Secrets: store})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.UpdateSettings(func(s *core.Settings) { s.Folders = []string{root} })
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

	v, err := c.CreateWorker(ctx, core.WorkerSpec{Repo: "proj", Title: "Deploy",
		Prompt: `mcp request_secret {"name":"CLOUDFLARE_API_TOKEN","reason":"to deploy the relay"}`})
	if err != nil {
		t.Fatal(err)
	}
	var pq core.PendingQuestion
	wait("the request on the device", func() bool {
		qs := client.PendingQuestions(v.ID)
		if len(qs) == 1 {
			pq = qs[0]
		}
		return len(qs) == 1
	})
	if s := pq.Secret(); s == nil || s.Name != "CLOUDFLARE_API_TOKEN" || s.Reason != "to deploy the relay" {
		t.Fatalf("mirrored %+v", pq)
	}
	// An answer with text from the device is refused at the machine.
	client.Answer(v.ID, pq.ToolUseID, map[string]string{pq.Questions[0].Question: "typed-on-the-phone-123"})
	time.Sleep(300 * time.Millisecond)
	if qs := c.PendingQuestions(v.ID); len(qs) != 1 {
		t.Fatal("a text answer from the device resolved the request")
	}
	if _, err := store.Get("CLOUDFLARE_API_TOKEN"); err == nil {
		t.Fatal("a value from the device was saved")
	}
	client.Dismiss(v.ID, pq.ToolUseID)
	wait("the thread to carry on", func() bool {
		th, _ := c.Thread(v.ID)
		return th.Status == core.StatusIdle && strings.Contains(th.Summary, "declined")
	})
}
