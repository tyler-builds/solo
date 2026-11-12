//go:build !js

package host

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"

	"github.com/tyler-builds/solo/internal/remote/relaytest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/remote/secure"
)

// TestAgainstTheWorker runs the tests that need only a relay against the real one: relay/'s Worker and
// Durable Object, in the Workers runtime on this machine. It ties the two halves together, which are
// otherwise checked against each other only by the token vectors.
//
//	SOLO_TEST_WORKER=1 go test ./internal/remote/host -run TestAgainstTheWorker
//
// It needs bun, and relay/'s tools installed (cd relay && bun install).
func TestAgainstTheWorker(t *testing.T) {
	if os.Getenv("SOLO_TEST_WORKER") == "" {
		t.Skip("set SOLO_TEST_WORKER=1 to run against relay/ in the Workers runtime")
	}
	seed, err := secure.NewSeed(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m, err := secure.MachineFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	dir, _ := filepath.Abs("../../../relay")
	// Over HTTPS, with a certificate wrangler makes: over plain HTTP it rewrites a page's Origin, and
	// the relay then takes the page for another site's.
	cmd := exec.Command("bun", "x", "wrangler", "dev", "--port", fmt.Sprint(port), "--ip", "127.0.0.1",
		"--local-protocol", "https",
		"--var", "HOST_KEYS:"+base64.RawURLEncoding.EncodeToString(m.RelayKey()),
		"--persist-to", t.TempDir(), "--show-interactive-dev-session=false")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CI=1", "WRANGLER_SEND_METRICS=false")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		cmd.Wait()
	}()
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			t.Log("wrangler:", sc.Text())
		}
	}()
	address := fmt.Sprintf("https://127.0.0.1:%d", port)
	deadline := time.Now().Add(90 * time.Second)
	for {
		resp, err := relaytest.LocalHTTP.Get(address + "/v1/info")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Worker didn't start: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	worker, workerSeed = address, base64.RawURLEncoding.EncodeToString(seed[:])
	defer func() { worker, workerSeed = "", "" }()

	t.Run("pair and use", TestPairAndUse)
	t.Run("a stolen pairing link", TestStolenPairingLink)
	t.Run("wrong digits", TestWrongDigitsEndThePairing)
	t.Run("pairing needs the code", TestPairingNeedsTheCode)
	t.Run("revoke and read-only", TestRevokeAndReadOnly)
}
