package gui

import (
	"strings"
	"testing"

	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/version"
)

// remoteBackend is a backend that drives another machine's Solo, as the web client's does: it knows
// that machine's version.
type remoteBackend struct {
	*fakeBackend
	host string
}

func (r remoteBackend) HostVersion() string { return r.host }

// Settings says which Solo this is. A page that drives another machine says its own version and the
// machine's, apart.
func TestSettingsShowTheVersion(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "9.8.7"
	page := func(a *App) string {
		t.Helper()
		_, dark := Themes()
		u := uitest.New(t, a.Root, uitest.Size(1440, 1400), uitest.Theme(dark))
		t.Cleanup(u.Close)
		a.settings.Set(true)
		u.Settle()
		return u.Get("settings-page-general").Text()
	}

	if got := page(New(demoBackend())); !strings.Contains(got, "Version") || !strings.Contains(got, "9.8.7") ||
		strings.Contains(got, "Web app") {
		t.Errorf("the desktop's settings don't show its version alone:\n%s", got)
	}

	a := New(remoteBackend{demoBackend(), "9.8.6"})
	a.Machine = "studio"
	got := page(a)
	for _, want := range []string{"Web app", "9.8.7", "Solo on studio", "9.8.6"} {
		if !strings.Contains(got, want) {
			t.Errorf("a page's settings don't show %q:\n%s", want, got)
		}
	}

	// A machine whose Solo is too old to say its version.
	if got := page(New(remoteBackend{demoBackend(), ""})); !strings.Contains(got, "Solo on the machine") ||
		!strings.Contains(got, "Not reported") {
		t.Errorf("an old machine's version isn't marked as not reported:\n%s", got)
	}
}
