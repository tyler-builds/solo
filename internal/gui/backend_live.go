//go:build !js

package gui

import (
	"context"
	"time"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/orchestrator"
)

// Live is the real backend.
type Live struct {
	*core.Core
	O *orchestrator.Orchestrator
}

func (l Live) SendToOrchestrator(ctx context.Context, text string, images ...string) error {
	return l.O.Send(ctx, text, images...)
}

// Open records the thread open in this window (api.Opener). It counts whether or not the window is in
// front: the user comes back to the thread as they left it.
func (l Live) Open(threadID string) { l.Core.SetViewing("window", threadID, time.Time{}) }

var _ api.Secrets = Live{}
