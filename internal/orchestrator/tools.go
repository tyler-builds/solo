package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/tyler-builds/solo/internal/forge"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/core"
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
		Name:        "list_threads",
		Description: "List worker threads with their short ID, title, repository, status and a one-line summary. By default lists open threads (not merged or discarded).",
		InputSchema: schema(`{"type":"object","properties":{
			"include_closed":{"type":"boolean","description":"Also list merged and discarded threads."},
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
			"images":{"type":"array","items":{"type":"string"},"description":"Optional paths of image files to send with it, such as the attached images the user gave you."}},
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
		Name:        "propose_merge",
		Description: "Show the user a landing card for a worker whose work you reviewed and recommend landing: Open PR (or MR) when the repository has a GitHub or GitLab remote, else Merge. Only the user can press it.",
		InputSchema: schema(`{"type":"object","properties":{
			"id":{"type":"string","description":"Thread ID or short ID."},
			"summary":{"type":"string","description":"One or two sentences: what the change does and why it's ready."}},"required":["id","summary"]}`),
	},
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
		Answers       map[string]string `json:"answers"`
		Images        []string          `json:"images"`
		Repos         []string          `json:"repos"`
		Effort        string            `json:"effort"`
		Folder        string            `json:"folder"`
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
		out, err = o.listThreads(a.Repo, a.IncludeClosed)
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
		if v, err = o.c.CreateWorker(ctx, core.WorkerSpec{Repo: a.Repo, Repos: a.Repos, Title: a.Title, Prompt: a.Prompt, Model: a.Model, Effort: a.Effort, Base: a.Base, Images: a.Images}); err == nil {
			out = fmt.Sprintf("Started thread %s %q in %s (status: %s). You'll get a thread update when it finishes or needs input.",
				short(v.ID), v.Title, repoList(v), v.Status)
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
			} else if err = o.c.Answer(v.ID, qs[0].ToolUseID, a.Answers); err == nil {
				out = fmt.Sprintf("Answered %s's question.", short(v.ID))
			}
		}
	case "propose_merge":
		var v core.ThreadView
		if v, err = worker(); err == nil {
			if err = o.c.AddProposal(v.ID, a.Summary); err == nil {
				out = "The user now sees a Merge card for " + short(v.ID) + "."
			}
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
	b.WriteString("\nNot in git (threads can't work here until init_repo, with the user's yes):\n")
	for _, d := range dirs {
		fmt.Fprintf(&b, "- %s (%s)\n", filepath.Base(d), d)
	}
	return b.String()
}

func (o *Orchestrator) listThreads(repo string, includeClosed bool) (string, error) {
	var repoPath string
	if repo != "" {
		p, err := o.c.ResolveRepo(repo)
		if err != nil {
			return "", err
		}
		repoPath = p
	}
	var b strings.Builder
	n := 0
	for _, t := range o.c.Threads() {
		if t.Kind != store.KindWorker || (repoPath != "" && t.RepoPath != repoPath) || (!includeClosed && (core.Finished(t.Status) || !t.DoneAt.IsZero())) {
			continue
		}
		n++
		fmt.Fprintf(&b, "- %s %q in %s: %s", short(t.ID), t.Title, filepath.Base(t.RepoPath), statusText(t))
		if t.Summary != "" && t.Status == core.StatusIdle {
			fmt.Fprintf(&b, " — %s", clip(oneLine(t.Summary), 160))
		}
		if t.Error != "" {
			fmt.Fprintf(&b, " — error: %s", clip(oneLine(t.Error), 160))
		}
		b.WriteByte('\n')
	}
	if n == 0 {
		return "No threads.", nil
	}
	return b.String(), nil
}

func statusText(t core.ThreadView) string {
	s := t.Status
	if t.PendingAsk {
		s += " (waiting on a question)"
	}
	if t.Status == core.StatusQueued {
		s += " (about to start)"
	}
	if t.NoChanges {
		s += " (finished; it changed no files, so there is nothing to merge)"
	}
	if !t.DoneAt.IsZero() {
		s += " (the user marked it done)"
	}
	return s + ", updated " + ago(t.UpdatedAt)
}

func (o *Orchestrator) describe(ctx context.Context, v core.ThreadView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Thread %s %q\n", v.ID, v.Title)
	fmt.Fprintf(&b, "Status: %s\n", statusText(v))
	fmt.Fprintf(&b, "Repository: %s\n", v.RepoPath)
	if v.Branch != "" {
		fmt.Fprintf(&b, "Branch: %s (from %s)\n", v.Branch, v.BaseBranch)
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
		fmt.Fprintf(&b, "\nLatest summary:\n%s\n", clip(v.Summary, 3000))
	}
	if v.Error != "" {
		fmt.Fprintf(&b, "\nError: %s\n", v.Error)
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
		} else if p.CanOpenPR {
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
			fmt.Fprintf(&b, "[Agent] %s\n", clip(d.Text, 2000))
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
