//go:build !js

// Package cloudflare deploys Solo's relay and web client to the user's own Cloudflare account, through
// the REST API alone: no wrangler, no Node. It needs an API token that may edit Workers scripts and
// read the account's settings, and nothing more.
//
// Each step was first tried against a real account (docs/REMOTE.md, "Cloudflare through the REST API
// alone").
package cloudflare

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultAPI is Cloudflare's API.
const DefaultAPI = "https://api.cloudflare.com/client/v4"

// The names of the relay's bindings, and the marker's value: how Solo knows a Worker is its relay.
const (
	bindingKeys   = "HOST_KEYS"
	bindingMarker = "SOLO_RELAY"
	bindingBundle = "SOLO_BUNDLE"
	bindingObject = "RELAY"
	objectClass   = "Relay"
	markerValue   = "1"
	// ScriptPrefix starts the name of every relay Solo deploys; the rest is random, so the address
	// can't be guessed from the account's name.
	ScriptPrefix = "solo-relay-"
	// compatibilityDate is the Workers runtime behavior the relay was written and tested against.
	compatibilityDate = "2026-10-01"
)

// Client talks to Cloudflare with a token. The token is sent to Cloudflare's API and nowhere else, and
// is never put in an error or a log.
type Client struct {
	Token string
	// API is the API's address; "" is DefaultAPI. Tests point it at a fake.
	API  string
	HTTP *http.Client
}

// Error is the API's refusal.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("Cloudflare: %s (HTTP %d)", e.Message, e.Status) }

// Unauthorized reports whether err is Cloudflare refusing the token.
func Unauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

// call makes a request with the token, or with bearer when that is set (an upload session's own), and
// decodes the result into out.
func (c *Client) call(ctx context.Context, method, p, contentType string, body io.Reader, bearer string, out any) error {
	api := c.API
	if api == "" {
		api = DefaultAPI
	}
	req, err := http.NewRequestWithContext(ctx, method, api+p, body)
	if err != nil {
		return err
	}
	if bearer == "" {
		bearer = c.Token
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	base := c.HTTP
	if base == nil {
		base = http.DefaultClient
	}
	// The API doesn't redirect, and the token follows no redirect anywhere.
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		// The error may quote the request; its headers are not in it, but say less anyway.
		return errors.New("Cloudflare can't be reached: " + clean(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var env struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return &Error{resp.StatusCode, "an answer that isn't the API's"}
	}
	if !env.Success {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		msg := strings.Join(msgs, "; ")
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return &Error{resp.StatusCode, msg}
	}
	if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// clean is a transport error without the URL's query, which never holds the token but is noise.
func clean(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

// Account is a Cloudflare account the token reaches.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Accounts lists the accounts the token reaches.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var out []Account
	err := c.call(ctx, "GET", "/accounts?per_page=50", "", nil, "", &out)
	return out, err
}

// Subdomain returns the account's workers.dev subdomain: a Worker named w is served at
// https://w.<subdomain>.workers.dev. It is "" when the account has none yet.
func (c *Client) Subdomain(ctx context.Context, account string) (string, error) {
	var out struct {
		Subdomain string `json:"subdomain"`
	}
	err := c.call(ctx, "GET", "/accounts/"+account+"/workers/subdomain", "", nil, "", &out)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return "", nil
	}
	return out.Subdomain, err
}

// Asset is one file of the web client.
type Asset struct {
	Path string // from the site's root, such as "/index.html"
	Data []byte
}

func (a Asset) mime() string {
	switch path.Ext(a.Path) {
	case ".html":
		return "text/html"
	case ".js":
		return "text/javascript"
	case ".css":
		return "text/css"
	case ".wasm":
		return "application/wasm"
	case ".ttf":
		return "font/ttf"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	}
	return "application/octet-stream"
}

// hash is the 32 hex characters Cloudflare knows an asset by: any that differ when the contents do.
func (a Asset) hash() string {
	sum := sha256.Sum256(append([]byte(base64.StdEncoding.EncodeToString(a.Data)), strings.TrimPrefix(path.Ext(a.Path), ".")...))
	return hex.EncodeToString(sum[:])[:32]
}

// uploadAssets uploads the files Cloudflare doesn't have yet, and returns the token that attaches the
// whole set to a script upload.
func (c *Client) uploadAssets(ctx context.Context, account, script string, assets []Asset) (string, error) {
	manifest := map[string]map[string]any{}
	byHash := map[string]Asset{}
	for _, a := range assets {
		h := a.hash()
		manifest[a.Path] = map[string]any{"hash": h, "size": len(a.Data)}
		byHash[h] = a
	}
	body, _ := json.Marshal(map[string]any{"manifest": manifest})
	var session struct {
		JWT     string     `json:"jwt"`
		Buckets [][]string `json:"buckets"`
	}
	if err := c.call(ctx, "POST", "/accounts/"+account+"/workers/scripts/"+script+"/assets-upload-session",
		"application/json", bytes.NewReader(body), "", &session); err != nil {
		return "", err
	}
	completion := session.JWT // with nothing to upload, the session's token is already the completion's
	for _, bucket := range session.Buckets {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, h := range bucket {
			a, ok := byHash[h]
			if !ok {
				return "", errors.New("Cloudflare asked for a file that isn't in the web client")
			}
			hdr := textproto.MIMEHeader{}
			hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, h, h))
			hdr.Set("Content-Type", a.mime())
			pw, _ := mw.CreatePart(hdr)
			pw.Write([]byte(base64.StdEncoding.EncodeToString(a.Data)))
		}
		mw.Close()
		var done struct {
			JWT string `json:"jwt"`
		}
		if err := c.call(ctx, "POST", "/accounts/"+account+"/workers/assets/upload?base64=true",
			mw.FormDataContentType(), &buf, session.JWT, &done); err != nil {
			return "", err
		}
		if done.JWT != "" {
			completion = done.JWT
		}
	}
	return completion, nil
}

