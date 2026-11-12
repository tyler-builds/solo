// What the tests share: tokens signed as a machine signs them, sockets through the Worker, and the
// frames of a machine's socket.

import { runInDurableObject } from "cloudflare:test";
import { env, exports } from "cloudflare:workers";
import { afterEach, expect } from "vitest";
import { parseHostKeys, signedMessage, ticketHash } from "../src/auth";
import { concat, fromBase64Url, fromHex, toBase64Url, toHex } from "../src/bytes";
import { FRAME, SUBPROTOCOL, TICKET } from "../src/protocol";
import type { Relay } from "../src/relay";

export const HOST = "relay.test";
export const ORIGIN = `https://${HOST}`;

// Signer is a machine's relay key: the private half to sign with, and the public half as the relay
// knows it.
export interface Signer {
	key: CryptoKey;
	public: Uint8Array;
}

// A key HOST_KEYS allows. It signs for every machine of the tests unless a test says otherwise: a
// machine is known to the relay by its key and its ID together, so one key may have many.
export const listed: Signer = {
	key: await crypto.subtle.importKey("pkcs8", fromBase64Url(env.TEST_KEY)!, "Ed25519", false, ["sign"]),
	public: parseHostKeys(env.HOST_KEYS)[1]!,
};

// Another key HOST_KEYS allows: another machine of the same user.
export const otherListed: Signer = {
	key: await crypto.subtle.importKey("pkcs8", fromBase64Url(env.TEST_KEY_2)!, "Ed25519", false, ["sign"]),
	public: parseHostKeys(env.HOST_KEYS)[2]!,
};

// unlisted makes a key that HOST_KEYS does not have.
export async function unlisted(): Promise<Signer> {
	const pair = (await crypto.subtle.generateKey("Ed25519", true, ["sign", "verify"])) as CryptoKeyPair;
	return { key: pair.privateKey, public: new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey) as ArrayBuffer) };
}

export function random(size: number): Uint8Array {
	return crypto.getRandomValues(new Uint8Array(size));
}

function uint64(value: number): Uint8Array {
	const out = new Uint8Array(8);
	new DataView(out.buffer).setBigUint64(0, BigInt(value));
	return out;
}

async function sign(signer: Signer, context: "machine" | "voucher", origin: string, body: Uint8Array): Promise<Uint8Array> {
	const signature = await crypto.subtle.sign("Ed25519", signer.key, signedMessage(context, origin, body));
	return concat(body, new Uint8Array(signature));
}

interface TokenOptions {
	signer?: Signer;
	origin?: string;
	// The token's time, in milliseconds.
	at?: number;
}

// machineAuth makes the token a machine connects with, as bytes.
export async function machineAuth(machineId: Uint8Array, options: TokenOptions = {}): Promise<Uint8Array> {
	const signer = options.signer ?? listed;
	const body = concat(new Uint8Array([1]), signer.public, machineId, uint64(options.at ?? Date.now()), random(16));
	return sign(signer, "machine", options.origin ?? ORIGIN, body);
}

// voucher makes the token a paired device connects with, as bytes.
export async function voucher(machineId: Uint8Array, deviceId: Uint8Array, options: TokenOptions = {}): Promise<Uint8Array> {
	const signer = options.signer ?? listed;
	const issued = Math.floor((options.at ?? Date.now()) / 1000);
	const body = concat(new Uint8Array([1]), signer.public, machineId, deviceId, uint64(issued));
	return sign(signer, "voucher", options.origin ?? ORIGIN, body);
}

export function pairToken(machineId: Uint8Array, ticket: Uint8Array): Uint8Array {
	return concat(new Uint8Array([1]), machineId, ticket);
}

// flipped is a token with one bit changed.
export function flipped(token: Uint8Array, at: number): Uint8Array {
	const out = token.slice();
	out[at]! ^= 1;
	return out;
}

