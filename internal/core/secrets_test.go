package core

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/secrets"
)

const testToken = "tok-4f9a2c71e8b3d605XYZ"

// leaksToken reports whether s holds the token, or its base64 or hex.
func leaksToken(s string) bool {
	for _, form := range []string{testToken, base64.StdEncoding.EncodeToString([]byte(testToken)), hex.EncodeToString([]byte(testToken))} {
		if strings.Contains(s, strings.TrimRight(form, "=")) {
			return true
		}
	}
	return false
}

// transcriptLeaks reports the first stored item of the thread that holds the token.
func transcriptLeaks(t *testing.T, c *Core, id string) string {
	t.Helper()
	items, err := c.Items(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if leaksToken(string(it.Data)) {
			return it.Kind + ": " + string(it.Data)
		}
	}
	return ""
}

// pendingSecret waits for the thread to ask for a secret and returns the request's key.
func pendingSecret(t *testing.T, c *Core, id string) PendingQuestion {
	t.Helper()
	waitStatusAllowTurn(t, c, id, StatusWaiting)
	qs := c.PendingQuestions(id)
	if len(qs) != 1 || qs[0].Secret() == nil {
		t.Fatalf("pending %+v, want one secret request", qs)
	}
	return qs[0]
}

const requestToken = `mcp request_secret {"name":"API_TOKEN","reason":"to call the example API"}`

func TestSecretRequestProvided(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	answered := watchAnswers(e.c)
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Call the API", Prompt: requestToken})
	pq := pendingSecret(t, e.c, v.ID)
	if s := pq.Secret(); s.Name != "API_TOKEN" || s.Reason != "to call the example API" {
		t.Fatalf("request %+v", s)
	}
	// It can't be answered with text, by the user or by Solo.
	if err := e.c.Answer(v.ID, pq.ToolUseID, map[string]string{pq.Questions[0].Question: testToken}); !errors.Is(err, ErrSecretAnswer) {
		t.Fatalf("Answer: %v, want ErrSecretAnswer", err)
	}
	if err := e.c.AnswerFromOrchestrator(v.ID, pq.ToolUseID, map[string]string{pq.Questions[0].Question: testToken}); !errors.Is(err, ErrSecretAnswer) {
		t.Fatalf("AnswerFromOrchestrator: %v, want ErrSecretAnswer", err)
	}
	if err := e.c.ProvideSecret(v.ID, pq.ToolUseID, testToken+"\n"); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if !strings.Contains(v.Summary, "The user saved API_TOKEN") || leaksToken(v.Summary) {
		t.Fatalf("the agent heard %q", v.Summary)
	}
	if got, _ := e.secrets.Get("API_TOKEN"); got != testToken {
		t.Fatalf("keyring holds %q, want the value without its newline", got)
	}
	if list := e.c.Secrets(); len(list) != 1 || list[0].Name != "API_TOKEN" || list[0].RequestedBy != "Call the API" {
		t.Fatalf("Secrets() = %+v", list)
	}
	if leak := transcriptLeaks(t, e.c, v.ID); leak != "" {
		t.Fatalf("transcript holds the value: %s", leak)
	}
	items, _ := e.c.Items(ctx, v.ID, 0)
	found := false
	for _, it := range items {
		if it.Kind == ItemQuestion {
			var d QuestionData
			it.Decode(&d)
			found = d.Provided && !d.Cancelled && len(d.Answers) == 0 && !d.Waiting()
		}
	}
	if !found {
		t.Fatal("the request's row isn't marked saved")
	}
	if got := answered(); len(got) != 1 || SecretOf(got[0].Questions) == nil || got[0].Skipped || len(got[0].Answers) != 0 {
		t.Fatalf("reported %+v", got)
	}
	// Settings' file holds no value either.
	if b, err := os.ReadFile(filepath.Join(e.dataDir, "solo.db")); err == nil && strings.Contains(string(b), testToken) {
		t.Fatal("the database file holds the value")
	}
}

