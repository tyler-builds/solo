//go:build !js

package relaytest

import (
	"crypto/tls"
	"net/http"
	"net/url"

	"github.com/tyler-builds/solo/internal/remote/device"
)

// RefusedError is a connection the relay did not let in.
type RefusedError = device.RefusedError

// LocalHTTP is an HTTP client that trusts whatever certificate a server on this machine shows, for a
// relay run locally over HTTPS with a certificate made for the run.
var LocalHTTP = &http.Client{Transport: &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback only; see Dialer
}}

// Dialer connects to a relay at an address such as "http://127.0.0.1:8787" as a browser would, for
// tests and tools that play a device. A relay on this machine over HTTPS is trusted whatever its
// certificate.
func Dialer(address string) device.Dialer {
	client := http.DefaultClient
	if u, err := url.Parse(address); err == nil && u.Scheme == "https" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
		client = LocalHTTP
	}
	return device.WebSocket(address, client)
}
