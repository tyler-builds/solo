//go:build !js

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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/proc"
)

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
	cmd := proc.CommandContext(ctx, c.Path, args...)
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

// FindURLs finds the pull and merge request addresses in text, such as what `gh pr create` printed,
// each once, in order.
func FindURLs(text string) []string {
	var out []string
	for _, m := range urlRE.FindAllString(text, -1) {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// Number is the number at the end of a pull request's address, or 0.
func Number(prURL string) int {
	m := urlRE.FindStringSubmatch(prURL)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// SameRepo reports whether a pull request's address belongs to the repository at remoteURL (origin's
// URL in any of its forms: https, ssh:// or git@host:path).
func SameRepo(prURL, remoteURL string) bool {
	pr := prRepo(prURL)
	return pr != "" && pr == remoteRepo(remoteURL)
}

// KnownRemote reports whether a remote URL names its repository (host and path), so SameRepo can
// tell; a local path or a file:// remote doesn't.
func KnownRemote(remoteURL string) bool { return remoteRepo(remoteURL) != "" }

// prRepo is "host/owner/name" of a pull request's address.
func prRepo(prURL string) string {
	u, err := url.Parse(strings.TrimSpace(prURL))
	if err != nil || u.Host == "" {
		return ""
	}
	path := u.Path
	for _, sep := range []string{"/-/merge_requests/", "/merge_requests/", "/pull/"} {
		if i := strings.Index(path, sep); i >= 0 {
			path = path[:i]
			return strings.ToLower(u.Hostname() + "/" + strings.Trim(path, "/"))
		}
	}
	return ""
}

// remoteRepo is "host/owner/name" of a git remote URL.
func remoteRepo(remote string) string {
	remote = strings.TrimSpace(remote)
	host := remoteHost(remote)
	if host == "" {
		return ""
	}
	var path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		path = u.Path
	} else if _, rest, ok := strings.Cut(remote, ":"); ok {
		path = rest
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if path == "" {
		return ""
	}
	return strings.ToLower(host + "/" + path)
}

// Listed is a pull request found by its branch, with when it was opened.
type Listed struct {
	PR
	Created time.Time
}

// ByHead lists the pull requests from branch head, in any state, newest first.
func (c *Client) ByHead(ctx context.Context, dir, head string) ([]Listed, error) {
	if c.Kind == GitLab {
		out, err := c.run(ctx, dir, "mr", "list", "--source-branch", head, "--all", "--output", "json")
		if err != nil {
			return nil, err
		}
		var v []struct {
			IID     int       `json:"iid"`
			WebURL  string    `json:"web_url"`
			State   string    `json:"state"`
			Draft   bool      `json:"draft"`
			Source  string    `json:"source_branch"`
			Created time.Time `json:"created_at"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			return nil, fmt.Errorf("reading glab's answer: %w", err)
		}
		var prs []Listed
		for _, m := range v {
			prs = append(prs, Listed{PR: PR{Kind: GitLab, URL: m.WebURL, Number: m.IID, State: gitlabState[m.State],
				Draft: m.Draft, Head: m.Source}, Created: m.Created})
		}
		return newestFirst(prs), nil
	}
	out, err := c.run(ctx, dir, "pr", "list", "--head", head, "--state", "all", "--limit", "10",
		"--json", "number,url,state,isDraft,headRefName,createdAt")
	if err != nil {
		return nil, err
	}
	var v []struct {
		Number  int       `json:"number"`
		URL     string    `json:"url"`
		State   string    `json:"state"`
		IsDraft bool      `json:"isDraft"`
		Head    string    `json:"headRefName"`
		Created time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("reading gh's answer: %w", err)
	}
	var prs []Listed
	for _, p := range v {
		prs = append(prs, Listed{PR: PR{Kind: GitHub, URL: p.URL, Number: p.Number, State: githubState[p.State],
			Draft: p.IsDraft, Head: p.Head}, Created: p.Created})
	}
	return newestFirst(prs), nil
}

func newestFirst(prs []Listed) []Listed {
	slices.SortStableFunc(prs, func(a, b Listed) int { return b.Created.Compare(a.Created) })
	return prs
}

var (
	githubState = map[string]string{"OPEN": StateOpen, "MERGED": StateMerged, "CLOSED": StateClosed}
	gitlabState = map[string]string{"opened": StateOpen, "merged": StateMerged, "closed": StateClosed, "locked": StateOpen}
)

// Open opens a pull request from head into base, from the worktree dir; draft opens it as a draft.
func (c *Client) Open(ctx context.Context, dir, base, head, title, body string, draft bool) (PR, error) {
	var args []string
	if c.Kind == GitLab {
		args = []string{"mr", "create", "--source-branch", head, "--target-branch", base,
			"--title", title, "--description", body, "--yes"}
	} else {
		args = []string{"pr", "create", "--base", base, "--head", head, "--title", title, "--body", body}
	}
	if draft {
		args = append(args, "--draft")
	}
	out, err := c.run(ctx, dir, args...)
	if err != nil {
		return PR{}, err
	}
	m := urlRE.FindStringSubmatch(out)
	if m == nil {
		return PR{}, fmt.Errorf("couldn't find the %s's address in: %s", c.Kind.Noun(), lastLines(out, 3))
	}
	n, _ := strconv.Atoi(m[1])
	pr := PR{Kind: c.Kind, URL: m[0], Number: n, State: StateOpen, Checks: ChecksPending, Draft: draft}
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

// EditBody replaces a pull request's description.
func (c *Client) EditBody(ctx context.Context, dir string, pr PR, body string) error {
	if c.Kind == GitLab {
		_, err := c.run(ctx, dir, "mr", "update", strconv.Itoa(pr.Number), "--description", body, "--yes")
		return err
	}
	_, err := c.run(ctx, dir, "pr", "edit", pr.URL, "--body", body)
	return err
}

// Ready marks a draft pull request ready for review.
func (c *Client) Ready(ctx context.Context, dir string, pr PR) error {
	if c.Kind == GitLab {
		_, err := c.run(ctx, dir, "mr", "update", strconv.Itoa(pr.Number), "--ready", "--yes")
		return err
	}
	_, err := c.run(ctx, dir, "pr", "ready", pr.URL)
	return err
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
		"number,url,state,isDraft,statusCheckRollup,autoMergeRequest,mergeStateStatus,reviewDecision,headRefName")
	if err != nil {
		return pr, err
	}
	var v struct {
		Number           int             `json:"number"`
		URL              string          `json:"url"`
		State            string          `json:"state"`
		IsDraft          bool            `json:"isDraft"`
		AutoMergeRequest json.RawMessage `json:"autoMergeRequest"`
		MergeStateStatus string          `json:"mergeStateStatus"`
		ReviewDecision   string          `json:"reviewDecision"`
		Head             string          `json:"headRefName"`
		Rollup           []rollupEntry   `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return pr, fmt.Errorf("reading gh's answer: %w", err)
	}
	pr.Kind, pr.Number, pr.URL = GitHub, v.Number, v.URL
	pr.State = githubState[v.State]
	if v.Head != "" {
		pr.Head = v.Head
	}
	pr.AutoMerge = len(v.AutoMergeRequest) > 0 && string(v.AutoMergeRequest) != "null"
	pr.Draft = v.IsDraft
	failed, counts := tallyChecks(v.Rollup)
	pr.Failed, pr.Counts = failed, counts
	switch {
	case len(failed) > 0:
		pr.Checks = ChecksFailing
	case counts.Pending > 0:
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
	case "DRAFT":
		pr.Blocked = "is a draft"
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

// rollupEntry is one check of gh's statusCheckRollup: a check run, or a commit status (StatusContext).
type rollupEntry struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
	DetailsURL string `json:"detailsUrl"`
	TargetURL  string `json:"targetUrl"`
}

// tallyChecks counts a pull request's checks by where they stand and lists the failed ones.
func tallyChecks(rollup []rollupEntry) ([]Check, CheckCounts) {
	var failed []Check
	var counts CheckCounts
	for _, r := range rollup {
		name, link, state := r.Name, r.DetailsURL, ""
		if r.Typename == "StatusContext" {
			name, link, state = r.Context, r.TargetURL, r.State
		} else if r.Status != "COMPLETED" {
			state = "PENDING"
		} else {
			state = r.Conclusion
		}
		counts.Total++
		switch state {
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
			failed = append(failed, Check{Name: name, URL: link})
			counts.Failed++
		case "PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "":
			counts.Pending++
		default: // SUCCESS, NEUTRAL, SKIPPED
			counts.Passed++
		}
	}
	return failed, counts
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
		Draft               bool   `json:"draft"`
		MergeWhenPipeline   bool   `json:"merge_when_pipeline_succeeds"`
		DetailedMergeStatus string `json:"detailed_merge_status"`
		Source              string `json:"source_branch"`
		HeadPipeline        *struct {
			Status string `json:"status"`
			WebURL string `json:"web_url"`
		} `json:"head_pipeline"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return pr, fmt.Errorf("reading glab's answer: %w", err)
	}
	pr.Kind, pr.Number, pr.URL = GitLab, v.IID, v.WebURL
	pr.State = gitlabState[v.State]
	if v.Source != "" {
		pr.Head = v.Source
	}
	pr.AutoMerge = v.MergeWhenPipeline
	pr.Draft = v.Draft
	pr.Failed, pr.Counts = nil, CheckCounts{}
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