interface UpgradeOptions {
	// The Origin header; null sends none. By default a machine sends none and a browser sends the
	// relay's own.
	origin?: string | null;
	// The whole Sec-WebSocket-Protocol header, in place of "solo.v1, <token>".
	protocols?: string | null;
	headers?: Record<string, string>;
	host?: string;
}

// upgrade asks the Worker for a socket and returns its answer, whatever it is.
export async function upgrade(path: string, token: Uint8Array | null, options: UpgradeOptions = {}): Promise<Response> {
	const headers = new Headers({ Upgrade: "websocket", ...options.headers });
	const protocols = options.protocols !== undefined ? options.protocols : `${SUBPROTOCOL}, ${toBase64Url(token!)}`;
	if (protocols !== null) {
		headers.set("Sec-WebSocket-Protocol", protocols);
	}
	const host = options.host ?? HOST;
	const origin = options.origin !== undefined ? options.origin : path === "/v1/machine" ? null : `https://${host}`;
	if (origin !== null) {
		headers.set("Origin", origin);
	}
	return exports.default.fetch(`https://${host}${path}`, { headers });
}

const open: Socket[] = [];

// Socket is the client's end of a socket the relay accepted, with what arrives on it in a queue.
export class Socket {
	// The code the socket closed with.
	readonly closed: Promise<number>;
	private queue: (Uint8Array | string)[] = [];
	private waiting: ((message: Uint8Array | string) => void)[] = [];

	constructor(readonly ws: WebSocket) {
		ws.binaryType = "arraybuffer";
		ws.addEventListener("message", (event) => {
			const message = typeof event.data === "string" ? event.data : new Uint8Array(event.data as ArrayBuffer);
			const waiter = this.waiting.shift();
			if (waiter) {
				waiter(message);
			} else {
				this.queue.push(message);
			}
		});
		this.closed = new Promise((resolve) => ws.addEventListener("close", (event) => resolve(event.code)));
		ws.accept();
		open.push(this);
	}

	// next is the next message, in the order they arrived. It fails when none comes.
	next(): Promise<Uint8Array | string> {
		const queued = this.queue.shift();
		if (queued !== undefined) {
			return Promise.resolve(queued);
		}
		return new Promise((resolve, reject) => {
			const timer = setTimeout(() => reject(new Error("no message arrived")), 3000);
			this.waiting.push((message) => {
				clearTimeout(timer);
				resolve(message);
			});
		});
	}

	// frame is the next message, which must be a binary one.
	async frame(): Promise<Uint8Array> {
		const message = await this.next();
		if (typeof message === "string") {
			throw new Error(`a text frame arrived: ${message}`);
		}
		return message;
	}

	// idle says whether nothing has arrived and nothing is about to. It gives the relay a moment, then
	// asks for a pong, which comes after anything the relay sent before.
	async idle(): Promise<boolean> {
		await new Promise((resolve) => setTimeout(resolve, 50));
		this.ws.send("ping");
		return (await this.next()) === "pong";
	}

	send(message: Uint8Array | string): void {
		this.ws.send(message);
	}

	close(): void {
		try {
			this.ws.close(1000);
		} catch {
			// It has closed already.
		}
	}
}

export function relay(): DurableObjectStub<Relay> {
	return env.RELAY.get(env.RELAY.idFromName("default"));
}

// Every test starts with no sockets: the limits count them.
afterEach(async () => {
	for (const socket of open.splice(0)) {
		socket.close();
	}
	for (let i = 0; i < 200; i++) {
		const left = await runInDurableObject(relay(), (_, state) => state.getWebSockets().length);
		if (left === 0) {
			return;
		}
		await new Promise((resolve) => setTimeout(resolve, 10));
	}
	throw new Error("sockets were left open in the relay");
});

export async function accepted(response: Response): Promise<Socket> {
	expect(response.status).toBe(101);
	expect(response.headers.get("Sec-WebSocket-Protocol")).toBe(SUBPROTOCOL);
	return new Socket(response.webSocket!);
}

