// Package forge opens and follows pull requests on GitHub (through gh) and merge requests on GitLab
// (through glab), using the user's own CLI and its sign-in.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Kind is a forge.
type Kind string

const (
	None   Kind = ""
	GitHub Kind = "github"
	GitLab Kind = "gitlab"
)

// Noun is what the forge calls a change request: "PR" or "MR".
func (k Kind) Noun() string {
	if k == GitLab {
		return "MR"
	}
	return "PR"
}

// Detect finds the forge from origin's URL. A repository can name it with `git config solo.forge
// github|gitlab`, for self-hosted forges whose host doesn't say.
func Detect(override, remoteURL string) Kind {
	switch Kind(strings.ToLower(strings.TrimSpace(override))) {
	case GitHub:
		return GitHub
	case GitLab:
		return GitLab
	}
	host := remoteHost(remoteURL)
	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		return GitHub
	case strings.Contains(host, "gitlab"):
		return GitLab
	}
	return None
}

// remoteHost is the host of a git remote URL: https://host/…, ssh://git@host/…, or git@host:….
func remoteHost(u string) string {
	u = strings.TrimSpace(u)
	if strings.Contains(u, "://") {
		if p, err := url.Parse(u); err == nil {
			return strings.ToLower(p.Hostname())
		}
		return ""
	}
	if at := strings.Index(u, "@"); at >= 0 {
		if colon := strings.Index(u[at:], ":"); colon > 0 {
			return strings.ToLower(u[at+1 : at+colon])
		}
	}
	return ""
}

// Check is a failed check, for telling the thread what to fix.
type Check struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Checks states.
const (
	ChecksNone    = "none"
	ChecksPending = "pending"
	ChecksPassing = "passing"
	ChecksFailing = "failing"
)

// PR states.
const (
	StateOpen   = "open"
	StateMerged = "merged"
	StateClosed = "closed"
)

// PR is a pull or merge request as Solo follows it.
type PR struct {
	Kind      Kind    `json:"kind"`
	URL       string  `json:"url"`
	Number    int     `json:"number"`
	State     string  `json:"state"`  // StateOpen, StateMerged or StateClosed
	Checks    string  `json:"checks"` // ChecksNone, ChecksPending, ChecksPassing or ChecksFailing
	Failed    []Check `json:"failed,omitempty"`
	AutoMerge bool    `json:"autoMerge,omitempty"`
	// Mergeable is true when it can merge now; Blocked says why not ("waiting for review", say).
	Mergeable bool   `json:"mergeable,omitempty"`
	Blocked   string `json:"blocked,omitempty"`
}

// Client drives one forge's CLI.
type Client struct {
	Kind Kind
	Path string // the CLI
}

// ErrNoCLI is returned when the forge's CLI isn't installed.
var ErrNoCLI = errors.New("not installed")

// New finds the forge's CLI (gh or glab).
func New(k Kind) (*Client, error) {
	name := map[Kind]string{GitHub: "gh", GitLab: "glab"}[k]
	if name == "" {
		return nil, errors.New("this repository has no GitHub or GitLab remote")
	}
	p := FindTool(name)
	if p == "" {
		return nil, fmt.Errorf("%s %w: install it and sign in (%s auth login)", name, ErrNoCLI, name)
	}
	return &Client{Kind: k, Path: p}, nil
}

// FindTool finds a CLI on PATH or where package managers put it; apps started from a desktop launcher
// often have a short PATH.
func FindTool(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{filepath.Join(home, ".local", "bin"), "/home/linuxbrew/.linuxbrew/bin",
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", filepath.Join(home, "bin")} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func (c *Client) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Path, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "NO_COLOR=1", "GLAB_NO_PROMPT=1")
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return s, fmt.Errorf("%s %s: %s", filepath.Base(c.Path), args[0], lastLines(s, 6))
	}
	return s, nil
}

var urlRE = regexp.MustCompile(`https?://\S+/(?:pull|merge_requests)/(\d+)`)

