// Package device is a device's side of pairing with a machine and of connecting to it afterwards: the
// web client runs it over the browser's WebSocket, and tests run it against a real machine.
package device

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/flynn/noise"

	"github.com/tyler-builds/solo/internal/remote/secure"
	"github.com/tyler-builds/solo/internal/remote/wire"
)

// Relay paths, and the first WebSocket subprotocol: the second is the token.
const (
	PathPair    = "/v1/pair"
	PathDevice  = "/v1/device"
	PathMachine = "/v1/machine"
	Subprotocol = "solo.v1"
)

// CloseError is a connection that was closed, with the WebSocket's close code: one of the relay's
// below, or CloseNormal when the machine hung up.
type CloseError struct{ Code int }

func (e *CloseError) Error() string {
	return "the connection was closed (" + strconv.Itoa(e.Code) + ")"
}

// The relay's close codes.
const (
	CloseNormal       = 1000 // the machine closed the connection
	CloseReplaced     = 4000 // another connection took this one's place
	CloseOffline      = 4001 // the machine isn't connected
	CloseUnauthorized = 4002
	CloseLimit        = 4003
	CloseProtocol     = 4004
	CloseReload       = 4005 // the web client is out of date
)

// Origin is the relay's origin as both ends hash it into a handshake: the address the web client is
// served from. host is the address's host, with its port if it has one.
func Origin(host string) string { return "https://" + host }

// Dialer opens a connection through the relay: path is one of the relay's, and token goes in the
// WebSocket's second subprotocol.
type Dialer func(ctx context.Context, path string, token []byte) (wire.Conn, error)

// Machine is what a device keeps of a machine it is paired with.
type Machine struct {
	ID       string `json:"id"`      // the machine's ID, in hex
	Key      []byte `json:"key"`     // the machine's public key, pinned at pairing
	PSK      []byte `json:"psk"`     // this device's own key for sessions with it
	Voucher  []byte `json:"voucher"` // what the relay lets this device in for
	Name     string `json:"name"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// The two messages of a pairing, on its secure channel.
type (
	// Request is the device's: who is asking.
	Request struct {
		Name string `json:"name"`
	}
	// Decision is the machine's.
	Decision struct {
		OK       bool   `json:"ok"`
		PSK      []byte `json:"psk,omitempty"`
		Voucher  []byte `json:"voucher,omitempty"`
		Name     string `json:"name,omitempty"`
		ReadOnly bool   `json:"readOnly,omitempty"`
	}
)

// Errors of pairing and connecting.
var (
	ErrDenied  = errors.New("The request was turned down at the machine.")
	ErrExpired = errors.New("This pairing code has expired. Make a new one at the machine.")
)

// Pairing is a request to pair, waiting for the person at the machine.
type Pairing struct {
	// Digits are the six digits to type at the machine.
	Digits string
	code   secure.PairingCode
	link   *wire.Link
}

// within closes conn if ctx ends before the function it returns is called: a relay that accepts a
// connection and then says nothing doesn't hold a handshake up for ever.
func within(ctx context.Context, conn wire.Conn) (done func()) {
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-finished:
		}
	}()
	return func() { close(finished) }
}

// Pair asks the machine a pairing code names to pair with this device. It returns once the machine has
// answered the handshake: the digits are then shown, and Wait waits for the person there.
func Pair(ctx context.Context, dial Dialer, origin string, code secure.PairingCode, key noise.DHKey, name string, random io.Reader) (*Pairing, error) {
	conn, err := dial(ctx, PathPair, code.RelayToken())
	if err != nil {
		return nil, err
	}
	defer within(ctx, conn)()
	psk := code.PSK()
	p := secure.Params{Purpose: secure.Pair, Origin: origin, Machine: code.Machine, Code: &code}
	ini, first, err := secure.Initiate(p, key, code.MachineKey[:], psk[:], random)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Write(first); err != nil {
		conn.Close()
		return nil, err
	}
	answer, err := conn.Read()
	if err != nil {
		conn.Close()
		return nil, err
	}
	ch, err := ini.Finish(answer)
	if err != nil {
		conn.Close()
		return nil, errors.New("The machine didn't accept this pairing code.")
	}
	link := wire.NewLink(conn, ch)
	req, _ := json.Marshal(Request{Name: name})
	if err := link.Send(req); err != nil {
		link.Close()
		return nil, err
	}
	return &Pairing{Digits: ch.SAS(), code: code, link: link}, nil
}

// Wait waits for the machine's decision, and returns what to keep of the machine.
func (p *Pairing) Wait() (Machine, error) {
	defer p.link.Close()
	msg, err := p.link.Receive()
	if err != nil {
		return Machine{}, err
	}
	var d Decision
	if json.Unmarshal(msg, &d) != nil {
		return Machine{}, errors.New("The machine's answer couldn't be read.")
	}
	if !d.OK {
		return Machine{}, ErrDenied
	}
	if len(d.PSK) != secure.KeySize || len(d.Voucher) != secure.VoucherSize {
		return Machine{}, errors.New("The machine's answer couldn't be read.")
	}
	return Machine{ID: p.code.Machine.String(), Key: p.code.MachineKey[:], PSK: d.PSK, Voucher: d.Voucher,
		Name: CleanName(d.Name), ReadOnly: d.ReadOnly}, nil
}

// Cancel gives the request up.
func (p *Pairing) Cancel() { p.link.Close() }

// Connect opens a session with a machine this device is paired with. The link it returns is ready for
// rpc.Client.Run.
func Connect(ctx context.Context, dial Dialer, origin string, m Machine, key noise.DHKey, random io.Reader) (*wire.Link, error) {
	id, err := secure.ParseID(m.ID)
	if err != nil {
		return nil, err
	}
	conn, err := dial(ctx, PathDevice, m.Voucher)
	if err != nil {
		return nil, err
	}
	defer within(ctx, conn)()
	p := secure.Params{Purpose: secure.Session, Origin: origin, Machine: id}
	ini, first, err := secure.Initiate(p, key, m.Key, m.PSK, random)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Write(first); err != nil {
		conn.Close()
		return nil, err
	}
	answer, err := conn.Read()
	if err != nil {
		conn.Close()
		return nil, err
	}
	ch, err := ini.Finish(answer)
	if err != nil {
		conn.Close()
		return nil, ErrRevoked
	}
	return wire.NewLink(conn, ch), nil
}

// ErrRevoked is a machine that no longer answers this device's key: the device was removed there, or
// the machine was set up anew.
var ErrRevoked = errors.New("This device is no longer paired with the machine.")

// CleanName makes a name safe to show: a device or a machine names itself, and the other end shows
// that to a person. Letters, digits, spaces and a few marks are kept, everything else is dropped
// (control characters, and the characters that reorder, hide or break text among them), and the name
// is cut to 40 characters. A device's name is not shown at the machine at all until the device has been
// approved.
func CleanName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.Is(unicode.Mn, r):
		case r == ' ', strings.ContainsRune("-_.,'()&+’", r):
		default:
			continue
		}
		b.WriteRune(r)
		if n++; n == 40 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