func TestSecretRequestDeclined(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: requestToken})
	pq := pendingSecret(t, e.c, v.ID)
	if err := e.c.Dismiss(v.ID, pq.ToolUseID); err != nil {
		t.Fatal(err)
	}
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if !strings.Contains(v.Summary, "declined to give API_TOKEN") {
		t.Fatalf("summary %q", v.Summary)
	}
	if _, err := e.secrets.Get("API_TOKEN"); !errors.Is(err, secrets.ErrNotFound) || len(e.c.Secrets()) != 0 {
		t.Fatal("a declined secret was saved")
	}
}

func TestSecretRequestWhenSavedOrUnavailable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.c.SetSecret("API_TOKEN", testToken); err != nil {
		t.Fatal(err)
	}
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: requestToken})
	if v = waitStatus(t, e.c, v.ID, StatusIdle); !strings.Contains(v.Summary, "already saved") {
		t.Fatalf("summary %q, want it told the secret is there without asking", v.Summary)
	}
	// replace asks again.
	v, _ = e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: `mcp request_secret {"name":"API_TOKEN","reason":"it expired","replace":true}`})
	if pq := pendingSecret(t, e.c, v.ID); !pq.Secret().Replace {
		t.Fatal("not marked as a replacement")
	}

	e.secrets.Fail = errors.New("no secret service")
	v, _ = e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: `mcp request_secret {"name":"OTHER","reason":"x"}`})
	if v = waitStatus(t, e.c, v.ID, StatusIdle); !strings.Contains(v.Summary, "can't keep secrets") {
		t.Fatalf("summary %q", v.Summary)
	}
	v, _ = e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: `mcp request_secret {"name":"9bad","reason":"x"}`})
	if v = waitStatus(t, e.c, v.ID, StatusIdle); !strings.Contains(v.Summary, "can't name a secret") {
		t.Fatalf("summary %q", v.Summary)
	}
}

func TestSecretStorageAndDeletion(t *testing.T) {
	e := newEnv(t)
	if err := e.c.SetSecret("bad name", "x"); err == nil {
		t.Fatal("a bad name was taken")
	}
	if err := e.c.SetSecret("EMPTY", "\n"); err == nil {
		t.Fatal("an empty value was taken")
	}
	if err := e.c.SetSecret("API_TOKEN", "first-value-123"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.SetSecret("API_TOKEN", testToken); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.secrets.Get("API_TOKEN"); got != testToken {
		t.Fatalf("replaced value %q", got)
	}
	if list := e.c.Secrets(); len(list) != 1 || list[0].UpdatedAt.Before(list[0].CreatedAt) {
		t.Fatalf("Secrets() = %+v", list)
	}
	// The list outlives a restart; values are only in the keyring.
	e.c.Close()
	c2 := e.open(t)
	if list := c2.Secrets(); len(list) != 1 || list[0].Name != "API_TOKEN" {
		t.Fatalf("after restart: %+v", list)
	}
	if err := c2.DeleteSecret("API_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.secrets.Get("API_TOKEN"); !errors.Is(err, secrets.ErrNotFound) || len(c2.Secrets()) != 0 {
		t.Fatal("not deleted")
	}
}

func TestRunWithSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands")
	}
	e := newEnv(t)
	t.Setenv("HOME", t.TempDir()) // credential files of the test's own
	ctx := context.Background()
	if err := e.c.SetSecret("API_TOKEN", testToken); err != nil {
		t.Fatal(err)
	}
	cmd := `test ${#API_TOKEN} -eq ` + fmt.Sprint(len(testToken)) + ` && echo has-it; echo \"$API_TOKEN\"; printf %s \"$API_TOKEN\" | base64; ` +
		`printf %s \"$API_TOKEN\" | od -An -tx1 | tr -d ' \\n'; echo; echo \"$API_TOKEN\" | rev; echo \"key=$API_TOKEN\" > leak.txt`
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Title: "Use it",
		Prompt: `mcp run_with_secrets {"command":"` + cmd + `","secrets":["API_TOKEN"]}`})
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if !strings.Contains(v.Summary, "Exit code: 0") || !strings.Contains(v.Summary, "has-it") {
		t.Fatalf("the command didn't run with the secret: %q", v.Summary)
	}
	if leaksToken(v.Summary) || !strings.Contains(v.Summary, "[secret API_TOKEN]") {
		t.Fatalf("the agent heard the value: %q", v.Summary)
	}
	if !strings.Contains(v.Summary, "leak.txt") {
		t.Fatalf("the agent wasn't told the file was scrubbed: %q", v.Summary)
	}
	b, err := os.ReadFile(filepath.Join(v.WorktreePath, "leak.txt"))
	if err != nil || leaksToken(string(b)) || string(b) != "key=[secret API_TOKEN]\n" {
		t.Fatalf("leak.txt = %q, %v", b, err)
	}
	if leak := transcriptLeaks(t, e.c, v.ID); leak != "" {
		t.Fatalf("transcript holds the value: %s", leak)
	}
	items, _ := e.c.Items(ctx, v.ID, 0)
	var tool ToolData
	for _, it := range items {
		if it.Kind == ItemTool {
			it.Decode(&tool)
		}
	}
	if tool.Name != "mcp__solo__run_with_secrets" || !strings.Contains(tool.Result, "[secret API_TOKEN]") {
		t.Fatalf("tool row %+v", tool)
	}
	if list := e.c.Secrets(); list[0].LastUsedBy != "Use it" || list[0].LastUsed.IsZero() {
		t.Fatalf("use not recorded: %+v", list)
	}
}