// Open opens a pull request from head into base, from the worktree dir.
func (c *Client) Open(ctx context.Context, dir, base, head, title, body string) (PR, error) {
	var out string
	var err error
	if c.Kind == GitLab {
		out, err = c.run(ctx, dir, "mr", "create", "--source-branch", head, "--target-branch", base,
			"--title", title, "--description", body, "--yes")
	} else {
		out, err = c.run(ctx, dir, "pr", "create", "--base", base, "--head", head, "--title", title, "--body", body)
	}
	if err != nil {
		return PR{}, err
	}
	m := urlRE.FindStringSubmatch(out)
	if m == nil {
		return PR{}, fmt.Errorf("couldn't find the %s's address in: %s", c.Kind.Noun(), lastLines(out, 3))
	}
	n, _ := strconv.Atoi(m[1])
	pr := PR{Kind: c.Kind, URL: m[0], Number: n, State: StateOpen, Checks: ChecksPending}
	if v, err := c.View(ctx, dir, pr); err == nil {
		pr = v
	}
	return pr, nil
}

// methodFlag is the CLI flag for a merge method.
func methodFlag(method string) string {
	switch method {
	case "merge":
		return "--merge"
	case "rebase":
		return "--rebase"
	}
	return "--squash"
}

// EnableAutoMerge has the forge merge the pull request once its requirements pass.
func (c *Client) EnableAutoMerge(ctx context.Context, dir string, pr PR, method string) error {
	if c.Kind == GitLab {
		args := []string{"mr", "merge", strconv.Itoa(pr.Number), "--yes", "--auto-merge"}
		if method == "squash" || method == "rebase" {
			args = append(args, methodFlag(method))
		}
		_, err := c.run(ctx, dir, args...)
		if err != nil && strings.Contains(err.Error(), "unknown flag") { // older glab
			args[4] = "--when-pipeline-succeeds"
			_, err = c.run(ctx, dir, args...)
		}
		return err
	}
	_, err := c.run(ctx, dir, "pr", "merge", pr.URL, "--auto", methodFlag(method))
	return err
}

// Merge merges the pull request now.
func (c *Client) Merge(ctx context.Context, dir string, pr PR, method string) error {
	if c.Kind == GitLab {
		args := []string{"mr", "merge", strconv.Itoa(pr.Number), "--yes", "--auto-merge=false"}
		if method == "squash" || method == "rebase" {
			args = append(args, methodFlag(method))
		}
		_, err := c.run(ctx, dir, args...)
		if err != nil && strings.Contains(err.Error(), "unknown flag") { // older glab merges at once
			_, err = c.run(ctx, dir, append(args[:4], args[5:]...)...)
		}
		return err
	}
	_, err := c.run(ctx, dir, "pr", "merge", pr.URL, methodFlag(method))
	return err
}

