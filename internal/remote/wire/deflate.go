//go:build !js

package wire

import (
	"bytes"
	"compress/flate"
)

// deflate compresses a message, or returns nil when it cannot.
func deflate(b []byte) []byte {
	var out bytes.Buffer
	w, err := flate.NewWriter(&out, flate.BestSpeed)
	if err != nil {
		return nil
	}
	if _, err := w.Write(b); err != nil || w.Close() != nil {
		return nil
	}
	return out.Bytes()
}
