package gui

import (
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
)

// The fake keeps secrets as the desktop's backend does (api.Secrets), recording each change.

func (f *fakeBackend) Secrets() []core.SecretInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.secretList)
}

func (f *fakeBackend) SecretsAvailable() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secretErr
}

func (f *fakeBackend) ProvideSecret(threadID, toolUseID, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provided = append(f.provided, threadID+" "+toolUseID+" "+value)
	return nil
}

func (f *fakeBackend) SetSecret(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setSecrets = append(f.setSecrets, name+"="+value)
	return nil
}

func (f *fakeBackend) DeleteSecret(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

var _ api.Secrets = (*fakeBackend)(nil)

// pageBackend is the fake as a web page's backend sees it: only api.Backend, so not an api.Secrets.
type pageBackend struct{ Backend }

// relayID is the thread that asks for a secret in these tests.
const relayID = "d5e6f7a8-relay"

// secretAsk adds the thread "Deploy the relay", waiting on a request for CLOUDFLARE_API_TOKEN, and lets
// the demo's other question go, so the request is the one pinned above Solo's composer.
func secretAsk(f *fakeBackend) {
	for i := range f.threads {
		if th := &f.threads[i]; th.ID == "b7e21a90-limit" {
			th.PendingAsk, th.InTurn, th.Status = false, false, core.StatusIdle
		}
	}
	delete(f.pending, "b7e21a90-limit")
	th := f.threads[1] // a worker to copy
	th.ID, th.Title, th.RepoPath = relayID, "Deploy the relay", "/home/dev/code/infra"
	th.Status, th.PendingAsk, th.InTurn, th.Step, th.Live = core.StatusWaiting, true, true, nil, nil
	th.TurnStarted = time.Now().Add(-3 * time.Minute)
	f.threads = append(f.threads, th)
	qs := []agent.Question{{Question: "Secret CLOUDFLARE_API_TOKEN: to deploy the relay to your Cloudflare account",
		Header: "Secret", Secret: &agent.SecretRequest{Name: "CLOUDFLARE_API_TOKEN", Reason: "to deploy the relay to your Cloudflare account"}}}
	f.pending[relayID] = []core.PendingQuestion{{ToolUseID: "toolu_s1", Questions: qs}}
	f.add(relayID, core.ItemUser, core.UserData{Text: "Deploy the relay to Cloudflare."})
	f.add(relayID, core.ItemAssistant, core.TextData{Text: "The relay builds and its tests pass. Deploying it with wrangler needs an API token for your Cloudflare account."})
	f.add(relayID, core.ItemTool, tool("r1", "mcp__solo__list_secrets", map[string]any{}, "No secrets saved."))
	f.add(relayID, core.ItemQuestion, core.QuestionData{ToolUseID: "toolu_s1", Questions: qs})
}

// demoSecrets are three saved secrets, one used a few minutes ago.
func demoSecrets() []core.SecretInfo {
	now := time.Now()
	return []core.SecretInfo{
		{Name: "CLOUDFLARE_API_TOKEN", CreatedAt: now.Add(-50 * time.Minute), UpdatedAt: now.Add(-50 * time.Minute),
			LastUsed: now.Add(-4 * time.Minute), LastUsedBy: "Deploy the relay", RequestedBy: "Deploy the relay"},
		{Name: "NPM_TOKEN", CreatedAt: now.AddDate(0, 0, -12), UpdatedAt: now.AddDate(0, 0, -12),
			LastUsed: now.Add(-26 * time.Hour), LastUsedBy: "Solo"},
		{Name: "SENTRY_AUTH_TOKEN", CreatedAt: now.AddDate(0, 0, -3), UpdatedAt: now.AddDate(0, 0, -3), RequestedBy: "Upload source maps"},
	}
}

// showsText reports whether any node on screen names, describes or holds s.
func showsText(u *uitest.UI, s string) bool {
	found := false
	u.Tree().Walk(func(n *access.Node) bool {
		if strings.Contains(n.Name, s) || strings.Contains(n.Value, s) || strings.Contains(n.Description, s) {
			found = true
		}
		return !found
	})
	return found
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s didn't happen", what)
}

// A request for a secret pinned above Solo's composer has a masked field; Enter saves what was typed
// through ProvideSecret and empties the field, and the value shows nowhere on screen.
func TestSecretRequestSaves(t *testing.T) {
	f := demoBackend()
	secretAsk(f)
	_, u := newUI(t, f)
	if !showsText(u, "CLOUDFLARE_API_TOKEN") || !u.HasRole(access.RoleButton, "Save securely") {
		t.Fatal("the request for a secret is not shown")
	}
	const value = "cf-test-value-0123456789"
	field := u.Get("secret-value")
	field.Type(value)
	u.Frame()
	if showsText(u, value) {
		t.Fatal("the value shows on screen")
	}
	u.Press("enter")
	u.Frame()
	waitFor(t, "ProvideSecret", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Equal(f.provided, []string{relayID + " toolu_s1 " + value})
	})
	if v := u.Get("secret-value").Value(); v != "" {
		t.Fatalf("the field still holds %q", v)
	}
	if showsText(u, value) {
		t.Fatal("the value shows on screen")
	}
}

