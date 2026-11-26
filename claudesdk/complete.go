package claudesdk

import (
	"context"
	"errors"
	"strings"
)

// CompleteParams is one small model call.
type CompleteParams struct {
	CLIPath string
	Model   string // e.g. "haiku"
	Effort  string
	System  string
	Prompt  string
	Env     map[string]string
	// Entrypoint is CLAUDE_CODE_ENTRYPOINT; empty means DefaultEntrypoint.
	Entrypoint string
}

// Complete asks a model for one answer and returns its text. The call has no tools, no MCP servers
// (not even claude.ai's), no settings and no saved session, which keeps it fast and its prompt small.
// ctx bounds the whole call.
func Complete(ctx context.Context, p CompleteParams) (string, error) {
	env := map[string]string{"ENABLE_CLAUDEAI_MCP_SERVERS": "false"}
	for k, v := range p.Env {
		env[k] = v
	}
	system := p.System
	if system == "" {
		system = "You are a helpful assistant."
	}
	c, err := Start(ctx, Options{
		CLIPath:              p.CLIPath,
		Env:                  env,
		Entrypoint:           p.Entrypoint,
		Model:                p.Model,
		Effort:               p.Effort,
		SystemPrompt:         system,
		Tools:                []string{},
		SettingSources:       []string{},
		MCPConfig:            `{"mcpServers":{}}`,
		StrictMCPConfig:      true,
		NoSessionPersistence: true,
		MaxTurns:             1,
	})
	if err != nil {
		return "", err
	}
	defer c.Close()
	if err := c.Send(UserMessage{Text: p.Prompt}); err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case m, ok := <-c.Messages():
			if !ok {
				if err := c.Err(); err != nil {
					return "", err
				}
				return "", errors.New("claudesdk: claude exited without an answer")
			}
			r, isResult := m.Result()
			if !isResult {
				continue
			}
			if r.Status() != TurnCompleted {
				return "", errors.New(r.ErrorText())
			}
			return strings.TrimSpace(r.Result), nil
		}
	}
}