func TestRunWithSecretsRefuses(t *testing.T) {
	e := newEnv(t)
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	e.c.SetSecret("API_TOKEN", testToken)
	for prompt, want := range map[string]string{
		`mcp run_with_secrets {"command":"echo hi","secrets":["MISSING"]}`:                                   "no secret named",
		`mcp run_with_secrets {"command":"secret-tool lookup service solo-secrets","secrets":["API_TOKEN"]}`: "from the system keyring",
		`mcp run_with_secrets {"command":"pwd","secrets":["API_TOKEN"],"cwd":"/"}`:                           "must be inside your working directory",
	} {
		v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: prompt})
		if v = waitStatus(t, e.c, v.ID, StatusIdle); !strings.Contains(v.Summary, want) {
			t.Errorf("%s: summary %q, want %q", prompt, v.Summary, want)
		}
	}
}

// The value is in no agent's environment: Claude's, or its own shell's.
func TestSecretNotInAgentEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands")
	}
	e := newEnv(t)
	ctx := context.Background()
	e.c.SetSecret("API_TOKEN", testToken)
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: "bash env"})
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	items, _ := e.c.Items(ctx, v.ID, 0)
	var out string
	for _, it := range items {
		if it.Kind == ItemTool {
			var d ToolData
			it.Decode(&d)
			out = d.Result
		}
	}
	if !strings.Contains(out, "PATH=") || strings.Contains(out, "API_TOKEN") {
		t.Fatalf("the worker's shell environment: %q", out)
	}
	row, _ := e.c.db.Threads()
	for _, r := range row {
		p := e.c.openParams(*r)
		for k, val := range p.Env {
			if strings.Contains(val, testToken) || k == "API_TOKEN" {
				t.Fatalf("session %s opens with the secret in its environment", r.ID)
			}
		}
		if strings.Contains(p.SystemAppend, testToken) {
			t.Fatal("the value is in a system prompt")
		}
	}
}

// A secret the user pastes into a message anyway reaches neither Claude nor the transcript.
func TestPastedSecretIsScrubbed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.c.SetSecret("API_TOKEN", testToken)
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: "here it is: " + testToken})
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if leaksToken(v.Summary) || !strings.Contains(v.Summary, "[secret API_TOKEN]") {
		t.Fatalf("Claude got %q", v.Summary)
	}
	if leak := transcriptLeaks(t, e.c, v.ID); leak != "" {
		t.Fatalf("transcript holds the value: %s", leak)
	}
	if got := e.c.Scrub("x " + base64.StdEncoding.EncodeToString([]byte(testToken))); leaksToken(got) {
		t.Fatalf("Scrub left the base64: %q", got)
	}
}

func TestListSecrets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.c.SetSecret("API_TOKEN", testToken)
	e.c.SetSecret("NPM_TOKEN", "npm-abcdef-123456")
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: "mcp list_secrets"})
	v = waitStatus(t, e.c, v.ID, StatusIdle)
	if !strings.Contains(v.Summary, "- API_TOKEN") || !strings.Contains(v.Summary, "- NPM_TOKEN") || leaksToken(v.Summary) ||
		strings.Contains(v.Summary, "npm-abcdef") {
		t.Fatalf("summary %q", v.Summary)
	}
}

