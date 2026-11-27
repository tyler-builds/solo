//go:build !js

package host

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/tyler-builds/solo/internal/remote/secure"
)

// Secrets keeps the machine's seed, and later the Cloudflare token: the OS keyring, or files only the
// user can read where there is none.
type Secrets interface {
	Get(name string) (string, error) // "" and no error when there is none
	Set(name, value string) error
	Delete(name string) error
	// InFiles reports whether secrets are kept in files, so that the UI can say so.
	InFiles() bool
}

const keyringService = "solo"

// osSecrets is the OS keyring, with files in dir as the fallback when the keyring can't be used (a
// Linux session with no secret service, say).
type osSecrets struct {
	dir   string
	files bool
}

// NewSecrets returns the OS keyring, or files in dir when the keyring doesn't work.
func NewSecrets(dir string) Secrets {
	s := &osSecrets{dir: dir}
	// A keyring that can't be written to is of no use, and one write tells.
	if err := keyring.Set(keyringService, "probe", "1"); err != nil {
		s.files = true
	} else {
		keyring.Delete(keyringService, "probe")
	}
	return s
}

func (s *osSecrets) InFiles() bool { return s.files }

func (s *osSecrets) path(name string) string { return filepath.Join(s.dir, "secret-"+name) }

func (s *osSecrets) Get(name string) (string, error) {
	if !s.files {
		v, err := keyring.Get(keyringService, name)
		if errors.Is(err, keyring.ErrNotFound) {
			return "", nil
		}
		return v, err
	}
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

func (s *osSecrets) Set(name, value string) error {
	if !s.files {
		return keyring.Set(keyringService, name, value)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return writePrivate(s.path(name), []byte(value))
}

func (s *osSecrets) Delete(name string) error {
	if !s.files {
		if err := keyring.Delete(keyringService, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
		return nil
	}
	if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// MemSecrets keeps secrets in memory, for tests.
type MemSecrets map[string]string

func (m MemSecrets) Get(name string) (string, error) { return m[name], nil }
func (m MemSecrets) Set(name, value string) error    { m[name] = value; return nil }
func (m MemSecrets) Delete(name string) error        { delete(m, name); return nil }
func (m MemSecrets) InFiles() bool                   { return false }

// writePrivate writes a file only the user can read, whole or not at all.
func writePrivate(path string, data []byte) error {
	tmp := path + ".tmp"
	os.Remove(tmp) // one left by a crash may have another mode
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync() // a removed device must stay removed over a power cut
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

const seedName = "remote-machine-seed"

// loadMachine returns the machine's identity, making its seed the first time.
func loadMachine(s Secrets) (secure.Machine, error) {
	v, err := s.Get(seedName)
	if err != nil {
		return secure.Machine{}, err
	}
	var seed [secure.KeySize]byte
	if raw, err := base64.RawURLEncoding.DecodeString(v); err == nil && len(raw) == len(seed) {
		copy(seed[:], raw)
	} else {
		if v != "" {
			return secure.Machine{}, errors.New("the machine's remote access key is damaged; turn remote access off and set it up again")
		}
		if seed, err = secure.NewSeed(rand.Reader); err != nil {
			return secure.Machine{}, err
		}
		if err := s.Set(seedName, base64.RawURLEncoding.EncodeToString(seed[:])); err != nil {
			return secure.Machine{}, err
		}
	}
	return secure.MachineFromSeed(seed)
}

// saved is what the machine keeps about remote access, in remote.json: the relay, and the devices
// paired with it. Each device's key for sessions is here too, so the file is the user's alone.
type saved struct {
	Relay   string        `json:"relay,omitempty"` // the relay's address, such as "https://solo-x.name.workers.dev"
	Enabled bool          `json:"enabled,omitempty"`
	Devices []savedDevice `json:"devices,omitempty"`
	Cloud   savedCloud    `json:"cloud,omitzero"`
	// Links are the Solos on other machines this one reads usage from (link.go).
	Links []savedLink `json:"links,omitempty"`
}

type savedDevice struct {
	ID       string `json:"id"`
	Key      []byte `json:"key"` // its public key
	PSK      []byte `json:"psk"` // its key for sessions
	Name     string `json:"name"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	// Linked: another machine's Solo, which may only read usage (rpc.Device.Linked).
	Linked   bool      `json:"linked,omitempty"`
	Added    time.Time `json:"added"`
	LastSeen time.Time `json:"lastSeen,omitzero"`
}

// unusedFor is how long a device may go unused before it is forgotten.
const unusedFor = 90 * 24 * time.Hour

func loadSaved(path string) (saved, error) {
	var s saved
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, errors.New("remote.json can't be read: " + err.Error())
	}
	return s, nil
}

func (s saved) write(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, b)
}
