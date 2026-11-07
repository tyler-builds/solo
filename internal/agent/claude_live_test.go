//go:build live

package agent

import (
	"context"
	"testing"
)

func TestLiveFindClaude(t *testing.T) {
	c := FindClaude("")
	v, err := ClaudeVersion(context.Background(), c.Path)
	t.Logf("path=%s source=%s version=%s err=%v", c.Path, c.Source, v, err)
	if err != nil {
		t.Fatal(err)
	}
}
