//go:build !js

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/secrets"
	"github.com/tyler-builds/solo/internal/store"
)

// Secrets: an agent asks the user for one by name (request_secret), the user types it in a masked field
// at the desktop, and it goes to the OS keyring. The agent then runs a command with it
// (run_with_secrets): Solo, not the agent, puts the value in that one command's environment and takes
// it back out of the output and of files the command wrote. The value is never in a tool result, the
// transcript, an agent's environment, or anything sent to a paired device. See docs/ARCHITECTURE.md.

// secretIndexKey is the setting that lists the secrets kept (names and times, no values).
const secretIndexKey = "secrets.index"

// Secret tool names.
const (
	ToolRequestSecret  = "request_secret"
	ToolListSecrets    = "list_secrets"
	ToolRunWithSecrets = "run_with_secrets"
)

// maxRunOutput is how much of a command's output run_with_secrets returns: its end.
const maxRunOutput = 30000

// SecretToolSpecs are the secret tools every session gets (Solo's and each thread's), served by Core.
var SecretToolSpecs = []agent.ToolSpec{
	{
		Name: ToolRequestSecret,
		Description: "Ask the user for a secret (an API key, token or password) by name. The app shows them a masked field; the value goes to the system keyring and never to you. " +
			"The result says only whether they saved it or declined. Use this instead of asking for a secret in chat. Then use it with run_with_secrets.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"The environment variable it is given as in run_with_secrets, such as CLOUDFLARE_API_TOKEN: letters, digits and underscores."},
			"reason":{"type":"string","description":"A few words on what it is for, shown to the user: \"to deploy the relay\"."},
			"replace":{"type":"boolean","description":"Ask again for one that is already kept, because it is wrong or stopped working."}},
			"required":["name","reason"]}`),
	},
	{
		Name:        ToolListSecrets,
		Description: "List the names of the secrets the user has saved for agents. Names only: values are never shown.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		ReadOnly:    true,
	},
	{
		Name: ToolRunWithSecrets,
		Description: "Run a shell command (bash) with saved secrets set as environment variables in it, and only in it: refer to them as $NAME. " +
			"The app runs it, not your shell, and returns its exit code and output (stdout and stderr) with every secret value, and common encodings of it, replaced by [secret NAME]. " +
			"Files it writes that hold a secret are scrubbed too, so don't use it to write a secret into a file or to log a tool in for later: pass the secret in each command that needs it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"command":{"type":"string","description":"The command, such as: npx wrangler deploy, or curl -H \"Authorization: Bearer $API_TOKEN\" https://..."},
			"secrets":{"type":"array","items":{"type":"string"},"description":"Names of the secrets to set, from list_secrets."},
			"cwd":{"type":"string","description":"Folder to run in; default is your working directory. A thread's must be inside its own."},
			"timeout_seconds":{"type":"integer","description":"Stop it after this long (default 120, at most 600)."}},
			"required":["command","secrets"]}`),
	},
}

// IsSecretTool reports whether name is one of SecretToolSpecs.
func IsSecretTool(name string) bool {
	return name == ToolRequestSecret || name == ToolListSecrets || name == ToolRunWithSecrets
}

// secretState is what Core keeps about secrets. It has its own lock (Core.secretMu), never held while
// calling the keyring or taking c.mu.
type secretState struct {
	index    []SecretInfo
	values   map[string]string // loaded from the keyring, to redact transcripts
	redactor *secrets.Redactor
}

func (c *Core) loadSecretIndex() {
	var idx []SecretInfo
	if _, err := c.db.GetSetting(secretIndexKey, &idx); err != nil {
		c.log.Warn("core: secret list unreadable", "err", err)
	}
	c.secretMu.Lock()
	c.sec.index = idx
	c.sec.values = map[string]string{}
	c.sec.redactor = secrets.NewRedactor(nil)
	c.secretMu.Unlock()
	if len(idx) > 0 {
		go c.loadSecretValues()
	}
}

// loadSecretValues reads the kept values from the keyring, so that transcripts can be redacted.
func (c *Core) loadSecretValues() {
	if c.secretStore.Available() != nil {
		return
	}
	for _, s := range c.Secrets() {
		v, err := c.secretStore.Get(s.Name)
		if err != nil {
			continue
		}
		c.secretMu.Lock()
		c.sec.values[s.Name] = v
		c.sec.redactor = secrets.NewRedactor(c.sec.values)
		c.secretMu.Unlock()
	}
}

