package gui

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/markdown"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/launch"
)

// filePath is a path to a file or folder found in text: where it is in the text, the file it names,
// and the line it points at (0 for none).
type filePath struct {
	start, end int
	path       string
	line       int
}

// pathRe matches what may be a path: absolute (/a/b, ~/a, C:\a), or relative with a folder in it
// (a/b.go, ./b.go). Only those that name a file that is there become links (see findPaths).
var pathRe = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|~/|\.{1,2}/|/)?[\w@+%~-][\w.@+%~-]*(?:[\\/][\w.@+%~-]+)+[\\/]?`)

// lineRe is a line after a path: file.go:12, file.go:12:4 or file.go#L12.
var lineRe = regexp.MustCompile(`^(?::(\d+)(?::\d+)?|#L(\d+))`)

// findPaths returns the paths in s that name a file or folder that is there. A relative path is
// looked up in each folder of bases, in order.
func findPaths(s string, bases []string) []filePath {
	var out []filePath
	for _, m := range pathRe.FindAllStringIndex(s, -1) {
		start, end := m[0], m[1]
		// Inside a word or a URL (https://host/a/b) it is not a path of its own.
		if start > 0 && strings.ContainsRune(`:/\.~-@`, rune(s[start-1])) || start > 0 && isWord(s[start-1]) {
			continue
		}
		raw := strings.TrimRight(s[start:end], ".,")
		end = start + len(raw)
		p := resolvePath(raw, bases)
		if p == "" {
			continue
		}
		fp := filePath{start: start, end: end, path: p}
		if lm := lineRe.FindStringSubmatch(s[end:]); lm != nil {
			n := lm[1] + lm[2]
			fp.line, _ = strconv.Atoi(n)
			fp.end += len(lm[0])
		}
		out = append(out, fp)
	}
	return out
}

func isWord(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// resolvePath returns the absolute path raw names, or "" when no file is there.
func resolvePath(raw string, bases []string) string {
	switch {
	case strings.HasPrefix(raw, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		raw = filepath.Join(home, raw[2:])
	case filepath.IsAbs(raw) || strings.HasPrefix(raw, "/"):
	default:
		for _, b := range bases {
			if b == "" {
				continue
			}
			if p := filepath.Join(b, raw); exists(p) {
				return p
			}
		}
		return ""
	}
	p := filepath.Clean(raw)
	// The root alone, or a folder right under it (/tmp, /s), is more likely prose than a path.
	if strings.Count(filepath.ToSlash(p), "/") < 2 || !exists(p) {
		return ""
	}
	return p
}

// statCache remembers which paths are there, so a transcript doesn't stat its paths on every frame.
// A path that isn't there is asked again after a while: an agent may be about to write it.
var statCache sync.Map // string → statEntry

type statEntry struct {
	ok bool
	at time.Time
}

func exists(p string) bool {
	if v, ok := statCache.Load(p); ok {
		e := v.(statEntry)
		if e.ok || time.Since(e.at) < 3*time.Second {
			return e.ok
		}
	}
	_, err := os.Stat(p)
	statCache.Store(p, statEntry{ok: err == nil, at: time.Now()})
	return err == nil
}

// fileURL is the link a path in a message becomes.
func fileURL(fp filePath) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(fp.path)}
	if fp.line > 0 {
		u.Fragment = "L" + strconv.Itoa(fp.line)
	}
	return u.String()
}

// fenceRe opens or closes a fenced code block.
var fenceRe = regexp.MustCompile("^\\s{0,3}(```|~~~)")

// skipRe is what linkMarkdown leaves alone in a line: links and images, autolinks and URLs, and
// code spans.
var skipRe = regexp.MustCompile("!?\\[[^\\]]*\\]\\([^)]*\\)|<[^>\\s]+>|https?://\\S+|`[^`]+`")

// linkMarkdown makes each path in markdown text a link to the file, and a code span that is just a
// path a link around the code. Code blocks, links and URLs stay as they are.
func linkMarkdown(md string, bases []string) string {
	if !strings.ContainsAny(md, "/\\") {
		return md
	}
	lines := strings.Split(md, "\n")
	fence := ""
	for i, ln := range lines {
		if m := fenceRe.FindStringSubmatch(ln); m != nil {
			switch {
			case fence == "":
				fence = m[1]
			case fence == m[1]:
				fence = ""
			}
			continue
		}
		if fence != "" || strings.HasPrefix(ln, "    ") || strings.HasPrefix(ln, "\t") {
			continue
		}
		lines[i] = linkLine(ln, bases)
	}
	return strings.Join(lines, "\n")
}

func linkLine(ln string, bases []string) string {
	var b strings.Builder
	last := 0
	for _, m := range skipRe.FindAllStringIndex(ln, -1) {
		b.WriteString(linkPlain(ln[last:m[0]], bases))
		tok := ln[m[0]:m[1]]
		if strings.HasPrefix(tok, "`") {
			inner := strings.TrimSpace(strings.Trim(tok, "`"))
			if ps := findPaths(inner, bases); len(ps) == 1 && ps[0].start == 0 && ps[0].end == len(inner) {
				tok = "[" + tok + "](<" + fileURL(ps[0]) + ">)"
			}
		}
		b.WriteString(tok)
		last = m[1]
	}
	b.WriteString(linkPlain(ln[last:], bases))
	return b.String()
}

func linkPlain(s string, bases []string) string {
	ps := findPaths(s, bases)
	if len(ps) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, p := range ps {
		b.WriteString(s[last:p.start])
		b.WriteString("[" + escapeLinkText(s[p.start:p.end]) + "](<" + fileURL(p) + ">)")
		last = p.end
	}
	b.WriteString(s[last:])
	return b.String()
}

// escapeLinkText keeps markdown from reading a path's _ * [ ] as emphasis or brackets.
func escapeLinkText(s string) string {
	return strings.NewReplacer(`\`, `\\`, `_`, `\_`, `*`, `\*`, `[`, `\[`, `]`, `\]`).Replace(s)
}

// pathFromLink returns the file a link in a message points at: a file:// link, or a path (as an
// agent writes [chat.go](internal/gui/chat.go:30)). ok is false for a web link.
func pathFromLink(link string, bases []string) (fp filePath, ok bool) {
	if u, err := url.Parse(link); err == nil && u.Scheme == "file" {
		fp.path = filepath.FromSlash(u.Path)
		if len(fp.path) > 2 && fp.path[0] == filepath.Separator && fp.path[2] == ':' {
			fp.path = fp.path[1:] // /C:/x on Windows
		}
		fp.line, _ = strconv.Atoi(strings.TrimPrefix(u.Fragment, "L"))
		return fp, true
	} else if err == nil && u.Scheme != "" && len(u.Scheme) > 1 {
		return fp, false
	}
	if ps := findPaths(link, bases); len(ps) == 1 && ps[0].start == 0 {
		return ps[0], true
	}
	return fp, false
}

// threadBases are the folders a thread's relative paths are in: its folder and each worktree.
func (a *App) threadBases(threadID string) []string {
	for _, th := range a.threads.Peek() {
		if th.ID == threadID {
			return viewBases(th)
		}
	}
	return nil
}

func viewBases(th core.ThreadView) []string {
	bases := []string{th.Dir}
	for _, p := range th.Parts {
		bases = append(bases, p.Worktree)
	}
	return bases
}

// md is a message in markdown with its paths as links: a click opens the file, and a right click
// offers what to do with each of them.
func (a *App) md(ctx *ui.Context, text string, bases []string) ui.Element {
	view := markdown.View(linkMarkdown(text, bases)).Code(code).Key("md").OnLink(func(link string) {
		if fp, ok := pathFromLink(link, bases); ok {
			a.openPath(fp)
		} else if err := ctx.OpenURL(link); err != nil {
			a.notify(ui.ToastError, "Couldn't open the link", err.Error())
		}
	})
	return a.withPathMenu(view, text, bases)
}

// hardBreaks keeps each line of what the user typed on its own line: markdown would join a line
// to the one before it. Fenced code is left as it is.
func hardBreaks(s string) string {
	lines := strings.Split(s, "\n")
	fence := ""
	for i, l := range lines[:len(lines)-1] {
		t := strings.TrimSpace(l)
		switch {
		case fence != "":
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		case strings.HasPrefix(t, "```"):
			fence = "```"
			continue
		case strings.HasPrefix(t, "~~~"):
			fence = "~~~"
			continue
		}
		if t != "" && strings.TrimSpace(lines[i+1]) != "" {
			lines[i] = strings.TrimRight(l, " \t") + "  "
		}
	}
	return strings.Join(lines, "\n")
}

