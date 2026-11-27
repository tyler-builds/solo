// The tokens that admit a socket, checked as internal/remote/secure/machine.go checks them
// (VerifyMachineAuth, VerifyVoucher). Nothing here reads the clock or the configuration: the caller
// passes the origin, the time and the allowed keys, so the tests can check the vectors Go made.

import { concat, equal, fromBase64Url } from "./bytes";

const VERSION = 1;
const KEY_SIZE = 32;
const ID_SIZE = 16;
const SIGNATURE_SIZE = 64;
const TICKET_SIZE = 32;

export const MACHINE_AUTH_SIZE = 1 + KEY_SIZE + ID_SIZE + 8 + 16 + SIGNATURE_SIZE;
export const VOUCHER_SIZE = 1 + KEY_SIZE + ID_SIZE + ID_SIZE + 8 + SIGNATURE_SIZE;
export const PAIR_TOKEN_SIZE = 1 + ID_SIZE + TICKET_SIZE;
// A machine's token for a socket to another machine: a machine's token with the other's ID added.
export const PEER_AUTH_SIZE = 1 + KEY_SIZE + ID_SIZE + 8 + 16 + ID_SIZE + SIGNATURE_SIZE;

// How far from the relay's clock a machine's token may be.
export const MACHINE_AUTH_WINDOW_MS = 120_000;

// Why a token is refused: "token" is a token that is not good (401), "key" is one signed by a key
// that is not in the list (403).
export type Refusal = "token" | "key";
export type Verdict<T> = ({ ok: true } & T) | { ok: false; why: Refusal };

const bad: Verdict<never> = { ok: false, why: "token" };
const unlisted: Verdict<never> = { ok: false, why: "key" };

// parseHostKeys reads HOST_KEYS. An entry that is not a 32-byte key allows nobody, so it is dropped.
export function parseHostKeys(list: string): Uint8Array[] {
	const keys: Uint8Array[] = [];
	for (const entry of list.split(",")) {
		const key = fromBase64Url(entry.trim());
		if (key?.length === KEY_SIZE) {
			keys.push(key);
		}
	}
	return keys;
}

// verifyMachineAuth checks the token a machine connects with, for the relay at origin and the time
// now, in milliseconds.
export async function verifyMachineAuth(
	origin: string,
	token: Uint8Array,
	now: number,
	hostKeys: readonly Uint8Array[],
): Promise<Verdict<{ machineId: Uint8Array; key: Uint8Array }>> {
	if (token.length !== MACHINE_AUTH_SIZE || token[0] !== VERSION) {
		return bad;
	}
	const body = token.subarray(0, token.length - SIGNATURE_SIZE);
	const key = body.subarray(1, 1 + KEY_SIZE);
	if (!hostKeys.some((k) => equal(k, key))) {
		return unlisted;
	}
	if (!recent(body, now)) {
		return bad;
	}
	if (!(await signedBy(key, "machine", origin, body, token.subarray(body.length)))) {
		return bad;
	}
	return { ok: true, machineId: body.slice(1 + KEY_SIZE, 1 + KEY_SIZE + ID_SIZE), key: key.slice() };
}

// recent says whether the time in a machine's token, or in its token for another machine, is within
// the window of now. The time is 64 bits, so the difference is taken in BigInt: a number would round it.
function recent(body: Uint8Array, now: number): boolean {
	const at = new DataView(body.buffer, body.byteOffset).getBigUint64(1 + KEY_SIZE + ID_SIZE);
	const age = BigInt(Math.trunc(now)) - at;
	return age <= BigInt(MACHINE_AUTH_WINDOW_MS) && age >= -BigInt(MACHINE_AUTH_WINDOW_MS);
}

// verifyPeerAuth checks the token a machine opens a socket to another machine with: a machine's
// token, signed for that purpose, that also names the machine it is for (target).
export async function verifyPeerAuth(
	origin: string,
	token: Uint8Array,
	now: number,
	hostKeys: readonly Uint8Array[],
): Promise<Verdict<{ machineId: Uint8Array; key: Uint8Array; target: Uint8Array }>> {
	if (token.length !== PEER_AUTH_SIZE || token[0] !== VERSION) {
		return bad;
	}
	const body = token.subarray(0, token.length - SIGNATURE_SIZE);
	const key = body.subarray(1, 1 + KEY_SIZE);
	if (!hostKeys.some((k) => equal(k, key))) {
		return unlisted;
	}
	if (!recent(body, now)) {
		return bad;
	}
	if (!(await signedBy(key, "peer", origin, body, token.subarray(body.length)))) {
		return bad;
	}
	return {
		ok: true,
		machineId: body.slice(1 + KEY_SIZE, 1 + KEY_SIZE + ID_SIZE),
		key: key.slice(),
		target: body.slice(body.length - ID_SIZE),
	};
}

