package gui

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/agent"
	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/core"
)

// Secrets: an agent asks for one with request_secret, the user types it into a masked field, and it
// goes to the OS keyring; the agent then runs commands with it (run_with_secrets) without seeing it.
// Only the desktop's backend keeps secrets (api.Secrets). A web page's isn't one, so a value never
// crosses the relay: a page shows that a request waits at the computer, and can decline it. Nothing
// here shows a value, or keeps one past the click that saves it.

// secretsBackend is the backend as an api.Secrets, or nil in a web page.
func (a *App) secretsBackend() api.Secrets {
	s, _ := a.b.(api.Secrets)
	return s
}

// loadSecrets lists the secrets kept, or nil when the backend keeps none.
func loadSecrets(b Backend) []core.SecretInfo {
	if s, ok := b.(api.Secrets); ok {
		return s.Secrets()
	}
	return nil
}

// secretsUnavailable says why this machine can't keep secrets, or "". The first check tries the
// keyring, which can be slow, so it runs off the UI thread; until it answers, secrets count as usable.
func (a *App) secretsUnavailable(ctx *ui.Context) string {
	sec := a.secretsBackend()
	if sec == nil {
		return ""
	}
	q := ui.Query(ctx, "secrets-available", func(context.Context) (string, error) {
		err := sec.SecretsAvailable()
		if err == nil {
			return "", nil
		}
		// Core's errors read as a clause ("the system keyring can't be used …"); this shows one alone.
		why := strings.TrimSpace(err.Error())
		why = strings.ToUpper(why[:1]) + why[1:]
		if !strings.HasSuffix(why, ".") {
			why += "."
		}
		return why, nil
	}, ui.QueryStaleTime(time.Minute))
	return q.Data
}

// validSecretName is core's rule for a secret's name: an environment variable's (letters, digits
// and underscores, not starting with a digit), at most 64 characters.
func validSecretName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// secretReason is what a request says the secret is for, as a sentence: "to deploy the relay"
// becomes "Needed to deploy the relay."
func secretReason(s agent.SecretRequest) string {
	r := strings.TrimSpace(s.Reason)
	if r == "" {
		return ""
	}
	lower := strings.ToLower(r)
	if strings.HasPrefix(lower, "to ") || strings.HasPrefix(lower, "for ") {
		r = "Needed " + r
	} else {
		r = strings.ToUpper(r[:1]) + r[1:]
	}
	if !strings.HasSuffix(r, ".") {
		r += "."
	}
	return r
}

// secretForm is a request for a secret (request_secret) where a question would show: the name and
// what it is for, a masked field, Save securely and Decline. title names the asking thread when the
// form is shown outside it (the strip above Solo's composer), which adds the fold button.
func (a *App) secretForm(ctx *ui.Context, threadID, toolUseID string, s agent.SecretRequest, title string) ui.Element {
	return ui.Component(func(ctx *ui.Context) ui.Element {
		t := ctx.Theme()
		sec := a.secretsBackend()
		value := ui.Local(ctx, "value", "")
		decline := ui.Button("Decline").Secondary().Small().
			Tooltip("Don't give it; the agent carries on without it").
			OnClick(func() {
				a.run("Declining", func(context.Context) error { return a.b.Dismiss(threadID, toolUseID) })
			})
		var head ui.Element = a.cardHeader(ctx, icons.KeyRound, t.Warning, "Secret", threadID)
		if title != "" {
			head = ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(ui.Div().Grow(1).MinWidth(ui.Px(0)).Child(head)).
				Child(ui.IconButton(icons.ChevronDown).Ghost().Small().Label("Fold the request").Tooltip("Fold").
					OnClick(func() {
						next := map[string]bool{toolUseID: true}
						for k, v := range a.foldedAsks.Peek() {
							next[k] = v
						}
						a.foldedAsks.Set(next)
					}))
		}
		what := ui.Div().Column().Gap(space1).
			Child(ui.Text(s.Name).Mono().FontSize(14).Medium().TextColor(t.Text).Selectable())
		if r := secretReason(s); r != "" {
			what = what.Child(muted(ctx, r))
		}
		if s.Replace {
			what = what.Child(caption(ctx, "One is saved under this name already; this replaces it."))
		}
		card := ui.Div().Column().Gap(space3).Padding(cardPad).Radius(radiusL).Background(t.Surface).
			Border(1, t.Warning.Alpha(0.4)).Role(access.RoleGroup).Label(cmp.Or(title, "Request for a secret")).
			Child(head).
			Child(what)
		note := func(icon *icons.Icon, color ui.Color, text string) ui.Element {
			return ui.Div().Row().Align(ui.Start).Gap(space2).
				Child(ui.Div().PaddingTop(1).Child(ui.Icon(icon).Size(13).Color(color))).
				Child(caption(ctx, text).Grow(1).MinWidth(ui.Px(0)))
		}
		if sec == nil {
			// A web page: the value is typed only at the computer, so it never crosses the relay.
			return card.
				Child(note(icons.Monitor, t.TextMuted, "Enter it in Solo on your computer. Secrets are typed only there, so they never pass through the relay.")).
				Child(ui.Div().Row().Child(decline))
		}
		if why := a.secretsUnavailable(ctx); why != "" {
			return card.
				Child(note(icons.CircleAlert, t.Danger, why)).
				Child(ui.Div().Row().Child(decline))
		}
		save := func() {
			v := value.Peek()
			if strings.TrimSpace(v) == "" {
				return
			}
			value.Set("") // the value leaves the UI with this click
			a.run("Saving the secret", func(context.Context) error { return sec.ProvideSecret(threadID, toolUseID, v) })
		}
		field := ui.TextInput(value).Masked().Placeholder("Paste or type the value").Label("Value of " + s.Name).
			Small().Grow(1).Key("secret-value").OnSubmit(func(string) { save() })
		return card.
			Child(ui.Div().Row().Align(ui.Center).Gap(space2).
				Child(field).
				Child(ui.Button("Save securely").Icon(icons.LockKeyhole).Primary().Small().
					Disabled(strings.TrimSpace(value.Get()) == "").OnClick(save)).
				Child(decline)).
			Child(note(icons.ShieldCheck, t.TextSubtle, "Saved in your system keyring. Solo and its threads can use it in commands but never see it."))
	})
}

