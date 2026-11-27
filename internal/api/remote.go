package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tyler-builds/solo/internal/core"
)

// Remote is a machine's remote access, as its Settings page drives it. The desktop app has one
// (internal/remote/host); a web page has none.
type Remote interface {
	State() RemoteState
	// Subscribe calls f, off the UI thread, whenever State changes.
	Subscribe(f func()) func()
	// SetRelay names the relay by the address its web client is served from, and turns remote access
	// on; "" forgets the relay.
	SetRelay(address string) error
	SetEnabled(on bool) error
	// StartPairing opens pairing, and Approve pairs the device that shows the six digits. Pairing is
	// with a code, which the device scans or is handed as a link, or, with open set, without one: for
	// a browser that was only given the relay's address. The two are never open together.
	StartPairing(open bool) (RemotePairing, error)
	CancelPairing()
	Approve(digits string, readOnly bool) (RemoteDevice, error)
	Revoke(deviceID string) error
	RevokeAll() error
	SetReadOnly(deviceID string, readOnly bool) error

	// The network: the machines on this relay that share a passphrase, and the devices that join by
	// typing it. SetPassphrase sets it here (and on the other members), turns joining by passphrase
	// on, and joins the machines on the relay that have the same one. SetNetworkOn turns joining by
	// passphrase off, or on again after too many wrong guesses turned it off. A device that proves the
	// passphrase waits (State.Network.Joins) until AllowJoin lets it into every machine of the
	// network, or DenyJoin turns it down; with SetAutoAllow on, it is let in at once. MemberUsage
	// fetches each other member's usage; a member that can't be reached comes with its last copy.
	SetPassphrase(passphrase string) error
	SetNetworkOn(on bool) error
	SetAutoAllow(on bool) error
	AllowJoin(requestID string, readOnly bool) error
	DenyJoin(requestID string)
	RemoveMember(machineID string) error
	MemberUsage(ctx context.Context) []MemberUsage

	// The relay lives in the user's own Cloudflare account. SetCloudToken keeps an API token for it,
	// DeployRelay puts the relay and the web client there (or joins the relay already there) and
	// connects to it, and RemoveRelay deletes it. AcceptRelay resumes remote access after a change to
	// the relay from elsewhere paused it (RemoteCloud.Paused), removing every device first if asked.
	SetCloudToken(ctx context.Context, token string) error
	ForgetCloudToken() error
	DeployRelay(ctx context.Context) error
	RemoveRelay(ctx context.Context) error
	AcceptRelay(ctx context.Context, removeDevices bool) error
}

// RemoteCloud is where remote access stands with the Cloudflare account.
type RemoteCloud struct {
	HasToken bool   // an API token is kept on this machine
	Account  string // the account's name, once a token was checked
	Deployed bool   // this machine set up, or joined, a relay there
	// CanDeploy: this build of Solo has the relay and the web client in it.
	CanDeploy bool
	Busy      string // what a deploy is doing now, or ""
	// Paused: the relay is not as this machine left it, and remote access waits for the user. Alert
	// says so, and Changes what was found. Alert is also set, without a pause, when the relay was
	// deleted from elsewhere.
	Paused  bool
	Alert   string
	Changes []string
	// CheckProblem: the relay couldn't be checked lately.
	CheckProblem string
}

// RemoteConnection is where the machine stands with its relay.
type RemoteConnection string

const (
	RemoteOff        RemoteConnection = "off"        // remote access is off, or has no relay
	RemoteConnecting RemoteConnection = "connecting" // connecting, or waiting to try again
	RemoteOnline     RemoteConnection = "online"
	// RemoteRefused: the relay doesn't know this machine. It was removed from the relay, or the relay
	// was set up again.
	RemoteRefused RemoteConnection = "refused"
)

// RemoteState is remote access as Settings shows it.
type RemoteState struct {
	Relay      string // the relay's address; "" when not set up
	Enabled    bool
	Connection RemoteConnection
	Problem    string // why the relay can't be reached, when it can't
	MachineID  string
	Name       string
	Devices    []RemoteDevice
	// Pairing is the pairing code on offer, if any, and Requests the devices asking to pair with it.
	Pairing  *RemotePairing
	Requests []RemoteRequest
	// SecretsInFiles: there is no OS keyring, so the machine's key is in a file only the user can read.
	SecretsInFiles bool
	Cloud          RemoteCloud
	Network        RemoteNetwork
}

// RemoteNetwork is the network as Settings shows it.
type RemoteNetwork struct {
	Passphrase bool // one is set on this machine
	On         bool // devices and machines may join by it
	// Locked: too many wrong passphrases turned joining off, and why; Wrong counts them.
	Locked    string
	Wrong     int
	AutoAllow bool
	Members   []RemoteMember
	// Joins are the devices that proved the passphrase and wait to be let in.
	Joins []RemoteJoin
	// Status is how joining the other machines on the relay last went; Problem why the network
	// can't be used (a relay too old for it).
	Status  string
	Problem string
}

// RemoteMember is another machine of the network.
type RemoteMember struct {
	ID    string
	Name  string
	Added time.Time
}

// RemoteJoin is a device that proved the passphrase and waits. Its name is its own choice of words.
type RemoteJoin struct {
	ID   string
	Name string
	At   time.Time
}

// MemberUsage is a member's usage, as last fetched.
type MemberUsage struct {
	ID     string
	Name   string
	Report core.UsageReport
	At     time.Time // when it was fetched; zero when never
	Err    string    // why it couldn't be fetched now
}

// RemoteDevice is a paired device.
type RemoteDevice struct {
	ID       string
	Name     string
	ReadOnly bool
	Added    time.Time
	LastSeen time.Time
	Online   bool
}

// RemotePairing is pairing as it stands open.
type RemotePairing struct {
	// Open: without a code. A device asks at the relay's address, and Link is empty.
	Open bool
	// Link opens the web client with the code; it is what the QR code holds. Anyone who has it can ask
	// to pair until it expires, so it is shown, not sent.
	Link    string
	Expires time.Time
}

// RemoteRequest is a device that holds the pairing code and asks to pair. Nothing the device says of
// itself is here, neither its name nor the six digits it shows: until the person has typed the digits
// they read from the device in their hand, anything a requester says is text chosen by whoever holds
// the code, and would be shown right beside the box the digits are typed in.
type RemoteRequest struct {
	At time.Time
}

// MinPassphrase is the shortest passphrase a network takes, in characters.
const MinPassphrase = 12

// CheckPassphrase says what is wrong with a passphrase for a network, or nil.
func CheckPassphrase(p string) error {
	switch {
	case utf8.RuneCountInString(p) < MinPassphrase:
		return fmt.Errorf("A passphrase needs at least %d characters: anyone who finds your relay's address can try a few guesses.", MinPassphrase)
	case len(p) > 1024:
		return errors.New("That passphrase is too long.")
	case strings.TrimSpace(p) != p:
		return errors.New("A passphrase can't start or end with a space.")
	}
	return nil
}

// SuggestPassphrase makes a passphrase from the system's random source: five groups of four letters
// and digits, about 100 bits.
func SuggestPassphrase() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no 0, o, 1, i or l: they're misread
	var b [20]byte
	rand.Read(b[:])
	var out strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			out.WriteByte('-')
		}
		out.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return out.String()
}