// verifyVoucher checks the token a paired device connects with: its machine's word, for the relay at
// origin, that the device is one of its own. A voucher does not expire; the machine revokes a device
// by refusing its handshake.
export async function verifyVoucher(
	origin: string,
	token: Uint8Array,
	hostKeys: readonly Uint8Array[],
): Promise<Verdict<{ machineId: Uint8Array; deviceId: Uint8Array; key: Uint8Array }>> {
	if (token.length !== VOUCHER_SIZE || token[0] !== VERSION) {
		return bad;
	}
	const body = token.subarray(0, token.length - SIGNATURE_SIZE);
	const key = body.subarray(1, 1 + KEY_SIZE);
	if (!hostKeys.some((k) => equal(k, key))) {
		return unlisted;
	}
	if (!(await signedBy(key, "voucher", origin, body, token.subarray(body.length)))) {
		return bad;
	}
	const ids = 1 + KEY_SIZE;
	return {
		ok: true,
		machineId: body.slice(ids, ids + ID_SIZE),
		deviceId: body.slice(ids + ID_SIZE, ids + 2 * ID_SIZE),
		key: key.slice(),
	};
}

// parsePairToken reads the token of a pairing socket: the machine it is for, and the ticket. Whether
// the ticket is good is the Durable Object's to say, from the ticket's hash.
export function parsePairToken(token: Uint8Array): { machineId: Uint8Array; ticket: Uint8Array } | null {
	if (token.length !== PAIR_TOKEN_SIZE || token[0] !== VERSION) {
		return null;
	}
	return { machineId: token.slice(1, 1 + ID_SIZE), ticket: token.slice(1 + ID_SIZE) };
}

// OPEN_PAIR_TOKEN is the token of a pairing socket that holds no code: one byte, which says so. It
// proves nothing, and the relay lets it in only while a machine has pairing open.
export const OPEN_PAIR_TOKEN = 2;

export function isOpenPairToken(token: Uint8Array): boolean {
	return token.length === 1 && token[0] === OPEN_PAIR_TOKEN;
}

// PASS_TOKEN starts the token of a pairing socket that will prove a network's passphrase to one
// machine of the network: the byte, then the machine's ID. The relay can't check the passphrase (the
// exchange is the machine's), so it lets the socket in to a machine that said it takes them.
export const PASS_TOKEN = 3;
export const PASS_TOKEN_SIZE = 1 + ID_SIZE;

export function parsePassToken(token: Uint8Array): Uint8Array | null {
	if (token.length !== PASS_TOKEN_SIZE || token[0] !== PASS_TOKEN) {
		return null;
	}
	return token.slice(1);
}

// NETWORK_TOKEN is the token of a socket that asks which machines take passphrases: one byte. What it
// is told, every machine of the network could tell it, and their IDs are no secret.
export const NETWORK_TOKEN = 4;

export function isNetworkToken(token: Uint8Array): boolean {
	return token.length === 1 && token[0] === NETWORK_TOKEN;
}

// ticketHash is what the relay stores of a ticket, and all it ever knows of it.
export async function ticketHash(ticket: Uint8Array): Promise<Uint8Array> {
	return new Uint8Array(await crypto.subtle.digest("SHA-256", ticket));
}

// What a signature is made for: a machine's own socket, a device's voucher, or a machine's socket to
// another machine.
export type Context = "machine" | "voucher" | "peer";

// signedMessage is what a token's signature covers: a context string, the relay's origin and the
// token's fields, so a signature made for one purpose or one relay is no use for another.
export function signedMessage(context: Context, origin: string, body: Uint8Array): Uint8Array {
	const text = new TextEncoder();
	const at = text.encode(origin);
	return concat(
		text.encode(`solo/remote/v1/relay/${context}`),
		new Uint8Array([0, at.length >> 8, at.length & 0xff]),
		at,
		body,
	);
}

async function signedBy(
	key: Uint8Array,
	context: Context,
	origin: string,
	body: Uint8Array,
	signature: Uint8Array,
): Promise<boolean> {
	try {
		const imported = await crypto.subtle.importKey("raw", key, "Ed25519", false, ["verify"]);
		return await crypto.subtle.verify("Ed25519", imported, signature, signedMessage(context, origin, body));
	} catch {
		// Bytes that are not a point on the curve do not import: no signature is theirs.
		return false;
	}
}
