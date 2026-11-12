// Package webheaders has the HTTP headers the web client is served with, wherever it is served from:
// the deployed relay, and the one that runs on this machine.
package webheaders

// For returns the headers for a web client whose relay is at host (with its port, if it has one),
// reached over TLS or not.
//
// The page may run its own scripts and its WebAssembly module, and may connect to its own origin, by
// HTTP and by WebSocket; nothing else. No inline script, no other site's script, no frames, no forms:
// what gets into the page must come from the relay's own files.
func For(host string, tls bool) map[string]string {
	ws := "ws://" + host
	if tls {
		ws = "wss://" + host
	}
	h := map[string]string{
		"Content-Security-Policy": "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; connect-src 'self' " + ws +
			"; style-src 'self'; font-src 'self'; img-src 'self' data: blob:; base-uri 'none'; form-action 'none'; " +
			"object-src 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), payment=(), usb=(), bluetooth=(), serial=(), hid=()",
		"X-Frame-Options":              "DENY",
	}
	if tls {
		h["Strict-Transport-Security"] = "max-age=31536000; includeSubDomains"
	}
	return h
}
