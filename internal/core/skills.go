package core

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Skill is a Claude Code skill or slash command that a conversation can invoke by typing "/" and its
// name, as Claude Code finds them: the user's skills (~/.claude/skills) and commands
// (~/.claude/commands), skills synced from the user's organization, installed plugins' skills and
// commands, and a project's own (.claude/skills, .claude/commands).
type Skill struct {
	// Name is what follows "/": "review", "frontend:review" for a command in a subfolder,
	// "plugin:skill" for a plugin's, "anthropic-skills:pdf" for an organization's.
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Command is set for a slash command (a .md file under commands), rather than a skill.
	Command bool `json:"command,omitempty"`
	// Source says where it comes from, for people: "Personal", "Organization", "Plugin <name>",
	// "Project", or "Project <repository>" in Settings' list of every repository's skills.
	Source string `json:"source"`
}

// PathHit is a file or folder found in a thread's working folder by SearchPaths.
type PathHit struct {
	Path string `json:"path"` // relative to the working folder, with forward slashes
	Dir  bool   `json:"dir,omitempty"`
}

// MaxPathHits is the most results SearchPaths returns.
const MaxPathHits = 50

// MatchScore scores how well candidate matches query, for the composer's lists and SearchPaths:
// higher is better, and ok is false when it doesn't match at all. Case is ignored. An exact match beats a
// prefix of the last part (after the last "/" or ":"), which beats a prefix of the whole, a substring,
// and finally the query's characters in order with gaps (fuzzy). Shorter candidates win ties. An empty
// query matches everything equally.
func MatchScore(query, candidate string) (score int, ok bool) {
	if query == "" {
		return 0, true
	}
	q, c := strings.ToLower(query), strings.ToLower(candidate)
	base := c
	if i := strings.LastIndexAny(strings.TrimRight(c, "/"), "/:"); i >= 0 {
		base = c[i+1:]
	}
	short := 200 - min(utf8.RuneCountInString(c), 200) // the shorter, the better among equals
	switch {
	case c == q || strings.TrimRight(base, "/") == q:
		return 10_000 + short, true
	case strings.HasPrefix(base, q):
		return 8_000 + short, true
	case strings.HasPrefix(c, q):
		return 7_000 + short, true
	case strings.Contains(base, q):
		return 6_000 + short, true
	case strings.Contains(c, q):
		return 5_000 + short, true
	}
	// Fuzzy: each of the query's characters in order. Runs and starts of words score more.
	qr := []rune(q)
	i, run, s := 0, 0, 0
	prev := rune(0)
	for _, r := range c {
		if i < len(qr) && r == qr[i] {
			i++
			run++
			s += run * 2
			if prev == 0 || !unicode.IsLetter(prev) && !unicode.IsDigit(prev) {
				s += 6 // the start of a word: "/", "-", "_", "." before it
			}
		} else {
			run = 0
		}
		prev = r
	}
	if i < len(qr) {
		return 0, false
	}
	return min(s, 4_000) + short, true
}

// splitFrontmatter separates a Markdown file's YAML frontmatter (between "---" lines at its start)
// from its body. Without frontmatter, the whole file is the body.
func splitFrontmatter(text string) (front, body string) {
	text = strings.TrimPrefix(text, "\uFEFF")
	t := strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(t, "---\n") {
		return "", t
	}
	rest := t[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", t
	}
	front = rest[:end]
	body = rest[end+len("\n---"):]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:] // the rest of the closing line
	} else {
		body = ""
	}
	return front, body
}

// frontmatterField reads a top-level "key: value" from YAML frontmatter, with quotes taken off. A
// folded or literal value ("key: >" or "key: |" and indented lines) is joined into one line.
func frontmatterField(front, key string) string {
	lines := strings.Split(front, "\n")
	for i, l := range lines {
		v, ok := strings.CutPrefix(l, key+":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == ">" || v == "|" || v == ">-" || v == "|-" || v == "" {
			var parts []string
			for _, next := range lines[i+1:] {
				if next != "" && next[0] != ' ' && next[0] != '\t' {
					break
				}
				if s := strings.TrimSpace(next); s != "" {
					parts = append(parts, s)
				}
			}
			return strings.Join(parts, " ")
		}
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		return v
	}
	return ""
}

// firstLine is a command's description when its frontmatter has none: its first line of text.
func firstTextLine(body string) string {
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#")); l != "" {
			return l
		}
	}
	return ""
}
