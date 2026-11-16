package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tyler-builds/solo/internal/forge"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// toolSpecs are the orchestrator's tools, served by handleTool. The model sees them as mcp__solo__<name>.
var toolSpecs = []agent.ToolSpec{
	{
		Name:        "list_repos",
		Description: "List the repositories Solo knows (from the folders in Settings): path, current branch, uncommitted changes, last commit, and how many threads work in each.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		ReadOnly:    true,
	},
	{
		Name: "init_repo",
		Description: "Put a folder that isn't a git repository in git, so threads can work in it: Solo runs git init on branch main, writes a .gitignore if there is none, and commits what's there. " +
			"Only for the folders list_repos names as not in git, and only after the user says yes.",
		InputSchema: schema(`{"type":"object","properties":{
			"folder":{"type":"string","description":"The folder's name or path, as list_repos shows it."}},"required":["folder"]}`),
	},
	{
		Name: "list_threads",
		Description: "List worker threads with their short ID, title, repository, status and a one-line summary. By default lists open threads: not merged, discarded or marked done. " +
			"With include_closed it adds closed ones, newest first: merged, discarded, and those the user marked done (in Settings › History, which reopen_thread brings back). " +
			"Closed threads are capped at the 20 newest from the last 30 days unless limit or days ask for more.",
		InputSchema: schema(`{"type":"object","properties":{
			"include_closed":{"type":"boolean","description":"Also list closed threads: merged, discarded and marked done."},
			"limit":{"type":"integer","description":"With include_closed: at most this many closed threads (default 20, max 100)."},
			"days":{"type":"integer","description":"With include_closed: closed threads from this many days back (default 30)."},
			"repo":{"type":"string","description":"Only threads in this repository (name or path)."}}}`),
		ReadOnly: true,
	},
	{
		Name:        "get_thread",
		Description: "Details of one thread: status, branch, the task it was given, its latest summary or error, files changed, pending questions, cost, and its recent activity.",
		InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Thread ID or short ID."}},"required":["id"]}`),
		ReadOnly:    true,
	},
	{
		Name:        "read_thread",
		Description: "Read a thread's transcript: the messages, tool calls and results, newest last.",
		InputSchema: schema(`{"type":"object","properties":{
			"id":{"type":"string","description":"Thread ID or short ID."},
			"limit":{"type":"integer","description":"How many of the newest entries (default 40, max 200)."}},"required":["id"]}`),
		ReadOnly: true,
	},
	{
		Name:        "get_diff",
		Description: "The unified diff of a worker's changes against the commit it started from, including uncommitted and new files.",
		InputSchema: schema(`{"type":"object","properties":{
			"id":{"type":"string","description":"Thread ID or short ID."},
			"max_chars":{"type":"integer","description":"Truncate after this many characters (default 30000)."}},"required":["id"]}`),
		ReadOnly: true,
	},
	{
		Name: "spawn_thread",
		Description: "Start a worker thread: a Claude Code agent in its own git worktree and branch of a repository, with full permissions there. " +
			"Use one thread per task; the thread runs its own subagents for parts of it. It starts at once. Returns its ID.",
		InputSchema: schema(`{"type":"object","properties":{
			"repo":{"type":"string","description":"Repository name or path, from list_repos."},
			"repos":{"type":"array","items":{"type":"string"},"description":"More repositories, for one task that spans them (a client, its API and its database, say). The thread gets a worktree of each, all on one branch, and a pull request in each it changes."},
			"title":{"type":"string","description":"Short title, a few words, shown to the user."},
			"prompt":{"type":"string","description":"The complete, self-contained task for the worker."},
			"model":{"type":"string","description":"Optional model alias (opus, sonnet, haiku) or ID."},
			"effort":{"type":"string","description":"Optional effort level (low, medium, high, xhigh, max); default from Settings."},
			"base":{"type":"string","description":"Optional branch to start from; default is the repository's current branch."},
			"images":{"type":"array","items":{"type":"string"},"description":"Optional paths of image files to send with it, such as the attached images the user gave you."},
			"todo":{"type":"string","description":"Optional ID of a todo (from add_todo or list_todos) that this thread's work finishes: the app ticks it off when the work lands. Leave it out when the thread does only part of the todo."},
			"scratch":{"type":"boolean","description":"Start it as a scratch thread: the user only wants to try or test something (a benchmark, a spike) and doesn't mean to land it through git. It still gets its worktree and branch, but nothing asks the user to land it, and you never propose a merge for it. Only when the user says so."}},
			"required":["repo","title","prompt"]}`),
	},
	{
		Name:        "message_thread",
		Description: "Send a message to a worker: more instructions, a correction, or what to fix. Works while it runs (it reads the message soon) or when idle (it starts a new turn).",
		InputSchema: schema(`{"type":"object","properties":{
			"id":{"type":"string","description":"Thread ID or short ID."},
			"text":{"type":"string"},
			"images":{"type":"array","items":{"type":"string"},"description":"Optional paths of image files to send with it, such as the attached images the user gave you."}},"required":["id","text"]}`),
	},
	{
		Name:        "interrupt_thread",
		Description: "Stop a worker's current turn. It keeps its worktree and can be messaged again.",
		InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Thread ID or short ID."}},"required":["id"]}`),
	},
	{
		Name: "answer_question",
		Description: "Answer a question a worker is waiting on (get_thread shows it). Only when the user's instructions already settle it; " +
			"otherwise ask the user. answers maps each question's text to the chosen option label or a free-text answer.",
		InputSchema: schema(`{"type":"object","properties":{
			"id":{"type":"string","description":"Thread ID or short ID."},
			"answers":{"type":"object","additionalProperties":{"type":"string"}}},"required":["id","answers"]}`),
	},
	{
		Name: "reopen_thread",
		Description: "Bring a thread the user marked done back to their list, as Settings › History's Restore does: its worktree is recreated and its Claude session resumes, idle. " +
			"Only when the user asks to bring a done thread back. Its branch comes back with its work if it was kept (work that hadn't landed); " +
			"one whose work landed, that had none, or that was discarded starts a new branch of the same name from its base branch's current head. " +
			"Find it with list_threads include_closed.",
		InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"Thread ID or short ID."}},"required":["id"]}`),
	},
	{
		Name: "set_scratch",
		Description: "Turn a worker's scratch mode on or off. A scratch thread is one the user only wants to try or test something with, not land: no merge card, and the app doesn't ask the user to land it. " +
			"Turning it on removes the thread's merge card; turning it off makes it a normal thread to land again. Its worktree, branch and commits don't change. Only when the user says so.",
		InputSchema: schema(`{"type":"object","properties":{
			"id":{"type":"string","description":"Thread ID or short ID."},
			"on":{"type":"boolean","description":"true to make it a scratch thread, false to make it a normal thread again."}},"required":["id","on"]}`),
	},
	{
		Name:        "propose_merge",
		Description: "Show the user a landing card for a worker whose work you reviewed and recommend landing: Open PR (or MR) when the repository has a GitHub or GitLab remote, else Merge. Only the user can press it.",
		InputSchema: schema(`{"type":"object","properties":{
			"id":{"type":"string","description":"Thread ID or short ID."},
			"summary":{"type":"string","description":"One or two sentences: what the change does and why it's ready."}},"required":["id","summary"]}`),
	},
}

// todoSpecs keep the user's to-do list, which the app pins beside the conversation.
var todoSpecs = []agent.ToolSpec{
	{
		Name:        "list_todos",
		Description: "The user's to-do list: open todos with their ID, text and the threads that finish them, and the ones completed lately.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		ReadOnly:    true,
	},
	{
		Name: "add_todo",
		Description: "Add a todo to the user's list: an outcome they want done, with or without a thread. " +
			"Pass threads only for threads whose work landing finishes it; the app then ticks it off when that work lands. Returns its ID.",
		InputSchema: schema(`{"type":"object","properties":{
			"text":{"type":"string","description":"A few words in the imperative, as the user would write it: \"Fix OAuth token refresh\"."},
			"threads":{"type":"array","items":{"type":"string"},"description":"Optional IDs or short IDs of the threads whose work finishes it, all of them needed."}},"required":["text"]}`),
	},
	{
		Name:        "update_todo",
		Description: "Change a todo: its text, the threads that finish it, or whether it is done. Tick one off when the conversation or a thread update shows it's done.",
		InputSchema: schema(`{"type":"object","properties":{
			"id":{"type":"string","description":"The todo's ID."},
			"text":{"type":"string"},
			"threads":{"type":"array","items":{"type":"string"},"description":"Replaces the threads that finish it; [] unlinks them."},
			"done":{"type":"boolean"}},"required":["id"]}`),
	},
	{
		Name:        "remove_todo",
		Description: "Delete a todo the user no longer wants (not one that is done: mark that done instead).",
		InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"The todo's ID."}},"required":["id"]}`),
	},
}

