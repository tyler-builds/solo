//go:build !js

package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/api"
	"github.com/tyler-builds/solo/internal/cloudflare"
	"github.com/tyler-builds/solo/internal/remote/bundle"
	"github.com/tyler-builds/solo/internal/remote/webheaders"
)

// The Cloudflare token's name among the secrets, and the environment variable used when none is kept.
const (
	tokenName = "cloudflare-token"
	tokenEnv  = "CLOUDFLARE_API_TOKEN"
)

// savedCloud is what remote.json keeps about the Cloudflare account: no secret, only where the relay
// is and which uploads of it this machine knows.
type savedCloud struct {
	Account     string `json:"account,omitempty"`
	AccountName string `json:"accountName,omitempty"`
	Script      string `json:"script,omitempty"`
	// Versions are the uploads of the relay this machine made or its user accepted. One that isn't
	// here was made elsewhere.
	Versions []string `json:"versions,omitempty"`
	// Paused: an upload this machine doesn't know appeared, and remote access waits for the user.
	Paused bool `json:"paused,omitempty"`
}

// token returns the Cloudflare token, from the secrets or else the environment.
func (r *Remote) token() (string, error) {
	r.mu.Lock()
	if r.secrets == nil {
		r.secrets = NewSecrets(r.cfg.Dir)
	}
	s := r.secrets
	r.mu.Unlock()
	t, err := s.Get(tokenName)
	if err != nil {
		return "", err
	}
	if t == "" {
		t = strings.TrimSpace(os.Getenv(tokenEnv))
	}
	return t, nil
}

func (r *Remote) cloudflare(token string) *cloudflare.Client {
	return &cloudflare.Client{Token: token, API: r.cfg.CloudflareAPI, HTTP: r.cfg.HTTP}
}

// busy shows what a long step is doing, in Settings.
func (r *Remote) busy(what string) {
	r.mu.Lock()
	r.cloudBusy = what
	r.mu.Unlock()
	r.changed()
}

// cloudState is the Cloudflare part of State. Call with r.mu held.
func (r *Remote) cloudStateLocked() api.RemoteCloud {
	c := api.RemoteCloud{HasToken: r.hasToken, Account: r.saved.Cloud.AccountName, Deployed: r.saved.Cloud.Script != "",
		Busy: r.cloudBusy, Paused: r.saved.Cloud.Paused}
	if c.Paused {
		c.Alert = "The relay in your Cloudflare account was changed by something other than this copy of Solo: another of your machines, the Cloudflare dashboard, or someone with access to the account. Remote access is paused. If the change was yours, resume it; if not, remove the relay and set it up again."
	}
	_, err := r.loadBundle()
	c.CanDeploy = err == nil
	return c
}

func (r *Remote) loadBundle() (*bundle.Bundle, error) {
	if r.cfg.Bundle != nil {
		return r.cfg.Bundle()
	}
	return bundle.Load()
}

// SetCloudToken keeps a Cloudflare API token, after checking that it reaches one account. The token
// goes to the OS keyring and is not shown again.
func (r *Remote) SetCloudToken(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("Paste the API token from Cloudflare.")
	}
	accounts, err := r.cloudflare(token).Accounts(ctx)
	if err != nil {
		if cloudflare.Unauthorized(err) {
			return errors.New("Cloudflare didn't accept that token. It needs permission to read account settings and to edit Workers scripts.")
		}
		return err
	}
	if len(accounts) == 0 {
		return errors.New("That token reaches no account. It needs permission to read account settings.")
	}
	if len(accounts) > 1 {
		return fmt.Errorf("That token reaches %d accounts. Make one for the single account the relay should live in.", len(accounts))
	}
	r.mu.Lock()
	if r.secrets == nil {
		r.secrets = NewSecrets(r.cfg.Dir)
	}
	s := r.secrets
	r.mu.Unlock()
	if err := s.Set(tokenName, token); err != nil {
		return err
	}
	r.mu.Lock()
	if r.saved.Cloud.Account != accounts[0].ID {
		r.saved.Cloud = savedCloud{}
	}
	r.saved.Cloud.Account, r.saved.Cloud.AccountName = accounts[0].ID, accounts[0].Name
	r.hasToken = true
	err = r.saved.write(r.path)
	r.mu.Unlock()
	r.audit.event("cloudflare-token-set", "", "")
	r.changed()
	return err
}

