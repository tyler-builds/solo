package gui

import (
	"slices"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
)

func TestHiddenActions(t *testing.T) {
	built := []bool{false, false, true, true, false, false}
	action := []bool{true, false, true, false, true, true}
	if got := hiddenActions(built, action, true); !slices.Equal(got, []int{0}) {
		t.Errorf("above: %v", got)
	}
	if got := hiddenActions(built, action, false); !slices.Equal(got, []int{4, 5}) {
		t.Errorf("below: %v", got)
	}
	if got := hiddenActions(make([]bool, 6), action, false); got != nil {
		t.Errorf("nothing built: %v", got)
	}
}

// The foot of a conversation shows one control at most: what needs the user wins over the way back.
func TestBottomJump(t *testing.T) {
	for _, c := range []struct {
		atEnd  bool
		needs  []int
		latest bool
		target int
	}{
		{atEnd: true},
		{atEnd: false, latest: true},
		{atEnd: false, needs: []int{7, 9}, target: 7},
		{atEnd: true, needs: []int{3}, target: 3},
	} {
		j := bottomJump(c.atEnd, c.needs)
		if c.target == 0 {
			c.target = -1
		}
		if j.latest != c.latest || len(j.needs) != len(c.needs) || j.target != c.target {
			t.Errorf("bottomJump(%v, %v) = %+v", c.atEnd, c.needs, j)
		}
		if j.latest && len(j.needs) > 0 {
			t.Errorf("bottomJump(%v, %v) offers both controls", c.atEnd, c.needs)
		}
	}
}

// Scrolled up past a question that waits on the user, Solo's chat shows only the "needs you" control,
// over the conversation: the conversation keeps its size, and the control takes the user to the question.
func TestOneJumpControl(t *testing.T) {
	f := demoBackend()
	longChat(f)
	f.add(core.OrchestratorID, core.ItemQuestion, core.QuestionData{ToolUseID: "toolu_jump",
		Questions: []agent.Question{{Question: "Which branch should the fix go to?", Header: "Branch",
			Options: []agent.QuestionOption{{Label: "main"}, {Label: "release"}}}}})
	_, u := newUI(t, f)
	settle := func() {
		for i := 0; i < 10; i++ {
			u.Advance(100 * time.Millisecond)
		}
	}
	settle()
	before := u.Get("orch-transcript").Bounds()
	if u.Has("to-latest") || u.Has("offscreen-pill") {
		t.Fatal("a control shows while the chat is at its latest message")
	}
	u.Get("orch-transcript").Scroll(0, -3000)
	settle()
	if !u.Has("offscreen-pill") || u.Has("to-latest") {
		t.Fatalf("scrolled up past a question: want only the needs-you control (pill %v, arrow %v)",
			u.Has("offscreen-pill"), u.Has("to-latest"))
	}
	if after := u.Get("orch-transcript").Bounds(); after != before {
		t.Fatalf("the control moved the conversation: %v, then %v", before, after)
	}
	u.Click("offscreen-pill")
	settle()
	if u.Has("offscreen-pill") {
		t.Fatal("the needs-you control stayed after taking the user to the question")
	}
}
