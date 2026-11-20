package core

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Files attached to a message. Any file can be: an image (PNG, JPEG, GIF or WebP that Claude takes) is
// sent as an image, and every other file is copied onto the machine that runs Solo, beside the images
// in the attachments folder, and the agent is told its path to read it with its own tools.
//
// A file attachment is named <uuid>-<name>, where name is the file's own name made safe (SafeFileName);
// an image attachment is <uuid>.<ext>. So the name of an attachment says which it is, and the original
// name of a file goes with it to the agent and back to the UI without a record of its own.
const (
	// MaxFileSize is the largest file attached to a message. A page reads a picked file whole before Solo
	// sees it, and sends it through the relay at about 1.5 MB a second, so this bounds both: it takes
	// ordinary documents, archives and logs.
	MaxFileSize = 64 << 20
	// MaxFiles is how many files (other than images) one message may carry.
	MaxFiles = 20
	// maxNameBytes is the longest name kept for an attached file, in bytes, beside the 37 of its prefix:
	// file systems take 255.
	maxNameBytes = 150
)

// FileRef is a file attached to a message, other than an image.
type FileRef struct {
	Path string `json:"path"` // the attachment, on the machine that runs Solo
	Name string `json:"name"` // the file's name, as the user knows it (made safe; see SafeFileName)
	Size int64  `json:"size"` // in bytes
}

// FileName returns the name an attached file was given, from its attachment's path, and false for a
// path that isn't a file attachment's (an image's, or another file's). It reads only the name: the
// machine checks that the file is in the attachments folder (Core.isAttachment).
func FileName(path string) (string, bool) {
	base := path[max(strings.LastIndexAny(path, `/\`)+1, 0):]
	if len(base) < 38 || base[36] != '-' || !isUUID(base[:36]) {
		return "", false
	}
	return base[37:], true
}

// IsFileAttachment reports whether path names a file attachment rather than an image (see FileName).
func IsFileAttachment(path string) bool {
	_, ok := FileName(path)
	return ok
}

// isUUID reports whether s is a UUID as agent.NewUUID writes it: lower-case hex in groups of 8, 4, 4, 4
// and 12.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// SafeFileName makes a file's name, which may come from another device, one that is safe to store: only
// its last element (no folders, no "." or ".."), without control or invisible formatting characters
// (which can disguise an extension), without the characters Windows refuses in a name, and without the
// dots and spaces at its ends that Windows drops. A long name is shortened, keeping its extension. A
// name with nothing left is "file".
func SafeFileName(name string) string {
	name = strings.ToValidUTF8(name, "_")
	name = name[max(strings.LastIndexAny(name, `/\`)+1, 0):]
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f || unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		case strings.ContainsRune(`<>:"|?*`, r):
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if len(name) > maxNameBytes {
		ext := ""
		if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 16 {
			ext = name[i:]
		}
		stem := name[:maxNameBytes-len(ext)]
		for len(stem) > 0 && !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1] // not in the middle of a character
		}
		name = strings.TrimRight(stem, ". ") + ext
	}
	if name == "" {
		return "file"
	}
	return name
}

// FormatSize writes a file's size for people, in units of 1024 as the limits are: "820 B", "14 KB",
// "2.4 MB".
func FormatSize(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", (n+1<<9)>>10)
	case n < 10<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d MB", (n+1<<19)>>20)
}
