package gui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
)

// Solo's chat has no line for each thread update or for the steps Solo took: what the threads reported
// and what Solo did about it are said in a sentence or two an exchange, and a rule sets each exchange
// apart from the one before. The threads' own chats show their steps; Solo's is for the conversation.

// summarized turns the rows of Solo's chat into the ones shown: the thread updates and the steps of
// an exchange become one summary row, ahead of the cards and the reply they led to.
func (a *App) summarized(rows []row) []row {
	out := rows[:0:0]
	var said, steps []*core.Item
	topic := false // the chat's first row needs no rule above it
	flush := func() {
		if len(said) > 0 || len(a.stepPhrases(steps)) > 0 {
			first := steps
			if len(said) > 0 {
				first = said
			}
			out = append(out, row{key: fmt.Sprintf("c%d", first[0].ID), summary: true, said: said, steps: steps, topic: topic})
			topic = false
		}
		said, steps = nil, nil
	}
	for _, r := range rows {
		switch {
		case r.item != nil && r.item.Kind == core.ItemUser:
			var d core.UserData
			r.item.Decode(&d)
			if !d.Synthetic {
				flush()
				r.topic, topic = len(out) > 0, false
				out = append(out, r)
				continue
			}
			// What Solo did for the user's last message is said before the updates that came after it.
			if len(said) == 0 && len(steps) > 0 {
				flush()
			}
			if len(said) == 0 {
				topic = len(out) > 0
			}
			said = append(said, r.item)
			steps = append(steps, r.steps...) // what Solo did about the update right away (mergeHandoffs)
		case r.item == nil && r.steps != nil:
			steps = append(steps, r.steps...)
		default:
			flush()
			r.topic, topic = topic, false
			out = append(out, r)
		}
	}
	flush()
	return out
}

// named is a thread a summary names: its title, and its ID when a click can open it.
type named struct{ id, title string }

// summaryView is a summary row: what the threads reported, then what Solo did, as muted text. A
// thread's name opens the thread.
func (a *App) summaryView(ctx *ui.Context, r row) ui.Element {
	t := ctx.Theme()
	var segs []ui.Segment
	say := func(s string) { segs = append(segs, ui.Seg(s)) }
	name := func(n named) {
		seg := ui.Seg(n.title).Color(t.TextMuted).Style(ui.Highlight{Weight: ui.FontMedium})
		if n.id != "" {
			id := n.id
			seg = seg.OnClick(func() { a.openThread(id, "conversation") }).Hover(ui.Highlight{Color: t.Text, Underline: true})
		}
		segs = append(segs, seg)
	}
	names := func(ns []named) {
		for i, n := range ns {
			switch {
			case i > 0 && i == len(ns)-1:
				say(" and ")
			case i > 0:
				say(", ")
			}
			name(n)
		}
	}
	for _, c := range a.updateClauses(t, r.said) {
		if len(segs) > 0 {
			say(" ")
		}
		names(c.threads)
		text := c.one
		if len(c.threads) > 1 {
			text = c.many
		}
		// The word that says what happened has its status's color, when the status has one.
		before, word, after := text, "", ""
		if c.color != t.TextMuted && c.color != t.TextSubtle {
			before, word, after = cutActionWord(text)
		}
		say(" " + before)
		if word != "" {
			segs = append(segs, ui.Seg(word).Color(c.color.Alpha(0.9)))
		}
		say(after + ".")
	}
	if phrases := a.stepPhrases(r.steps); len(phrases) > 0 {
		if len(segs) > 0 {
			say(" ")
		}
		say("Solo ")
		for i, p := range phrases {
			switch {
			case i > 0 && i == len(phrases)-1:
				say(" and ")
			case i > 0:
				say(", ")
			}
			say(p.text)
			if len(p.threads) > 0 {
				say(" ")
				names(p.threads)
			}
		}
		say(".")
	}
	failures := 0
	for _, it := range r.steps {
		if it.Kind == core.ItemTool && itemTool(it).IsError {
			failures++
		}
	}
	if failures == 1 {
		segs = append(segs, ui.Seg(" 1 step failed.").Color(t.Danger))
	} else if failures > 1 {
		segs = append(segs, ui.Seg(fmt.Sprintf(" %d steps failed.", failures)).Color(t.Danger))
	}
	return ui.Rich(segs...).FontSize(13).LineHeight(1.5).TextColor(t.TextSubtle) // the reply's line height, for a summary that wraps
}

// clause is what one or more threads reported, as the end of a sentence about them: "has a question"
// for one thread, "have questions" for several.
type clause struct {
	threads   []named
	one, many string
	color     ui.Color // its status's (describeUpdate's)
}

// updateClauses reads the thread updates in the messages and groups the threads that reported the
// same thing, in the order the first of each came.
func (a *App) updateClauses(t *ui.Theme, said []*core.Item) []clause {
	var out []clause
	for _, it := range said {
		var d core.UserData
		it.Decode(&d)
		for _, u := range a.parseUpdates(t, d.Text) {
			one, many := updateWords(u.what)
			th := named{u.threadID, u.name}
			if i := slices.IndexFunc(out, func(c clause) bool { return c.one == one }); i >= 0 {
				if !slices.Contains(out[i].threads, th) {
					out[i].threads = append(out[i].threads, th)
				}
				continue
			}
			out = append(out, clause{[]named{th}, one, many, u.color})
		}
	}
	return out
}