// A value too short to redact safely can't be saved, and the card says why.
func TestSecretRequestTooShort(t *testing.T) {
	f := demoBackend()
	secretAsk(f)
	_, u := newUI(t, f)
	u.Get("secret-value").Type("1234")
	u.Frame()
	if !showsText(u, "At least 8 characters") {
		t.Fatal("the card doesn't say why a short value can't be saved")
	}
	u.Press("enter")
	u.Frame()
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	provided := slices.Clone(f.provided)
	f.mu.Unlock()
	if len(provided) != 0 {
		t.Fatalf("a short value was saved: %v", provided)
	}
	u.Get("secret-value").Type("5678")
	u.Frame()
	if showsText(u, "At least 8 characters") {
		t.Fatal("the warning stays once the value is long enough")
	}
}

// Decline drops the request with Dismiss, as Skip does a question.
func TestSecretRequestDeclines(t *testing.T) {
	f := demoBackend()
	secretAsk(f)
	_, u := newUI(t, f)
	u.ByRole(access.RoleButton, "Decline").Click()
	waitFor(t, "Dismiss", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Equal(f.dismissed, []string{relayID + " toolu_s1"})
	})
}

// A web page's backend keeps no secrets: the request says to enter it at the computer, with no field,
// and can still be declined. Settings has no Secrets page there.
func TestSecretRequestOnAPage(t *testing.T) {
	f := demoBackend()
	secretAsk(f)
	a := New(pageBackend{f})
	_, dark := Themes()
	u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(dark))
	t.Cleanup(u.Close)
	started(a, u)
	if !showsText(u, "Enter it in Solo on your computer") {
		t.Fatal("the request doesn't say where to enter it")
	}
	if u.Has("secret-value") || u.HasRole(access.RoleButton, "Save securely") {
		t.Fatal("a page shows a field for the value")
	}
	if !u.HasRole(access.RoleButton, "Decline") {
		t.Fatal("a page can't decline the request")
	}
	a.settings.Set(true)
	u.Frame()
	if u.HasRole(access.RoleTab, "Secrets") {
		t.Fatal("a page lists Settings › Secrets")
	}
}

// When the keyring can't be used, the request says why, with no field.
func TestSecretRequestWithoutKeyring(t *testing.T) {
	f := demoBackend()
	secretAsk(f)
	f.secretErr = errors.New("the system keyring can't be used (no D-Bus)")
	_, u := newUI(t, f)
	u.WaitFor("the keyring's check", func() bool { u.Frame(); return showsText(u, "no D-Bus") })
	if u.Has("secret-value") {
		t.Fatal("a field shows without a keyring")
	}
	if !u.HasRole(access.RoleButton, "Decline") {
		t.Fatal("no Decline")
	}
}

// Finished requests show one line each in a transcript, saved or declined, and wait on no one.
func TestSecretRequestAnswered(t *testing.T) {
	f := demoBackend()
	s := &agent.SecretRequest{Name: "NPM_TOKEN", Reason: "to publish the package"}
	qs := []agent.Question{{Question: "Secret NPM_TOKEN: to publish the package", Header: "Secret", Secret: s}}
	f.add(core.OrchestratorID, core.ItemQuestion, core.QuestionData{ToolUseID: "toolu_a", Questions: qs, Provided: true})
	f.add(core.OrchestratorID, core.ItemQuestion, core.QuestionData{ToolUseID: "toolu_b", Questions: qs, Cancelled: true})
	a, u := newUI(t, f)
	if !showsText(u, "· saved") || !showsText(u, "· declined") {
		t.Fatal("the finished requests aren't shown")
	}
	if u.Has("secret-value") {
		t.Fatal("a finished request still asks")
	}
	for _, it := range f.items[core.OrchestratorID] {
		if a.isAction(row{item: &it}) {
			t.Fatal("a finished request waits on the user")
		}
	}
}