// secretDone is a finished request for a secret in a transcript: its name, and whether it was saved.
func secretDone(ctx *ui.Context, s agent.SecretRequest, d core.QuestionData) ui.Element {
	t := ctx.Theme()
	state := "declined"
	if d.Provided {
		state = "saved"
	}
	return lineRow().Align(ui.Center).Gap(6).
		Child(ui.Icon(icons.KeyRound).Size(13).Color(t.TextSubtle)).
		Child(subtle(ctx, "Secret")).
		Child(ui.Text(s.Name).Mono().FontSize(12).TextColor(t.TextSubtle)).
		Child(subtle(ctx, "· "+state))
}

// secretDate says when a secret was saved: "today", "Nov 3", or with the year when it isn't this one.
func secretDate(at, now time.Time) string {
	at, now = at.Local(), now.Local()
	switch {
	case at.YearDay() == now.YearDay() && at.Year() == now.Year():
		return "today"
	case at.Year() == now.Year():
		return at.Format("Jan 2")
	}
	return at.Format("Jan 2, 2006")
}

// secretAbout is what Settings says under a secret's name: when it was saved and who asked for it,
// then, when it has been used, when and by whom ("" if never).
func secretAbout(s core.SecretInfo, now time.Time) (saved, used string) {
	saved = "Saved " + secretDate(cmp.Or(s.UpdatedAt, s.CreatedAt), now)
	if s.RequestedBy != "" {
		saved += " · Asked for by " + s.RequestedBy
	}
	if !s.LastUsed.IsZero() {
		used = "Last used " + doneAgo(s.LastUsed, now)
		if s.LastUsedBy != "" {
			used += " by " + s.LastUsedBy
		}
	}
	return saved, used
}

// secretsPage is Settings › Secrets: the secrets kept, each with Replace and Delete, and a row that
// adds one. nil in a web page, where secrets aren't managed.
func (a *App) secretsPage(ctx *ui.Context) ui.Element {
	sec := a.secretsBackend()
	if sec == nil {
		return nil
	}
	t := ctx.Theme()
	why := a.secretsUnavailable(ctx)
	list := a.secrets.Get()
	var rows []ui.Element
	for _, s := range list {
		rows = append(rows, ui.Component(func(ctx *ui.Context) ui.Element {
			return a.secretRow(ctx, sec, s, why == "")
		}).Key("secret-"+s.Name))
	}
	if len(rows) == 0 {
		rows = append(rows, ui.Div().PaddingX(space4).PaddingY(space3).
			Child(caption(ctx, "None yet. When an agent asks for one, it is saved here; or add one below.")))
	}
	var problem, add ui.Element
	if why != "" {
		problem = settingsSection(ctx, "Unavailable", "", nil,
			ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).PaddingY(space3).MinHeight(ui.Px(60)).
				Child(ui.Icon(icons.CircleAlert).Size(16).Color(t.Danger)).
				Child(ui.Text(why).FontSize(13).TextColor(t.Text).Grow(1).MinWidth(ui.Px(0))))
	} else {
		add = settingsSection(ctx, "Add a secret", "The name is the environment variable a command gets it as.", nil,
			ui.Component(func(ctx *ui.Context) ui.Element { return a.addSecretRow(ctx, sec) }).Key("secret-add"))
	}
	return settingsPage(ctx, "Secrets",
		"Agents ask for a secret with request_secret and use it through run_with_secrets, which sets it in one command's environment. "+
			"Values live in your system keyring and are never shown to agents, or here.",
		problem,
		settingsSection(ctx, "Saved", "", nil, rows...),
		add)
}

