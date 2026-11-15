package gui

import (
	"context"
	"fmt"

	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
)

// Notification is a desktop notification about a thread. Thread is the thread a click opens ("" for
// Solo's conversation).
type Notification struct {
	Title, Body, Thread string
}

// notifyChange decides whether a change is worth a desktop notification: a thread asking a question,
// failing, finishing, or its pull request's checks failing or merging, and Solo replying. It only
// notifies while Solo's window isn't in front, and not when Settings turns notifications off.
func (a *App) notifyChange(ch core.Change) {
	if a.Notify == nil || ch.Kind != core.ChangeThread {
		return
	}
	if ch.PrevStatus == ch.Status && ch.PREvent == "" {
		return
	}
	v, ok := a.b.Thread(ch.ThreadID)
	if !ok || a.b.Settings().NoNotifications {
		return
	}
	var n *Notification
	switch {
	case !v.IsWorker:
		if ch.PrevStatus == core.StatusRunning && ch.Status == core.StatusIdle {
			if body := a.lastReply(); body != "" && body != "(noted)" { // not its nod to a thread update
				n = &Notification{Title: "Solo replied.", Body: body, Thread: ""}
			}
		}
	case ch.PREvent == core.PRChecksFailed && !v.Experiment: // an experiment isn't meant to land
		n = &Notification{Title: fmt.Sprintf("Checks failed on “%s”.", v.Title), Body: prBody(v, ch.Repo), Thread: v.ID}
	case ch.PREvent == core.PRMerged:
		n = &Notification{Title: fmt.Sprintf("“%s” merged.", v.Title), Body: prBody(v, ch.Repo), Thread: v.ID}
	case ch.Status == core.StatusWaiting && ch.PrevStatus != core.StatusWaiting:
		body := "It's waiting for your answer."
		if qs := a.b.PendingQuestions(v.ID); len(qs) > 0 && len(qs[0].Questions) > 0 {
			body = qs[0].Questions[0].Question
		}
		n = &Notification{Title: fmt.Sprintf("“%s” has a question.", v.Title), Body: body, Thread: v.ID}
	case ch.Status == core.StatusFailed && ch.PrevStatus != core.StatusFailed:
		n = &Notification{Title: fmt.Sprintf("“%s” failed.", v.Title), Body: firstLine(v.Error), Thread: v.ID}
	case ch.Status == core.StatusIdle && ch.PrevStatus == core.StatusRunning:
		n = &Notification{Title: fmt.Sprintf("“%s” finished.", v.Title), Body: firstLine(stripMarkdown(v.Summary)), Thread: v.ID}
	}
	if n == nil {
		return
	}
	a.onUI(func() {
		if a.focused != nil && a.focused() {
			return // the user is looking at Solo
		}
		a.Notify(*n)
	})
}

// focusWatch follows the window's focus (reading it re-runs this when it changes) and draws nothing.
func (a *App) focusWatch(ctx *ui.Context) ui.Element {
	a.windowFocus(ctx.Window().Focused())
	return ui.Div()
}

// windowFocus clears Solo's notifications, and with them the badge on its icon, when the window comes
// to the front: the user is looking at what they were about.
func (a *App) windowFocus(focused bool) {
	if focused == a.wasFocused {
		return
	}
	a.wasFocused = focused
	if focused && a.ClearNotifications != nil {
		go a.ClearNotifications()
	}
}

// prBody names a thread's pull request in a notification: "PR #142 in api".
func prBody(v core.ThreadView, repo string) string {
	for _, p := range v.Parts {
		if p.Repo == repo && p.PR != nil {
			s := fmt.Sprintf("%s #%d", p.PR.Kind.Noun(), p.PR.Number)
			if v.MultiRepo() {
				s += " in " + p.Name
			}
			if p.PR.State != forge.StateMerged && len(p.PR.Failed) > 0 {
				s += ": " + p.PR.Failed[0].Name
			}
			return s + "."
		}
	}
	return ""
}

// lastReply is the start of Solo's latest message.
func (a *App) lastReply() string {
	items, err := a.b.Items(context.Background(), core.OrchestratorID, 20)
	if err != nil {
		return ""
	}
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Kind == core.ItemAssistant {
			var d core.TextData
			items[i].Decode(&d)
			return firstLine(stripMarkdown(d.Text))
		}
	}
	return ""
}

// OpenFromNotification shows what a clicked notification was about.
func (a *App) OpenFromNotification(threadID string) {
	a.onUI(func() {
		if a.raise != nil {
			a.raise()
		}
		if _, ok := a.thread(threadID); ok && threadID != "" && threadID != core.OrchestratorID {
			a.openThread(threadID, "conversation")
		} else {
			a.closeThread()
		}
	})
}
