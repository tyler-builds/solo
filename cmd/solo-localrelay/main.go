// Command solo-localrelay runs Solo's relay and web client on this machine, without Cloudflare, for
// development and for trying remote access before deploying it:
//
//	scripts/web.sh                 # builds the web client into dist/web
//	go run ./cmd/solo-localrelay   # serves it, and the relay, at http://127.0.0.1:8787
//
// Then in Solo, Settings › Remote, set the relay address to http://127.0.0.1:8787 and add a device:
// the pairing link opens in a browser on this machine.
//
// It speaks the protocol of relay/ and checks every token as the Worker does, but it lets any machine
// in, since only programs on this machine can reach it; it refuses to listen anywhere else.
//
// With -demo it also runs a machine of its own over sample data, prints a pairing link, and approves
// the six digits typed into its terminal: the whole path, with no Claude and no desktop app.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyler-builds/solo/internal/remote/host"
	"github.com/tyler-builds/solo/internal/remote/relaytest"
	"github.com/tyler-builds/solo/internal/remote/webheaders"
	"github.com/tyler-builds/solo/internal/sample"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "the address to listen on; this machine only")
	web := flag.String("web", "dist/web", "the built web client (scripts/web.sh)")
	demo := flag.Bool("demo", false, "also run a machine over sample data, and pair with digits typed here")
	stream := flag.Bool("stream", false, "with -demo: one of the sample threads streams text")
	flag.Parse()
	if err := run(*addr, *web, *demo, *stream); err != nil {
		fmt.Fprintln(os.Stderr, "solo-localrelay:", err)
		os.Exit(1)
	}
}

func run(addr, web string, demo, stream bool) error {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(h); h != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s isn't this machine alone: this relay lets any machine in, so it listens on 127.0.0.1 only", h)
	}
	if _, err := os.Stat(filepath.Join(web, "index.html")); err != nil {
		return fmt.Errorf("no web client in %s: build it with scripts/web.sh", web)
	}
	relay, err := relaytest.NewAt(addr)
	if err != nil {
		return err
	}
	defer relay.Close()
	relay.AllowAny = true
	files := http.FileServer(http.Dir(web))
	headers := webheaders.For(strings.TrimPrefix(relay.URL(), "http://"), false)
	relay.Static = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The headers the deployed web client is served with: no script but the page's own.
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	fmt.Println("relay and web client at", relay.URL())
	if !demo {
		fmt.Println("In Solo, Settings › Remote, set the relay address to", relay.URL())
		select {}
	}

	dir, err := os.MkdirTemp("", "solo-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	remote, err := host.New(host.Config{Dir: dir, Backend: sample.New(sample.Options{Threads: 24, Items: 120, Stream: stream}),
		Secrets: host.MemSecrets{}, Name: "demo machine"})
	if err != nil {
		return err
	}
	defer remote.Close()
	if err := remote.SetRelay(relay.URL()); err != nil {
		return err
	}
	for remote.State().Connection != host.Online {
		time.Sleep(20 * time.Millisecond)
	}
	offer := func() {
		p, err := remote.StartPairing()
		if err != nil {
			fmt.Println("pairing:", err)
			return
		}
		fmt.Println("PAIR", p.Link)
		fmt.Println("Open that link in a browser, then type the six digits it shows here.")
	}
	offer()
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		switch {
		case line == "":
		case line == "new":
			offer()
		case line == "off":
			remote.SetEnabled(false)
			fmt.Println("OFF")
		case line == "on":
			remote.SetEnabled(true)
			fmt.Println("ON")
		case line == "revoke":
			remote.RevokeAll()
			fmt.Println("REVOKED")
		default:
			d, err := remote.Approve(line, false)
			if err != nil {
				fmt.Println("NOT PAIRED:", err)
				continue
			}
			fmt.Println("PAIRED", d.Name)
		}
	}
	return nil
}
