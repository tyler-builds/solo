package core

import (
	"path/filepath"
	"strings"
)

// MarkdownImage is an image in markdown text, ![Alt](Dest "title"): text[Start:End] is all of it,
// and text[DestStart:DestEnd] its destination as written (with its angle brackets, if any).
type MarkdownImage struct {
	Start, End         int
	DestStart, DestEnd int
	Alt                string
	Dest               string // with its backslash escapes undone
}

// MarkdownImages finds the images in markdown text, outside code blocks and code spans. It reads the
// inline syntax only, each image on one line, which is how agents write them.
func MarkdownImages(text string) []MarkdownImage {
	if !strings.Contains(text, "![") {
		return nil
	}
	var out []MarkdownImage
	fence, off := "", 0
	for _, line := range strings.SplitAfter(text, "\n") {
		t := strings.TrimLeft(line, " ")
		indented := len(line)-len(t) > 3
		switch {
		case fence != "":
			if !indented && strings.HasPrefix(t, fence) {
				fence = ""
			}
		case indented:
			out = scanImages(line, off, out)
		case strings.HasPrefix(t, "```"):
			fence = "```"
		case strings.HasPrefix(t, "~~~"):
			fence = "~~~"
		default:
			out = scanImages(line, off, out)
		}
		off += len(line)
	}
	return out
}

// scanImages appends the images in one line, which starts at off in the text.
func scanImages(s string, off int, out []MarkdownImage) []MarkdownImage {
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\':
			i += 2
		case s[i] == '`':
			n := 1
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			ticks := s[i : i+n]
			j := i + n
			for {
				k := strings.Index(s[j:], ticks)
				if k < 0 {
					i += n // no closing run: the backticks are text
					break
				}
				j += k
				e := j + n
				for e < len(s) && s[e] == '`' {
					e++
				}
				if e-j == n {
					i = e
					break
				}
				j = e
			}
		case s[i] == '!' && i+1 < len(s) && s[i+1] == '[':
			if im, ok := parseImage(s, i); ok {
				im.Start, im.End, im.DestStart, im.DestEnd = im.Start+off, im.End+off, im.DestStart+off, im.DestEnd+off
				out = append(out, im)
				i = im.End - off
			} else {
				i += 2
			}
		default:
			i++
		}
	}
	return out
}

// parseImage reads the image that starts at s[i], "![".
func parseImage(s string, i int) (MarkdownImage, bool) {
	j, depth := i+2, 1
	for ; j < len(s) && depth > 0; j++ {
		switch s[j] {
		case '\\':
			j++
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	if depth > 0 || j >= len(s) || s[j] != '(' {
		return MarkdownImage{}, false
	}
	im := MarkdownImage{Start: i, Alt: unescapeMarkdown(s[i+2 : j-1])}
	j = skipSpace(s, j+1)
	im.DestStart = j
	var dest string
	if j < len(s) && s[j] == '<' {
		k := strings.IndexAny(s[j+1:], "<>\n")
		if k < 0 || s[j+1+k] != '>' {
			return MarkdownImage{}, false
		}
		dest, j = s[j+1:j+1+k], j+k+2
	} else {
		k, parens := j, 0
	dest:
		for ; k < len(s); k++ {
			switch s[k] {
			case ' ', '\t', '\n', '\r':
				break dest
			case '\\':
				k++
			case '(':
				parens++
			case ')':
				if parens == 0 {
					break dest
				}
				parens--
			}
		}
		dest, j = s[j:min(k, len(s))], min(k, len(s))
	}
	im.DestEnd = j
	if dest == "" {
		return MarkdownImage{}, false
	}
	im.Dest = unescapeMarkdown(dest)
	j = skipSpace(s, j)
	if j < len(s) && (s[j] == '"' || s[j] == '\'' || s[j] == '(') {
		closer := s[j]
		if closer == '(' {
			closer = ')'
		}
		k := j + 1
		for k < len(s) && s[k] != closer {
			if s[k] == '\\' {
				k++
			}
			k++
		}
		if k >= len(s) {
			return MarkdownImage{}, false
		}
		j = skipSpace(s, k+1)
	}
	if j >= len(s) || s[j] != ')' {
		return MarkdownImage{}, false
	}
	im.End = j + 1
	return im, true
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// unescapeMarkdown undoes markdown's backslash escapes, which only apply before ASCII punctuation (so
// a Windows path keeps its backslashes).
func unescapeMarkdown(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", s[i+1]) >= 0 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ImageDest writes a file's path as a markdown image destination: with forward slashes, which
// markdown leaves alone and every OS takes, in angle brackets when it has spaces or parentheses.
func ImageDest(path string) string {
	s := filepath.ToSlash(path)
	if strings.ContainsAny(s, " \t()") && !strings.ContainsAny(s, "<>") {
		return "<" + s + ">"
	}
	return s
}

// SamePath reports whether a and b name the same file, as written by Windows or by ImageDest.
func SamePath(a, b string) bool {
	return strings.ReplaceAll(a, `\`, "/") == strings.ReplaceAll(b, `\`, "/")
}

// DropImages removes the images from markdown, for text that can't show them: a one-line excerpt, or a
// pull request, where an attachment's path means nothing.
func DropImages(md string) string {
	ims := MarkdownImages(md)
	if len(ims) == 0 {
		return md
	}
	var b strings.Builder
	last := 0
	for _, im := range ims {
		b.WriteString(md[last:im.Start])
		last = im.End
	}
	b.WriteString(md[last:])
	return b.String()
}
