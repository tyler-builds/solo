package sample

import (
	"context"
	"testing"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/gui"
)

var _ gui.Backend = (*Backend)(nil)

// The sample is as large as it says: a transcript of the asked length, about half a kilobyte an item.
func TestSampleSize(t *testing.T) {
	b := New(Options{Threads: 60, Items: 400})
	if n := len(b.Threads()); n != 61 {
		t.Errorf("%d threads, want 60 workers and Solo", n)
	}
	items, _ := b.Items(context.Background(), core.OrchestratorID, 400)
	bytes := 0
	for _, it := range items {
		bytes += len(it.Data)
	}
	if len(items) != 400 || bytes < 100_000 || bytes > 700_000 {
		t.Errorf("Solo's transcript is %d items and %d bytes, want 400 items of 100 to 700 KB", len(items), bytes)
	}
}