// secretRow is a secret in Settings: its name and history, with Replace (a masked field that opens
// under it) and Delete, which asks first. canChange false leaves both out.
func (a *App) secretRow(ctx *ui.Context, sec api.Secrets, s core.SecretInfo, canChange bool) ui.Element {
	t := ctx.Theme()
	name := s.Name
	replacing := ui.Local(ctx, "replacing", false)
	value := ui.Local(ctx, "value", "")
	saved, used := secretAbout(s, a.now())
	about := ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
		Child(ui.Text(name).Mono().FontSize(13).Medium().TextColor(t.Text).Truncate().Selectable()).
		Child(caption(ctx, saved).Truncate())
	if used != "" {
		about = about.Child(caption(ctx, used).Truncate())
	}
	row := ui.Div().Row().Align(ui.Center).Gap(space3).PaddingLeft(space4).PaddingRight(space3).PaddingY(space3).
		Child(ui.Icon(icons.KeyRound).Size(15).Color(t.TextMuted)).
		Child(about)
	if canChange && !replacing.Get() {
		row = row.
			Child(ui.Button("Replace").Secondary().Small().Label("Replace " + name).
				Tooltip("Enter a new value").OnClick(func() { replacing.Set(true) })).
			Child(ui.Button("Delete").Secondary().Small().Label("Delete " + name).OnClick(func() {
				a.confirm.Set(&confirmation{title: "Delete " + name + "?",
					body:   "It is removed from your system keyring. Commands that use it fail until an agent asks for it again or you add it here.",
					action: "Delete",
					run: func() {
						a.run("Deleting the secret", func(context.Context) error { return sec.DeleteSecret(name) })
					}})
			}))
	}
	if !canChange || !replacing.Get() {
		return row
	}
	cancel := func() {
		value.Set("")
		replacing.Set(false)
	}
	save := func() {
		v := value.Peek()
		if strings.TrimSpace(v) == "" {
			return
		}
		value.Set("") // the value leaves the UI with this click
		replacing.Set(false)
		a.run("Replacing the secret", func(context.Context) error { return sec.SetSecret(name, v) })
	}
	return ui.Div().Column().
		Child(row).
		Child(ui.Div().Row().Align(ui.Center).Gap(space2).PaddingLeft(space4 + 15 + space3).PaddingRight(space3).PaddingBottom(space3).
			Child(ui.TextInput(value).Masked().Placeholder("Paste or type the new value").Label("New value of " + name).
				Small().Grow(1).AutoFocus().Key("secret-new-value").OnSubmit(func(string) { save() })).
			Child(ui.Button("Save").Primary().Small().Disabled(strings.TrimSpace(value.Get()) == "").OnClick(save)).
			Child(ui.Button("Cancel").Ghost().Small().OnClick(cancel)))
}

// addSecretRow adds a secret by hand: its name, checked as it is typed, and a masked value.
func (a *App) addSecretRow(ctx *ui.Context, sec api.Secrets) ui.Element {
	t := ctx.Theme()
	name := ui.Local(ctx, "name", "")
	value := ui.Local(ctx, "value", "")
	n := strings.TrimSpace(name.Get())
	saved := slices.ContainsFunc(a.secrets.Get(), func(s core.SecretInfo) bool { return s.Name == n })
	add := func() {
		n := strings.TrimSpace(name.Peek())
		v := value.Peek()
		if !validSecretName(n) || strings.TrimSpace(v) == "" {
			return
		}
		value.Set("") // the value leaves the UI with this click
		name.Set("")
		a.run("Saving the secret", func(context.Context) error { return sec.SetSecret(n, v) })
	}
	inputs := ui.Div().Row().Align(ui.Center).Gap(space2).
		Child(ui.TextInput(name).Mono().Placeholder("NAME").Label("Name of the secret").Small().Width(ui.Px(220)).
			Key("secret-add-name").OnSubmit(func(string) { add() })).
		Child(ui.TextInput(value).Masked().Placeholder("Value").Label("Value of the secret").Small().Grow(1).
			Key("secret-add-value").OnSubmit(func(string) { add() })).
		Child(ui.Button("Add").Icon(icons.Plus).Secondary().Small().
			Disabled(!validSecretName(n) || strings.TrimSpace(value.Get()) == "").OnClick(add))
	col := ui.Div().Column().Gap(space2).PaddingX(space4).PaddingY(space3).Child(inputs)
	switch {
	case n != "" && !validSecretName(n):
		col = col.Child(ui.Div().Row().Align(ui.Center).Gap(space1 + 2).
			Child(ui.Icon(icons.CircleAlert).Size(13).Color(t.Danger)).
			Child(ui.Text("Use letters, digits and underscores, not starting with a digit, such as NPM_TOKEN.").FontSize(12).TextColor(t.Danger)))
	case saved:
		col = col.Child(caption(ctx, "Replaces the saved "+n+"."))
	}
	return col
}
