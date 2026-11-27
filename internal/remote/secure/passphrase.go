package secure

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/bytemare/opaque"
	"github.com/flynn/noise"
)

// A network is the machines that share a relay and a passphrase, and the devices paired with them.
// Whoever knows the passphrase can ask to join: a device, to be allowed in, or another machine on the
// relay, to become a member. The passphrase is checked with OPAQUE (RFC 9807, github.com/bytemare/
// opaque), a password-authenticated key exchange, so that:
//
//   - the passphrase never leaves the device it is typed on, and nothing that crosses the relay lets
//     anyone test guesses offline: each guess takes a run of the exchange with a machine, which counts
//     them (the caller does);
//   - a machine keeps a record of the passphrase, not the passphrase, and its keys; stolen, those still
//     cost a slow key stretch (Argon2id) per guess;
//   - each end ends up with the same key only if the passphrase was right, which proves it to both,
//     and the machine to the device: only a machine that was given the passphrase has a record that
//     answers to it.
//
// The exchange's key then keys a Noise handshake (XXpsk3) in which the two ends learn each other's
// Noise keys, so that what is kept afterwards (a paired device, a member) is a key the passphrase
// vouched for. Its prologue names the relay and the machine, as every handshake's does here.
//
// The frames, from the one who asks (the client) to the machine (the server):
//
//	client: KE1                    (OPAQUE's first message)
//	server: KE2
//	client: KE3 ‖ Noise message 1  (the passphrase proven; the Noise handshake starts)
//	server: Noise message 2
//	client: Noise message 3        (the channel is up)

// Pass is the purpose of a passphrase handshake, and Peer of a session between two members of a
// network.
const (
	Pass Purpose = 4
	Peer Purpose = 5
)

// passConf is the one OPAQUE configuration: ristretto255 with SHA-512, and Argon2id to stretch.
var passConf = opaque.DefaultConfiguration()

// The identities both ends give OPAQUE. Every machine of a network answers to the same passphrase,
// so it has no identity of its own here; which machine it is, is in the Noise handshake's prologue.
var (
	passClientID = []byte(Version + "/network/client")
	passServerID = []byte(Version + "/network/machine")
	passCredID   = []byte(Version + "/network")
)

// ErrWrongPassphrase is a passphrase that isn't the network's.
var ErrWrongPassphrase = errors.New("secure: the passphrase is not the network's")

// PassRecord is what a machine keeps of the network's passphrase: its OPAQUE keys and the record of
// the passphrase. It is secret: with it, guesses can be tested offline, if slowly.
type PassRecord struct {
	Keys   []byte `json:"keys"`   // opaque.ServerKeyMaterial, encoded
	Record []byte `json:"record"` // the registration record
}

// Encode and DecodePassRecord keep a record as text.
func (r PassRecord) Encode() string {
	b, _ := json.Marshal(r)
	return string(b)
}

func DecodePassRecord(s string) (PassRecord, error) {
	var r PassRecord
	if err := json.Unmarshal([]byte(s), &r); err != nil || len(r.Keys) == 0 || len(r.Record) == 0 {
		return PassRecord{}, errors.New("secure: the passphrase's record is damaged")
	}
	return r, nil
}

// NewPassRecord makes a machine's record of the passphrase: new keys, and the passphrase registered
// with them. It runs both ends of OPAQUE's registration on the machine.
func NewPassRecord(passphrase []byte) (PassRecord, error) {
	if len(passphrase) == 0 {
		return PassRecord{}, errors.New("secure: a passphrase is needed")
	}
	sk, pk := passConf.KeyGen()
	skm := &opaque.ServerKeyMaterial{PrivateKey: sk, PublicKeyBytes: pk.Encode(), OPRFGlobalSeed: passConf.GenerateOPRFSeed(),
		Identity: passServerID}
	server, err := passServer(skm)
	if err != nil {
		return PassRecord{}, err
	}
	client, err := passConf.Client()
	if err != nil {
		return PassRecord{}, err
	}
	req, err := client.RegistrationInit(passphrase)
	if err != nil {
		return PassRecord{}, err
	}
	res, err := server.RegistrationResponse(req, passCredID, nil)
	if err != nil {
		return PassRecord{}, err
	}
	rec, _, err := client.RegistrationFinalize(res, passClientID, passServerID)
	if err != nil {
		return PassRecord{}, err
	}
	return PassRecord{Keys: skm.Encode(), Record: rec.Serialize()}, nil
}

func passServer(skm *opaque.ServerKeyMaterial) (*opaque.Server, error) {
	server, err := passConf.Server()
	if err != nil {
		return nil, err
	}
	if err := server.SetKeyMaterial(skm); err != nil {
		return nil, err
	}
	return server, nil
}

// passPrologue binds a passphrase handshake, or a session between members, to its relay and machine.
func passPrologue(purpose Purpose, origin string, machine ID) ([]byte, error) {
	if origin == "" || len(origin) > 255 {
		return nil, errors.New("secure: a handshake needs the relay's origin")
	}
	b := []byte(Version)
	b = append(b, 0, byte(purpose), 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(origin)))
	b = append(b, origin...)
	return append(b, machine[:]...), nil
}

func passNoise(origin string, machine ID, static noise.DHKey, psk []byte, random io.Reader, initiator bool) (*noise.HandshakeState, error) {
	prologue, err := passPrologue(Pass, origin, machine)
	if err != nil {
		return nil, err
	}
	return noise.NewHandshakeState(noise.Config{CipherSuite: suite, Random: random, Pattern: noise.HandshakeXX,
		Initiator: initiator, Prologue: prologue, StaticKeypair: static, PresharedKey: psk, PresharedKeyPlacement: 3})
}

