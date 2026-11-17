package gui

import (
	"fmt"
	"testing"
)

// The times offered for PR text skills are in order, with the default stored as "0", and a time stored
// that isn't offered (set elsewhere) is added rather than shown as nothing.
func TestPRSkillMinuteChoices(t *testing.T) {
	labels := func(current string) (out []string) {
		for _, o := range prSkillMinuteChoices(current) {
			out = append(out, o.Value+"="+o.Label)
		}
		return out
	}
	for current, want := range map[string]string{
		"0":  "[2=2 minutes 0=5 minutes 10=10 minutes 15=15 minutes 30=30 minutes]",
		"1":  "[1=1 minute 2=2 minutes 0=5 minutes 10=10 minutes 15=15 minutes 30=30 minutes]",
		"12": "[2=2 minutes 0=5 minutes 10=10 minutes 12=12 minutes 15=15 minutes 30=30 minutes]",
		"99": "[2=2 minutes 0=5 minutes 10=10 minutes 15=15 minutes 30=30 minutes]",
	} {
		if got := labels(current); fmt.Sprint(got) != want {
			t.Errorf("%s: %v, want %s", current, got, want)
		}
	}
}