type binding struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Text      string `json:"text,omitempty"`
	ClassName string `json:"class_name,omitempty"`
}

func bindings(keys []string, bundle int) []binding {
	return []binding{
		{Type: "durable_object_namespace", Name: bindingObject, ClassName: objectClass},
		{Type: "plain_text", Name: bindingMarker, Text: markerValue},
		{Type: "plain_text", Name: bindingBundle, Text: strconv.Itoa(bundle)},
		{Type: "plain_text", Name: bindingKeys, Text: strings.Join(keys, ",")},
	}
}

// Upload is a relay to put in place.
type Upload struct {
	Account, Script string
	Worker          []byte  // the relay's one module
	Assets          []Asset // the web client
	Headers         string  // the web client's headers, in Cloudflare's _headers form
	Keys            []string
	Bundle          int
	// First says the script doesn't exist yet: the Durable Object's class is then declared, which must
	// happen once.
	First bool
}

// Put creates or replaces the relay: its code, its web client and its bindings.
func (c *Client) Put(ctx context.Context, u Upload) error {
	jwt, err := c.uploadAssets(ctx, u.Account, u.Script, u.Assets)
	if err != nil {
		return fmt.Errorf("uploading the web client: %w", err)
	}
	meta := map[string]any{
		"main_module":        "worker.js",
		"compatibility_date": compatibilityDate,
		"bindings":           bindings(u.Keys, u.Bundle),
		"assets": map[string]any{"jwt": jwt, "config": map[string]any{
			"html_handling": "auto-trailing-slash", "not_found_handling": "none", "_headers": u.Headers}},
		// What the relay says is nobody's business: no logs are kept.
		"observability": map[string]any{"enabled": false},
	}
	if u.First {
		meta["migrations"] = map[string]any{"new_tag": "v1", "new_sqlite_classes": []string{objectClass}}
	}
	mj, _ := json.Marshal(meta)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="metadata"; filename="metadata.json"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	pw.Write(mj)
	h = textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="worker.js"; filename="worker.js"`)
	h.Set("Content-Type", "application/javascript+module")
	pw, _ = mw.CreatePart(h)
	pw.Write(u.Worker)
	mw.Close()
	if err := c.call(ctx, "PUT", "/accounts/"+u.Account+"/workers/scripts/"+u.Script, mw.FormDataContentType(), &buf, "", nil); err != nil {
		return fmt.Errorf("uploading the relay: %w", err)
	}
	return nil
}

// SetKeys replaces the relay's list of machines, leaving its code, its web client and every other
// binding as they are: how a machine adds itself to a relay another machine deployed, whatever version
// that one is and whatever bindings a later version has added.
func (c *Client) SetKeys(ctx context.Context, account, script string, keys []string) error {
	var now struct {
		Bindings []map[string]any `json:"bindings"`
	}
	if err := c.call(ctx, "GET", "/accounts/"+account+"/workers/scripts/"+script+"/settings", "", nil, "", &now); err != nil {
		return err
	}
	found := false
	for _, b := range now.Bindings {
		if b["name"] == bindingKeys {
			b["type"], b["text"] = "plain_text", strings.Join(keys, ",")
			found = true
		}
	}
	if !found {
		return errors.New("the relay has no list of machines to add this one to")
	}
	settings, _ := json.Marshal(map[string]any{"bindings": now.Bindings})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="settings"; filename="settings.json"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	pw.Write(settings)
	mw.Close()
	return c.call(ctx, "PATCH", "/accounts/"+account+"/workers/scripts/"+script+"/settings", mw.FormDataContentType(), &buf, "", nil)
}

// Publish serves the relay at its workers.dev address, without the preview addresses that would serve
// other versions of it.
func (c *Client) Publish(ctx context.Context, account, script string) error {
	return c.call(ctx, "POST", "/accounts/"+account+"/workers/scripts/"+script+"/subdomain", "application/json",
		strings.NewReader(`{"enabled":true,"previews_enabled":false}`), "", nil)
}

// Relay is a relay found in an account.
type Relay struct {
	Script string
	Keys   []string // the machines it lets in
	Bundle int      // the version of what is deployed
}

// Settings reads a script's bindings, and reports whether the script is one of Solo's relays.
func (c *Client) Settings(ctx context.Context, account, script string) (Relay, bool, error) {
	var out struct {
		Bindings []binding `json:"bindings"`
	}
	if err := c.call(ctx, "GET", "/accounts/"+account+"/workers/scripts/"+script+"/settings", "", nil, "", &out); err != nil {
		return Relay{}, false, err
	}
	r := Relay{Script: script}
	marked := false
	for _, b := range out.Bindings {
		switch b.Name {
		case bindingMarker:
			marked = b.Type == "plain_text" && b.Text == markerValue
		case bindingBundle:
			r.Bundle, _ = strconv.Atoi(b.Text)
		case bindingKeys:
			for _, k := range strings.Split(b.Text, ",") {
				if k = strings.TrimSpace(k); k != "" {
					r.Keys = append(r.Keys, k)
				}
			}
		}
	}
	return r, marked, nil
}

// Find looks for Solo's relay in the account: a script with Solo's name and its marker. With several,
// it is the one that sorts first, so every machine picks the same one.
func (c *Client) Find(ctx context.Context, account string) (*Relay, error) {
	var scripts []struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, "GET", "/accounts/"+account+"/workers/scripts", "", nil, "", &scripts); err != nil {
		return nil, err
	}
	var names []string
	for _, s := range scripts {
		if strings.HasPrefix(s.ID, ScriptPrefix) {
			names = append(names, s.ID)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		r, ok, err := c.Settings(ctx, account, name)
		if err != nil {
			return nil, err
		}
		if ok {
			return &r, nil
		}
	}
	return nil, nil
}

// Version is one upload of a script.
type Version struct {
	ID      string
	Created time.Time
	Source  string // how it was made: "api" for an upload through the API, "dash" for the dashboard, …
}

// Versions lists a script's uploads, newest first.
func (c *Client) Versions(ctx context.Context, account, script string) ([]Version, error) {
	var out struct {
		Items []struct {
			ID       string `json:"id"`
			Metadata struct {
				CreatedOn string `json:"created_on"`
				Source    string `json:"source"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := c.call(ctx, "GET", "/accounts/"+account+"/workers/scripts/"+script+"/versions", "", nil, "", &out); err != nil {
		return nil, err
	}
	vs := make([]Version, 0, len(out.Items))
	for _, it := range out.Items {
		t, _ := time.Parse(time.RFC3339Nano, it.Metadata.CreatedOn)
		vs = append(vs, Version{ID: it.ID, Created: t, Source: it.Metadata.Source})
	}
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].Created.After(vs[j].Created) })
	return vs, nil
}

