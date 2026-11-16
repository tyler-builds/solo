//go:build !js

package core

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyler-builds/solo/internal/agent"
)

// embedImages takes in the images an agent's message shows from files on its machine: markdown images
// with a local path, ![caption](path). Each is copied into the attachments (as ImportImage does), and
// the message points at the copy, so the transcript keeps it once the worktree is gone and Solo can
// read it. It runs before the event is applied, as importing reads and may scale each image.
func (c *Core) embedImages(e agent.Event) agent.Event {
	need := e.Kind == agent.EventTurnEnd && strings.Contains(e.Result, "![")
	if e.Kind == agent.EventAssistant && e.IsRoot() {
		for _, b := range e.Blocks {
			need = need || b.Type == "text" && strings.Contains(b.Text, "![")
		}
	}
	if !need {
		return e
	}
	c.mu.Lock()
	t := c.bySession[e.SessionID]
	dir := ""
	if t != nil {
		if dir = t.cwd; dir == "" {
			dir = c.sessionDir(t.row)
		}
	}
	c.mu.Unlock()
	if t == nil {
		return e
	}
	if e.Kind == agent.EventAssistant {
		e.Blocks = slices.Clone(e.Blocks)
		for i, b := range e.Blocks {
			if b.Type == "text" {
				e.Blocks[i].Text = c.embedIn(t, b.Text, dir)
			}
		}
	} else {
		e.Result = c.embedIn(t, e.Result, dir)
	}
	return e
}

// embedIn imports the local images text shows (at most maxImages) and returns text pointing at the
// copies. An image that can't be imported (a missing file, not an image, too large) is left as written.
func (c *Core) embedIn(t *thread, text, dir string) string {
	var b strings.Builder
	last, n := 0, 0
	for _, im := range MarkdownImages(text) {
		if n == maxImages {
			break
		}
		att, err := c.embedImage(t, im.Dest, dir)
		if err != nil {
			c.log.Debug("core: not embedding an image", "src", im.Dest, "err", err)
			continue
		}
		n++
		b.WriteString(text[last:im.DestStart])
		b.WriteString(ImageDest(att))
		last = im.DestEnd
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// embedImage returns the attachment for an image a message names by src, importing it the first time.
// A file that changes (a screenshot taken again to the same path) is imported again.
func (c *Core) embedImage(t *thread, src, dir string) (string, error) {
	p, err := localImagePath(src, dir)
	if err != nil {
		return "", err
	}
	if c.isAttachment(p) {
		// Shown again, as Solo does with a thread's screenshot. As ReadImage, a link isn't followed.
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%s isn't an attachment", filepath.Base(p))
		}
		return p, nil
	}
	fi, err := os.Stat(p)
	if err != nil && strings.Contains(src, "%") {
		if u, uerr := url.PathUnescape(src); uerr == nil {
			if p, err = localImagePath(u, dir); err == nil {
				fi, err = os.Stat(p)
			}
		}
	}
	if err != nil {
		return "", err
	}
	// Reading a device or a pipe could block, and a folder has nothing to show.
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s isn't a file", filepath.Base(p))
	}
	key := fmt.Sprintf("%s\x00%d\x00%d", p, fi.Size(), fi.ModTime().UnixNano())
	c.mu.Lock()
	att := t.embeds[key]
	c.mu.Unlock()
	if att != "" {
		if _, err := os.Stat(att); err == nil {
			return att, nil
		}
	}
	att, err = c.ImportImage(p)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if t.embeds == nil || len(t.embeds) >= 256 {
		t.embeds = map[string]string{}
	}
	t.embeds[key] = att
	c.mu.Unlock()
	return att, nil
}

// localImagePath returns the file an image's source names: an absolute path, a path relative to dir,
// ~/..., or a file:// URL. Anything else (a web URL, a network path, which Windows would connect to)
// isn't a local image.
func localImagePath(src, dir string) (string, error) {
	s := strings.TrimSpace(src)
	switch {
	case s == "":
		return "", fmt.Errorf("no source")
	case strings.HasPrefix(s, "file://"):
		u, err := url.Parse(s)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			return "", fmt.Errorf("%q isn't a local file", src)
		}
		s = u.Path
		if len(s) > 2 && s[0] == '/' && s[2] == ':' {
			s = s[1:] // /C:/x on Windows
		}
	case hasScheme(s):
		return "", fmt.Errorf("%q is a URL", src)
	case strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		s = filepath.Join(home, s[2:])
	}
	if strings.HasPrefix(s, "//") || strings.HasPrefix(s, `\\`) || strings.HasPrefix(s, `/\`) || strings.HasPrefix(s, `\/`) {
		return "", fmt.Errorf("%q is a network path", src)
	}
	p := filepath.FromSlash(s)
	if !filepath.IsAbs(p) {
		if dir == "" {
			return "", fmt.Errorf("%q is relative, with no folder to read it from", src)
		}
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p), nil
}

// hasScheme reports whether s starts with a URL scheme: two or more letters, digits, "+", "-" or ".",
// starting with a letter, then a colon. One letter and a colon is a Windows drive.
func hasScheme(s string) bool {
	i := strings.IndexByte(s, ':')
	if i < 2 {
		return false
	}
	for j, r := range s[:i] {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (j == 0 || !(r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.')) {
			return false
		}
	}
	return true
}

// ImagesIn returns the attachments that text shows as markdown images, as embedImages left them.
func (c *Core) ImagesIn(text string) []string {
	var out []string
	for _, im := range MarkdownImages(text) {
		p := filepath.Clean(filepath.FromSlash(im.Dest))
		if !c.isAttachment(p) || slices.Contains(out, p) {
			continue
		}
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}
