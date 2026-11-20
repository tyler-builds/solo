//go:build !js

package secrets

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const token = "sk-live-9f8e7d6c5b4a3210ZYXWvuts"

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"CLOUDFLARE_API_TOKEN": true, "_x": true, "a1": true,
		"": false, "1A": false, "A-B": false, "A B": false, "A=B": false, strings.Repeat("A", MaxName+1): false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRedactEncodings(t *testing.T) {
	r := NewRedactor(map[string]string{"TOKEN": token})
	rev := []rune(token)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	forms := map[string]string{
		"raw":            token,
		"base64":         base64.StdEncoding.EncodeToString([]byte(token)),
		"base64 raw url": base64.RawURLEncoding.EncodeToString([]byte(token)),
		"base64 inside":  base64.StdEncoding.EncodeToString([]byte("Authorization: Bearer " + token + "\n")),
		"base64 inside1": base64.StdEncoding.EncodeToString([]byte("x" + token)),
		"base64 inside2": base64.StdEncoding.EncodeToString([]byte("xy" + token + "z")),
		"hex":            hex.EncodeToString([]byte(token)),
		"HEX":            strings.ToUpper(hex.EncodeToString([]byte(token))),
		"url":            url.QueryEscape("a=" + token + "&b"),
		"reversed":       string(rev),
		"json":           `{"token":"` + token + `"}`,
	}
	for name, s := range forms {
		got := r.Redact("before " + s + " after")
		if strings.Contains(got, token) || leaks(got) {
			t.Errorf("%s: Redact left the value: %q", name, got)
		}
		if !strings.HasPrefix(got, "before ") || !strings.HasSuffix(got, " after") {
			t.Errorf("%s: Redact changed the text around it: %q", name, got)
		}
	}
}

// leaks reports whether s still holds any 8-character run of the token, or a base64 or hex run of it.
func leaks(s string) bool {
	for i := 0; i+8 <= len(token); i++ {
		if strings.Contains(s, token[i:i+8]) {
			return true
		}
	}
	for _, e := range Encodings(token) {
		if len(e) >= minVariant && strings.Contains(s, e) {
			return true
		}
	}
	return false
}

func TestRedactFragments(t *testing.T) {
	r := NewRedactor(map[string]string{"TOKEN": token})
	half := len(token) / 2
	got := r.Redact("first " + token[:half] + "\nsecond " + token[half:] + "\nprefix " + token[:10])
	if leaks(got) {
		t.Errorf("Redact left a run of the value: %q", got)
	}
	if !strings.Contains(got, "[secret TOKEN]") {
		t.Errorf("Redact = %q, want placeholders", got)
	}
	if got := r.Redact("nothing secret here\n"); got != "nothing secret here\n" {
		t.Errorf("Redact changed plain text: %q", got)
	}
}

func TestRedactFullJSONSafe(t *testing.T) {
	// A value with characters JSON escapes: its escaped form is redacted, and the result stays valid.
	v := `pa"ss\word<&>!`
	r := NewRedactor(map[string]string{"PW": v})
	for _, s := range []string{`{"text":"` + mustJSON(v) + `"}`} {
		got := r.RedactFull(s)
		if strings.Contains(got, mustJSON(v)) {
			t.Errorf("RedactFull(%q) = %q", s, got)
		}
	}
}

func mustJSON(v string) string {
	q := Encodings(v)
	for _, e := range q {
		if strings.Contains(e, `\"`) {
			return e
		}
	}
	return v
}

func shell(t *testing.T) []string {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands")
	}
	return []string{"/bin/sh", "-c"}
}

func TestRunInjectsAndRedacts(t *testing.T) {
	home(t)
	sh := shell(t)
	dir := t.TempDir()
	r := NewRedactor(map[string]string{"TOKEN": token, "OTHER": "unused-other-secret-value"})
	res, err := Run(context.Background(), RunSpec{
		Command: `test "$TOKEN" = "` + token + `" && echo ok; echo "$TOKEN"; printf %s "$TOKEN" | base64; ` +
			`printf %s "$TOKEN" | od -An -tx1 | tr -d ' \n'; echo; echo "$TOKEN" | rev; echo err "$TOKEN" >&2; ` +
			`echo "tmp=$TMPDIR"; exit 3`,
		Dir:      dir,
		Secrets:  map[string]string{"TOKEN": token},
		Redactor: r,
		Shell:    sh,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if !strings.HasPrefix(res.Output, "ok\n") {
		t.Errorf("the command didn't get the value in its environment: %q", res.Output)
	}
	if leaks(res.Output) || !res.Redacted {
		t.Errorf("output still holds the value: %q (Redacted %v)", res.Output, res.Redacted)
	}
	if !strings.Contains(res.Output, "err [secret TOKEN]") {
		t.Errorf("stderr isn't in the output, redacted: %q", res.Output)
	}
	// The temporary folder was the command's own, and is gone.
	for _, line := range strings.Split(res.Output, "\n") {
		if tmp, ok := strings.CutPrefix(line, "tmp="); ok {
			if !strings.Contains(tmp, "solo-run-") {
				t.Errorf("TMPDIR = %q, want a folder of the command's own", tmp)
			}
			if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the command's temporary folder %s is still there", tmp)
			}
		}
	}
	// Solo's own environment never had it.
	if strings.Contains(strings.Join(os.Environ(), "\n"), token) {
		t.Error("the value is in Solo's environment")
	}
}

func TestRunKillsWhatItLeft(t *testing.T) {
	home(t)
	sh := shell(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "alive")
	res, err := Run(context.Background(), RunSpec{
		Command:  `(sleep 2; echo "$TOKEN" > "` + mark + `") >/dev/null 2>&1 & echo started`,
		Dir:      dir,
		Secrets:  map[string]string{"TOKEN": token},
		Redactor: NewRedactor(map[string]string{"TOKEN": token}),
		Shell:    sh,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "started" {
		t.Errorf("Output = %q", res.Output)
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(mark); err == nil {
		t.Error("a background process the command started outlived it")
	}
}

func TestRunTimeout(t *testing.T) {
	home(t)
	sh := shell(t)
	start := time.Now()
	res, err := Run(context.Background(), RunSpec{Command: "echo hi; sleep 30", Dir: t.TempDir(), Timeout: 500 * time.Millisecond,
		Redactor: NewRedactor(nil), Shell: sh})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || time.Since(start) > 10*time.Second {
		t.Errorf("TimedOut = %v after %v", res.TimedOut, time.Since(start))
	}
	if !strings.Contains(res.Output, "hi") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestRunDefaultShell(t *testing.T) {
	home(t)
	if _, err := exec.LookPath("bash"); err != nil && runtime.GOOS == "windows" {
		t.Skip("no bash")
	}
	res, err := Run(context.Background(), RunSpec{Command: "echo $X", Dir: t.TempDir(),
		Secrets: map[string]string{"X": "value-of-x-123"}, Redactor: NewRedactor(map[string]string{"X": "value-of-x-123"})})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "[secret X]" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestMem(t *testing.T) {
	var m Mem
	if _, err := m.Get("A"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of nothing: %v", err)
	}
	m.Set("A", "1")
	if v, _ := m.Get("A"); v != "1" {
		t.Errorf("Get = %q", v)
	}
	m.Delete("A")
	if _, err := m.Get("A"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: %v", err)
	}
}
