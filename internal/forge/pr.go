// Package forge opens and follows pull requests on GitHub (through gh) and merge requests on GitLab
// (through glab), using the user's own CLI and its sign-in.
package forge

import "time"

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

// Check is a failed check, for telling the thread what to fix.
type Check struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Checks states. ChecksWaiting is a pull request the forge reports no checks on yet: right after it
// opens or gets new commits, a forge takes a few seconds to queue the checks that run on it, so no
// checks may still mean some are on their way. Solo's core settles it to ChecksNone once none can be.
const (
	ChecksNone    = "none"
	ChecksWaiting = "waiting"
	ChecksPending = "pending"
	ChecksPassing = "passing"
	ChecksFailing = "failing"
)

// CheckCounts counts a pull request's checks by where they stand. Passed counts every check that
// finished without failing, skipped and neutral ones included, so Passed+Failed+Pending is Total.
// GitLab reports one pipeline rather than its jobs, so its counts stay zero.
type CheckCounts struct {
	Total   int `json:"total,omitempty"`
	Passed  int `json:"passed,omitempty"`
	Failed  int `json:"failed,omitempty"`
	Pending int `json:"pending,omitempty"`
}

// PR states.
const (
	StateOpen   = "open"
	StateMerged = "merged"
	StateClosed = "closed"
)

// PR is a pull or merge request as Solo follows it.
type PR struct {
	Kind   Kind    `json:"kind"`
	URL    string  `json:"url"`
	Number int     `json:"number"`
	State  string  `json:"state"`  // StateOpen, StateMerged or StateClosed
	Checks string  `json:"checks"` // ChecksNone, ChecksWaiting, ChecksPending, ChecksPassing or ChecksFailing
	Failed []Check `json:"failed,omitempty"`
	// Counts says how many checks passed, failed and are still running.
	Counts    CheckCounts `json:"counts,omitzero"`
	AutoMerge bool        `json:"autoMerge,omitempty"`
	Draft     bool        `json:"draft,omitempty"`
	// Mergeable is true when it can merge now; Blocked says why not ("waiting for review", say).
	Mergeable bool   `json:"mergeable,omitempty"`
	Blocked   string `json:"blocked,omitempty"`
	// Head is the branch it merges from, as the forge reports it ("" before Solo read it).
	Head string `json:"head,omitempty"`
	// ChecksSince is when its head last changed, so its checks started over: when it opened, or new
	// commits were pushed to it. Zero when not known.
	ChecksSince time.Time `json:"checksSince,omitzero"`
}
