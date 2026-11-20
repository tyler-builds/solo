// Package secrets keeps the secrets agents ask the user for (API keys, tokens, passwords) and lets an
// agent use them without seeing them: the values live in the OS keyring (Store), the app puts them in
// the environment of one command it runs itself (Run), and takes them back out of whatever the agent
// would read: the command's output and files it wrote (Redactor).
//
// An agent with a shell runs as the user, so this keeps values out of every path the app controls and
// closes the obvious deliberate ones; it can't stop a determined agent that transforms a value before
// printing it (see docs/ARCHITECTURE.md, Secrets).
package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxName is the longest secret name.
const MaxName = 64

var nameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidName reports whether name can name a secret: an environment variable name (letters, digits and
// underscores, not starting with a digit), at most MaxName characters.
func ValidName(name string) bool { return len(name) <= MaxName && nameRE.MatchString(name) }

// Placeholder is what stands in for a secret's value in text the agent or the user reads.
func Placeholder(name string) string { return "[secret " + name + "]" }

// fragLen is the shortest run of a value's characters that output redaction removes on its own (a
// prefix, a half): any run this long of a longer value is taken out.
const fragLen = 8

// minVariant is the shortest encoded form of a value that is redacted: shorter ones would take out
// ordinary text.
const minVariant = 6

// Redactor takes secret values, and their common encodings, out of text.
type Redactor struct {
	full  *strings.Replacer // every value and its encodings, longest first
	mask  *strings.Replacer // the same, each replaced with as many '*' (Mask)
	pats  []string          // the same, for Contains
	frags map[string]string // runs of fragLen bytes of each value (and of it reversed), by run: the name
	empty bool
}

// NewRedactor returns a Redactor for values, by secret name. Empty values are left out.
func NewRedactor(values map[string]string) *Redactor {
	type pat struct{ s, name string }
	var pats []pat
	frags := map[string]string{}
	seen := map[string]bool{}
	add := func(s, name string, min int) {
		if len(s) < min || seen[s] {
			return
		}
		seen[s] = true
		pats = append(pats, pat{s, name})
	}
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	slices.Sort(names) // the same input redacts the same way
	for _, name := range names {
		v := values[name]
		if v == "" {
			continue
		}
		add(v, name, 1)
		for _, e := range Encodings(v) {
			add(e, name, minVariant)
		}
		parts := secretParts(v)
		for _, p := range parts {
			if p != v {
				add(p, name, fragLen) // a URL's password, printed alone
			}
		}
		for _, p := range parts {
			if len(p) <= fragLen {
				continue
			}
			for _, s := range []string{p, reverse(p)} {
				for i := 0; i+fragLen <= len(s); i++ {
					frags[s[i:i+fragLen]] = name
				}
			}
		}
	}
	// Longest first: at one position, strings.Replacer takes the first pattern that matches.
	slices.SortStableFunc(pats, func(a, b pat) int { return len(b.s) - len(a.s) })
	r := &Redactor{frags: frags, empty: len(pats) == 0}
	args := make([]string, 0, 2*len(pats))
	masks := make([]string, 0, 2*len(pats))
	for _, p := range pats {
		args = append(args, p.s, Placeholder(p.name))
		masks = append(masks, p.s, strings.Repeat("*", len(p.s)))
		r.pats = append(r.pats, p.s)
	}
	r.full = strings.NewReplacer(args...)
	r.mask = strings.NewReplacer(masks...)
	return r
}

// secretParts are the parts of v whose runs the fragment rule takes out. For most values that is v
// itself. A URL (postgres://app:pw@localhost:5432/app) is mostly not secret: its scheme, host, port and
// path are words ordinary output has, so only its password and query values count, and the whole URL
// is still taken out wherever it appears whole or encoded.
func secretParts(v string) []string {
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" || strings.ContainsAny(v, " \t\n") {
		return []string{v}
	}
	var parts []string
	if pw, ok := u.User.Password(); ok && pw != "" {
		parts = append(parts, pw)
	}
	for _, vals := range u.Query() {
		for _, q := range vals {
			if q != "" {
				parts = append(parts, q)
			}
		}
	}
	return parts
}

