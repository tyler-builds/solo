package api

import (
	"context"
	"time"

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

	// LinkTo links this machine's Solo to another's, to share usage both ways, with the pairing link
	// that machine shows under Add a device. It returns once the request is made: State.Linking then
	// has the six digits to type there, and how it ends. CancelLink gives it up, and Unlink removes a
	// link (and the other machine's way back here).
	LinkTo(link string) error
	CancelLink()
	Unlink(machineID string) error
	// LinkedUsage fetches the usage of each linked machine. A machine that can't be reached comes
	// with the last copy fetched, if any, and why.
	LinkedUsage(ctx context.Context) []LinkedUsage

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
	// Links are the Solos on other machines this one shares usage with, and Linking a link being made.
	Links   []RemoteLink
	Linking *RemoteLinking
}

// RemoteLink is another machine's Solo that this one is linked to.
type RemoteLink struct {
	ID    string // the other machine's ID
	Name  string
	Relay string
	Added time.Time
	// OneWay: this machine reads the other's usage, but the other can't read this one's (this
	// machine had no relay when they were linked).
	OneWay bool
}

// RemoteLinking is a link being made from this machine (LinkTo).
type RemoteLinking struct {
	Digits string // to type at the other machine
	Done   bool
	Err    string
	Note   string // how it went, once done
}

// LinkedUsage is a linked machine's usage, as last fetched.
type LinkedUsage struct {
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
	// Linked: it is another machine's Solo, which may only read usage.
	Linked bool
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
