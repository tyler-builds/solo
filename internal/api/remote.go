package api

import (
	"context"
	"time"
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
	// StartPairing offers a pairing code, and Approve pairs the device that shows the six digits.
	StartPairing() (RemotePairing, error)
	CancelPairing()
	Approve(digits string, readOnly bool) (RemoteDevice, error)
	Revoke(deviceID string) error
	RevokeAll() error
	SetReadOnly(deviceID string, readOnly bool) error

	// The relay lives in the user's own Cloudflare account. SetCloudToken keeps an API token for it,
	// DeployRelay puts the relay and the web client there (or joins the relay already there) and
	// connects to it, and RemoveRelay deletes it. AcceptRelay resumes remote access after a change to
	// the relay from elsewhere paused it (RemoteCloud.Paused).
	SetCloudToken(ctx context.Context, token string) error
	ForgetCloudToken() error
	DeployRelay(ctx context.Context) error
	RemoveRelay(ctx context.Context) error
	AcceptRelay(ctx context.Context) error
}

// RemoteCloud is where remote access stands with the Cloudflare account.
type RemoteCloud struct {
	HasToken bool   // an API token is kept on this machine
	Account  string // the account's name, once a token was checked
	Deployed bool   // this machine set up, or joined, a relay there
	// CanDeploy: this build of Solo has the relay and the web client in it.
	CanDeploy bool
	Busy      string // what a deploy is doing now, or ""
	// Paused: the relay was changed from elsewhere, and remote access waits for the user. Alert says so.
	Paused bool
	Alert  string
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

// RemotePairing is a pairing code on offer.
type RemotePairing struct {
	// Link opens the web client with the code; it is what the QR code holds. Anyone who has it can ask
	// to pair until it expires, so it is shown, not sent.
	Link    string
	Expires time.Time
}

// RemoteRequest is a device that holds the pairing code and asks to pair. The six digits it shows are
// not here: the person reads them from the device and types them, which is what proves the device in
// their hand is the one that asked.
type RemoteRequest struct {
	Name string
	At   time.Time
}