// Mask is RedactFull for binary data: each value, and each of its encodings, becomes as many '*', so
// that nothing in the data moves.
func (r *Redactor) Mask(b []byte) []byte {
	if r.Empty() || !r.Contains(string(b)) {
		return b
	}
	return []byte(r.mask.Replace(string(b)))
}

// Empty reports whether the Redactor has nothing to take out.
func (r *Redactor) Empty() bool { return r == nil || r.empty }

// Contains reports whether s holds a value or one of its encodings (RedactFull would change it).
func (r *Redactor) Contains(s string) bool {
	if r.Empty() {
		return false
	}
	for _, p := range r.pats {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// RedactFull replaces each value, and each of its encodings, in s with its Placeholder. It is cheap
// enough for every message stored.
func (r *Redactor) RedactFull(s string) string {
	if r.Empty() || !r.Contains(s) {
		return s
	}
	return r.full.Replace(s)
}

// Redact is RedactFull, and then also takes out any run of fragLen or more characters of a value, or
// of the value reversed: a prefix, a half, the value cut in two. It is for the output of a command
// that had the values, where an agent may have tried to get one past RedactFull.
func (r *Redactor) Redact(s string) string {
	if r.Empty() {
		return s
	}
	s = r.RedactFull(s)
	if len(r.frags) == 0 || len(s) < fragLen {
		return s
	}
	// Mark every byte inside a run, then replace each marked stretch.
	var marked []bool
	names := map[int]string{}
	for i := 0; i+fragLen <= len(s); i++ {
		if name, ok := r.frags[s[i:i+fragLen]]; ok {
			if marked == nil {
				marked = make([]bool, len(s))
			}
			for j := i; j < i+fragLen; j++ {
				marked[j] = true
			}
			if _, ok := names[i]; !ok {
				names[i] = name
			}
		}
	}
	if marked == nil {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if !marked[i] {
			b.WriteByte(s[i])
			i++
			continue
		}
		name := names[i]
		for i < len(s) && marked[i] {
			i++
		}
		b.WriteString(Placeholder(name))
	}
	return strings.ToValidUTF8(b.String(), "")
}

// Encodings are the forms of v that a command's output might show it in, other than as it is: hex,
// base64 (standard and URL alphabets, at each of the three alignments it can have inside longer
// encoded text), URL escaping, JSON and Go string escaping, and reversed.
func Encodings(v string) []string {
	out := []string{
		reverse(v),
		hex.EncodeToString([]byte(v)),
		strings.ToUpper(hex.EncodeToString([]byte(v))),
		url.QueryEscape(v),
		url.PathEscape(v),
		strings.ReplaceAll(url.QueryEscape(v), "+", "%20"),
	}
	if q := strconv.Quote(v); len(q) > 2 {
		out = append(out, q[1:len(q)-1])
	}
	if j, err := json.Marshal(v); err == nil && len(j) > 2 {
		out = append(out, string(j[1:len(j)-1]))
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) == nil {
		if j := strings.TrimSpace(b.String()); len(j) > 2 {
			out = append(out, j[1:len(j)-1])
		}
	}
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		out = append(out, e.EncodeToString([]byte(v))) // the value alone, with its padding
		for k := range 3 {
			out = append(out, base64Inside(e, []byte(v), k))
		}
	}
	return slices.DeleteFunc(out, func(s string) bool { return s == v || s == "" })
}

// base64Inside is the run of base64 characters that v alone decides when it is encoded after k other
// bytes: the characters that also hold bits of the bytes around it are left out, so it is found in
// any longer encoded text ("Bearer <token>" encoded, say).
func base64Inside(e *base64.Encoding, v []byte, k int) string {
	buf := make([]byte, k, k+len(v)+2)
	buf = append(append(buf, v...), 0, 0) // trailing bytes keep padding out of the run
	enc := e.EncodeToString(buf)
	first := (8*k + 5) / 6       // the first character all of whose bits are v's
	last := (8*(k+len(v)))/6 - 1 // the last one
	if last < first || last >= len(enc) {
		return ""
	}
	return enc[first : last+1]
}

func reverse(s string) string {
	if !utf8.ValidString(s) {
		b := []byte(s)
		slices.Reverse(b)
		return string(b)
	}
	r := []rune(s)
	slices.Reverse(r)
	return string(r)
}
