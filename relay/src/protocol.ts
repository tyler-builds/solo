// The numbers of version 1 of the relay's protocol (docs/REMOTE.md, "The relay"). The machine's side
// of the same protocol is in Go.

import type { Relay } from "./relay";

export interface Env {
	// The relay keys of the machines allowed to connect: unpadded base64url, separated by commas.
	HOST_KEYS: string;
	SOLO_RELAY: string;
	// The version of the deployed bundle, an integer as text.
	SOLO_BUNDLE: string;
	RELAY: DurableObjectNamespace<Relay>;
}

// The subprotocol every socket asks for; the token is the second one, and is never echoed.
export const SUBPROTOCOL = "solo.v1";

// A peer is a machine's socket to another machine: one of the network's machines reaching another.
export type Role = "machine" | "device" | "pair" | "peer";

// The headers the Worker tells the Durable Object what it verified with. The Worker builds that
// request itself and copies nothing from the client's, so a client cannot set them.
export const HEADER = {
	role: "X-Solo-Role",
	machine: "X-Solo-Machine",
	// The relay key that signed a machine's token or a device's voucher.
	key: "X-Solo-Key",
	device: "X-Solo-Device",
	ticket: "X-Solo-Ticket",
	// Set for a pairing socket that holds no code: it is for whichever machine has pairing open.
	open: "X-Solo-Open",
	// Set for a pairing socket that will prove the network's passphrase to the machine it names.
	pass: "X-Solo-Pass",
	// Set for a socket that only asks which machines take passphrases.
	list: "X-Solo-List",
	// The machine a peer socket is for, by ID.
	target: "X-Solo-Target",
} as const;

// The first byte of every frame on a machine's socket.
export const FRAME = { data: 1, open: 2, close: 3, ticket: 4, usage: 5 } as const;

// The kind in an open frame: a paired device, a pairing with a code, a pairing without one, a pairing
// with the network's passphrase, and another machine. Only another machine's open frame is longer:
// after its ID comes the relay key it connected under.
export const KIND = { device: 1, pair: 2, openPair: 3, passPair: 4, peer: 5 } as const;

// The operation in a ticket frame. A machine opens pairing to devices that hold no code with `open`
// (the frame's hash is unused, its expiry and uses are the window's) and closes it with `close`. It
// starts and stops taking the network's passphrase with `networkOn` and `networkOff`, for which the
// frame's other fields are unused.
export const TICKET = { add: 1, remove: 2, open: 3, close: 4, networkOn: 5, networkOff: 6 } as const;

export const CLOSE = {
	normal: 1000,
	replaced: 4000,
	offline: 4001,
	unauthorized: 4002,
	limit: 4003,
	protocol: 4004,
	// Reserved: nothing sends it yet.
	reload: 4005,
	// A device with no code asked to pair while more than one machine had pairing open: the relay
	// won't choose which machine a device is paired with.
	ambiguous: 4006,
} as const;

export const LIMIT = {
	payload: 65536,
	machines: 8,
	devices: 16,
	pairs: 2,
	// Other machines' sockets to one machine.
	peers: 16,
	// The token bucket of a device's or a pairing socket's frames.
	burst: 200,
	perSecond: 50,
	pairLifetimeMs: 10 * 60 * 1000,
	// Wrong tickets for one machine before pairing with it is refused for the rest of the window.
	failures: 20,
	failureWindowMs: 10 * 60 * 1000,
	// Pairing opened to devices with no code lasts at most this long and lets in at most this many,
	// whatever the machine asks for.
	openPairSeconds: 10 * 60,
	openPairUses: 16,
	// Pairing sockets with the passphrase let in to one machine in a window. The machine counts the
	// wrong passphrases itself; this only keeps a flood from reaching it.
	passPairs: 30,
	passWindowMs: 10 * 60 * 1000,
	// How many forwarded frames go by between writes of the count, and between usage frames.
	flushEvery: 1000,
} as const;

export const CONN_SIZE = 8;
export const ID_SIZE = 16;
export const HASH_SIZE = 32;
export const KEY_SIZE = 32;
