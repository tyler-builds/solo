// The types in this file, with the transcript's in items.go, are what a client of Solo sees: threads,
// their parts, todos, settings and the changes that are announced. They and what they need build on
// every platform, a web page included, where the rest of the package (the files marked !js), which
// runs git, Claude and the database, does not. Keep code that touches the machine out of this file.

package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/store"
)

// OrchestratorID is the ID of the one orchestrator thread.
const OrchestratorID = "orchestrator"

// ItemProposal is a merge proposal the orchestrator made, shown as a card with a Merge button. Its
// data is ProposalData; Ref is the worker's thread ID. A thread has at most one: proposing it again,
// or its checks passing after they failed, removes the card and posts it anew at the end of the
// conversation, so the card the user can act on is always the latest one.
const ItemProposal = "proposal"

// ProposalData is a merge proposal.
type ProposalData struct {
	ThreadID string `json:"threadId"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	// ChecksFailed is set while the thread's pull request checks have failed since the card was
	// posted; when they pass again, the card is posted anew.
	ChecksFailed bool `json:"checksFailed,omitempty"`
}

// Thread statuses.
const (
	StatusQueued   = "queued"   // created; about to start
	StatusStarting = "starting" // creating its worktree and session
	StatusRunning  = "running"  // a turn is in progress
	StatusWaiting  = "waiting"  // asked the user a question
	// StatusBackground: its turn ended, but work it started in the background (a command, a subagent)
	// still runs, and Claude carries on when it finishes. Not ready until then.
	StatusBackground  = "background"
	StatusIdle        = "idle"        // its last turn finished; ready for review or more instructions
	StatusFailed      = "failed"      // its last turn, or its start, failed
	StatusInterrupted = "interrupted" // stopped by the user, or by Solo closing
	StatusMerged      = "merged"      // its branch was merged and its worktree removed
	StatusDiscarded   = "discarded"   // its worktree and branch were deleted
	// StatusArchived: marked done before it merged. Its worktree was removed, and its branch too unless it
	// held work that hadn't landed (ThreadView.Unlanded), which Restore brings back.
	StatusArchived = "archived"
)

// Active reports whether a thread with the status is starting or working, its background work included.
func Active(status string) bool {
	return status == StatusStarting || status == StatusRunning || status == StatusWaiting || status == StatusBackground
}

// Finished reports whether a thread with the status is closed for good.
func Finished(status string) bool {
	return status == StatusMerged || status == StatusDiscarded || status == StatusArchived
}

// ThreadView is a snapshot of a thread.
type ThreadView struct {
	store.Thread
	InTurn      bool
	TurnStarted time.Time // when the running turn began
	Step        *ToolData // the tool call in progress, if any
	// Thought is the running turn's newest thinking, streaming or finished. Claude sends a summary of
	// its thinking only when that thinking is done, often all at once just before the finished block
	// replaces the live one, so Live rarely holds any for long; Thought keeps the newest until the next.
	Thought     string
	ActualModel string
	Retry       string
	PendingAsk  bool
	// Queued are the user's messages to Solo that wait for its turn to end, oldest first: a message sent
	// to Solo while it works doesn't interrupt it, and Solo reads each as a turn of its own.
	Queued []QueuedMessage
	// Resolving: the worker is resolving merge conflicts, and its branch merges when it finishes.
	Resolving bool
	Live      []LiveBlock
	// Background is the work still running in the background, oldest first. With status Background the
	// turn has ended and the thread waits on it; it can also run on under a turn.
	Background   []BackgroundTask
	IsWorker     bool
	HasWorktree  bool
	CanMerge     bool
	CanDiscard   bool
	CanInterrupt bool
	// CanDone: the user may mark it done, which takes it out of the rail into History: any worker that
	// isn't working and isn't done yet, landed or not.
	CanDone bool
	// Unlanded: marking it done now leaves changes that haven't landed (or aren't counted yet). Done
	// keeps its branch then, so nothing is lost; the user confirms first. Discard is what deletes work.
	Unlanded bool
	// Changed is how many files a worker's branch changes from where it started; -1 until counted.
	Changed int
	// NoChanges is a worker whose last turn finished with nothing to merge.
	NoChanges bool
	// Forge is where the repository's pull requests go (None: it merges locally), and PR the thread's
	// pull request once opened.
	Forge forge.Kind
	PR    *forge.PR
	// CanOpenPR: it has changes to land in a repository with a forge, and no open pull request.
	CanOpenPR bool
	// CanMergeLocal: it has changes and no open pull request, so it can merge in the local checkout.
	CanMergeLocal bool
	// CanMarkReady: its pull request is an open draft.
	CanMarkReady bool
	// Dir is the folder it works in: its worktree, or for several repositories the folder that holds
	// theirs. "" without a worktree.
	Dir string
	// InPlace: it works directly in a folder that isn't in git (Part.InPlace), so there is nothing to
	// review or merge; the user marks it done when it has done its work.
	InPlace bool
	// Parts are the repositories it works in, each with its branch and pull request. The fields above
	// sum them up: Changed adds them, the Can flags hold when any part can, and Forge and PR are the
	// first part's, set only when there is one part.
	Parts []PartView
}

// QueuedMessage is a message to Solo waiting to be read (ThreadView.Queued). It joins the transcript as
// a user item when Solo is handed it.
type QueuedMessage struct {
	Text   string    `json:"text"`
	Images []string  `json:"images,omitempty"`
	At     time.Time `json:"at"`
}

// MultiRepo reports whether the thread works in more than one repository.
func (v ThreadView) MultiRepo() bool { return len(v.Parts) > 1 }

// Part is one repository a worker works in: its worktree, branch and pull request. Most threads have
// one; a task that spans repositories (a client, its API and its database, say) has one per repository.
type Part struct {
	Repo       string    `json:"repo"`
	Worktree   string    `json:"worktree,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	BaseBranch string    `json:"baseBranch,omitempty"`
	BaseCommit string    `json:"baseCommit,omitempty"`
	PR         *forge.PR `json:"pr,omitempty"`
	// InPlace: Repo isn't in git, so the worker works directly in it, as t3code does: no worktree,
	// branch, diff or merge, and nothing of it is ever removed.
	InPlace bool `json:"inPlace,omitempty"`
}