// memorySpecs are Solo's long-term memory, which outlasts its Claude sessions.
var memorySpecs = []agent.ToolSpec{
	{
		Name: "remember",
		Description: "Save one durable fact, decision or preference to your memory, which outlasts this conversation. " +
			"Your current memories are listed in your system prompt; to change one, save the new version with replaces.",
		InputSchema: schema(`{"type":"object","properties":{
			"text":{"type":"string","description":"One self-contained fact in a sentence or two (at most 400 characters), with the names it's about, readable with no other context."},
			"keywords":{"type":"string","description":"Other words someone might search for it by: synonyms, related terms, names. Space separated."},
			"repo":{"type":"string","description":"The repository it is about, when it holds only there."},
			"replaces":{"type":"array","items":{"type":"integer"},"description":"IDs of memories this one replaces (outdated or merged into it)."}},"required":["text","keywords"]}`),
	},
	{
		Name: "recall",
		Description: "Search your memories, your earlier conversation with the user, and past threads. " +
			"Pass two or three phrasings in one call (different words for the same thing); results come back ranked, one line each. " +
			"At most twice a turn.",
		InputSchema: schema(`{"type":"object","properties":{
			"queries":{"type":"array","items":{"type":"string"},"description":"Two or three short searches in the words the answer would contain: names, terms, synonyms."}},"required":["queries"]}`),
		ReadOnly: true,
	},
	{
		Name:        "forget",
		Description: "Delete a memory that is wrong or that the user asks you to forget. To change one, use remember with replaces instead.",
		InputSchema: schema(`{"type":"object","properties":{"memory_id":{"type":"integer"}},"required":["memory_id"]}`),
	},
}

