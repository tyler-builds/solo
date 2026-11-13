package version

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	Version = "0.6.0-3-gcb015ff"
	if got := String(); got != "0.6.0-3-gcb015ff" {
		t.Errorf("a set version shows as %q", got)
	}
	Version = ""
	if got := String(); got != "dev" && !strings.HasPrefix(got, "dev-") {
		t.Errorf("an unset version shows as %q, want dev or dev-<commit>", got)
	}
}