// ForgetCloudToken deletes the Cloudflare token from this machine. The relay stays as it is: a relay
// that is deployed needs no token to be used, only to be changed.
func (r *Remote) ForgetCloudToken() error {
	r.mu.Lock()
	if r.secrets == nil {
		r.secrets = NewSecrets(r.cfg.Dir)
	}
	s := r.secrets
	r.mu.Unlock()
	err := s.Delete(tokenName)
	r.mu.Lock()
	r.hasToken = strings.TrimSpace(os.Getenv(tokenEnv)) != ""
	r.mu.Unlock()
	r.audit.event("cloudflare-token-removed", "", "")
	r.changed()
	return err
}

// DeployRelay puts the relay and the web client in the Cloudflare account, or joins the relay that is
// already there, and connects this machine to it.
//
// A relay another machine deployed is found by its marker; this machine adds its key to the relay's
// list, and replaces the code and the web client only when its own are newer.
func (r *Remote) DeployRelay(ctx context.Context) error {
	defer r.busy("")
	r.busy("Checking the Cloudflare account…")
	token, err := r.token()
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("Add a Cloudflare API token first.")
	}
	cf := r.cloudflare(token)
	r.mu.Lock()
	account := r.saved.Cloud.Account
	m, err := r.identityLocked()
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if account == "" {
		accounts, err := cf.Accounts(ctx)
		if err != nil {
			return err
		}
		if len(accounts) != 1 {
			return errors.New("The Cloudflare token must reach exactly one account.")
		}
		account = accounts[0].ID
		r.mu.Lock()
		r.saved.Cloud.Account, r.saved.Cloud.AccountName = account, accounts[0].Name
		r.mu.Unlock()
	}
	sub, err := cf.Subdomain(ctx, account)
	if err != nil {
		return err
	}
	if sub == "" {
		return errors.New("This Cloudflare account has no workers.dev address yet. Open Workers & Pages once in the Cloudflare dashboard, which makes it, then try again.")
	}
	key, err := r.RelayKey()
	if err != nil {
		return err
	}
	_ = m
	found, err := cf.Find(ctx, account)
	if err != nil {
		return err
	}
	b, bundleErr := r.loadBundle()

	script := ""
	switch {
	case found == nil:
		if bundleErr != nil {
			return bundleErr
		}
		var rnd [5]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return err
		}
		script = cloudflare.ScriptPrefix + hex.EncodeToString(rnd[:])
		r.busy("Uploading the relay and the web app…")
		if err := cf.Put(ctx, r.upload(account, script, sub, b, []string{key}, true)); err != nil {
			return err
		}
		if err := cf.Publish(ctx, account, script); err != nil {
			return err
		}
	default:
		script = found.Script
		keys := found.Keys
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if bundleErr == nil && b.Version > found.Bundle {
			r.busy("Updating the relay and the web app…")
			if err := cf.Put(ctx, r.upload(account, script, sub, b, keys, false)); err != nil {
				return err
			}
		} else if !slices.Equal(keys, sortedCopy(found.Keys)) {
			r.busy("Adding this machine to the relay…")
			if err := cf.SetKeys(ctx, account, script, keys, found.Bundle); err != nil {
				return err
			}
		}
		// Two machines that add themselves at once can each write a list without the other: read it
		// back, and add this one again if it was lost.
		for try := 0; try < 3; try++ {
			now, ok, err := cf.Settings(ctx, account, script)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("The relay changed while it was being set up. Try again.")
			}
			if slices.Contains(now.Keys, key) {
				break
			}
			merged := append(slices.Clone(now.Keys), key)
			sort.Strings(merged)
			if err := cf.SetKeys(ctx, account, script, merged, now.Bundle); err != nil {
				return err
			}
		}
	}

	address := "https://" + script + "." + sub + ".workers.dev"
	r.busy("Waiting for the relay to answer…")
	if err := r.waitForRelay(ctx, address); err != nil {
		return err
	}
	// What is deployed now is what this machine accepts as its relay.
	versions, err := cf.Versions(ctx, account, script)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.saved.Cloud.Script = script
	r.saved.Cloud.Versions = versionIDs(versions)
	r.saved.Cloud.Paused = false
	err = r.saved.write(r.path)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	r.audit.event("relay-deployed", "", script)
	return r.SetRelay(address)
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	sort.Strings(out)
	return out
}

func versionIDs(vs []cloudflare.Version) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.ID)
	}
	return out
}

func (r *Remote) upload(account, script, sub string, b *bundle.Bundle, keys []string, first bool) cloudflare.Upload {
	host := script + "." + sub + ".workers.dev"
	headers := webheaders.For(host, true)
	// The files keep their names from one version to the next, so a browser asks each time whether
	// its copy is current; an unchanged file costs a short answer.
	headers["Cache-Control"] = "no-cache"
	u := cloudflare.Upload{Account: account, Script: script, Worker: b.Worker, Headers: cloudflare.HeadersFile(headers),
		Keys: keys, Bundle: b.Version, First: first}
	for p, data := range b.Web {
		u.Assets = append(u.Assets, cloudflare.Asset{Path: p, Data: data})
	}
	sort.Slice(u.Assets, func(i, j int) bool { return u.Assets[i].Path < u.Assets[j].Path })
	return u
}

