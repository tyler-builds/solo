//go:build live

package core

import (
	"context"

	"github.com/tyler-builds/solo/internal/agent"
	"testing"
	"time"
)

// go test -tags live -run LiveBrief ./internal/core: a worker's turn summarized by the real summary model.
func TestLiveBrief(t *testing.T) {
	e := newEnv(t)
	// The worker is the fake claude; the summary model is the real one.
	e.summarize = func(ctx context.Context, req SummarizeRequest) (string, error) {
		req.ClaudePath = agent.FindClaude("").Path
		return SummarizeWithClaude(ctx, req)
	}
	e.c.Close()
	e.c = e.open(t)
	v, err := e.c.CreateWorker(context.Background(), WorkerSpec{Repo: "proj", Prompt: "write hello.txt hi there", FromUser: true})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
	deadline := time.Now().Add(60 * time.Second)
	for {
		v, _ = e.c.Thread(v.ID)
		if v.Brief != nil && v.Brief.Need != "" && v.Title != "write hello.txt hi there" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no brief or title: %+v %q", v.Brief, v.Title)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("title %q\nturn:\n%s\noverall: %s\nneed: %s", v.Title, v.Brief.Turn, v.Brief.Overall, v.Brief.Need)
}
