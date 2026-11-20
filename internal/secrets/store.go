//go:build !js

package secrets

import (
	"errors"
	"fmt"
	"sync"

	"github.com/zalando/go-keyring"
)

// Store keeps secret values by name.
type Store interface {
	// Get returns a value, or ErrNotFound.
	Get(name string) (string, error)
	Set(name, value string) error
	// Delete removes a value; one that isn't there is no error.
	Delete(name string) error
	// Available says why values can't be kept here, or nil when they can.
	Available() error
}

// ErrNotFound is Store.Get's error for a name with no value.
var ErrNotFound = errors.New("no such secret")

// Service is the keyring service Solo keeps agents' secrets under, apart from its own ("solo").
const Service = "solo-secrets"

// Keyring is the OS keyring: the Secret Service (GNOME Keyring, KWallet) on Linux, Credential Manager on
// Windows, the Keychain on macOS. Unlike Solo's own keys (remote/host), agents' secrets never fall back
// to files: a file in Solo's data folder is one an agent can read.
type Keyring struct {
	once sync.Once
	err  error
}

// NewKeyring returns the OS keyring. Whether it works is found on first use (Available).
func NewKeyring() *Keyring { return &Keyring{} }

func (k *Keyring) Available() error {
	k.once.Do(func() {
		// A keyring that can't be written to is of no use, and one write tells.
		const probe = "__solo_probe__"
		if err := keyring.Set(Service, probe, "1"); err != nil {
			k.err = fmt.Errorf("the system keyring can't be used (%v); Solo keeps secrets only there", err)
			return
		}
		_ = keyring.Delete(Service, probe)
	})
	return k.err
}

func (k *Keyring) Get(name string) (string, error) {
	if err := k.Available(); err != nil {
		return "", err
	}
	v, err := keyring.Get(Service, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return v, err
}

func (k *Keyring) Set(name, value string) error {
	if err := k.Available(); err != nil {
		return err
	}
	if err := keyring.Set(Service, name, value); err != nil {
		// go-keyring's errors don't quote the value; say only which secret.
		return fmt.Errorf("saving %s in the system keyring: %w", name, err)
	}
	return nil
}

func (k *Keyring) Delete(name string) error {
	if err := k.Available(); err != nil {
		return err
	}
	if err := keyring.Delete(Service, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// Mem keeps values in memory, for tests.
type Mem struct {
	mu   sync.Mutex
	m    map[string]string
	Fail error // when set, Available and every call return it
}

func (m *Mem) Available() error { return m.Fail }

func (m *Mem) Get(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return "", m.Fail
	}
	v, ok := m.m[name]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *Mem) Set(name, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if m.m == nil {
		m.m = map[string]string{}
	}
	m.m[name] = value
	return nil
}

func (m *Mem) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	delete(m.m, name)
	return nil
}

// Unavailable is a Store that keeps nothing, for a Core with no keyring configured.
type Unavailable struct{}

var errUnavailable = errors.New("Solo has no secret store here")

func (Unavailable) Available() error           { return errUnavailable }
func (Unavailable) Get(string) (string, error) { return "", errUnavailable }
func (Unavailable) Set(string, string) error   { return errUnavailable }
func (Unavailable) Delete(string) error        { return errUnavailable }