// waitForRelay waits until the relay at address answers as Solo's relay: a new workers.dev address
// takes up to a minute to start answering.
func (r *Remote) waitForRelay(ctx context.Context, address string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, ok := relayClock(ctx, r.cfg.HTTP, address); ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("The relay was uploaded but doesn't answer yet at " + address + ". Try again in a minute.")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.cfg.Retry):
		}
	}
}

// RemoveRelay deletes the relay from the Cloudflare account. Every machine and device that used it is
// cut off, and its address stops answering: this is also how access is ended from anywhere, by
// deleting the Worker in the Cloudflare dashboard.
func (r *Remote) RemoveRelay(ctx context.Context) error {
	defer r.busy("")
	r.busy("Removing the relay…")
	token, err := r.token()
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("Removing the relay needs the Cloudflare API token. Without it, delete the Worker in the Cloudflare dashboard.")
	}
	r.mu.Lock()
	account, script := r.saved.Cloud.Account, r.saved.Cloud.Script
	r.mu.Unlock()
	if account == "" || script == "" {
		return errors.New("This machine didn't set up a relay.")
	}
	if err := r.cloudflare(token).Delete(ctx, account, script); err != nil {
		return err
	}
	r.mu.Lock()
	r.saved.Cloud.Script, r.saved.Cloud.Versions, r.saved.Cloud.Paused = "", nil, false
	r.mu.Unlock()
	r.audit.event("relay-removed", "", script)
	return r.SetRelay("")
}

// CheckRelay looks at what is deployed and pauses remote access if the relay was uploaded by anything
// this machine doesn't know of. Whoever controls the Cloudflare account can replace the web client,
// and a replaced web client could use a paired device's keys: this is the check for that. It needs the
// token, and does nothing without it.
func (r *Remote) CheckRelay(ctx context.Context) error {
	token, err := r.token()
	if err != nil || token == "" {
		return err
	}
	r.mu.Lock()
	account, script, known := r.saved.Cloud.Account, r.saved.Cloud.Script, slices.Clone(r.saved.Cloud.Versions)
	r.mu.Unlock()
	if account == "" || script == "" {
		return nil
	}
	versions, err := r.cloudflare(token).Versions(ctx, account, script)
	if err != nil {
		var e *cloudflare.Error
		if errors.As(err, &e) && e.Status == http.StatusNotFound {
			// The relay is gone: removed from another machine, or in the dashboard.
			r.mu.Lock()
			r.saved.Cloud.Script, r.saved.Cloud.Versions = "", nil
			r.mu.Unlock()
			return r.SetRelay("")
		}
		return err
	}
	strange := false
	for _, v := range versions {
		if !slices.Contains(known, v.ID) {
			strange = true
		}
	}
	if !strange {
		return nil
	}
	r.mu.Lock()
	already := r.saved.Cloud.Paused
	r.saved.Cloud.Paused = true
	r.stopLocked()
	err = r.saved.write(r.path)
	r.mu.Unlock()
	if !already {
		r.audit.event("relay-changed-elsewhere", "", script)
		if r.cfg.Notify != nil {
			r.cfg.Notify("Remote access is paused", "The relay in your Cloudflare account was changed from somewhere else. Open Settings, Remote.")
		}
	}
	r.changed()
	return err
}

// AcceptRelay records what is deployed as the user's own, and resumes remote access after CheckRelay
// paused it.
func (r *Remote) AcceptRelay(ctx context.Context) error {
	token, err := r.token()
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("Add the Cloudflare API token to check the relay.")
	}
	r.mu.Lock()
	account, script := r.saved.Cloud.Account, r.saved.Cloud.Script
	r.mu.Unlock()
	versions, err := r.cloudflare(token).Versions(ctx, account, script)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.saved.Cloud.Versions = versionIDs(versions)
	r.saved.Cloud.Paused = false
	err = r.saved.write(r.path)
	if err == nil && r.saved.Enabled && r.saved.Relay != "" {
		r.start()
	}
	r.mu.Unlock()
	r.audit.event("relay-accepted", "", script)
	r.changed()
	return err
}

// watchRelay checks the relay when Solo starts and a few times a day.
func (r *Remote) watchRelay() {
	check := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := r.CheckRelay(ctx); err != nil {
			r.log.Warn("remote: checking the relay", "err", err)
		}
	}
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	check()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			check()
		}
	}
}
