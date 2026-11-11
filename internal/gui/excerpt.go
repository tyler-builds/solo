package gui

import (
	"regexp"
	"strings"
	"unicode"
)

// minExcerptWords is how many whole words an unfinished line needs before it shows in Solo's working line.
const minExcerptWords = 6

var listMarker = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)

// thoughtExcerpt is the line of thinking text Solo's working line shows: the first line of the newest
// paragraph (paragraphs are separated by blank lines), without markdown markers. So that the line doesn't
// change on every token, a paragraph shows once its first line is finished (a newline follows it) or has
// minExcerptWords whole words, cut back to its last whole word; until then the paragraph before it shows.
// It returns "" when there is nothing to show yet.
func thoughtExcerpt(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		// Only the first line of a paragraph counts: one that is not blank, after a blank line or the start.
		if strings.TrimSpace(lines[i]) == "" || (i > 0 && strings.TrimSpace(lines[i-1]) != "") {
			continue
		}
		line := lines[i]
		if i == len(lines)-1 { // still being written
			words := strings.Fields(line)
			if !unicode.IsSpace(rune(line[len(line)-1])) {
				words = words[:len(words)-1] // the last word may be cut
			}
			if len(words) < minExcerptWords {
				continue
			}
			line = strings.Join(words, " ")
		}
		if s := plainLine(line); s != "" {
			return s
		}
	}
	return ""
}

// plainLine strips the markdown markers from one line of text, or returns "" if it has no words.
func plainLine(s string) string {
	s = strings.TrimLeft(strings.TrimSpace(s), "#> \t")
	s = listMarker.ReplaceAllString(s, "")
	s = strings.TrimSpace(strings.NewReplacer("**", "", "`", "").Replace(s))
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return ""
	}
	return s
}
