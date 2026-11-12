// Package git finds repositories and manages the worktrees worker threads run in, through the git CLI.
package git

// FileChange is one changed file.
type FileChange struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // A, M, D, R, ?
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}
