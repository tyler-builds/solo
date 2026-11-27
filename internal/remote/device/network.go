package device

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/flynn/noise"

	"github.com/tyler-builds/solo/internal/remote/secure"
	"github.com/tyler-builds/solo/internal/remote/wire"
)

// A network is the machines on a relay that share a passphrase (secure/passphrase.go). A device that
// knows the passphrase pairs with all of them: it asks the relay which they are, then runs the
// passphrase handshake with each in turn, and each waits until the person allows it at one of them.

// ErrNoNetwork is a relay with no machine whose passphrase pairing is on.
var ErrNoNetwork = errors.New("No machine here is taking a passphrase. On a computer that runs Solo, open Settings, then Remote, and set the passphrase.")

// Network asks the relay for the machines of its network, by ID. The relay is trusted with nothing,
// this list included: a machine it makes up fails the handshake, and one it leaves out isn't paired.
func Network(ctx context.Context, dial Dialer) ([]secure.ID, error) {
	conn, err := dial(ctx, PathPair, []byte{NetworkToken})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer within(ctx, conn)()
	list, err := conn.Read()
	if err != nil {
		return nil, err
	}
	if len(list)%len(secure.ID{}) != 0 || len(list) > 64*len(secure.ID{}) {
		return nil, errors.New("The relay's list of machines couldn't be read.")
	}
	var out []secure.ID
	for i := 0; i < len(list); i += len(secure.ID{}) {
		out = append(out, secure.ID(list[i:i+len(secure.ID{})]))
	}
	return out, nil
}

// PassPairToken is the token of a pairing by passphrase with a machine.
func passToken(machine secure.ID) []byte { return append([]byte{PassPairToken}, machine[:]...) }

// PairPass asks a machine of the network to pair with this device, by the passphrase. It returns once
// the passphrase has been proven and the request made; Wait then waits for the person to allow it at
// one of the network's machines. A wrong passphrase is secure.ErrWrongPassphrase, and the machine
// counts it.
func PairPass(ctx context.Context, dial Dialer, origin string, machine secure.ID, passphrase []byte, key noise.DHKey, ask Request, random io.Reader) (*Pairing, error) {
	conn, err := dial(ctx, PathPair, passToken(machine))
	if err != nil {
		return nil, err
	}
	link, err := passHandshake(ctx, conn, origin, machine, passphrase, key, random)
	if err != nil {
		return nil, err
	}
	ask.Nonce = nil
	req, _ := json.Marshal(ask)
	if err := link.Send(req); err != nil {
		link.Close()
		return nil, err
	}
	return &Pairing{link: link, machineID: machine.String(), machineKey: link.Channel().Peer(), deviceKey: key.Public}, nil
}

// PassHandshake runs the passphrase handshake on a connection the caller opened, as the one who asks:
// a device, or a machine that joins the network (it opens the connection as a peer). The link's Peer is
// the machine's Noise key.
func PassHandshake(ctx context.Context, conn wire.Conn, origin string, machine secure.ID, passphrase []byte, key noise.DHKey, random io.Reader) (*wire.Link, error) {
	return passHandshake(ctx, conn, origin, machine, passphrase, key, random)
}

func passHandshake(ctx context.Context, conn wire.Conn, origin string, machine secure.ID, passphrase []byte, key noise.DHKey, random io.Reader) (*wire.Link, error) {
	defer within(ctx, conn)()
	fail := func(err error) (*wire.Link, error) {
		conn.Close()
		return nil, err
	}
	c, f1, err := secure.StartPass(passphrase)
	if err != nil {
		return fail(err)
	}
	if err := conn.Write(f1); err != nil {
		return fail(err)
	}
	f2, err := conn.Read()
	if err != nil {
		return fail(err)
	}
	f3, err := c.Answer(f2, origin, machine, key, random)
	if err != nil {
		return fail(err)
	}
	if err := conn.Write(f3); err != nil {
		return fail(err)
	}
	f4, err := conn.Read()
	if err != nil {
		return fail(err)
	}
	f5, ch, err := c.Finish(f4)
	if err != nil {
		return fail(err)
	}
	if err := conn.Write(f5); err != nil {
		return fail(err)
	}
	return wire.NewLink(conn, ch), nil
}