// recallsPerTurn caps recall calls in one turn of Solo's, so a search can't turn into a hunt.
const recallsPerTurn = 2

func init() {
	toolSpecs = append(toolSpecs, todoSpecs...)
	toolSpecs = append(toolSpecs, memorySpecs...)
}

// handleTool runs one orchestrator tool. Errors go back to the model as error results.
func (o *Orchestrator) handleTool(ctx context.Context, threadID, name string, raw []byte) (string, bool) {
	if threadID != core.OrchestratorID {
		return "Only Solo itself has these tools.", true
	}
	var a struct {
		ID            string            `json:"id"`
		Repo          string            `json:"repo"`
		Title         string            `json:"title"`
		Prompt        string            `json:"prompt"`
		Model         string            `json:"model"`
		Base          string            `json:"base"`
		Text          string            `json:"text"`
		Summary       string            `json:"summary"`
		Limit         int               `json:"limit"`
		MaxChars      int               `json:"max_chars"`
		IncludeClosed bool              `json:"include_closed"`
		Days          int               `json:"days"`
		Answers       map[string]string `json:"answers"`
		Images        []string          `json:"images"`
		Repos         []string          `json:"repos"`
		Effort        string            `json:"effort"`
		Folder        string            `json:"folder"`
		Todo          string            `json:"todo"`
		Threads       *[]string         `json:"threads"`
		Done          *bool             `json:"done"`
		Keywords      string            `json:"keywords"`
		Replaces      []int64           `json:"replaces"`
		Queries       []string          `json:"queries"`
		MemoryID      int64             `json:"memory_id"`
		Scratch       bool              `json:"scratch"`
		On            *bool             `json:"on"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "Bad arguments: " + err.Error(), true
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	worker := func() (core.ThreadView, error) {
		v, err := o.c.FindThread(a.ID)
		if err != nil {
			return v, err
		}
		if v.Kind != store.KindWorker {
			return v, fmt.Errorf("%s is not a worker thread", a.ID)
		}
		return v, nil
	}

	var out string
	var err error
	switch name {
	case "list_repos":
		out = o.listRepos(ctx)
	case "init_repo":
		var dir string
		if dir, err = o.c.InitRepo(ctx, a.Folder); err == nil {
			out = fmt.Sprintf("%s is a git repository now, on branch main with a first commit of its files. Threads can work in it as %q.", dir, filepath.Base(dir))
		}
	case "list_threads":
		out, err = o.listThreads(a.Repo, a.IncludeClosed, a.Limit, a.Days)
	case "get_thread":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			out = o.describe(ctx, v)
		}
	case "read_thread":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			out, err = o.transcript(ctx, v.ID, a.Limit)
		}
	case "get_diff":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			var d string
			if d, err = o.c.Diff(ctx, v.ID); err == nil {
				max := a.MaxChars
				if max <= 0 {
					max = 30000
				}
				if d == "" {
					d = "(no changes)"
				}
				out = clip(d, max)
			}
		}
	case "spawn_thread":
		var v core.ThreadView
		if v, err = o.c.CreateWorker(ctx, core.WorkerSpec{Repo: a.Repo, Repos: a.Repos, Title: a.Title, Prompt: a.Prompt, Model: a.Model, Effort: a.Effort, Base: a.Base, Images: a.Images, Scratch: a.Scratch}); err == nil {
			out = fmt.Sprintf("Started thread %s %q in %s (status: %s). You'll get a thread update when it finishes or needs input.",
				short(v.ID), v.Title, repoList(v), v.Status)
			if v.InPlace {
				out += " It works directly in the folder, which isn't in git: no branch, diff or merge."
			} else if v.Scratch {
				out += " It's a scratch thread: when it finishes, report what it found or did; don't propose a merge."
			}
			if a.Todo != "" {
				if err = o.linkTodo(a.Todo, v.ID); err != nil {
					out += " Linking the todo failed: " + err.Error()
					err = nil
				} else {
					out += " It's linked to todo " + a.Todo + "."
				}
			}
		}
	case "message_thread":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			var images []string
			if images, err = o.c.ImportImages(a.Images); err != nil {
				break
			}
			if err = o.c.SendFromOrchestrator(ctx, v.ID, a.Text, images...); err == nil {
				out = fmt.Sprintf("Sent to %s.", short(v.ID))
			}
		}
	case "interrupt_thread":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			if err = o.c.Interrupt(ctx, v.ID); err == nil {
				out = fmt.Sprintf("Interrupting %s.", short(v.ID))
			}
		}
	case "answer_question":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			qs := o.c.PendingQuestions(v.ID)
			if len(qs) == 0 {
				err = fmt.Errorf("%s has no pending question", short(v.ID))
			} else if err = o.c.AnswerFromOrchestrator(v.ID, qs[0].ToolUseID, a.Answers); err == nil {
				out = fmt.Sprintf("Answered %s's question.", short(v.ID))
			}
		}
	case "reopen_thread":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			out, err = o.reopen(ctx, v)
		}
	case "set_scratch":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			if a.On == nil {
				err = errors.New("say on: true or on: false")
				break
			}
			if v, err = o.c.SetScratchFromOrchestrator(v.ID, *a.On); err == nil {
				out = fmt.Sprintf("%s is a scratch thread now: nothing asks the user to land it, and its merge card, if it had one, is gone. Report what it finds; don't propose a merge.", short(v.ID))
				if !v.Scratch {
					out = fmt.Sprintf("%s is a normal thread again: when its work looks right, propose_merge it as usual.", short(v.ID))
				}
			}
		}
	case "propose_merge":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			if err = o.c.AddProposal(v.ID, a.Summary); err == nil {
				out = "The user now sees a Merge card for " + short(v.ID) + "."
			}
		}
	case "remember":
		var m core.Memory
		var similar []core.Memory
		if m, similar, err = o.c.Remember(ctx, a.Text, a.Keywords, a.Repo, a.Replaces); err == nil {
			out = fmt.Sprintf("Saved memory #%d.", m.ID)
			if len(similar) > 0 {
				out += " Similar memories; if one says the same, forget it, or save again with replaces:\n" + memoryLines(similar)
			}
		}
	case "recall":
		if !o.takeRecall() {
			err = fmt.Errorf("you've searched %d times this turn; answer with what you found, or ask the user", recallsPerTurn)
			break
		}
		var r core.Recalled
		if r, err = o.c.Recall(ctx, a.Queries); err == nil {
			out = o.recalled(r)
		}
	case "forget":
		if err = o.c.Forget(a.MemoryID); err == nil {
			out = fmt.Sprintf("Forgot memory #%d.", a.MemoryID)
		}
	case "list_todos":
		out = o.listTodos()
	case "add_todo":
		var threads []string
		if a.Threads != nil {
			threads = *a.Threads
		}
		var td core.Todo
		if td, err = o.c.AddTodo(a.Text, threads); err == nil {
			out = fmt.Sprintf("Added todo %s %q.", td.ID, td.Text)
			if td.Done() {
				out += " Its work has already landed, so it's done."
			}
		}
	case "update_todo":
		ch := core.TodoChange{Threads: a.Threads, Done: a.Done, By: core.TodoBySolo}
		if a.Text != "" {
			ch.Text = &a.Text
		}
		var td core.Todo
		if td, err = o.c.UpdateTodo(a.ID, ch); err == nil {
			out = fmt.Sprintf("Updated todo %s %q (%s).", td.ID, td.Text, map[bool]string{true: "done", false: "open"}[td.Done()])
		}
	case "remove_todo":
		if err = o.c.RemoveTodo(a.ID); err == nil {
			out = "Removed todo " + a.ID + "."
		}
	default:
		err = fmt.Errorf("unknown tool %s", name)
	}
	if err != nil {
		return err.Error(), true
	}
	return out, false
}

func (o *Orchestrator) listRepos(ctx context.Context) string {
	repos := o.c.Repos(ctx)
	plain := plainList(o.c.PlainFolders())
	if len(repos) == 0 {
		if plain != "" {
			return "No repositories." + plain
		}
		return "No repositories. The user adds folders to search in Settings."
	}
	counts := map[string]int{}
	for _, t := range o.c.Threads() {
		if t.Kind == store.KindWorker && !core.Finished(t.Status) {
			counts[t.RepoPath]++
		}
	}
	var b strings.Builder
	for _, r := range repos {
		fmt.Fprintf(&b, "- %s (%s): branch %s", r.Name, r.Path, r.Branch)
		if r.Dirty {
			b.WriteString(", uncommitted changes")
		}
		if r.LastCommit != "" {
			fmt.Fprintf(&b, ", last commit %q %s", clip(r.LastCommit, 80), ago(r.LastCommitAt))
		}
		if n := counts[r.Path]; n > 0 {
			fmt.Fprintf(&b, ", %d open thread(s)", n)
		}
		if r.Err != "" {
			fmt.Fprintf(&b, " [error: %s]", clip(r.Err, 120))
		}
		b.WriteByte('\n')
	}
	return b.String() + plain
}

// plainList names the folders that aren't in git for list_repos.
func plainList(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nNot in git (a thread works directly in the folder, alone, with no diff or merge; init_repo puts one in git, with the user's yes):\n")
	for _, d := range dirs {
		fmt.Fprintf(&b, "- %s (%s)\n", filepath.Base(d), d)
	}
	return b.String()
}

// Closed threads in list_threads: the newest closedLimit from the last closedDays days, unless the
// model asks for more, up to maxClosedLimit. Open threads are never capped.
const (
	closedLimit    = 20
	maxClosedLimit = 100
	closedDays     = 30
)

func (o *Orchestrator) listThreads(repo string, includeClosed bool, limit, days int) (string, error) {
	var repoPath string
	if repo != "" {
		p, err := o.c.ResolveRepo(repo)
		if err != nil {
			dir, ok := o.c.ResolvePlain(repo)
			if !ok {
				return "", err
			}
			p = dir
		}
		repoPath = p
	}
	var open, closed []core.ThreadView
	for _, t := range o.c.Threads() {
		if t.Kind != store.KindWorker || (repoPath != "" && t.RepoPath != repoPath) {
			continue
		}
		if isClosed(t) {
			closed = append(closed, t)
		} else {
			open = append(open, t)
		}
	}
	omitted := 0
	if includeClosed {
		closed, omitted = recentClosed(closed, limit, days, time.Now())
	} else {
		closed = nil
	}
	var b strings.Builder
	for _, t := range append(open, closed...) {
		fmt.Fprintf(&b, "- %s %q in %s: %s", short(t.ID), t.Title, filepath.Base(t.RepoPath), statusText(t))
		if t.Summary != "" && (t.Status == core.StatusIdle || t.Status == core.StatusBackground) {
			fmt.Fprintf(&b, " — %s", clip(oneLine(t.Summary), 160))
		}
		if t.Error != "" {
			fmt.Fprintf(&b, " — error: %s", clip(oneLine(t.Error), 160))
		}
		b.WriteByte('\n')
	}
	if len(open)+len(closed) == 0 {
		b.WriteString("No threads.\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "%d older closed thread(s) not shown; pass a larger limit (up to %d) or days to see them.\n", omitted, maxClosedLimit)
	}
	return b.String(), nil
}

// isClosed reports whether a worker is out of the user's list or finished for good: merged, discarded,
// or marked done.
func isClosed(t core.ThreadView) bool { return core.Finished(t.Status) || !t.DoneAt.IsZero() }

// closedAt is when a closed thread closed: when it was marked done, else its last update.
func closedAt(t core.ThreadView) time.Time {
	if !t.DoneAt.IsZero() {
		return t.DoneAt
	}
	return t.UpdatedAt
}

// recentClosed keeps the newest closed threads, newest first: at most limit (closedLimit when not
// positive, maxClosedLimit at most), closed within days of now (closedDays when not positive). It
// returns how many it left out.
func recentClosed(closed []core.ThreadView, limit, days int, now time.Time) ([]core.ThreadView, int) {
	if limit <= 0 {
		limit = closedLimit
	}
	limit = min(limit, maxClosedLimit)
	if days <= 0 {
		days = closedDays
	}
	since := now.AddDate(0, 0, -days)
	closed = slices.Clone(closed)
	slices.SortStableFunc(closed, func(x, y core.ThreadView) int { return closedAt(y).Compare(closedAt(x)) })
	n := len(closed)
	for i, t := range closed {
		if i == limit || closedAt(t).Before(since) {
			return closed[:i], n - i
		}
	}
	return closed, 0
}

func statusText(t core.ThreadView) string {
	s := t.Status
	if t.PendingAsk {
		s += " (waiting on a question)"
	}
	if t.Status == core.StatusQueued {
		s += " (about to start)"
	}
	if t.Status == core.StatusBackground {
		s += " (not finished: its turn ended, but it waits on work it runs in the background, " +
			core.DescribeBackground(t.Background) + "; it carries on when that ends, so don't propose landing yet)"
	} else if len(t.Background) > 0 {
		s += " (also running in the background: " + core.DescribeBackground(t.Background) + ")"
	}
	if t.NoChanges {
		s += " (finished; it changed no files, so there is nothing to merge)"
	}
	if t.InPlace {
		s += " (works directly in a folder outside git: its edits are already in the user's files; nothing to review or merge)"
	} else if t.Scratch {
		s += " (a scratch thread: the user doesn't mean to land it; report what it found or did, and don't propose a merge)"
	}
	if !t.DoneAt.IsZero() && t.Status != core.StatusDiscarded {
		s += " (the user marked it done: it's out of their list, in Settings › History)"
	}
	return s + ", updated " + ago(t.UpdatedAt)
}

func (o *Orchestrator) describe(ctx context.Context, v core.ThreadView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Thread %s %q\n", v.ID, v.Title)
	fmt.Fprintf(&b, "Status: %s\n", statusText(v))
	if v.InPlace {
		fmt.Fprintf(&b, "Folder: %s (not in git; the thread edits it directly)\n", v.RepoPath)
	} else {
		fmt.Fprintf(&b, "Repository: %s\n", v.RepoPath)
	}
	if v.Branch != "" {
		fmt.Fprintf(&b, "Branch: %s (from %s)\n", v.Branch, v.BaseBranch)
	}
	if v.Scratch && !v.InPlace {
		b.WriteString("Landing: none, it's a scratch thread: the user started it to try or test something and doesn't mean to land it. Report what it found or did; don't propose a merge.\n")
	}
	if v.ActualModel != "" || v.Model != "" {
		m := v.ActualModel
		if m == "" {
			m = v.Model
		}
		fmt.Fprintf(&b, "Model: %s\n", m)
	}
	fmt.Fprintf(&b, "Cost so far: $%.2f; context in use: %dk tokens\n", v.CostUSD, v.ContextTokens/1000)
	fmt.Fprintf(&b, "\nTask:\n%s\n", clip(v.Prompt, 3000))
	if v.Summary != "" {
		fmt.Fprintf(&b, "\nLatest summary:\n%s\n", o.clipImages(v.Summary, 3000))
	}
	if v.Error != "" {
		fmt.Fprintf(&b, "\nError: %s\n", v.Error)
	}
	if !v.DoneAt.IsZero() {
		fmt.Fprintf(&b, "\nClosed %s, so it has no worktree. If the user asks, reopen_thread brings it back: %s\n",
			ago(v.DoneAt), branchNotes(ctx, v, false))
	}
	for _, p := range v.Parts {
		where := ""
		if v.MultiRepo() {
			where = p.Name + ": "
			fmt.Fprintf(&b, "\n%s%s, %d files changed", where, p.Repo, max(p.Changed, 0))
		}
		if pr := p.PR; pr != nil {
			fmt.Fprintf(&b, "\n%s%s #%d: %s, %s, checks %s", where, pr.Kind.Noun(), pr.Number, pr.URL, pr.State, pr.Checks)
			if pr.Draft {
				b.WriteString(", draft")
			}
			if pr.AutoMerge {
				b.WriteString(", auto-merge on")
			}
			if pr.Blocked != "" && pr.State == forge.StateOpen {
				b.WriteString(", can't merge yet: " + pr.Blocked)
			}
			if len(pr.Failed) > 0 {
				b.WriteString("; failed: " + failedChecks(pr.Failed))
			}
		} else if p.CanOpenPR && !v.Scratch {
			fmt.Fprintf(&b, "\n%sLanding: the user opens a %s from your merge proposal.", where, p.Forge.Noun())
		}
	}
	b.WriteString("\n")
	if qs := o.c.PendingQuestions(v.ID); len(qs) > 0 {
		b.WriteString("\nWaiting on these questions:\n")
		for _, pq := range qs {
			for _, q := range pq.Questions {
				fmt.Fprintf(&b, "- %s", q.Question)
				var opts []string
				for _, op := range q.Options {
					opts = append(opts, fmt.Sprintf("%q (%s)", op.Label, op.Description))
				}
				if len(opts) > 0 {
					fmt.Fprintf(&b, " Options: %s", strings.Join(opts, "; "))
				}
				if q.MultiSelect {
					b.WriteString(" [several may be chosen, comma-separated]")
				}
				b.WriteByte('\n')
			}
		}
	}
	if v.HasWorktree && v.BaseCommit != "" {
		if changes, err := o.c.Changes(ctx, v.ID); err == nil {
			if len(changes) == 0 {
				b.WriteString("\nFiles changed: none yet\n")
			} else {
				b.WriteString("\nFiles changed:\n")
				for i, fc := range changes {
					if i == 50 {
						fmt.Fprintf(&b, "  … and %d more\n", len(changes)-50)
						break
					}
					fmt.Fprintf(&b, "  %s %s (+%d -%d)\n", fc.Status, fc.Path, fc.Added, fc.Removed)
				}
			}
		}
	}
	if recent, err := o.transcript(ctx, v.ID, 8); err == nil {
		b.WriteString("\nRecent activity:\n" + recent)
	}
	return b.String()
}

// transcript renders a thread's newest items as text for the model.
func (o *Orchestrator) transcript(ctx context.Context, threadID string, limit int) (string, error) {
	if limit <= 0 {
		limit = 40
	}
	if limit > 200 {
		limit = 200
	}
	items, err := o.c.Items(ctx, threadID, limit)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, it := range items {
		switch it.Kind {
		case core.ItemUser:
			var d core.UserData
			it.Decode(&d)
			who := "User"
			if d.Synthetic {
				who = "Solo"
			}
			fmt.Fprintf(&b, "[%s] %s\n", who, clip(d.Text, 1500))
		case core.ItemAssistant:
			var d core.TextData
			it.Decode(&d)
			fmt.Fprintf(&b, "[Agent] %s\n", o.clipImages(d.Text, 2000))
		case core.ItemTool:
			var d core.ToolData
			it.Decode(&d)
			fmt.Fprintf(&b, "[Tool %s] %s", d.Name, clip(oneLine(string(d.Input)), 200))
			if d.Done {
				res := clip(oneLine(d.Result), 300)
				if d.IsError {
					res = "ERROR " + res
				}
				fmt.Fprintf(&b, " → %s", res)
			}
			b.WriteByte('\n')
		case core.ItemTask:
			var d core.TaskData
			it.Decode(&d)
			fmt.Fprintf(&b, "[Subagent] %s: %s %s\n", d.Description, d.Status, clip(oneLine(d.Summary), 200))
		case core.ItemQuestion:
			var d core.QuestionData
			it.Decode(&d)
			for _, q := range d.Questions {
				ans := d.Answers[q.Question]
				switch {
				case d.Cancelled:
					ans = "(dismissed)"
				case ans == "":
					ans = "(waiting)"
				}
				fmt.Fprintf(&b, "[Question] %s → %s\n", q.Question, ans)
			}
		case core.ItemTurn:
			var d core.TurnData
			it.Decode(&d)
			if d.Status != agent.TurnCompleted {
				fmt.Fprintf(&b, "[Turn %s] %s\n", d.Status, d.Error)
			}
		case core.ItemNotice:
			var d core.NoticeData
			it.Decode(&d)
			fmt.Fprintf(&b, "[Solo] %s\n", d.Text)
		}
	}
	if b.Len() == 0 {
		return "(empty)\n", nil
	}
	return b.String(), nil
}

// reopen brings a thread the user marked done back to their list, as Settings › History's Restore does,
// and says what it came back with.
func (o *Orchestrator) reopen(ctx context.Context, v core.ThreadView) (string, error) {
	if v.DoneAt.IsZero() {
		return "", fmt.Errorf("%q isn't done: it's still in the user's list (%s), so there is nothing to reopen", v.Title, v.Status)
	}
	for _, p := range v.Parts {
		if _, err := os.Stat(p.Repo); err != nil {
			return "", fmt.Errorf("can't reopen %q: its folder %s is gone", v.Title, p.Repo)
		}
	}
	notes := branchNotes(ctx, v, true) // before Restore makes the branches it lacked
	o.mu.Lock()
	o.reopening[v.ID] = true // Solo knows; onChange needn't tell it
	o.mu.Unlock()
	err := o.c.Restore(ctx, v.ID)
	o.mu.Lock()
	delete(o.reopening, v.ID)
	o.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("couldn't reopen %q: %w", v.Title, err)
	}
	return fmt.Sprintf("Reopened %s %q: it's back in the user's list, idle, with its worktree and Claude session. %s "+
		"It does nothing until you message_thread it.", short(v.ID), v.Title, notes), nil
}

// branchNotes says what each of a done thread's repositories comes back with when it reopens: the
// branch it kept, with its work, or a new branch from its base branch's current head when Done or
// Discard deleted it. reopened words it for after the fact.
func branchNotes(ctx context.Context, v core.ThreadView, reopened bool) string {
	var notes []string
	for _, p := range v.Parts {
		where := ""
		if v.MultiRepo() {
			where = p.Name + ": "
		}
		var n string
		switch {
		case p.InPlace:
			n = fmt.Sprintf("it works directly in %s, which isn't in git, as before.", p.Repo)
		case p.Branch == "":
			n = "it gets a new worktree from " + p.BaseBranch + "."
		case localBranch(ctx, p.Repo, p.Branch):
			n = fmt.Sprintf("its branch %s was kept with its work, so it picks up where it left off.", p.Branch)
			if reopened {
				n = fmt.Sprintf("it is on its kept branch %s, with its work, and picks up where it left off.", p.Branch)
			}
		default:
			why := "it had no work left to land when it was marked done"
			switch v.Status {
			case core.StatusMerged:
				why = "its work had landed"
			case core.StatusDiscarded:
				why = "it was discarded, which deleted its work"
			}
			n = fmt.Sprintf("its branch %s is gone (%s), so it starts a new branch %s from %s's current head.", p.Branch, why, p.Branch, p.BaseBranch)
			if reopened {
				n = fmt.Sprintf("its branch %s was gone (%s), so it is on a new branch %s from %s's current head, without its earlier changes.", p.Branch, why, p.Branch, p.BaseBranch)
			}
		}
		notes = append(notes, where+n)
	}
	if len(notes) == 0 {
		return ""
	}
	notes[0] = strings.ToUpper(notes[0][:1]) + notes[0][1:]
	return strings.Join(notes, " ")
}

// localBranch reports whether the repository has the branch locally, which is what Restore checks out.
func localBranch(ctx context.Context, repo, branch string) bool {
	_, err := git.Run(ctx, repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	return err == nil
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// repoList names a thread's repositories: "api", or "client, api, db".
func repoList(v core.ThreadView) string {
	if len(v.Parts) == 0 {
		return filepath.Base(v.RepoPath)
	}
	names := make([]string, len(v.Parts))
	for i, p := range v.Parts {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

// linkTodo adds a thread to a todo's linked threads.
func (o *Orchestrator) linkTodo(id, threadID string) error {
	for _, td := range o.c.Todos(true) {
		if td.ID == id {
			threads := append(td.Threads, threadID)
			_, err := o.c.UpdateTodo(id, core.TodoChange{Threads: &threads})
			return err
		}
	}
	return fmt.Errorf("no todo %q", id)
}

// listTodos describes the to-do list for list_todos.
func (o *Orchestrator) listTodos() string {
	var open, done []string
	for _, td := range o.c.Todos(true) {
		line := fmt.Sprintf("- %s %q", td.ID, td.Text)
		var threads []string
		for _, id := range td.Threads {
			if v, ok := o.c.Thread(id); ok {
				threads = append(threads, fmt.Sprintf("%s %q (%s)", short(v.ID), v.Title, statusText(v)))
			}
		}
		if len(threads) > 0 {
			line += ", threads: " + strings.Join(threads, "; ")
		}
		if td.Done() {
			by := map[string]string{core.TodoByUser: "by the user", core.TodoBySolo: "by you", core.TodoByThread: "when its threads landed"}[td.DoneBy]
			done = append(done, fmt.Sprintf("%s, done %s %s", line, ago(td.DoneAt), by))
			continue
		}
		open = append(open, line)
	}
	out := "Open todos:\n"
	if len(open) == 0 {
		out += "(none)\n"
	}
	out += strings.Join(open, "\n")
	if len(done) > 0 {
		out += "\n\nCompleted lately:\n" + strings.Join(done[max(0, len(done)-10):], "\n")
	}
	return strings.TrimSpace(out)
}

// openTodos lists the open todos for a thread update, one per line, or "" when there are none.
func (o *Orchestrator) openTodos() string {
	var lines []string
	for _, td := range o.c.Todos(false) {
		line := fmt.Sprintf("- %s %q", td.ID, td.Text)
		var ids []string
		for _, id := range td.Threads {
			ids = append(ids, short(id))
		}
		if len(ids) > 0 {
			line += " (threads " + strings.Join(ids, ", ") + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// takeRecall counts a recall against the current turn's allowance, reporting whether there was one left.
func (o *Orchestrator) takeRecall() bool {
	v, _ := o.c.Thread(core.OrchestratorID)
	o.mu.Lock()
	defer o.mu.Unlock()
	if !v.TurnStarted.Equal(o.recallTurn) {
		o.recallTurn, o.recalls = v.TurnStarted, 0
	}
	if o.recalls >= recallsPerTurn {
		return false
	}
	o.recalls++
	return true
}

// memoryLines lists memories one per line: ID, repository, date and text.
func memoryLines(ms []core.Memory) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "- #%d", m.ID)
		if m.Repo != "" {
			fmt.Fprintf(&b, " [%s]", m.Repo)
		}
		fmt.Fprintf(&b, " (%s) %s\n", m.UpdatedAt.Format("2006-01-02"), m.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// recalled formats a recall's results compactly, one line each.
func (o *Orchestrator) recalled(r core.Recalled) string {
	var parts []string
	if len(r.Memories) > 0 {
		parts = append(parts, "Memories:\n"+memoryLines(r.Memories))
	}
	if len(r.Messages) > 0 {
		var b strings.Builder
		b.WriteString("Earlier conversation:")
		for _, h := range r.Messages {
			who := "User"
			if h.Kind == core.ItemAssistant {
				who = "You"
			}
			fmt.Fprintf(&b, "\n- (%s) %s: %s", h.CreatedAt.Format("2006-01-02"), who, oneLine(h.Snippet))
		}
		parts = append(parts, b.String())
	}
	if len(r.Threads) > 0 {
		var b strings.Builder
		b.WriteString("Threads:")
		for _, h := range r.Threads {
			v, ok := o.c.Thread(h.ThreadID)
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "\n- %s %q (%s, %s): %s", short(v.ID), v.Title, repoList(v), v.Status, oneLine(h.Snippet))
		}
		parts = append(parts, b.String())
	}
	if len(parts) == 0 {
		return "Nothing found. Try other words, or ask the user."
	}
	return strings.Join(parts, "\n\n")
}