// A request still waiting when Solo closes can be answered after the restart.
func TestRestartKeepsSecretRequest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: requestToken})
	pendingSecret(t, e.c, v.ID)
	e.c.Close()

	c2 := e.open(t)
	c2.ResumeInterrupted()
	qs := c2.PendingQuestions(v.ID)
	if len(qs) != 1 || qs[0].Secret() == nil {
		t.Fatalf("after restart: %+v", qs)
	}
	if err := c2.Answer(v.ID, qs[0].ToolUseID, map[string]string{qs[0].Questions[0].Question: testToken}); !errors.Is(err, ErrSecretAnswer) {
		t.Fatalf("Answer: %v", err)
	}
	if err := c2.ProvideSecret(v.ID, qs[0].ToolUseID, testToken); err != nil {
		t.Fatal(err)
	}
	var v2 ThreadView
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if v2, _ = c2.Thread(v.ID); v2.Status == StatusIdle && v2.Summary != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(v2.Summary, "the secret API_TOKEN (request_secret). They saved it") || leaksToken(v2.Summary) {
		t.Fatalf("summary %q", v2.Summary)
	}
	if got, _ := e.secrets.Get("API_TOKEN"); got != testToken {
		t.Fatal("not saved")
	}
	if leak := transcriptLeaks(t, c2, v.ID); leak != "" {
		t.Fatalf("transcript holds the value: %s", leak)
	}
}

// A value too short to take out of text safely is refused, by Settings and by a request's card; the
// request then still waits.
func TestShortSecretRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, v := range []string{"a", "1234", "admin", "true", "1234567\n"} {
		if err := e.c.SetSecret("PIN", v); !errors.Is(err, ErrSecretTooShort) {
			t.Errorf("SetSecret(%q) = %v, want ErrSecretTooShort", v, err)
		}
	}
	v, _ := e.c.CreateWorker(ctx, WorkerSpec{Repo: "proj", Prompt: requestToken})
	pq := pendingSecret(t, e.c, v.ID)
	if err := e.c.ProvideSecret(v.ID, pq.ToolUseID, "1234"); !errors.Is(err, ErrSecretTooShort) {
		t.Fatalf("ProvideSecret: %v", err)
	}
	if len(e.c.PendingQuestions(v.ID)) != 1 || len(e.c.Secrets()) != 0 {
		t.Fatal("a short value answered the request")
	}
	if err := e.c.ProvideSecret(v.ID, pq.ToolUseID, "12345678"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.c, v.ID, StatusIdle)
}

// A short value kept from before the rule never reaches transcript redaction, which would take
// ordinary text out of every message.
func TestTranscriptRedactionSkipsShortValues(t *testing.T) {
	r := transcriptRedactor(map[string]string{"PIN": "a", "API_TOKEN": testToken})
	if got := r.RedactFull("a banana and a database"); got != "a banana and a database" {
		t.Errorf("RedactFull = %q", got)
	}
	if got := r.RedactFull("t " + testToken); got != "t [secret API_TOKEN]" {
		t.Errorf("RedactFull = %q", got)
	}
}

// A session's PreToolUse hook asks core about an agent's own tool calls (Callbacks.OnGuard).
func TestGuardRoute(t *testing.T) {
	e := newEnv(t)
	ask := func(tool, input string) string {
		res, err := e.c.onGuard(context.Background(), agent.GuardParams{ToolName: tool, Input: json.RawMessage(input)})
		if err != nil {
			t.Fatal(err)
		}
		return res.Deny
	}
	if ask("Bash", `{"command":"secret-tool lookup service solo-secrets username API_TOKEN"}`) == "" {
		t.Error("reading Solo's keyring items is allowed")
	}
	for tool, in := range map[string]string{
		"Bash": `{"command":"grep -rn solo-secrets internal/"}`,
		"Grep": `{"pattern":"gnome-keyring","path":"."}`,
		"Read": `{"file_path":"/proc/1/environ"}`,
	} {
		if why := ask(tool, in); why != "" {
			t.Errorf("%s %s refused: %s", tool, in, why)
		}
	}
}