// PartView is a snapshot of a Part, with what can be done with it.
type PartView struct {
	Part
	Name    string // the repository's folder name
	Changed int    // -1 until counted
	Forge   forge.Kind
	// CanOpenPR, CanMerge, CanMergeLocal and CanMarkReady are as on ThreadView, for this repository.
	CanOpenPR, CanMerge, CanMergeLocal, CanMarkReady bool
}

// BackgroundTask is work a thread's agent runs in the background: a command run with
// run_in_background, or a background subagent.
type BackgroundTask struct {
	ID          string
	Type        string // local_bash, local_agent, …
	Description string
	Started     time.Time // when Solo first saw it running
}

// Kind names what the task is, for people: "command", "subagent" or "task".
func (b BackgroundTask) Kind() string { return taskKind(b.Type) }

// taskKind names what a background task of the type is, for people.
func taskKind(taskType string) string {
	switch {
	case strings.Contains(taskType, "bash"), strings.Contains(taskType, "shell"):
		return "command"
	case strings.Contains(taskType, "agent"):
		return "subagent"
	}
	return "task"
}

// ResolvingError is a merge that conflicted and went back to its worker to resolve; the branch merges
// on its own once the worker finishes.
type ResolvingError struct{ Title string }

func (e *ResolvingError) Error() string {
	return fmt.Sprintf("%q conflicts with newer work; its thread is resolving that and merges when done", e.Title)
}

// ErrBusy is returned by a landing action (merging, opening a pull request, marking ready, discarding,
// marking done, restoring) on a thread that has one under way: from another window or device, or from
// Solo. The one under way decides what the thread becomes; the second would act on a stale view of it.
var ErrBusy = errors.New("another action on this thread is still running; try again when it finishes")

// PendingQuestion is a question a thread is waiting on.
type PendingQuestion struct {
	ToolUseID string
	Questions []agent.Question
}

// ChangeKind says what changed.
type ChangeKind int

const (
	ChangeThread   ChangeKind = iota // a thread was added, or its status or details changed
	ChangeItems                      // a thread's transcript gained or updated an item
	ChangeStream                     // a thread's streaming text changed (frequent)
	ChangeSettings                   // settings changed
	ChangeRepos                      // the repository list changed
	ChangeTodos                      // the to-do list changed
	ChangeAnswer                     // a thread's question was answered or skipped (Change.Answer)
)

// Change is one notification to subscribers.
type Change struct {
	Kind     ChangeKind
	ThreadID string
	// For ChangeThread: the status before and after, equal when something else changed.
	PrevStatus, Status string
	// PREvent is set when something happened to one of the thread's pull requests: PROpened, PRFound,
	// PRChecksFailed, PRChecksPassed, PRMerged or PRClosed. Repo is its repository.
	PREvent string
	Repo    string
	// For ChangeAnswer: the question and what became of it.
	Answer *Answered
	// ScratchSet: for ChangeThread, the thread's scratch mode was turned on or off
	// (Core.SetScratch), by ScratchBy: FromUser or FromOrchestrator.
	ScratchSet bool
	ScratchBy  string
}

// Answered is a question a thread asked and what became of it.
type Answered struct {
	Questions []agent.Question
	Answers   map[string]string // keyed by question text
	Skipped   bool              // dismissed rather than answered
	From      string            // who answered: FromUser or FromOrchestrator
}