// View reads the pull request's state, checks and mergeability.
func (c *Client) View(ctx context.Context, dir string, pr PR) (PR, error) {
	if c.Kind == GitLab {
		return c.viewGitLab(ctx, dir, pr)
	}
	out, err := c.run(ctx, dir, "pr", "view", pr.URL, "--json",
		"number,url,state,statusCheckRollup,autoMergeRequest,mergeStateStatus,reviewDecision")
	if err != nil {
		return pr, err
	}
	var v struct {
		Number           int             `json:"number"`
		URL              string          `json:"url"`
		State            string          `json:"state"`
		AutoMergeRequest json.RawMessage `json:"autoMergeRequest"`
		MergeStateStatus string          `json:"mergeStateStatus"`
		ReviewDecision   string          `json:"reviewDecision"`
		Rollup           []struct {
			Typename   string `json:"__typename"`
			Name       string `json:"name"`
			Context    string `json:"context"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
			DetailsURL string `json:"detailsUrl"`
			TargetURL  string `json:"targetUrl"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return pr, fmt.Errorf("reading gh's answer: %w", err)
	}
	pr.Kind, pr.Number, pr.URL = GitHub, v.Number, v.URL
	pr.State = map[string]string{"OPEN": StateOpen, "MERGED": StateMerged, "CLOSED": StateClosed}[v.State]
	pr.AutoMerge = len(v.AutoMergeRequest) > 0 && string(v.AutoMergeRequest) != "null"
	pending, failed := false, []Check(nil)
	for _, r := range v.Rollup {
		name, link, state := r.Name, r.DetailsURL, ""
		if r.Typename == "StatusContext" {
			name, link, state = r.Context, r.TargetURL, r.State
		} else if r.Status != "COMPLETED" {
			state = "PENDING"
		} else {
			state = r.Conclusion
		}
		switch state {
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
			failed = append(failed, Check{Name: name, URL: link})
		case "PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "":
			pending = true
		}
	}
	pr.Failed = failed
	switch {
	case len(failed) > 0:
		pr.Checks = ChecksFailing
	case pending:
		pr.Checks = ChecksPending
	case len(v.Rollup) > 0:
		pr.Checks = ChecksPassing
	default:
		pr.Checks = ChecksNone
	}
	pr.Mergeable, pr.Blocked = false, ""
	switch v.MergeStateStatus {
	case "CLEAN", "HAS_HOOKS", "UNSTABLE":
		pr.Mergeable = true
	case "BEHIND":
		pr.Blocked = "behind its base branch"
	case "DIRTY":
		pr.Blocked = "has conflicts"
	case "BLOCKED":
		switch {
		case pr.Checks == ChecksFailing:
			pr.Blocked = "checks failed"
		case pr.Checks == ChecksPending:
			pr.Blocked = "waiting for checks"
		case v.ReviewDecision == "REVIEW_REQUIRED" || v.ReviewDecision == "CHANGES_REQUESTED":
			pr.Blocked = "waiting for review"
		default:
			pr.Blocked = "blocked by the repository's rules"
		}
	default:
		pr.Blocked = "GitHub is still working out whether it can merge"
	}
	return pr, nil
}

func (c *Client) viewGitLab(ctx context.Context, dir string, pr PR) (PR, error) {
	out, err := c.run(ctx, dir, "mr", "view", strconv.Itoa(pr.Number), "--output", "json")
	if err != nil {
		return pr, err
	}
	var v struct {
		IID                 int    `json:"iid"`
		WebURL              string `json:"web_url"`
		State               string `json:"state"`
		MergeWhenPipeline   bool   `json:"merge_when_pipeline_succeeds"`
		DetailedMergeStatus string `json:"detailed_merge_status"`
		HeadPipeline        *struct {
			Status string `json:"status"`
			WebURL string `json:"web_url"`
		} `json:"head_pipeline"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return pr, fmt.Errorf("reading glab's answer: %w", err)
	}
	pr.Kind, pr.Number, pr.URL = GitLab, v.IID, v.WebURL
	pr.State = map[string]string{"opened": StateOpen, "merged": StateMerged, "closed": StateClosed, "locked": StateOpen}[v.State]
	pr.AutoMerge = v.MergeWhenPipeline
	pr.Failed = nil
	pr.Checks = ChecksNone
	if p := v.HeadPipeline; p != nil {
		switch p.Status {
		case "success":
			pr.Checks = ChecksPassing
		case "failed", "canceled":
			pr.Checks = ChecksFailing
			pr.Failed = []Check{{Name: "pipeline", URL: p.WebURL}}
		case "skipped", "manual":
			pr.Checks = ChecksNone
		default:
			pr.Checks = ChecksPending
		}
	}
	pr.Mergeable = v.DetailedMergeStatus == "mergeable"
	pr.Blocked = map[string]string{
		"ci_must_pass": "waiting for the pipeline", "ci_still_running": "waiting for the pipeline",
		"not_approved": "waiting for approval", "conflict": "has conflicts", "need_rebase": "needs a rebase",
		"draft_status": "is a draft", "discussions_not_resolved": "has open discussions",
	}[v.DetailedMergeStatus]
	if !pr.Mergeable && pr.Blocked == "" {
		pr.Blocked = "GitLab can't merge it yet"
	}
	return pr, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " ")
}
