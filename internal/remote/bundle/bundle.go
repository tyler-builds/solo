//go:build !js

// Package bundle is what Solo deploys to Cloudflare, built into the app: the relay's module and the
// web client. scripts/bundle.sh puts them in files/ before the app is built.
package bundle

import (
	"embed"
	"errors"
	"io/fs"
	"strconv"
	"strings"
)

//go:embed all:files
var files embed.FS

// Bundle is the relay and the web client.
type Bundle struct {
	// Version orders bundles: a machine doesn't replace a deployed bundle with an older one.
	Version int
	Worker  []byte
	// Web is the web client's files, by their paths from the site's root ("/index.html").
	Web map[string][]byte
}

// ErrMissing is a build of Solo without the bundle in it.
var ErrMissing = errors.New("this build of Solo doesn't include the relay and the web client (scripts/bundle.sh adds them)")

// Load returns the bundle built into the app.
func Load() (*Bundle, error) {
	b := &Bundle{Web: map[string][]byte{}}
	var err error
	if b.Worker, err = files.ReadFile("files/worker.js"); err != nil {
		return nil, ErrMissing
	}
	v, err := files.ReadFile("files/version")
	if err != nil {
		return nil, ErrMissing
	}
	if b.Version, err = strconv.Atoi(strings.TrimSpace(string(v))); err != nil || b.Version <= 0 {
		return nil, ErrMissing
	}
	err = fs.WalkDir(files, "files/web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		b.Web[strings.TrimPrefix(p, "files/web")] = data
		return nil
	})
	if err != nil || b.Web["/index.html"] == nil || b.Web["/app.wasm"] == nil {
		return nil, ErrMissing
	}
	return b, nil
}