// pathText is plain text with its paths as links.
func (a *App) pathText(s string, bases []string) *ui.TextElement {
	ps := findPaths(s, bases)
	if len(ps) == 0 {
		return ui.Text(s)
	}
	var segs []ui.Segment
	last := 0
	for _, p := range ps {
		if p.start > last {
			segs = append(segs, ui.Seg(s[last:p.start]))
		}
		segs = append(segs, ui.Link(s[p.start:p.end], func() { a.openPath(p) }))
		last = p.end
	}
	if last < len(s) {
		segs = append(segs, ui.Seg(s[last:]))
	}
	return ui.Rich(segs...)
}

// withPathMenu wraps e so that a right click on it offers what to do with the paths in text.
func (a *App) withPathMenu(e ui.Element, text string, bases []string) ui.Element {
	if !strings.ContainsAny(text, "/\\") {
		return e
	}
	return ui.Div().Column().ContextMenu(func() []ui.MenuItem { return a.pathMenu(findPaths(text, bases)) }).Child(e)
}

// pathMenu is what a right click offers for the paths in a message: one group for one path, a
// submenu for each of several.
func (a *App) pathMenu(ps []filePath) []ui.MenuItem {
	seen := map[string]bool{}
	var uniq []filePath
	for _, p := range ps {
		if !seen[p.path] {
			seen[p.path] = true
			uniq = append(uniq, p)
		}
	}
	switch len(uniq) {
	case 0:
		return nil
	case 1:
		return append([]ui.MenuItem{ui.MenuSection(filepath.Base(uniq[0].path))}, a.pathItems(uniq[0])...)
	}
	var items []ui.MenuItem
	for _, p := range uniq[:min(len(uniq), 12)] {
		icon := icons.File
		if isDir(p.path) {
			icon = icons.Folder
		}
		items = append(items, ui.MenuItem{Label: tildePath(p.path), Icon: icon, Submenu: a.pathItems(p)})
	}
	return items
}

