//go:build !js

package core

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/git"
)

// Limits of SearchPaths' lists of a folder.
const (
	maxPathEntries = 25_000           // files and folders listed in one working folder
	pathListTTL    = 10 * time.Second // how long a folder's list is reused, while the user types
)

var pathLists struct {
	sync.Mutex
	m map[string]pathList
}

type pathList struct {
	at      time.Time
	entries []PathHit
}

// SearchPaths finds the files and folders in a thread's working folder that match query (see
// MatchScore), best first, at most limit of them (up to MaxPathHits). Files git ignores are left out.
// An empty query lists the folder's top level, folders first.
func (c *Core) SearchPaths(ctx context.Context, threadID, query string, limit int) ([]PathHit, error) {
	c.mu.Lock()
	t := c.threads[threadID]
	dir := ""
	if t != nil {
		dir = c.sessionDir(t.row)
	}
	c.mu.Unlock()
	if t == nil || threadID == OrchestratorID {
		return nil, fmt.Errorf("no thread %s", threadID)
	}
	if dir == "" || !isDir(dir) {
		return nil, fmt.Errorf("the thread's folder isn't there")
	}
	entries, err := listPathsCached(ctx, dir)
	if err != nil {
		return nil, err
	}
	return rankPaths(entries, query, min(max(limit, 1), MaxPathHits)), nil
}

func listPathsCached(ctx context.Context, dir string) ([]PathHit, error) {
	pathLists.Lock()
	l, ok := pathLists.m[dir]
	pathLists.Unlock()
	if ok && time.Since(l.at) < pathListTTL {
		return l.entries, nil
	}
	entries, err := listPaths(ctx, dir)
	if err != nil {
		return nil, err
	}
	pathLists.Lock()
	if pathLists.m == nil {
		pathLists.m = map[string]pathList{}
	}
	for k, v := range pathLists.m { // keep the cache small
		if time.Since(v.at) >= pathListTTL {
			delete(pathLists.m, k)
		}
	}
	pathLists.m[dir] = pathList{time.Now(), entries}
	pathLists.Unlock()
	return entries, nil
}

// rankPaths picks the entries matching query, best first.
func rankPaths(entries []PathHit, query string, limit int) []PathHit {
	query = strings.Trim(strings.ReplaceAll(strings.TrimSpace(query), "\\", "/"), "\"")
	if strings.HasPrefix(query, "./") {
		query = query[2:]
	}
	if query == "" {
		var top []PathHit
		for _, e := range entries {
			if !strings.Contains(e.Path, "/") {
				top = append(top, e)
			}
		}
		slices.SortFunc(top, func(a, b PathHit) int {
			if a.Dir != b.Dir {
				if a.Dir {
					return -1
				}
				return 1
			}
			return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
		})
		return top[:min(len(top), limit)]
	}
	type scored struct {
		PathHit
		score int
	}
	var hits []scored
	for _, e := range entries {
		if s, ok := MatchScore(query, e.Path); ok {
			hits = append(hits, scored{e, s})
		}
	}
	slices.SortFunc(hits, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return strings.Compare(a.Path, b.Path)
	})
	out := make([]PathHit, 0, min(len(hits), limit))
	for _, h := range hits[:min(len(hits), limit)] {
		out = append(out, h.PathHit)
	}
	return out
}

// listPaths lists the files and folders under dir, with paths relative to it. A git repository (dir,
// or a folder in it, such as each repository of a thread across several) lists what git does: its
// files, tracked or not, less those it ignores. Elsewhere every file is listed, but .git and
// node_modules folders.
func listPaths(ctx context.Context, dir string) ([]PathHit, error) {
	var files []string
	var walk func(abs, rel string) error
	walk = func(abs, rel string) error {
		if len(files) >= maxPathEntries {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil {
			out, err := git.Run(ctx, abs, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
			if err == nil {
				for _, f := range strings.Split(out, "\x00") {
					if f != "" && len(files) < maxPathEntries {
						files = append(files, path.Join(rel, f))
					}
				}
				return nil
			}
			// Not a repository after all (or git failed): list it as a folder.
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			if rel == "" {
				return err
			}
			return nil
		}
		for _, e := range entries {
			n := e.Name()
			if n == ".git" || n == "node_modules" {
				continue
			}
			r := path.Join(rel, n)
			if e.IsDir() {
				files = append(files, r+"/")
				if err := walk(filepath.Join(abs, n), r); err != nil {
					return err
				}
				continue
			}
			if e.Type()&fs.ModeType == 0 || e.Type()&os.ModeSymlink != 0 {
				files = append(files, r)
			}
		}
		return nil
	}
	if err := walk(dir, ""); err != nil {
		return nil, err
	}
	// Folders are the files' parents (and, outside git, the folders walked, which end in "/").
	seen := map[string]bool{}
	var out []PathHit
	for _, f := range files {
		if d, ok := strings.CutSuffix(f, "/"); ok {
			if !seen[d] {
				seen[d] = true
				out = append(out, PathHit{Path: d, Dir: true})
			}
			continue
		}
		for p := path.Dir(f); p != "." && p != "/" && !seen[p]; p = path.Dir(p) {
			seen[p] = true
			out = append(out, PathHit{Path: p, Dir: true})
		}
		out = append(out, PathHit{Path: f})
	}
	return out, nil
}