// Pull request events (Change.PREvent).
const (
	PROpened       = "opened"
	PRFound        = "found" // opened outside Solo (by the thread itself, say), and now followed
	PRChecksFailed = "checks_failed"
	PRChecksPassed = "checks_passed"
	PRMerged       = "merged"
	PRClosed       = "closed"
)

// Todo is one entry of the user's to-do list, which Solo keeps as they talk: an outcome the user
// wants, not a thread. Most have no thread, and Solo ticks them off when it sees they're done. Solo
// links threads only when their work finishes the todo; it is then done once that work lands: a pull
// request opened or merged, a local merge, or the user marking the thread done.
type Todo struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Threads   []string  `json:"threads,omitempty"` // threads whose work landing finishes it; most todos have none
	CreatedAt time.Time `json:"created_at"`
	// DoneAt is when it was completed, and DoneBy who completed it: TodoByUser, TodoBySolo or
	// TodoByThread (its threads landed).
	DoneAt time.Time `json:"done_at,omitzero"`
	DoneBy string    `json:"done_by,omitempty"`
}

// Who completed a todo (Todo.DoneBy).
const (
	TodoByUser   = "user"
	TodoBySolo   = "solo"
	TodoByThread = "thread"
)

// Done reports whether the todo is completed.
func (t Todo) Done() bool { return !t.DoneAt.IsZero() }

// TodoChange changes a todo; nil fields stay as they are.
type TodoChange struct {
	Text    *string
	Threads *[]string // replaces the linked threads
	Done    *bool
	By      string // who completes it, when Done is true; default TodoBySolo
}

// Settings are the user's preferences.
type Settings struct {
	// Folders are searched for git repositories, which the orchestrator then knows about.
	Folders           []string `json:"folders"`
	ScanDepth         int      `json:"scanDepth"`
	OrchestratorModel string   `json:"orchestratorModel"`
	WorkerModel       string   `json:"workerModel"`
	ClaudePath        string   `json:"claudePath,omitempty"`
	// ShowCosts shows what threads have cost in the UI.
	ShowCosts bool `json:"showCosts,omitempty"`
	// EditorCommand and TerminalCommand open a thread's folder ({path} marks where it goes); empty
	// finds one.
	EditorCommand   string `json:"editorCommand,omitempty"`
	TerminalCommand string `json:"terminalCommand,omitempty"`
	// NoNotifications turns off desktop notifications.
	NoNotifications bool `json:"noNotifications,omitempty"`
	// NoResumeAfterRestart leaves threads that were working when Solo closed stopped, instead of
	// carrying them on when it starts again (ResumeInterrupted).
	NoResumeAfterRestart bool `json:"noResumeAfterRestart,omitempty"`

	// SoloSessionTokens is the context size at which Solo moves to a fresh Claude session, with a recap
	// and its memory: 0 for the default, -1 for never (Claude compacts the session itself instead).
	SoloSessionTokens int `json:"soloSessionTokens,omitempty"`

	// Effort for each model; "" leaves it to Claude.
	OrchestratorEffort string `json:"orchestratorEffort,omitempty"`
	WorkerEffort       string `json:"workerEffort,omitempty"`
	// NamingModel and NamingEffort name branches (see BranchRules).
	NamingModel  string `json:"namingModel,omitempty"`
	NamingEffort string `json:"namingEffort,omitempty"`

	// BranchRules tell the naming model how to name a thread's branch; RepoBranchRules override them for
	// a repository (by path). With no rules, branches are named solo/<title>-<id>.
	BranchRules     string            `json:"branchRules,omitempty"`
	RepoBranchRules map[string]string `json:"repoBranchRules,omitempty"`

	// CommitRules tell every thread how to write its commit messages; they go in its instructions, as
	// threads make their own commits. Empty leaves them to the default (Conventional Commits).
	CommitRules string `json:"commitRules,omitempty"`

	// PRModel and PREffort write pull request titles and descriptions, following PRTitleRules and
	// PRDescriptionRules. With SkipPRDescription, pull requests get a title only.
	PRModel            string `json:"prModel,omitempty"`
	PREffort           string `json:"prEffort,omitempty"`
	PRTitleRules       string `json:"prTitleRules,omitempty"`
	PRDescriptionRules string `json:"prDescriptionRules,omitempty"`
	SkipPRDescription  bool   `json:"skipPRDescription,omitempty"`
	// PRSkills name Claude Code skills (see Skill) that write the pull request text, with the rules
	// above: a Claude Code agent loads them in the branch's worktree and can run their scripts. None by
	// default.
	PRSkills []string `json:"prSkills,omitempty"`
	// PRSkillMinutes is how long the agent writing the pull request text with PRSkills may take, in
	// minutes (PRSkillMinutesMin to PRSkillMinutesMax); 0 for the default. See PRSkillTimeout.
	PRSkillMinutes int `json:"prSkillMinutes,omitempty"`
	// PRRules are the rules for titles and descriptions together, from before they were split. They
	// are only read: normalize moves them to both PRTitleRules and PRDescriptionRules.
	PRRules string `json:"prRules,omitempty"`

	// AutoMerge turns on the forge's auto-merge when Solo opens a pull request, so it merges once its
	// required checks pass.
	AutoMerge bool `json:"autoMerge,omitempty"`
	// MergeMethod is how pull requests merge: squash, merge or rebase.
	MergeMethod string `json:"mergeMethod,omitempty"`
	// OnChecksFail is what happens when a pull request's checks fail: ChecksFix or ChecksNotify.
	OnChecksFail string `json:"onChecksFail,omitempty"`
}

