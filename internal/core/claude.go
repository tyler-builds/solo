//go:build !js

package core

import (
	"context"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
)

// CheckClaude finds Claude Code with override as the Settings path ("" to detect it), runs it for its
// version, and starts it once to read the signed-in account.
func (c *Core) CheckClaude(ctx context.Context, override string) ClaudeStatus {
	found := agent.FindClaude(override)
	st := ClaudeStatus{Path: found.Path, Source: found.Source}
	if found.Path == "" {
		st.Err = "Claude Code isn't installed, or isn't where Solo looks. Install it, or set its path."
		return st
	}
	v, err := agent.ClaudeVersion(ctx, found.Path)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	st.Version = v
	if c.agent == nil {
		return st
	}
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := c.agent.Probe(pctx, found.Path)
	if err != nil {
		st.Err = "Claude Code didn't start: " + err.Error()
		return st
	}
	st.Models = res.Models
	acct := res.Account
	switch {
	case acct.Email != "":
		st.Account = acct.Email
		if acct.SubscriptionType != "" {
			st.Account += " · " + titleCase(acct.SubscriptionType)
		}
	case acct.APIKeySource != "" && acct.APIKeySource != "none":
		st.Account = "API key (" + acct.APIKeySource + ")"
	default:
		st.Err = "Not signed in. Run claude in a terminal and sign in with /login."
		st.SignedOut = true
	}
	return st
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
