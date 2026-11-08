package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"

	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/git"
	"github.com/tyler-builds/solo/internal/store"
)

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

// part is a Part with what Solo learns at run time.
type part struct {
	Part
	changed int        // files its branch changes from where it started; -1 until counted
	forge   forge.Kind // where its pull requests go; found with the change count
}

func (p *part) prOpen() bool { return p.PR != nil && p.PR.State == forge.StateOpen }

// PartView is a snapshot of a Part, with what can be done with it.
type PartView struct {
	Part
	Name    string // the repository's folder name
	Changed int    // -1 until counted
	Forge   forge.Kind
	// CanOpenPR, CanMerge, CanMergeLocal and CanMarkReady are as on ThreadView, for this repository.
	CanOpenPR, CanMerge, CanMergeLocal, CanMarkReady bool
}

// loadParts reads a thread's parts from its row: the parts column, or, for rows written before
// threads could span repositories, its single repository's columns.
func loadParts(row store.Thread) []*part {
	var ps []Part
	if row.Parts != "" {
		_ = json.Unmarshal([]byte(row.Parts), &ps)
	}
	if len(ps) == 0 && row.RepoPath != "" {
		p := Part{Repo: row.RepoPath, Worktree: row.WorktreePath, Branch: row.Branch, BaseBranch: row.BaseBranch, BaseCommit: row.BaseCommit}
		if row.PR != "" {
			var pr forge.PR
			if json.Unmarshal([]byte(row.PR), &pr) == nil {
				p.PR = &pr
			}
		}
		ps = []Part{p}
	}
	out := make([]*part, len(ps))
	for i := range ps {
		out[i] = &part{Part: ps[i], changed: -1}
	}
	return out
}

// syncRow writes the parts into the row: all of them as JSON, and the first into the single-repository
// columns, which most of the app reads.
func (t *thread) syncRow() {
	if len(t.parts) == 0 {
		return
	}
	ps := make([]Part, len(t.parts))
	for i, p := range t.parts {
		ps[i] = p.Part
	}
	data, _ := json.Marshal(ps)
	t.row.Parts = string(data)
	p0 := t.parts[0]
	t.row.RepoPath, t.row.WorktreePath, t.row.Branch, t.row.BaseBranch, t.row.BaseCommit =
		p0.Repo, p0.Worktree, p0.Branch, p0.BaseBranch, p0.BaseCommit
	t.row.PR = ""
	if p0.PR != nil {
		data, _ := json.Marshal(p0.PR)
		t.row.PR = string(data)
	}
}

// saveLocked saves a thread. Call with c.mu held.
func (c *Core) saveLocked(t *thread) error {
	t.syncRow()
	return c.db.SaveThread(&t.row)
}

// part finds a thread's part by repository path or folder name.
func (t *thread) part(repo string) *part {
	for _, p := range t.parts {
		if p.Repo == repo || filepath.Base(p.Repo) == repo {
			return p
		}
	}
	return nil
}

// threadDir holds a multi-repository thread's worktrees, one folder per repository; its session runs
// there.
func (c *Core) threadDir(row store.Thread) string {
	return filepath.Join(c.worktreesDir(), "threads", git.Slug(row.Title)+"-"+row.ID[:6])
}

// partDirs are the folder names of a thread's repositories inside its threadDir: their names, made
// unique when two share one.
func partDirs(repos []string) []string {
	seen := map[string]int{}
	for _, r := range repos {
		seen[filepath.Base(r)]++
	}
	out := make([]string, len(repos))
	for i, r := range repos {
		name := filepath.Base(r)
		if seen[name] > 1 {
			h := sha256.Sum256([]byte(r))
			name += "-" + hex.EncodeToString(h[:2])
		}
		out[i] = name
	}
	return out
}

// partWorktreePath is where part i of a thread lives. A single-repository thread keeps the layout it
// always had; it depends only on the thread, so Restore finds the same path (and with it the Claude
// session, which is keyed by directory).
func (c *Core) partWorktreePath(row store.Thread, repos []string, i int) string {
	if len(repos) == 1 {
		return c.worktreePath(repos[0], git.Slug(row.Title)+"-"+row.ID[:6])
	}
	return filepath.Join(c.threadDir(row), partDirs(repos)[i])
}

func (t *thread) repos() []string {
	out := make([]string, len(t.parts))
	for i, p := range t.parts {
		out[i] = p.Repo
	}
	return out
}

// sessionDir is where a worker's session runs: its worktree, or for several repositories the folder
// that holds their worktrees.
func (c *Core) sessionDir(row store.Thread) string {
	ps := loadParts(row)
	if len(ps) > 1 {
		return c.threadDir(row)
	}
	if len(ps) == 1 && ps[0].InPlace {
		return ps[0].Repo
	}
	return row.WorktreePath
}

// inPlace reports whether a thread works directly in a folder that isn't in git.
func (t *thread) inPlace() bool { return len(t.parts) == 1 && t.parts[0].InPlace }
