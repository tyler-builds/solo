//go:build !js

package gui

import (
	"context"

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
