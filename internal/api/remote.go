package api

import "time"

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