func (a *App) pathItems(p filePath) []ui.MenuItem {
	path := p.path
	items := []ui.MenuItem{
		{Label: "Open", Icon: icons.ExternalLink, OnClick: func() { a.openDefault(path) }},
	}
	if !isDir(path) {
		items = append(items, ui.MenuItem{Label: "Open in editor", Icon: icons.SquareCode, OnClick: func() { a.editFile(p) }})
	}
	return append(items,
		ui.MenuItem{Label: "Show in Files", Icon: icons.FolderOpen, OnClick: func() {
			if err := launch.Reveal(path); err != nil {
				a.notify(ui.ToastError, "Couldn't show the file", err.Error())
			}
		}},
		ui.MenuSeparator(),
		ui.MenuItem{Label: "Copy path", Icon: icons.Copy, OnClick: func() { a.copyText("the path", path) }},
	)
}

// openPath opens a path from a message: a folder, image, PDF or the like in its default app, and
// other files in the editor (in the default app when there is no editor).
func (a *App) openPath(p filePath) {
	if isDir(p.path) || opensInApp(p.path) {
		a.openDefault(p.path)
		return
	}
	a.editFile(p)
}

func (a *App) editFile(p filePath) {
	err := launch.EditFile(a.cfg.Peek().EditorCommand, p.path, p.line)
	if err == launch.ErrNoEditor {
		err = launch.Open(p.path)
	}
	if err != nil {
		a.notify(ui.ToastError, "Couldn't open the file", err.Error())
	}
}

func (a *App) openDefault(path string) {
	if err := launch.Open(path); err != nil {
		a.notify(ui.ToastError, "Couldn't open the file", err.Error())
	}
}

// opensInApp reports whether a file is better in its default app than in a code editor.
func opensInApp(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".tif", ".tiff", ".avif", ".heic",
		".pdf", ".mp4", ".mov", ".webm", ".mkv", ".mp3", ".wav", ".ogg", ".flac",
		".zip", ".tar", ".gz", ".docx", ".xlsx", ".pptx", ".odt", ".ods", ".odp", ".appimage":
		return true
	}
	return false
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
