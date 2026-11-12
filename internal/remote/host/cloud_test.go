//go:build !js

package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/remote/bundle"
	"github.com/tyler-builds/solo/internal/remote/relaytest"
	"github.com/tyler-builds/solo/internal/sample"
)

const goodToken = "cf-test-token-0123456789"

// fakeCloudflare is the part of Cloudflare's API the deploy uses, over one account, with a relay
// standing in for whatever script is deployed there.
type fakeCloudflare struct {
	t     *testing.T
	srv   *httptest.Server
	relay *relaytest.Relay

	mu       sync.Mutex
	scripts  map[string]*fakeScript
	requests []string
	refuse   bool // every request is refused, as with a token that was revoked
}

type fakeScript struct {
	bindings  []map[string]string
	assets    map[string]string // path to hash
	headers   string
	worker    string
	migrated  int
	published bool
	versions  []string
	active    string // the version served; "" for the newest
}

func newFakeCloudflare(t *testing.T) *fakeCloudflare {
	f := &fakeCloudflare{t: t, relay: relaytest.New(), scripts: map[string]*fakeScript{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Cleanup(f.relay.Close)
	return f
}

// client is an HTTP client for which every workers.dev address is the fake's relay, as a deployed
// script's address would be the relay itself.
func (f *fakeCloudflare) client() *http.Client {
	to, _ := url.Parse(f.relay.URL())
	return &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Hostname(), ".workers.dev") {
			req = req.Clone(req.Context())
			req.Host = req.URL.Host // the relay sees the address it was asked for
			req.URL.Scheme, req.URL.Host = "http", to.Host
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (f *fakeCloudflare) keysOf(s *fakeScript) []string {
	for _, b := range s.bindings {
		if b["name"] == "HOST_KEYS" {
			return strings.Split(b["text"], ",")
		}
	}
	return nil
}

// apply makes the relay let in the machines the script's binding lists.
func (f *fakeCloudflare) apply(s *fakeScript) {
	for _, k := range f.keysOf(s) {
		if raw, err := base64.RawURLEncoding.DecodeString(k); err == nil {
			f.relay.Allow(raw)
		}
	}
}

func (f *fakeCloudflare) serve(w http.ResponseWriter, r *http.Request) {
	answer := func(status int, result any, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		out := map[string]any{"success": status < 300, "result": result}
		if msg != "" {
			out["errors"] = []map[string]any{{"code": 1000, "message": msg}}
		}
		json.NewEncoder(w).Encode(out)
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p := strings.TrimPrefix(r.URL.Path, "/accounts/acct1/workers")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if f.refuse || auth != goodToken && !(strings.HasPrefix(auth, "session-") && p == "/assets/upload") {
		answer(403, nil, "Authentication error")
		return
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case r.URL.Path == "/accounts":
		answer(200, []map[string]string{{"id": "acct1", "name": "Personal"}}, "")
	case p == "/subdomain":
		answer(200, map[string]string{"subdomain": "sub"}, "")
	case p == "/scripts" && r.Method == "GET":
		var list []map[string]string
		for name := range f.scripts {
			list = append(list, map[string]string{"id": name})
		}
		answer(200, list, "")
	case p == "/assets/upload":
		answer(201, map[string]string{"jwt": "complete-" + auth}, "")
	case len(parts) >= 2 && parts[0] == "scripts":
		name, rest := parts[1], strings.Join(parts[2:], "/")
		s := f.scripts[name]
		switch {
		case rest == "assets-upload-session":
			var body struct {
				Manifest map[string]struct {
					Hash string `json:"hash"`
					Size int    `json:"size"`
				} `json:"manifest"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			var bucket []string
			for _, a := range body.Manifest {
				bucket = append(bucket, a.Hash)
			}
			answer(200, map[string]any{"jwt": "session-" + name, "buckets": [][]string{bucket}}, "")
		case rest == "" && r.Method == "PUT":
			meta, files := multipartOf(f.t, r)
			var m struct {
				Bindings   []map[string]string `json:"bindings"`
				Migrations map[string]any      `json:"migrations"`
				Assets     struct {
					JWT    string `json:"jwt"`
					Config struct {
						Headers string `json:"_headers"`
					} `json:"config"`
				} `json:"assets"`
			}
			json.Unmarshal(meta["metadata"], &m)
			if s == nil {
				if m.Migrations == nil {
					answer(400, nil, "a new script needs its Durable Object migration")
					return
				}
				s = &fakeScript{}
				f.scripts[name] = s
			} else if m.Migrations != nil {
				answer(412, nil, "migration tag precondition failed")
				return
			}
			if m.Migrations != nil {
				s.migrated++
			}
			if !strings.HasPrefix(m.Assets.JWT, "complete-session-") {
				answer(400, nil, "the assets were not uploaded")
				return
			}
			s.bindings, s.headers, s.worker = m.Bindings, m.Assets.Config.Headers, string(files["worker.js"])
			s.versions = append(s.versions, fmt.Sprintf("v%d-%s", len(s.versions)+1, name))
			f.apply(s)
			answer(200, map[string]string{"id": name}, "")
		case s == nil:
			answer(404, nil, "This Worker does not exist")
		case rest == "settings" && r.Method == "GET":
			answer(200, map[string]any{"bindings": s.bindings}, "")
		case rest == "settings" && r.Method == "PATCH":
			meta, _ := multipartOf(f.t, r)
			var m struct {
				Bindings []map[string]string `json:"bindings"`
			}
			json.Unmarshal(meta["settings"], &m)
			s.bindings = m.Bindings
			s.versions = append(s.versions, fmt.Sprintf("v%d-%s", len(s.versions)+1, name))
			f.apply(s)
			answer(200, nil, "")
		case rest == "deployments":
			active := s.active
			if active == "" && len(s.versions) > 0 {
				active = s.versions[len(s.versions)-1]
			}
			answer(200, map[string]any{"deployments": []map[string]any{{"versions": []map[string]any{{"version_id": active, "percentage": 100}}}}}, "")
		case rest == "subdomain":
			s.published = true
			answer(200, nil, "")
		case rest == "versions":
			var items []map[string]any
			for i, id := range s.versions {
				items = append(items, map[string]any{"id": id, "metadata": map[string]string{
					"created_on": time.Unix(int64(1_800_000_000+i), 0).UTC().Format(time.RFC3339), "source": "api"}})
			}
			answer(200, map[string]any{"items": items}, "")
		case rest == "" && r.Method == "DELETE":
			delete(f.scripts, name)
			answer(200, nil, "")
		default:
			answer(404, nil, "no such route")
		}
	default:
		answer(404, nil, "no such route")
	}
}

// multipartOf reads a multipart body's parts by name.
func multipartOf(t *testing.T, r *http.Request) (fields, files map[string][]byte) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Errorf("content type: %v", err)
		return nil, nil
	}
	fields = map[string][]byte{}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		data, _ := io.ReadAll(part)
		fields[part.FormName()] = data
	}
	return fields, fields
}

func testBundle(version int) func() (*bundle.Bundle, error) {
	return func() (*bundle.Bundle, error) {
		return &bundle.Bundle{Version: version, Worker: []byte(fmt.Sprintf("export default {} // %d", version)),
			Web: map[string][]byte{"/index.html": []byte("<!doctype html>"), "/app.wasm": []byte("\x00asm"), "/fonts/a.ttf": []byte("font")}}, nil
	}
}

func (f *fakeCloudflare) machine(t *testing.T, name string, bundleVersion int) (*Remote, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := New(Config{Dir: dir, Backend: sample.New(sample.Options{Threads: 3, Items: 5}), Secrets: MemSecrets{}, Name: name,
		Retry: 10 * time.Millisecond, FlushEvery: 5 * time.Millisecond, HTTP: f.client(), CloudflareAPI: f.srv.URL,
		Bundle: testBundle(bundleVersion)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, dir
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// From a token to a working relay: Solo deploys the relay and the web client, connects to it, and a
// second machine joins the same relay rather than deploying another.
func TestDeployAndJoin(t *testing.T) {
	f := newFakeCloudflare(t)
	ctx := context.Background()
	first, dir := f.machine(t, "desk", 5)

	if err := first.SetCloudToken(ctx, "wrong"); err == nil {
		t.Fatal("a token Cloudflare refuses was kept")
	}
	if err := first.DeployRelay(ctx); err == nil {
		t.Fatal("deployed without a token")
	}
	if err := first.SetCloudToken(ctx, "  "+goodToken+"\n"); err != nil {
		t.Fatal(err)
	}
	if st := first.State().Cloud; !st.HasToken || st.Account != "Personal" || st.Deployed || !st.CanDeploy {
		t.Fatalf("after the token: %+v", st)
	}
	if err := first.DeployRelay(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first machine to connect", func() bool { return first.State().Connection == Online })
	st := first.State()
	if st.Relay != "https://solo.sub.workers.dev" || !st.Cloud.Deployed || st.Cloud.Busy != "" {
		t.Fatalf("after deploying: %+v", st)
	}
	f.mu.Lock()
	if len(f.scripts) != 1 {
		t.Fatalf("%d scripts", len(f.scripts))
	}
	var name string
	var s *fakeScript
	for name, s = range f.scripts {
	}
	key, _ := first.RelayKey()
	host := name + ".sub.workers.dev"
	if got := f.keysOf(s); !slices.Equal(got, []string{key}) {
		t.Errorf("HOST_KEYS %v, want this machine's key", got)
	}
	if !s.published || s.migrated != 1 || !strings.Contains(s.worker, "// 5") {
		t.Errorf("the script: published %v, migrated %d, worker %q", s.published, s.migrated, s.worker)
	}
	for _, want := range []string{"/*\n", "Content-Security-Policy: default-src 'none'", "connect-src 'self' wss://" + host,
		"Strict-Transport-Security:", "X-Content-Type-Options: nosniff", "Cache-Control: no-cache"} {
		if !strings.Contains(s.headers, want) {
			t.Errorf("the web client's headers lack %q:\n%s", want, s.headers)
		}
	}
	bundleBinding := ""
	for _, b := range s.bindings {
		if b["name"] == "SOLO_BUNDLE" {
			bundleBinding = b["text"]
		}
	}
	if bundleBinding != "5" {
		t.Errorf("SOLO_BUNDLE %q", bundleBinding)
	}
	f.mu.Unlock()

	// The token is in the secrets, and nowhere on disk.
	for _, file := range []string{"remote.json", "remote-audit.log"} {
		if data, _ := os.ReadFile(filepath.Join(dir, file)); strings.Contains(string(data), goodToken) {
			t.Errorf("%s holds the token", file)
		}
	}

	// A second machine, with the same bundle: it adds its key and uploads nothing.
	second, _ := f.machine(t, "laptop", 5)
	if err := second.SetCloudToken(ctx, goodToken); err != nil {
		t.Fatal(err)
	}
	if err := second.DeployRelay(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second machine to connect", func() bool { return second.State().Connection == Online })
	key2, _ := second.RelayKey()
	f.mu.Lock()
	if len(f.scripts) != 1 || s.migrated != 1 {
		t.Errorf("the second machine deployed its own: %d scripts, %d migrations", len(f.scripts), s.migrated)
	}
	if got := f.keysOf(s); len(got) != 2 || !slices.Contains(got, key) || !slices.Contains(got, key2) {
		t.Errorf("HOST_KEYS after joining: %v", got)
	}
	if !strings.Contains(s.worker, "// 5") || len(s.versions) != 2 {
		t.Errorf("joining changed the code: %q, %d versions", s.worker, len(s.versions))
	}
	f.mu.Unlock()
	if second.State().Relay != first.State().Relay {
		t.Error("the two machines use different relays")
	}

	// The first machine didn't make that change: it pauses until its user says the change was theirs.
	if err := first.CheckRelay(ctx); err != nil {
		t.Fatal(err)
	}
	if st := first.State(); !st.Cloud.Paused || st.Connection != Off || st.Cloud.Alert == "" || len(st.Cloud.Changes) != 1 {
		t.Fatalf("after a change from elsewhere: %+v", st)
	}
	// While it is paused nothing gets it going again but the user's answer: not turning it on, not
	// entering the relay's address again, not setting it up again, not a restart.
	address := first.State().Relay
	first.SetEnabled(true)
	first.SetRelay(address)
	if err := first.DeployRelay(ctx); err == nil {
		t.Error("setting up again got round the pause")
	}
	time.Sleep(30 * time.Millisecond)
	if st := first.State(); !st.Cloud.Paused || st.Connection != Off {
		t.Fatalf("the pause was got round: %+v", st)
	}
	if err := first.AcceptRelay(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first machine to resume", func() bool { return first.State().Connection == Online })
	if err := first.CheckRelay(ctx); err != nil || first.State().Cloud.Paused {
		t.Errorf("a check after accepting paused again: %v", err)
	}

	// A relay switched back to an earlier upload, with no new one, is a change too.
	f.mu.Lock()
	s.active = s.versions[0]
	f.mu.Unlock()
	if err := first.CheckRelay(ctx); err != nil || !first.State().Cloud.Paused {
		t.Errorf("a rollback wasn't noticed: %v", err)
	}
	f.mu.Lock()
	s.active = ""
	f.mu.Unlock()
	// Resuming with the devices removed leaves none.
	if err := first.AcceptRelay(ctx, true); err != nil || len(first.State().Devices) != 0 {
		t.Fatalf("resuming without the devices: %v", err)
	}
	// A token Cloudflare no longer takes leaves the relay unchecked, which pauses it as well.
	f.mu.Lock()
	f.refuse = true
	f.mu.Unlock()
	if err := first.CheckRelay(ctx); err != nil || !first.State().Cloud.Paused {
		t.Errorf("a refused token didn't pause: %v", err)
	}
	f.mu.Lock()
	f.refuse = false
	f.mu.Unlock()
	if err := first.AcceptRelay(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first machine to resume again", func() bool { return first.State().Connection == Online })

	// A third machine with a newer bundle replaces the code and the web client, and keeps the keys.
	third, _ := f.machine(t, "tower", 9)
	third.SetCloudToken(ctx, goodToken)
	if err := third.DeployRelay(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if !strings.Contains(s.worker, "// 9") || s.migrated != 1 || len(f.keysOf(s)) != 3 {
		t.Errorf("after a newer bundle: worker %q, %d migrations, keys %v", s.worker, s.migrated, f.keysOf(s))
	}
	f.mu.Unlock()
	// And an older one never replaces a newer.
	if err := second.DeployRelay(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if !strings.Contains(s.worker, "// 9") {
		t.Errorf("an older bundle replaced a newer: %q", s.worker)
	}
	f.mu.Unlock()

	// Removing the relay deletes the script and leaves the machine with no relay.
	if err := third.RemoveRelay(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	n := len(f.scripts)
	f.mu.Unlock()
	if st := third.State(); n != 0 || st.Relay != "" || st.Cloud.Deployed || st.Connection != Off {
		t.Errorf("after removing: %d scripts, %+v", n, st)
	}
	// A machine whose relay was removed elsewhere notices at its next check.
	if err := first.CheckRelay(ctx); err != nil {
		t.Fatal(err)
	}
	if st := first.State(); st.Relay != "" || st.Cloud.Deployed || st.Cloud.Alert == "" || st.Cloud.Paused {
		t.Errorf("the first machine after the relay was removed elsewhere: %+v", st)
	}

	// Forgetting the token leaves nothing of it.
	if err := first.ForgetCloudToken(); err != nil || first.State().Cloud.HasToken {
		t.Errorf("after forgetting the token: %v %+v", err, first.State().Cloud)
	}
}

// The relay gets the short name unless the account has another Worker of that name; a relay an earlier
// version deployed under a longer name is still found and joined.
func TestRelayName(t *testing.T) {
	ctx := context.Background()
	t.Run("the name is taken", func(t *testing.T) {
		f := newFakeCloudflare(t)
		f.scripts["solo"] = &fakeScript{} // somebody's own Worker: no marker
		r, _ := f.machine(t, "desk", 5)
		r.SetCloudToken(ctx, goodToken)
		if err := r.DeployRelay(ctx); err != nil {
			t.Fatal(err)
		}
		if got := r.State().Relay; got != "https://solo-relay.sub.workers.dev" {
			t.Errorf("the relay is at %s", got)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.scripts["solo"].bindings) != 0 || f.scripts["solo"].worker != "" {
			t.Error("another Worker was written over")
		}
	})
	t.Run("an earlier version's relay", func(t *testing.T) {
		f := newFakeCloudflare(t)
		old, _ := f.machine(t, "desk", 5)
		old.SetCloudToken(ctx, goodToken)
		if err := old.DeployRelay(ctx); err != nil {
			t.Fatal(err)
		}
		// As an earlier version named it.
		f.mu.Lock()
		f.scripts["solo-relay-0a1b2c3d4e"] = f.scripts["solo"]
		delete(f.scripts, "solo")
		f.mu.Unlock()
		joiner, _ := f.machine(t, "laptop", 5)
		joiner.SetCloudToken(ctx, goodToken)
		if err := joiner.DeployRelay(ctx); err != nil {
			t.Fatal(err)
		}
		if got := joiner.State().Relay; got != "https://solo-relay-0a1b2c3d4e.sub.workers.dev" {
			t.Errorf("the second machine is at %s", got)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.scripts) != 1 {
			t.Errorf("%d scripts: a second relay was deployed", len(f.scripts))
		}
	})
}