// passPSK is the Noise handshake's pre-shared key, from OPAQUE's session key.
func passPSK(session []byte) []byte {
	k := derive(session, "network/pass/psk")
	return k[:]
}

// PassClient is the one who asks, part-way through a passphrase handshake.
type PassClient struct {
	client *opaque.Client
	hs     *noise.HandshakeState
}

// StartPass begins a passphrase handshake. It returns the first frame, for the machine.
func StartPass(passphrase []byte) (*PassClient, []byte, error) {
	if len(passphrase) == 0 {
		return nil, nil, errors.New("secure: a passphrase is needed")
	}
	client, err := passConf.Client()
	if err != nil {
		return nil, nil, err
	}
	ke1, err := client.GenerateKE1(passphrase)
	if err != nil {
		return nil, nil, err
	}
	return &PassClient{client: client}, ke1.Serialize(), nil
}

// Answer takes the machine's answer and, if the passphrase was right, returns the next frame: proof of
// the passphrase, and the start of a Noise handshake as static with the machine the relay at origin
// knows as machine. A wrong passphrase is ErrWrongPassphrase.
func (c *PassClient) Answer(ke2 []byte, origin string, machine ID, static noise.DHKey, random io.Reader) ([]byte, error) {
	m, err := c.client.Deserialize.KE2(ke2)
	if err != nil {
		return nil, fmt.Errorf("secure: the machine's answer is not valid: %w", err)
	}
	ke3, session, _, err := c.client.GenerateKE3(m, passClientID, passServerID)
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	hs, err := passNoise(origin, machine, static, passPSK(session), random, true)
	if err != nil {
		return nil, err
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	c.hs = hs
	return append(ke3.Serialize(), msg...), nil
}

// Finish takes the machine's Noise message and returns the last frame and the channel. The channel's
// Peer is the machine's Noise key.
func (c *PassClient) Finish(msg2 []byte) ([]byte, *Channel, error) {
	if c.hs == nil {
		return nil, nil, errors.New("secure: the handshake isn't at its end")
	}
	if _, _, _, err := c.hs.ReadMessage(nil, msg2); err != nil {
		return nil, nil, fmt.Errorf("secure: the machine's answer is not valid: %w", err)
	}
	msg3, send, recv, err := c.hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	if send == nil || recv == nil {
		return nil, nil, errors.New("secure: the handshake did not end")
	}
	return msg3, &Channel{send: send, recv: recv, hash: c.hs.ChannelBinding(), peer: c.hs.PeerStatic()}, nil
}

// PassServer is a machine part-way through a passphrase handshake.
type PassServer struct {
	server *opaque.Server
	out    *opaque.ServerOutput
	hs     *noise.HandshakeState
}

// RespondPass answers the first frame of a passphrase handshake. The answer lets the one who asks test
// one guess of the passphrase, so the caller counts it as one until Verify says it was right.
func RespondPass(rec PassRecord, ke1 []byte) (*PassServer, []byte, error) {
	skm, err := passConf.DecodeServerKeyMaterial(rec.Keys)
	if err != nil {
		return nil, nil, errors.New("secure: the passphrase's record is damaged")
	}
	server, err := passServer(skm)
	if err != nil {
		return nil, nil, err
	}
	r, err := server.Deserialize.RegistrationRecord(rec.Record)
	if err != nil {
		return nil, nil, errors.New("secure: the passphrase's record is damaged")
	}
	m, err := server.Deserialize.KE1(ke1)
	if err != nil {
		return nil, nil, fmt.Errorf("secure: the first message is not valid: %w", err)
	}
	ke2, out, err := server.GenerateKE2(m, &opaque.ClientRecord{RegistrationRecord: r, CredentialIdentifier: passCredID,
		ClientIdentity: passClientID})
	if err != nil {
		return nil, nil, err
	}
	return &PassServer{server: server, out: out}, ke2.Serialize(), nil
}

// ke3Size is the size of OPAQUE's last message: a MAC with SHA-512.
const ke3Size = 64

// Verify takes the third frame: it checks the proof of the passphrase (ErrWrongPassphrase when it is
// not), and answers the Noise handshake as static, the machine the relay at origin knows as machine.
func (s *PassServer) Verify(frame []byte, origin string, machine ID, static noise.DHKey, random io.Reader) ([]byte, error) {
	if len(frame) <= ke3Size {
		return nil, ErrWrongPassphrase
	}
	ke3, err := s.server.Deserialize.KE3(frame[:ke3Size])
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	if err := s.server.LoginFinish(ke3, s.out.ClientMAC); err != nil {
		return nil, ErrWrongPassphrase
	}
	hs, err := passNoise(origin, machine, static, passPSK(s.out.SessionSecret), random, false)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, frame[ke3Size:]); err != nil {
		return nil, fmt.Errorf("secure: the handshake is not valid: %w", err)
	}
	msg2, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	s.hs = hs
	return msg2, nil
}

// Finish takes the last frame and returns the channel. Its Peer is the Noise key of the one who asked.
func (s *PassServer) Finish(msg3 []byte) (*Channel, error) {
	if s.hs == nil {
		return nil, errors.New("secure: the handshake isn't at its end")
	}
	_, recv, send, err := s.hs.ReadMessage(nil, msg3)
	if err != nil {
		return nil, fmt.Errorf("secure: the handshake is not valid: %w", err)
	}
	if send == nil || recv == nil {
		return nil, errors.New("secure: the handshake did not end")
	}
	return &Channel{send: send, recv: recv, hash: s.hs.ChannelBinding(), peer: s.hs.PeerStatic()}, nil
}

// Derive is a key both ends of a channel agree on, for one named use: a member's key for later
// sessions, made where it joined.
func (c *Channel) Derive(use string) []byte {
	k := derive(c.hash, "channel/"+use)
	return k[:]
}