// Machine is a machine's socket and its ID.
export interface Machine {
	id: Uint8Array;
	// The key it connected under.
	signer: Signer;
	socket: Socket;
	// The usage frame the relay sent when the machine connected.
	usage: { day: string; frames: number };
}

// connectMachine connects a machine, with a new ID unless one is given.
export async function connectMachine(id: Uint8Array = random(16), signer: Signer = listed): Promise<Machine> {
	const socket = await accepted(await upgrade("/v1/machine", await machineAuth(id, { signer })));
	const first = await socket.frame();
	expect(first[0]).toBe(FRAME.usage);
	return { id, signer, socket, usage: JSON.parse(new TextDecoder().decode(first.subarray(1))) };
}

export async function connectDevice(machineId: Uint8Array, deviceId: Uint8Array = random(16)): Promise<Socket> {
	return accepted(await upgrade("/v1/device", await voucher(machineId, deviceId)));
}

export async function connectPair(machineId: Uint8Array, ticket: Uint8Array): Promise<Socket> {
	return accepted(await upgrade("/v1/pair", pairToken(machineId, ticket)));
}

// Opened is an open frame, read.
export interface Opened {
	conn: Uint8Array;
	kind: number;
	deviceId: Uint8Array;
}

// opened reads the machine's next frame, which must be an open frame.
export async function opened(machine: Machine): Promise<Opened> {
	const frame = await machine.socket.frame();
	expect(frame[0]).toBe(FRAME.open);
	expect(frame.length).toBe(26);
	return { conn: frame.slice(1, 9), kind: frame[9]!, deviceId: frame.slice(10) };
}

export function dataFrame(conn: Uint8Array, payload: Uint8Array): Uint8Array {
	return concat(new Uint8Array([FRAME.data]), conn, payload);
}

export function closeFrame(conn: Uint8Array): Uint8Array {
	return concat(new Uint8Array([FRAME.close]), conn);
}

function ticketFrame(op: number, hash: Uint8Array, expires = 0, uses = 0): Uint8Array {
	return concat(new Uint8Array([FRAME.ticket, op]), hash, uint64(Math.floor(expires)), new Uint8Array([uses]));
}

// handled waits until the relay has dealt with every frame the machine has sent so far. A socket's
// frames are dealt with in order, so a ticket that is put in the relay's storage here, and is gone
// after the machine removed it, shows that they have been. (A request to the object does not: it may
// overtake a frame.)
export async function handled(machine: Machine): Promise<void> {
	const marker = toHex(random(32));
	await runInDurableObject(relay(), (_, state) => {
		state.storage.sql.exec("INSERT INTO tickets (hash, machine, expires, uses) VALUES (?, ?, ?, 1)", marker, `${toHex(machine.signer.public)}:${toHex(machine.id)}`, Date.now());
	});
	machine.socket.send(ticketFrame(TICKET.remove, fromHex(marker, 32)!));
	for (let i = 0; i < 300; i++) {
		const left = await runInDurableObject(relay(), (_, state) =>
			state.storage.sql.exec("SELECT 1 FROM tickets WHERE hash = ?", marker).toArray().length,
		);
		if (left === 0) {
			return;
		}
		await new Promise((resolve) => setTimeout(resolve, 10));
	}
	throw new Error("the relay did not deal with the machine's frames");
}

// addTicket registers a ticket with the relay, as the machine does, and waits until the relay has
// dealt with it.
export async function addTicket(machine: Machine, ticket: Uint8Array, uses = 1, expires = Date.now() / 1000 + 300): Promise<void> {
	machine.socket.send(ticketFrame(TICKET.add, await ticketHash(ticket), expires, uses));
	await handled(machine);
}

export async function removeTicket(machine: Machine, ticket: Uint8Array): Promise<void> {
	machine.socket.send(ticketFrame(TICKET.remove, await ticketHash(ticket)));
	await handled(machine);
}

export const hex = toHex;
