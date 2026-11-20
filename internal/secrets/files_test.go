//go:build !js

package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// home gives the test a home folder of its own, for the credential files.
func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	return h
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runIn(t *testing.T, own, dir, command string) RunResult {
	t.Helper()
	res, err := Run(context.Background(), RunSpec{
		Command:  command,
		Dir:      dir,
		Own:      own,
		Secrets:  map[string]string{"TOKEN": token},
		Redactor: NewRedactor(map[string]string{"TOKEN": token}),
		Shell:    shell(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// In the agent's own folder, a text file the command wrote a secret into gets its placeholder.
func TestRunScrubsWhatItWroteInItsFolder(t *testing.T) {
	home(t)
	own := t.TempDir()
	res := runIn(t, own, own, `mkdir -p sub && echo "TOKEN=$TOKEN" > .env && printf %s "$TOKEN" | base64 > sub/b64 && echo done`)
	if got := read(t, filepath.Join(own, ".env")); got != "TOKEN=[secret TOKEN]\n" {
		t.Errorf(".env = %q", got)
	}
	if got := read(t, filepath.Join(own, "sub", "b64")); leaks(got) {
		t.Errorf("sub/b64 = %q", got)
	}
	if len(res.Scrubbed) != 2 || res.Unchecked || strings.TrimSpace(res.Output) != "done" {
		t.Errorf("result %+v", res)
	}
}

// A file that held the value before the command ran is left alone, even when the command changed it.
func TestRunLeavesFilesThatHeldTheValue(t *testing.T) {
	home(t)
	own := t.TempDir()
	cfg := filepath.Join(own, "config.txt")
	write(t, cfg, "token="+token+"\n")
	res := runIn(t, own, own, `echo "other=1" >> config.txt`)
	if got := read(t, cfg); got != "token="+token+"\nother=1\n" {
		t.Errorf("config.txt = %q", got)
	}
	if len(res.Scrubbed) != 0 {
		t.Errorf("Scrubbed = %v", res.Scrubbed)
	}
}

// A binary file keeps its length: the value becomes as many '*'.
func TestRunMasksBinaryFiles(t *testing.T) {
	home(t)
	own := t.TempDir()
	res := runIn(t, own, own, `printf '\000\001\002%s\000\377' "$TOKEN" > blob.bin`)
	got, _ := os.ReadFile(filepath.Join(own, "blob.bin"))
	want := append(append([]byte{0, 1, 2}, bytes.Repeat([]byte("*"), len(token))...), 0, 0xff)
	if !bytes.Equal(got, want) {
		t.Errorf("blob.bin = %q, want %q", got, want)
	}
	if len(res.Scrubbed) != 1 {
		t.Errorf("Scrubbed = %v", res.Scrubbed)
	}
}

// Credential files in the home folder are put back as they were, never given a placeholder; one that
// held the value before is left alone.
func TestRunPutsBackCredentialFiles(t *testing.T) {
	h := home(t)
	npmrc := filepath.Join(h, ".npmrc")
	write(t, npmrc, "registry=https://registry.example.com/\n")
	hosts := filepath.Join(h, ".config", "gh", "hosts.yml") // not there before
	aws := filepath.Join(h, ".aws", "credentials")
	write(t, aws, "[default]\naws_secret_access_key = "+token+"\n") // the user's own login
	own := t.TempDir()
	res := runIn(t, own, own, `echo "//registry.example.com/:_authToken=$TOKEN" >> "$HOME/.npmrc"; `+
		`mkdir -p "$HOME/.config/gh" && echo "oauth_token: $TOKEN" > "$HOME/.config/gh/hosts.yml"; `+
		`echo "region = eu-west-1" >> "$HOME/.aws/credentials"`)
	if got := read(t, npmrc); got != "registry=https://registry.example.com/\n" {
		t.Errorf(".npmrc = %q, want it as it was", got)
	}
	if _, err := os.Stat(hosts); !os.IsNotExist(err) {
		t.Errorf("hosts.yml, made by the command with the value, is still there (%v)", err)
	}
	if got := read(t, aws); got != "[default]\naws_secret_access_key = "+token+"\nregion = eu-west-1\n" {
		t.Errorf("the user's own login was changed: %q", got)
	}
	if len(res.Restored) != 2 {
		t.Errorf("Restored = %v, want .npmrc and hosts.yml", res.Restored)
	}
}

// Outside the agent's own folder, nothing is looked through or changed (Solo's own commands may run
// in the user's checkout); the repository's git config is put back.
func TestRunTouchesNothingOutsideItsFolder(t *testing.T) {
	home(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	own, checkout := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://example.com/r.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", checkout}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	gitCfg := filepath.Join(checkout, ".git", "config")
	before := read(t, gitCfg)
	write(t, filepath.Join(checkout, "app.env"), "KEY="+token+"\n") // the user's, from before
	past := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(checkout, "app.env"), past, past)
	res := runIn(t, own, checkout, `git remote set-url origin "https://x:$TOKEN@example.com/r.git"; echo "more" >> app.env; echo "$TOKEN" > new.txt`)
	if got := read(t, gitCfg); got != before {
		t.Errorf("git config = %q, want it as it was", got)
	}
	if got := read(t, filepath.Join(checkout, "app.env")); got != "KEY="+token+"\nmore\n" {
		t.Errorf("app.env = %q, want it left alone", got)
	}
	if got := read(t, filepath.Join(checkout, "new.txt")); got != token+"\n" {
		t.Errorf("new.txt = %q: a file outside the agent's folder was changed", got)
	}
	if len(res.Restored) != 1 || len(res.Scrubbed) != 0 {
		t.Errorf("Restored %v, Scrubbed %v", res.Restored, res.Scrubbed)
	}
}

func TestRedactURL(t *testing.T) {
	const pw = "s3cr3t-Pa55w0rd-9xq"
	v := "postgres://app:" + pw + "@localhost:5432/app?sslmode=disable"
	r := NewRedactor(map[string]string{"DATABASE_URL": v})
	plain := "connecting to postgres on localhost:5432 as app (sslmode=disable)\n"
	if got := r.Redact(plain); got != plain {
		t.Errorf("Redact changed ordinary words: %q", got)
	}
	for _, s := range []string{v, pw, pw[:10], base64.StdEncoding.EncodeToString([]byte(v)), "postgres://app:" + pw + "@db"} {
		if got := r.Redact("x " + s + " y"); strings.Contains(got, pw[:8]) || strings.Contains(got, pw[8:16]) ||
			strings.Contains(got, strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(v)), "=")) {
			t.Errorf("Redact(%q) = %q", s, got)
		}
	}
	// A plain token still loses any run of 8 of its characters.
	tok := NewRedactor(map[string]string{"TOKEN": token})
	if got := tok.Redact("x " + token[3:12] + " y"); strings.Contains(got, token[3:12]) {
		t.Errorf("Redact = %q", got)
	}
}

func TestGuard(t *testing.T) {
	for _, c := range []string{
		"secret-tool lookup service solo-secrets username X",
		"secret-tool search --all service solo-secrets",
		"security find-generic-password -s solo-secrets -w",
		`python3 -c 'import keyring; print(keyring.get_password("solo-secrets", "X"))'`,
		"busctl --user call org.freedesktop.secrets /org/freedesktop/secrets x y solo-secrets",
		"cmdkey /list:solo-secrets*",
	} {
		if Forbidden(c) == "" {
			t.Errorf("Forbidden(%q) = \"\", want a reason", c)
		}
	}
	// Ordinary work that only mentions these words, or reads keyrings and processes that aren't Solo's.
	other := exec.Command("sleep", "5")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer other.Process.Kill()
	otherEnv := "/proc/" + strconv.Itoa(other.Process.Pid) + "/environ"
	for tool, inputs := range map[string][]map[string]any{
		"Bash": {
			{"command": "grep -rn solo-secrets internal/"},
			{"command": "grep -rn gnome-keyring . && grep -rn kwallet ."},
			{"command": "secret-tool lookup service other-app username me"},
			{"command": "cmdkey /list"},
			{"command": "cat " + otherEnv + " | tr '\\0' '\\n'"},
			{"command": "for p in /proc/*/environ; do wc -c $p; done"},
			{"command": `curl -H "Authorization: Bearer $TOKEN" https://api.example.com`},
		},
		"Grep": {{"pattern": "solo-secrets", "path": "."}, {"pattern": "secret-tool|/proc/1/environ"}},
		"Glob": {{"pattern": "**/*keyring*"}},
		"Read": {{"file_path": otherEnv}, {"file_path": "/proc/self/environ"}},
	} {
		for _, in := range inputs {
			if why := Guard(tool, in); why != "" {
				t.Errorf("Guard(%s, %v) = %q, want it allowed", tool, in, why)
			}
		}
	}
}

// While a command runs with secrets, its environment (and its children's) is off limits; others' aren't.
func TestGuardRunningCommand(t *testing.T) {
	home(t)
	sh := shell(t)
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(context.Background(), RunSpec{Command: `sleep 3 & echo "$$ $!" > pids; wait`, Dir: dir,
			Secrets: map[string]string{"TOKEN": token}, Redactor: NewRedactor(map[string]string{"TOKEN": token}), Shell: sh})
	}()
	var pids []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil && strings.Count(string(b), " ") == 1 && strings.HasSuffix(string(b), "\n") {
			pids = strings.Fields(string(b))
			break
		}
	}
	if len(pids) != 2 {
		t.Fatal("the command didn't start")
	}
	for _, p := range pids {
		if Guard("Read", map[string]any{"file_path": "/proc/" + p + "/environ"}) == "" {
			t.Errorf("reading /proc/%s/environ is allowed", p)
		}
		if Forbidden("strings /proc/"+p+"/environ") == "" {
			t.Errorf("strings /proc/%s/environ is allowed", p)
		}
	}
	for _, c := range []string{"cat /proc/*/environ", "for p in $(pgrep sleep); do cat /proc/$p/environ; done", "ps eww"} {
		if Forbidden(c) == "" {
			t.Errorf("Forbidden(%q) = \"\" while a command runs", c)
		}
	}
	if why := Guard("Read", map[string]any{"file_path": "/proc/" + strconv.Itoa(os.Getpid()) + "/environ"}); why != "" {
		t.Errorf("an unrelated process's environment is refused: %s", why)
	}
	<-done
	if why := Forbidden("cat /proc/*/environ"); why != "" {
		t.Errorf("still refused after the command ended: %s", why)
	}
}