// ActiveVersion returns the ID of the version of a script that is being served now, or "" when the
// API doesn't say.
func (c *Client) ActiveVersion(ctx context.Context, account, script string) (string, error) {
	var out struct {
		Deployments []struct {
			Versions []struct {
				ID      string  `json:"version_id"`
				Percent float64 `json:"percentage"`
			} `json:"versions"`
		} `json:"deployments"`
	}
	if err := c.call(ctx, "GET", "/accounts/"+account+"/workers/scripts/"+script+"/deployments", "", nil, "", &out); err != nil {
		return "", err
	}
	if len(out.Deployments) == 0 || len(out.Deployments[0].Versions) != 1 {
		return "", nil // none, or traffic split between versions: nothing Solo does
	}
	return out.Deployments[0].Versions[0].ID, nil
}

// ValidHost reports whether s is a host name of plain letters, digits, hyphens and dots, as a
// workers.dev address is: it goes into the headers the web client is served with.
func ValidHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// Delete removes a script, its Durable Object and its web client. Its address stops answering.
func (c *Client) Delete(ctx context.Context, account, script string) error {
	err := c.call(ctx, "DELETE", "/accounts/"+account+"/workers/scripts/"+script+"?force=true", "", nil, "", nil)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// HeadersFile writes headers for every path in the form of Cloudflare's _headers file.
func HeadersFile(headers map[string]string) string {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("/*\n")
	for _, k := range names {
		fmt.Fprintf(&b, "  %s: %s\n", k, headers[k])
	}
	return b.String()
}
