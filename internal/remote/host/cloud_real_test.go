//go:build !js

package host

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/flynn/noise"

	"github.com/tyler-builds/solo/internal/cloudflare"
	"github.com/tyler-builds/solo/internal/remote/device"
	"github.com/tyler-builds/solo/internal/remote/relaytest"
	"github.com/tyler-builds/solo/internal/remote/rpc"
	"github.com/tyler-builds/solo/internal/remote/secure"
	"github.com/tyler-builds/solo/internal/sample"
)

// TestRealCloudflare sets remote access up in a real Cloudflare account, uses it, and removes it: the
// deploy, the relay at the edge, pairing and a session through it, a second machine joining, the check
// for changes from elsewhere, and removal.
//
//	SOLO_CF_TOKEN_FILE=/path/to/token go test ./internal/remote/host -run TestRealCloudflare -v
//
// The file holds an API token that can edit Workers scripts and read account settings; it is never
// printed. The app must have been built with the bundle (scripts/bundle.sh). The test refuses to run in
// an account that already has a relay, and deletes the one it makes whatever happens.
func TestRealCloudflare(t *testing.T) {
	file := os.Getenv("SOLO_CF_TOKEN_FILE")
	if file == "" {
		t.Skip("set SOLO_CF_TOKEN_FILE to run against a real Cloudflare account")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("the token file can't be read")
	}
	token := strings.TrimSpace(string(raw))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	machine := func(name string) *Remote {
		r, err := New(Config{Dir: t.TempDir(), Backend: sample.New(sample.Options{Threads: 5, Items: 20}),
			Secrets: MemSecrets{}, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Close)
		if err := r.SetCloudToken(ctx, token); err != nil {
			t.Fatalf("the token: %v", err)
		}
		return r
	}
	first := machine("solo-test-one")
	cf := &cloudflare.Client{Token: token}
	accounts, err := cf.Accounts(ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts: %d, %v", len(accounts), err)
	}
	if found, err := cf.Find(ctx, accounts[0].ID); err != nil {
		t.Fatal(err)
	} else if found != nil {
		t.Fatal("this account already has a Solo relay; the test would change it and then delete it, so it stops here")
	}
	deployed := false
	defer func() {
		if !deployed {
			return
		}
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := first.RemoveRelay(c); err != nil {
			t.Errorf("REMOVE THE RELAY BY HAND in the Cloudflare dashboard (a Worker named solo): %v", err)
		} else {
			t.Log("the relay was removed")
		}
	}()

	began := time.Now()
	err = first.DeployRelay(ctx)
	deployed = first.State().Cloud.Deployed || err == nil
	if err != nil {
		// A deploy that failed after the upload leaves a script behind: find it and delete it.
		if found, ferr := cf.Find(ctx, accounts[0].ID); ferr == nil && found != nil {
			cf.Delete(ctx, accounts[0].ID, found.Script)
		}
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("deployed and answering after %s", time.Since(began).Round(time.Second))
	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", what, first.State().Problem)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	wait("the machine to connect to the relay", func() bool { return first.State().Connection == Online })
	address := first.State().Relay

	// The web client is served, under its headers.
	for _, p := range []string{"/", "/app.wasm", "/solo.js", "/fonts/Go-Regular.ttf"} {
		resp, err := http.Get(address + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || n == 0 {
			t.Errorf("GET %s: HTTP %d, %d bytes", p, resp.StatusCode, n)
		}
		if p == "/" {
			csp := resp.Header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self' wss://") {
				t.Errorf("the page's content security policy: %q", csp)
			}
			if resp.Header.Get("Strict-Transport-Security") == "" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("the page's headers: %v", resp.Header)
			}
			t.Logf("the page: %d bytes, cache %q, encoding %q", n, resp.Header.Get("Cache-Control"), resp.Header.Get("Content-Encoding"))
		}
		if p == "/app.wasm" {
			t.Logf("app.wasm: %d bytes, type %q", n, resp.Header.Get("Content-Type"))
		}
	}

	// A device pairs through the relay at the edge and opens a session.
	var key noise.DHKey
	dial := relaytest.Dialer(address)
	origin := device.Origin(strings.TrimPrefix(address, "https://"))
	pair := func() (*device.Pairing, error) {
		p, err := first.StartPairing(false)
		if err != nil {
			return nil, err
		}
		time.Sleep(time.Second) // the ticket reaches the relay
		_, frag, _ := strings.Cut(p.Link, "#p=")
		code, err := secure.ParsePairingCode(frag)
		if err != nil {
			return nil, err
		}
		key = newKey(t)
		return device.Pair(ctx, dial, origin, code, key, "Test device", rand.Reader)
	}
	req, err := pair()
	if err != nil {
		t.Fatalf("pairing through the real relay: %v", err)
	}
	wait("the request", func() bool { return len(first.State().Requests) == 1 })
	if _, err := first.Approve(req.Digits, false); err != nil {
		t.Fatal(err)
	}
	m, err := req.Wait()
	if err != nil {
		t.Fatal(err)
	}
	link, err := device.Connect(ctx, dial, origin, m, key, rand.Reader)
	if err != nil {
		t.Fatalf("a session through the real relay: %v", err)
	}
	client := rpc.NewClient(rpc.ClientOptions{Random: rand.Reader})
	done := make(chan error, 1)
	go func() { done <- client.Run(link) }()
	wait("the device to sync", func() bool { return client.Status().Synced })
	if n := len(client.Threads()); n == 0 {
		t.Error("the device sees no threads")
	}
	if err := client.SendToOrchestrator(ctx, "hello through Cloudflare"); err != nil {
		t.Errorf("a request through the real relay: %v", err)
	}
	t.Logf("paired and in session through the edge; %d threads", len(client.Threads()))

	// A second machine joins the same relay by adding its key, and the first one's connection and its
	// device's session carry on.
	second := machine("solo-test-two")
	if err := second.DeployRelay(ctx); err != nil {
		t.Fatalf("a second machine: %v", err)
	}
	wait("the second machine to connect", func() bool { return second.State().Connection == Online })
	if second.State().Relay != address {
		t.Error("the second machine deployed a relay of its own")
	}
	if err := client.SendToOrchestrator(ctx, "still here"); err != nil {
		t.Errorf("the first machine's session after the second joined: %v", err)
	}
	// Joining changed the list of machines and nothing else: the web client is still served, under
	// its headers.
	if resp, err := http.Get(address + "/"); err != nil {
		t.Errorf("the page after a second machine joined: %v", err)
	} else {
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || n == 0 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("the page after a second machine joined: HTTP %d, %d bytes, policy %q", resp.StatusCode, n, resp.Header.Get("Content-Security-Policy"))
		}
	}

	// The first machine sees a change it didn't make, pauses, and resumes when told it was the user's.
	if err := first.CheckRelay(ctx); err != nil {
		t.Fatal(err)
	}
	if st := first.State().Cloud; !st.Paused {
		t.Error("a change from another machine wasn't noticed")
	} else {
		t.Logf("what the check found: %d changes", len(st.Changes))
	}
	if err := first.AcceptRelay(ctx, false); err != nil {
		t.Fatal(err)
	}
	wait("the first machine to resume", func() bool { return first.State().Connection == Online })
	second.Close()
}
