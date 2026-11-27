// Package device is a device's side of pairing with a machine and of connecting to it afterwards: the
// web client runs it over the browser's WebSocket, and tests run it against a real machine.
package device

import (
	"bytes"
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
	// PathPeer is a machine's connection to another machine on the relay (a member of its network).
	PathPeer    = "/v1/peer"
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
	CloseAmbiguous    = 4006 // more than one machine has pairing open to devices with no code
)

// OpenPairToken is the token of a pairing connection that holds no code: one byte that says so. The
// relay lets it in only while a machine has pairing open.
const OpenPairToken = 2

// The tokens of a network (network.go): a pairing by passphrase with one machine (PassPairToken and the
// machine's ID), and a request for the machines of the relay's network (NetworkToken alone).
const (
	PassPairToken = 3
	NetworkToken  = 4
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
	// Request is the device's: who is asking. In a pairing without a code it also reveals the number
	// the device committed to before the handshake (secure.OpenCommitment).
	Request struct {
		Name  string `json:"name"`
		Nonce []byte `json:"nonce,omitempty"`
	}
	// Decision is the machine's. Machine is the machine's ID, which a device that had no code learns
	// here.
	Decision struct {
		OK       bool   `json:"ok"`
		PSK      []byte `json:"psk,omitempty"`
		Voucher  []byte `json:"voucher,omitempty"`
		Name     string `json:"name,omitempty"`
		ReadOnly bool   `json:"readOnly,omitempty"`
		Machine  string `json:"machine,omitempty"`
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
	link   *wire.Link
	// The machine, as far as the device knows it: from the code, or from the handshake when there was
	// no code, in which case its ID comes with the decision.
	machineID  string
	machineKey []byte
	deviceKey  []byte
	// Unconfirmed: nothing told this device beforehand which machine it was talking to (it had no
	// code), so the machine's "paired" is only the word of whoever answered. The person confirms it at
	// the machine before the device keeps anything (see Wait).
	Unconfirmed bool
}

// ErrSeveralOpen is a pairing without a code while more than one machine has pairing open: the relay
// won't choose between them.
var ErrSeveralOpen = errors.New("More than one machine has Add a device open. Close it on all but the one you mean, then try again here.")

// ErrNotOpen is a pairing without a code that found no machine waiting for one.
var ErrNotOpen = errors.New("No machine is waiting to pair a browser. On the computer that runs Solo, open Settings, then Remote, choose Add a device, and then its option for a computer's browser. Then try again here.")

// PairOpen asks to pair with whichever machine has pairing open, for a device that holds no code: a
// browser that was only given the relay's address. The digits it returns are the only thing that ties
// this device to the machine, so the person must type exactly these at the machine (see
// secure/open.go for why nobody in between can arrange for them to match).
func PairOpen(ctx context.Context, dial Dialer, origin string, key noise.DHKey, name string, random io.Reader) (*Pairing, error) {
	// notOpen says what a refusal means here. A browser doesn't say why a connection was refused, and
	// a machine with no pairing open hangs up without a word.
	notOpen := func(err error) error {
		var closed *CloseError
		switch {
		case errors.As(err, &closed) && closed.Code == CloseAmbiguous:
			return ErrSeveralOpen
		case errors.As(err, &closed) && closed.Code != CloseNormal:
			return err
		}
		return ErrNotOpen
	}
	conn, err := dial(ctx, PathPair, []byte{OpenPairToken})
	if err != nil {
		return nil, notOpen(err)
	}
	defer within(ctx, conn)()
	ini, first, err := secure.InitiateOpen(origin, key, random)
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
		return nil, notOpen(err)
	}
	third, ch, err := ini.Finish(answer)
	if err != nil {
		conn.Close()
		return nil, errors.New("The machine's answer couldn't be read.")
	}
	if err := conn.Write(third); err != nil {
		conn.Close()
		return nil, err
	}
	link := wire.NewLink(conn, ch)
	// Only now, with the handshake behind both ends, is the number revealed.
	req, _ := json.Marshal(Request{Name: name, Nonce: ini.Nonce()})
	if err := link.Send(req); err != nil {
		link.Close()
		return nil, err
	}
	return &Pairing{Digits: secure.OpenDigits(ch, ini.Nonce()), link: link, machineKey: append([]byte(nil), ch.Peer()...),
		deviceKey: key.Public, Unconfirmed: true}, nil
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
	return &Pairing{Digits: ch.SAS(), link: link, machineID: code.Machine.String(), machineKey: code.MachineKey[:],
		deviceKey: key.Public}, nil
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
	// What the machine sent must hang together: the voucher is for this device, and it names the
	// machine whose key the handshake was with. A machine's ID is a hash of that key and of the key the
	// voucher is signed with, so a machine can't claim another's ID here.
	const relayKeyAt, machineAt, deviceAt = 1, 1 + secure.KeySize, 1 + secure.KeySize + 16
	relayKey := d.Voucher[relayKeyAt:machineAt]
	id := secure.MachineID(p.machineKey, relayKey)
	own := secure.DeviceID(p.deviceKey)
	if !bytes.Equal(d.Voucher[machineAt:deviceAt], id[:]) || !bytes.Equal(d.Voucher[deviceAt:deviceAt+16], own[:]) {
		return Machine{}, errors.New("The machine's answer doesn't hold together. Nothing was kept.")
	}
	if p.machineID != "" && p.machineID != id.String() {
		return Machine{}, errors.New("The machine that answered isn't the one the code is from. Nothing was kept.")
	}
	return Machine{ID: id.String(), Key: p.machineKey, PSK: d.PSK, Voucher: d.Voucher,
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