// What happens when a pull request's checks fail.
const (
	ChecksFix    = "fix"    // Solo hears about it and usually has the thread fix it
	ChecksNotify = "notify" // the thread shows the failure and waits for the user
)

// DefaultSettings are used until the user changes them.
func DefaultSettings() Settings {
	s := Settings{ScanDepth: 3, OrchestratorModel: "opus", WorkerModel: "sonnet"}
	s.normalize()
	return s
}

// normalize fills settings added after the user's were saved.
func (s *Settings) normalize() {
	if s.NamingModel == "" {
		s.NamingModel = "haiku"
	}
	if s.PRModel == "" {
		s.PRModel = "haiku"
	}
	if s.MergeMethod == "" {
		s.MergeMethod = "squash"
	}
	if s.OnChecksFail == "" {
		s.OnChecksFail = ChecksFix
	}
	// One set of rules covered both title and description; it can't be split reliably, so it goes to
	// both, which is what it meant. A device still on the old setting writes it here too.
	if r := strings.TrimSpace(s.PRRules); r != "" {
		s.PRTitleRules, s.PRDescriptionRules = r, r
	}
	s.PRRules = ""
	s.PRSkills = cleanSkillNames(s.PRSkills)
	if s.PRSkillMinutes < PRSkillMinutesMin || s.PRSkillMinutes > PRSkillMinutesMax || s.PRSkillMinutes == PRSkillMinutesDefault {
		s.PRSkillMinutes = 0
	}
}

// The time the PR text agent may take, in minutes (Settings.PRSkillMinutes).
const (
	PRSkillMinutesDefault = 5
	PRSkillMinutesMin     = 1
	PRSkillMinutesMax     = 30
)

// PRStepTimeout is how long each step of opening a pull request may take besides its text: committing
// and pushing, and the forge's calls that open it.
const PRStepTimeout = 2 * time.Minute

// PRSkillTimeout is how long the agent writing the pull request text with skills may take: the
// default when PRSkillMinutes is unset or out of range.
func (s Settings) PRSkillTimeout() time.Duration {
	m := s.PRSkillMinutes
	if m < PRSkillMinutesMin || m > PRSkillMinutesMax {
		m = PRSkillMinutesDefault
	}
	return time.Duration(m) * time.Minute
}

// OpenPRTimeout is the longest opening pull requests in repos repositories takes on the machine:
// each repository's commit and push, text and forge calls, then linking them to each other.
func (s Settings) OpenPRTimeout(repos int) time.Duration {
	return time.Duration(max(repos, 1))*(2*PRStepTimeout+s.PRSkillTimeout()) + PRStepTimeout
}

// cleanSkillNames trims names of skills and drops empty and repeated ones.
func cleanSkillNames(names []string) []string {
	var out []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// BranchRulesFor returns the branch naming rules for a repository.
func (s Settings) BranchRulesFor(repo string) string {
	if r := strings.TrimSpace(s.RepoBranchRules[repo]); r != "" {
		return r
	}
	return strings.TrimSpace(s.BranchRules)
}

// ClaudeStatus is what Settings shows about Claude Code.
type ClaudeStatus struct {
	Path    string // the executable Solo runs; "" when none was found
	Source  string // how it was found: "setting", "PATH", "default location" or "bundled"
	Version string
	Account string // such as "you@example.com · Max"; "" when unknown
	Err     string // why Claude Code can't be used; "" when it can
	Models  []agent.ModelInfo
}

// SearchHit is a message that matched a search.
type SearchHit struct {
	ThreadID string
	ItemID   int64
	Kind     string // ItemUser or ItemAssistant
	Snippet  string // the text around the match, on one line
	At       time.Time
}

// UsedFraction is how much of a limit an event says is used, from 0 to 1, and whether it says. Claude
// Code reports a fraction, which can pass 1 when usage runs past the cap; that shows as all used.
func UsedFraction(e agent.Event) (float64, bool) {
	if e.Utilization == nil {
		return 0, false
	}
	return min(max(*e.Utilization, 0), 1), true
}