// Settings › Secrets lists the secrets by name; Replace saves a new value through SetSecret, Delete
// asks first, and Add checks the name before saving.
func TestSettingsSecrets(t *testing.T) {
	f := demoBackend()
	f.secretList = demoSecrets()
	a, u := newUI(t, f)
	a.settingsTab = "secrets"
	a.settings.Set(true)
	u.Frame()
	for _, s := range f.secretList {
		if !showsText(u, s.Name) {
			t.Fatalf("%s isn't listed", s.Name)
		}
	}
	if !showsText(u, "Last used") || !showsText(u, "by Deploy the relay") {
		t.Fatal("the last use isn't shown")
	}

	u.ByRole(access.RoleButton, "Replace NPM_TOKEN").Click()
	u.Frame()
	u.Get("secret-new-value").Type("npm-new-value-42")
	u.Press("enter")
	u.Frame()
	waitFor(t, "SetSecret", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Equal(f.setSecrets, []string{"NPM_TOKEN=npm-new-value-42"})
	})
	if u.Has("secret-new-value") || showsText(u, "npm-new-value-42") {
		t.Fatal("the new value is still on screen")
	}

	u.ByRole(access.RoleButton, "Delete SENTRY_AUTH_TOKEN").Click()
	u.Frame()
	f.mu.Lock()
	early := len(f.deleted)
	f.mu.Unlock()
	if early != 0 {
		t.Fatal("deleted without asking")
	}
	u.Get("confirm").ByRole(access.RoleButton, "Delete").Click()
	waitFor(t, "DeleteSecret", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Equal(f.deleted, []string{"SENTRY_AUTH_TOKEN"})
	})

	u.Get("secret-add-name").Type("1PASSWORD")
	u.Frame()
	if !showsText(u, "not starting with a digit") {
		t.Fatal("a bad name isn't flagged")
	}
	u.Press("ctrl+a")
	u.Type("GITHUB_TOKEN")
	u.Get("secret-add-value").Type("ghp-test-value")
	u.Press("enter")
	waitFor(t, "SetSecret for the new one", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.setSecrets, "GITHUB_TOKEN=ghp-test-value")
	})
	u.Frame()
	if showsText(u, "ghp-test-value") || u.Get("secret-add-value").Value() != "" {
		t.Fatal("the value is still on screen")
	}
}

// The list follows the backend's changes (core.ChangeSecrets).
func TestSecretsRefresh(t *testing.T) {
	f := demoBackend()
	a, u := newUI(t, f)
	f.mu.Lock()
	f.secretList = demoSecrets()[:1]
	f.mu.Unlock()
	a.onChange(core.Change{Kind: core.ChangeSecrets})
	u.WaitFor("the list to change", func() bool { u.Frame(); return len(a.secrets.Peek()) == 1 })
}

// The tools agents use secrets with read as what they did.
func TestDescribesSecretTools(t *testing.T) {
	a := New(demoBackend())
	for _, c := range []struct {
		d            core.ToolData
		verb, detail string
	}{
		{tool("1", "mcp__solo__request_secret", map[string]any{"name": "NPM_TOKEN", "reason": "to publish"}, ""), "Asked for a secret", "NPM_TOKEN"},
		{tool("2", "mcp__solo__list_secrets", map[string]any{}, ""), "Listed secrets", ""},
		{tool("3", "mcp__solo__run_with_secrets", map[string]any{"command": "npx wrangler deploy\necho done",
			"secrets": []string{"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_ACCOUNT_ID"}}, ""),
			"Ran with secrets", "CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID · npx wrangler deploy"},
	} {
		if _, verb, detail := a.describeTool(c.d); verb != c.verb || detail != c.detail {
			t.Errorf("%s: %q %q, want %q %q", c.d.Name, verb, detail, c.verb, c.detail)
		}
	}
	if !validSecretName("NPM_TOKEN") || !validSecretName("_x1") || validSecretName("1X") || validSecretName("A-B") || validSecretName("") {
		t.Error("validSecretName disagrees with core's rule")
	}
}

