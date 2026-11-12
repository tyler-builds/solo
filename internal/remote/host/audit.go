//go:build !js

package host

import (
	"encoding/json"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/remote/rpc"
)

const goos = runtime.GOOS

// auditLog is the record of what devices changed on this machine: one JSON object a line, appended to
// a file only the user can read. It is for the person, after the fact: which device merged that?
type auditLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
	log  *slog.Logger
}

type auditEntry struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"` // a request's method, or "paired", "revoked", …
	Device string    `json:"device"`
	Name   string    `json:"name,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Error  string    `json:"error,omitempty"`
}

func newAuditLog(path string, log *slog.Logger) *auditLog {
	return &auditLog{path: path, log: log}
}

func (a *auditLog) write(e auditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			a.log.Warn("remote: opening the audit log", "err", err)
			return
		}
		a.f = f
	}
	e.Time = time.Now().UTC()
	line, _ := json.Marshal(e)
	if _, err := a.f.Write(append(line, '\n')); err != nil {
		a.log.Warn("remote: writing the audit log", "err", err)
	}
}

func (a *auditLog) request(d rpc.Device, method, detail string, err error) {
	e := auditEntry{Event: method, Device: d.ID, Name: d.Name, Detail: detail}
	if err != nil {
		e.Error = err.Error()
	}
	a.write(e)
}

func (a *auditLog) event(event, deviceID, name string) {
	a.write(auditEntry{Event: event, Device: deviceID, Name: name})
}

func (a *auditLog) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		a.f.Close()
		a.f = nil
	}
}