// updateWords turns an update's short text (describeUpdate's) into the end of a sentence about one
// thread, and about several.
func updateWords(what string) (one, many string) {
	head, _, _ := strings.Cut(what, " · ")
	switch {
	case head == "has a question":
		return "has a question", "have questions"
	case head == "finished", head == "failed", head == "merged in every repository":
		return head, head
	case head == "was stopped", head == "was discarded":
		return head, "were" + strings.TrimPrefix(head, "was")
	case head == "resolved its merge conflicts":
		return head, "resolved their merge conflicts"
	case head == "waiting on background work":
		return "is " + head, "are " + head
	case strings.HasPrefix(head, "opened "):
		s := "opened as " + strings.TrimPrefix(head, "opened ")
		return s, s
	case strings.HasPrefix(head, "checks passed on "), strings.HasPrefix(head, "checks failed on "):
		s := strings.Replace(strings.TrimPrefix(head, "checks "), " on ", " its checks on ", 1)
		return s, strings.Replace(s, " its ", " their ", 1)
	case strings.Contains(head, "merged into "):
		_, into, _ := strings.Cut(head, "merged into ")
		return "merged into " + into, "merged into " + into
	case strings.HasSuffix(head, " closed without merging"):
		s := "had " + head
		return s, s
	}
	return head, head
}

// actionWords are the words that say what happened to a thread, in updateWords's sentences.
var actionWords = []string{"questions", "question", "failed", "finished", "merged", "opened", "passed", "resolved"}

// cutActionWord splits the end of a sentence about a thread around the word that says what happened:
// "failed its checks on PR #57" around "failed". The word is empty when there is none.
func cutActionWord(text string) (before, word, after string) {
	for _, w := range actionWords {
		for _, f := range strings.Fields(text) {
			if f == w {
				before, after, _ = strings.Cut(text, w)
				return before, w, after
			}
		}
	}
	return text, "", ""
}

// phrase is one thing Solo did in an exchange, as part of a sentence ("started 4 threads"), with the
// threads it names after it ("messaged", then a thread).
type phrase struct {
	text    string
	threads []named
}

// stepPhrases says what Solo did in the steps, one phrase a kind of step, in the order the first of
// each came. Steps that only read where things stand (listing threads, reading the to-do list) and a
// merge proposal, which its card says, are left out.
func (a *App) stepPhrases(steps []*core.Item) []phrase {
	type kind struct {
		key     string
		n       int
		threads []named
	}
	var kinds []kind
	add := func(key string, th named) {
		i := slices.IndexFunc(kinds, func(k kind) bool { return k.key == key })
		if i < 0 {
			kinds = append(kinds, kind{key: key})
			i = len(kinds) - 1
		}
		kinds[i].n++
		if th.title != "" && !slices.Contains(kinds[i].threads, th) {
			kinds[i].threads = append(kinds[i].threads, th)
		}
	}
	for _, it := range steps {
		if it.Kind != core.ItemTool {
			continue
		}
		d := itemTool(it)
		var th named
		if id := a.toolThread(d); id != "" {
			if v, ok := a.thread(id); ok {
				th = named{id, v.Title}
			}
		}
		switch strings.TrimPrefix(d.Name, "mcp__solo__") {
		case "spawn_thread":
			add("started", named{})
		case "message_thread":
			add("messaged", th)
		case "get_thread", "read_thread", "get_diff":
			add("checked", th)
		case "answer_question":
			add("answered", th)
		case "interrupt_thread":
			add("stopped", th)
		case "add_todo", "update_todo", "remove_todo":
			add("todo", named{})
		case "remember":
			add("remembered", named{})
		case "recall":
			add("recalled", named{})
		case "forget":
			add("forgot", named{})
		case "init_repo":
			_, _, folder := a.describeTool(d)
			add("git "+filepath.Base(folder), named{})
		case "Read", "Grep", "Glob":
			add("read", named{})
		case "Bash", "run_with_secrets":
			add("ran", named{})
		case "request_secret":
			add("secret", named{})
		case "Edit", "MultiEdit", "Write":
			add("edited", named{})
		case "WebFetch", "WebSearch":
			add("web", named{})
		case "Agent", "Task":
			add("subagent", named{})
		}
	}
	count := func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return fmt.Sprintf(many, n)
	}
	var out []phrase
	for _, k := range kinds {
		// A step about threads names them when it can; else it counts them.
		about := func(verb, one, many string) phrase {
			if len(k.threads) > 0 && len(k.threads) <= 3 {
				return phrase{verb, k.threads}
			}
			return phrase{text: count(k.n, one, many)}
		}
		switch k.key {
		case "started":
			out = append(out, phrase{text: count(k.n, "started a thread", "started %d threads")})
		case "messaged":
			out = append(out, about("messaged", "messaged a thread", "messaged %d threads"))
		case "checked":
			out = append(out, about("checked", "checked a thread", "checked %d threads"))
		case "answered":
			out = append(out, about("answered", "answered a question", "answered %d questions"))
		case "stopped":
			out = append(out, about("stopped", "stopped a thread", "stopped %d threads"))
		case "todo":
			out = append(out, phrase{text: "updated the to-do list"})
		case "remembered":
			out = append(out, phrase{text: "saved a memory"})
		case "recalled":
			out = append(out, phrase{text: "searched its memory"})
		case "forgot":
			out = append(out, phrase{text: "forgot a memory"})
		case "read":
			out = append(out, phrase{text: "looked through the code"})
		case "ran":
			out = append(out, phrase{text: count(k.n, "ran a command", "ran %d commands")})
		case "edited":
			out = append(out, phrase{text: count(k.n, "edited a file", "edited %d files")})
		case "web":
			out = append(out, phrase{text: "searched the web"})
		case "secret":
			out = append(out, phrase{text: "asked you for a secret"})
		case "subagent":
			out = append(out, phrase{text: count(k.n, "used a subagent", "used %d subagents")})
		default:
			if folder, ok := strings.CutPrefix(k.key, "git "); ok {
				out = append(out, phrase{text: "put " + folder + " in git"})
			}
		}
	}
	return out
}