// Secrets lists the secrets kept for agents, by name. Values are in the keyring only.
func (c *Core) Secrets() []SecretInfo {
	c.secretMu.Lock()
	defer c.secretMu.Unlock()
	out := slices.Clone(c.sec.index)
	slices.SortFunc(out, func(a, b SecretInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// SecretsAvailable says why secrets can't be kept on this machine (no usable OS keyring), or nil.
func (c *Core) SecretsAvailable() error { return c.secretStore.Available() }

func (c *Core) hasSecret(name string) bool {
	c.secretMu.Lock()
	defer c.secretMu.Unlock()
	return slices.ContainsFunc(c.sec.index, func(s SecretInfo) bool { return s.Name == name })
}

// SetSecret saves a secret's value in the keyring under name, adding it or replacing it, as the user
// does in Settings.
func (c *Core) SetSecret(name, value string) error { return c.setSecret(name, value, "") }

func (c *Core) setSecret(name, value, requestedBy string) error {
	value = strings.Trim(value, "\r\n")
	switch {
	case !secrets.ValidName(name):
		return fmt.Errorf("%q can't name a secret: use letters, digits and underscores, not starting with a digit", name)
	case value == "":
		return errors.New("the secret is empty")
	}
	if err := c.secretStore.Set(name, value); err != nil {
		return err
	}
	c.secretMu.Lock()
	at := now()
	i := slices.IndexFunc(c.sec.index, func(s SecretInfo) bool { return s.Name == name })
	if i < 0 {
		c.sec.index = append(c.sec.index, SecretInfo{Name: name, CreatedAt: at})
		i = len(c.sec.index) - 1
	}
	c.sec.index[i].UpdatedAt = at
	if requestedBy != "" {
		c.sec.index[i].RequestedBy = requestedBy
	}
	c.sec.values[name] = value
	c.sec.redactor = secrets.NewRedactor(c.sec.values)
	err := c.saveSecretIndexLocked()
	c.secretMu.Unlock()
	c.notify(Change{Kind: ChangeSecrets})
	return err
}

// DeleteSecret removes a secret from the keyring and the list.
func (c *Core) DeleteSecret(name string) error {
	if err := c.secretStore.Delete(name); err != nil {
		return err
	}
	c.secretMu.Lock()
	c.sec.index = slices.DeleteFunc(c.sec.index, func(s SecretInfo) bool { return s.Name == name })
	// Its value stays in the redactor until Solo restarts: what it already wrote is still redacted.
	err := c.saveSecretIndexLocked()
	c.secretMu.Unlock()
	c.notify(Change{Kind: ChangeSecrets})
	return err
}

// saveSecretIndexLocked saves the list. Call with c.secretMu held.
func (c *Core) saveSecretIndexLocked() error {
	return c.db.SetSetting(secretIndexKey, c.sec.index)
}

// Scrub takes every kept secret's value, and its common encodings, out of s.
func (c *Core) Scrub(s string) string {
	c.secretMu.Lock()
	r := c.sec.redactor
	c.secretMu.Unlock()
	return r.RedactFull(s)
}

// scrubJSON takes secret values out of every string in an item's data. It is a net under the rest:
// a value reaches an item only if the user pasted it into a message or an agent got hold of it.
func (c *Core) scrubJSON(raw []byte) []byte {
	c.secretMu.Lock()
	r := c.sec.redactor
	c.secretMu.Unlock()
	if !r.Contains(string(raw)) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return []byte(r.RedactFull(string(raw)))
	}
	out, err := json.Marshal(scrubValue(v, r))
	if err != nil {
		return raw
	}
	return out
}

func scrubValue(v any, r *secrets.Redactor) any {
	switch x := v.(type) {
	case string:
		return r.RedactFull(x)
	case []any:
		for i := range x {
			x[i] = scrubValue(x[i], r)
		}
	case map[string]any:
		for k := range x {
			x[k] = scrubValue(x[k], r)
		}
	}
	return v
}

// ProvideSecret saves the value the user typed for a thread's request_secret, and tells the thread it
// was saved. Only the desktop calls it: a paired device can decline a request (Dismiss) but not answer
// one, so a value never crosses the relay.
func (c *Core) ProvideSecret(threadID, toolUseID, value string) error {
	c.mu.Lock()
	t := c.threads[threadID]
	var req *agent.SecretRequest
	title := ""
	if t != nil {
		req = SecretOf(t.askQs[toolUseID])
		title = t.row.Title
		if t.row.ID == OrchestratorID {
			title = "Solo"
		}
	}
	c.mu.Unlock()
	if req == nil {
		return errors.New("that request is no longer waiting for a secret")
	}
	if err := c.setSecret(req.Name, value, title); err != nil {
		return err
	}
	return c.resolveAsk(threadID, toolUseID, agent.AskResult{}, FromUser)
}

// secretTool serves a secret tool for thread t. ok is false for any other tool.
func (c *Core) secretTool(ctx context.Context, t *thread, name string, args []byte) (text string, isErr, ok bool) {
	if !IsSecretTool(name) {
		return "", false, false
	}
	var err error
	switch name {
	case ToolRequestSecret:
		text, err = c.requestSecret(ctx, t, args)
	case ToolListSecrets:
		text = c.listSecretsText()
	case ToolRunWithSecrets:
		text, err = c.runWithSecrets(ctx, t, args)
	}
	if err != nil {
		return err.Error(), true, true
	}
	return text, false, true
}

func (c *Core) listSecretsText() string {
	list := c.Secrets()
	if len(list) == 0 {
		return "No secrets saved. Ask the user for one with request_secret."
	}
	var b strings.Builder
	b.WriteString("Saved secrets (use them with run_with_secrets, as $NAME):\n")
	for _, s := range list {
		fmt.Fprintf(&b, "- %s (saved %s", s.Name, s.UpdatedAt.Format("2006-01-02"))
		if !s.LastUsed.IsZero() {
			fmt.Fprintf(&b, ", last used %s", s.LastUsed.Format("2006-01-02"))
		}
		b.WriteString(")\n")
	}
	return b.String()
}

func (c *Core) requestSecret(ctx context.Context, t *thread, args []byte) (string, error) {
	var a struct {
		Name    string `json:"name"`
		Reason  string `json:"reason"`
		Replace bool   `json:"replace"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	a.Name, a.Reason = strings.TrimSpace(a.Name), strings.TrimSpace(a.Reason)
	switch {
	case !secrets.ValidName(a.Name):
		return "", fmt.Errorf("%q can't name a secret: use an environment variable name (letters, digits and underscores, not starting with a digit), at most %d characters", a.Name, secrets.MaxName)
	case a.Reason == "":
		return "", errors.New("say what the secret is for in reason: the user sees it")
	}
	if err := c.secretStore.Available(); err != nil {
		return "", fmt.Errorf("Solo can't keep secrets on this machine: %v. Tell the user; don't ask for the value in chat", err)
	}
	if c.hasSecret(a.Name) && !a.Replace {
		return fmt.Sprintf("%s is already saved. Use it with run_with_secrets (secrets: [%q]). Pass replace: true only when it is wrong or stopped working.", a.Name, a.Name), nil
	}
	reason := a.Reason
	if len(reason) > 300 {
		reason = reason[:300]
	}
	q := agent.Question{
		Question: fmt.Sprintf("Secret %s: %s", a.Name, reason),
		Header:   "Secret",
		Secret:   &agent.SecretRequest{Name: a.Name, Reason: reason, Replace: a.Replace},
	}
	c.mu.Lock()
	res, ok := c.askLocked(ctx, t, agent.NewUUID(), []agent.Question{q})
	if !ok {
		return "", errors.New("the request was withdrawn")
	}
	if res.Cancelled {
		if res.Message != "" {
			return "", errors.New(res.Message)
		}
		return fmt.Sprintf("The user declined to give %s. Don't ask for it in chat or any other way; carry on without it, or say what you couldn't do without it.", a.Name), nil
	}
	return fmt.Sprintf("The user saved %s. You can't read its value: run the commands that need it with run_with_secrets (secrets: [%q]), where it is the environment variable $%s.", a.Name, a.Name, a.Name), nil
}

func (c *Core) runWithSecrets(ctx context.Context, t *thread, args []byte) (string, error) {
	var a struct {
		Command string   `json:"command"`
		Secrets []string `json:"secrets"`
		Cwd     string   `json:"cwd"`
		Timeout int      `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("command is empty")
	}
	if why := secrets.Forbidden(a.Command); why != "" {
		return "", errors.New(why)
	}
	if err := c.secretStore.Available(); err != nil {
		return "", fmt.Errorf("Solo can't keep secrets on this machine: %v", err)
	}
	c.mu.Lock()
	row := t.row
	c.mu.Unlock()
	root, dir, skip, err := c.runDir(row, a.Cwd)
	if err != nil {
		return "", err
	}
	env := map[string]string{}
	for _, name := range a.Secrets {
		if !c.hasSecret(name) {
			return "", fmt.Errorf("no secret named %q is saved (list_secrets shows the names). Ask the user for it with request_secret", name)
		}
		v, err := c.secretStore.Get(name)
		if errors.Is(err, secrets.ErrNotFound) {
			return "", fmt.Errorf("%s is no longer in the system keyring. Ask the user for it again with request_secret (replace: true)", name)
		} else if err != nil {
			return "", fmt.Errorf("reading %s from the system keyring: %v", name, err)
		}
		env[name] = v
	}
	// Every value kept is redacted, not only these: the command could reach the others some other way.
	c.loadSecretValues()
	c.secretMu.Lock()
	all := map[string]string{}
	for k, v := range c.sec.values {
		all[k] = v
	}
	c.secretMu.Unlock()
	for k, v := range env {
		all[k] = v
	}
	var roots []string
	if root != dir {
		roots = append(roots, root)
	}
	res, err := secrets.Run(ctx, secrets.RunSpec{
		Command:  a.Command,
		Dir:      dir,
		Secrets:  env,
		Redactor: secrets.NewRedactor(all),
		Timeout:  time.Duration(a.Timeout) * time.Second,
		Roots:    roots,
		Skip:     skip,
	})
	if err != nil {
		return "", err
	}
	c.markUsed(a.Secrets, row)

	var b strings.Builder
	switch {
	case res.TimedOut:
		b.WriteString("Timed out, and stopped.\n")
	default:
		fmt.Fprintf(&b, "Exit code: %d\n", res.ExitCode)
	}
	out := res.Output
	if len(out) > maxRunOutput {
		cut := len(out) - maxRunOutput
		out = fmt.Sprintf("[… %d earlier characters left out]\n", cut) + strings.ToValidUTF8(out[cut:], "")
	}
	if strings.TrimSpace(out) == "" {
		out = "(no output)"
	}
	b.WriteString(out)
	if !strings.HasSuffix(out, "\n") {
		b.WriteByte('\n')
	}
	if res.Dropped > 0 {
		fmt.Fprintf(&b, "[%d more bytes of output weren't kept]\n", res.Dropped)
	}
	if res.Redacted {
		b.WriteString("[Secret values in the output are shown as [secret NAME].]\n")
	}
	if len(res.Scrubbed) > 0 {
		fmt.Fprintf(&b, "[The command wrote a secret into %s; Solo replaced it there with [secret NAME]. Pass secrets to each command that needs them instead of saving them in files or logins.]\n",
			strings.Join(res.Scrubbed, ", "))
	}
	return b.String(), nil
}

// runDir is where a thread's run_with_secrets command runs: root is the thread's own folder (its
// worktree, or Solo's working folder), dir is cwd resolved in it. A thread's command stays inside its
// own folder. skip are folders not to look through for files the command wrote.
func (c *Core) runDir(row store.Thread, cwd string) (root, dir string, skip []string, err error) {
	if row.Kind == store.KindOrchestrator {
		root = filepath.Join(c.cfg.DataDir, "orchestrator")
		if _, err := os.Stat(root); err != nil {
			root = c.cfg.DataDir
		}
		// Every thread's worktree is under the data folder: too much to look through after each command.
		skip = []string{filepath.Join(c.cfg.DataDir, "worktrees"), filepath.Join(c.cfg.DataDir, "attachments")}
	} else {
		root = c.sessionDir(row)
	}
	if root == "" {
		return "", "", nil, errors.New("this thread has no folder to run commands in")
	}
	dir = root
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		if !filepath.IsAbs(cwd) {
			cwd = filepath.Join(root, cwd)
		}
		dir = filepath.Clean(cwd)
		if row.Kind != store.KindOrchestrator {
			if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", "", nil, fmt.Errorf("cwd must be inside your working directory, %s", root)
			}
		}
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", "", nil, fmt.Errorf("%s isn't a folder", dir)
	}
	return root, dir, skip, nil
}

func (c *Core) markUsed(names []string, row store.Thread) {
	by := row.Title
	if row.ID == OrchestratorID {
		by = "Solo"
	}
	c.secretMu.Lock()
	at := now()
	for i := range c.sec.index {
		if slices.Contains(names, c.sec.index[i].Name) {
			c.sec.index[i].LastUsed, c.sec.index[i].LastUsedBy = at, by
		}
	}
	err := c.saveSecretIndexLocked()
	c.secretMu.Unlock()
	if err != nil {
		c.log.Warn("core: saving the secret list", "err", err)
	}
	c.notify(Change{Kind: ChangeSecrets})
}