// TestShotsSecrets renders a request for a secret, as the desktop and a web page show it, and Settings ›
// Secrets, for design review:
//
//	SOLO_SHOTS=/tmp/shots go test ./internal/gui -run TestShotsSecrets
func TestShotsSecrets(t *testing.T) {
	out := os.Getenv("SOLO_SHOTS")
	if out == "" {
		t.Skip("set SOLO_SHOTS to an output directory")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	light, dark := Themes()
	shots := []struct {
		name  string
		light bool // in the light theme too
		setup func(f *fakeBackend) (Backend, func(a *App))
	}{
		{"secret-request", true, func(f *fakeBackend) (Backend, func(a *App)) { secretAsk(f); return f, nil }},
		{"secret-request-short", true, func(f *fakeBackend) (Backend, func(a *App)) { secretAsk(f); return f, nil }},
		{"secret-request-page", false, func(f *fakeBackend) (Backend, func(a *App)) { secretAsk(f); return pageBackend{f}, nil }},
		{"secret-request-thread", false, func(f *fakeBackend) (Backend, func(a *App)) {
			secretAsk(f)
			return f, func(a *App) { a.openThread(relayID, "conversation") }
		}},
		{"secret-request-folded", false, func(f *fakeBackend) (Backend, func(a *App)) {
			secretAsk(f)
			return f, func(a *App) { a.foldedAsks.Set(map[string]bool{"toolu_s1": true}) }
		}},
		{"secret-request-nokeyring", false, func(f *fakeBackend) (Backend, func(a *App)) {
			secretAsk(f)
			f.secretErr = errors.New("the system keyring can't be used (no D-Bus session); Solo keeps secrets only there")
			return f, nil
		}},
		{"secret-answered", false, func(f *fakeBackend) (Backend, func(a *App)) {
			secretAsk(f)
			delete(f.pending, relayID)
			items := f.items[relayID]
			var d core.QuestionData
			items[len(items)-1].Decode(&d)
			d.Provided = true
			f.items[relayID] = items[:len(items)-1]
			f.add(relayID, core.ItemQuestion, d)
			f.add(relayID, core.ItemTool, tool("r2", "mcp__solo__run_with_secrets", map[string]any{"command": "npx wrangler deploy",
				"secrets": []string{"CLOUDFLARE_API_TOKEN"}}, "Published relay (2.31 sec)"))
			f.add(relayID, core.ItemAssistant, core.TextData{Text: "Deployed the relay to your Cloudflare account."})
			for i := range f.threads {
				if f.threads[i].ID == relayID {
					f.threads[i].Status, f.threads[i].PendingAsk, f.threads[i].InTurn = core.StatusIdle, false, false
				}
			}
			return f, func(a *App) { a.openThread(relayID, "conversation") }
		}},
		{"settings-secrets", true, func(f *fakeBackend) (Backend, func(a *App)) {
			f.secretList = demoSecrets()
			return f, func(a *App) { a.settingsTab = "secrets"; a.settings.Set(true) }
		}},
		{"settings-secrets-short", false, func(f *fakeBackend) (Backend, func(a *App)) {
			f.secretList = demoSecrets()
			return f, func(a *App) { a.settingsTab = "secrets"; a.settings.Set(true) }
		}},
		{"settings-secrets-empty", false, func(f *fakeBackend) (Backend, func(a *App)) {
			return f, func(a *App) { a.settingsTab = "secrets"; a.settings.Set(true) }
		}},
		{"settings-secrets-nokeyring", false, func(f *fakeBackend) (Backend, func(a *App)) {
			f.secretList = demoSecrets()
			f.secretErr = errors.New("the system keyring can't be used (no D-Bus session); Solo keeps secrets only there")
			return f, func(a *App) { a.settingsTab = "secrets"; a.settings.Set(true) }
		}},
	}
	// Typed or clicked once the window is up.
	after := map[string]func(u *uitest.UI){
		"secret-request":       func(u *uitest.UI) { u.Get("secret-value").Type("cf-test-value") },
		"secret-request-short": func(u *uitest.UI) { u.Get("secret-value").Type("1234") },
		"settings-secrets-short": func(u *uitest.UI) {
			u.ByRole(access.RoleButton, "Replace NPM_TOKEN").Click()
			u.Advance(100 * time.Millisecond)
			u.Get("secret-new-value").Type("abc")
			u.Get("secret-add-name").Type("SENTRY_DSN")
			u.Get("secret-add-value").Type("12345")
		},
		"settings-secrets": func(u *uitest.UI) {
			u.ByRole(access.RoleButton, "Replace NPM_TOKEN").Click()
			u.Advance(100 * time.Millisecond)
			u.Get("secret-add-name").Type("9_LIVES")
		},
	}
	for _, theme := range []struct {
		name string
		t    *ui.Theme
	}{{"dark", dark}, {"light", light}} {
		for _, sh := range shots {
			if theme.name == "light" && !sh.light {
				continue
			}
			f := demoBackend()
			b, setup := sh.setup(f)
			a := New(b)
			if setup != nil {
				setup(a)
			}
			u := uitest.New(t, a.Root, uitest.Size(1440, 900), uitest.Theme(theme.t), uitest.Bindings(Bindings()...))
			for i := 0; i < 10; i++ {
				u.Advance(100 * time.Millisecond)
				time.Sleep(20 * time.Millisecond)
			}
			if do := after[sh.name]; do != nil {
				do(u)
				for i := 0; i < 5; i++ {
					u.Advance(100 * time.Millisecond)
				}
			}
			u.Settle()
			file, err := os.Create(filepath.Join(out, theme.name+"-"+sh.name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			png.Encode(file, u.Screenshot())
			file.Close()
			u.Close()
		}
	}
}
