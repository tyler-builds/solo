// The relay's one Durable Object. It holds every socket and forwards frames between a machine and
// the devices and other machines connected to it. It cannot read a frame's payload, and what it sends
// a device is only its machine's bytes, a close code, or the IDs of the machines in the network, which
// a device trusts no more than anything else the relay says.
//
// The sockets are hibernating ones, so the object may be evicted while they stay open. Who a socket is
// lives with the socket (its tags and its attachment), and routing uses nothing else: the maps below
// are only counters that may start again from nothing.

import { DurableObject } from "cloudflare:workers";
import { concat, fromHex, toHex } from "./bytes";
import {
	CLOSE,
	CONN_SIZE,
	FRAME,
	HASH_SIZE,
	HEADER,
	ID_SIZE,
	KEY_SIZE,
	KIND,
	LIMIT,
	SUBPROTOCOL,
	TICKET,
	type Env,
	type Role,
} from "./protocol";

// Peer is what a socket's attachment says of it. The connection and the device are in hex.
interface Peer {
	role: Role;
	// The machine the socket is, or is for: the relay key that vouches for it and its ID, both in hex
	// (see owner). A machine is known by both, so a machine can't take another's place by claiming its
	// ID: under its own key that is another machine. For another machine's socket it is the machine
	// the socket is to, which its frames go to.
	machine: string;
	// The connection number the machine knows a device's, a pairing or another machine's socket by;
	// empty for a machine.
	conn: string;
	// The device's ID, or for another machine's socket that machine's ID.
	device: string;
	// When the socket was admitted, in milliseconds.
	at: number;
	// Set when the relay has finished with the socket, so that what still arrives on it, its close
	// included, is ignored.
	gone?: true;
}

type TicketRow = { machine: string; expires: number; uses: number };
type FailureRow = { since: number; count: number };

const TICKET_FRAME_SIZE = 1 + 1 + HASH_SIZE + 8 + 1;
const NO_DEVICE = "0".repeat(ID_SIZE * 2);

type Client = Exclude<Role, "machine">;

// Tags, which are how sockets are found: every machine's socket, one machine's socket, one machine's
// device, pairing or other machines' sockets, and the one socket of a connection. The connection's tag
// has the machine in it, and a machine is its key and its ID together, so a machine can only ever name
// its own.
const everyMachine = "machine";
const machineTag = (machine: string) => `m:${machine}`;
const clientsTag = (role: Client, machine: string) => `${CLIENT_TAG[role]}:${machine}`;
const CLIENT_TAG = { device: "d", pair: "p", peer: "q" } as const;
const connTag = (machine: string, conn: string) => `c:${machine}:${conn}`;

// How many sockets of each kind one machine may have at once.
const CLIENT_LIMIT = { device: LIMIT.devices, pair: LIMIT.pairs, peer: LIMIT.peers } as const;

export class Relay extends DurableObject<Env> {
	// The token bucket of each device's, pairing and other machine's socket's frames, by connection.
	private buckets = new Map<string, { tokens: number; at: number }>();
	// Frames forwarded since the count was last written.
	private pending = 0;

	constructor(ctx: DurableObjectState, env: Env) {
		super(ctx, env);
		// The keepalive is answered without waking the object.
		ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
		const sql = ctx.storage.sql;
		sql.exec(
			"CREATE TABLE IF NOT EXISTS tickets (hash TEXT PRIMARY KEY, machine TEXT NOT NULL, expires INTEGER NOT NULL, uses INTEGER NOT NULL)",
		);
		sql.exec("CREATE TABLE IF NOT EXISTS usage (day TEXT PRIMARY KEY, frames INTEGER NOT NULL)");
		sql.exec(
			"CREATE TABLE IF NOT EXISTS failures (machine TEXT PRIMARY KEY, since INTEGER NOT NULL, count INTEGER NOT NULL)",
		);
		// The machines that take the network's passphrase, and how many pairing sockets with it each
		// has been sent in the current window.
		sql.exec("CREATE TABLE IF NOT EXISTS network (machine TEXT PRIMARY KEY)");
		sql.exec(
			"CREATE TABLE IF NOT EXISTS passes (machine TEXT PRIMARY KEY, since INTEGER NOT NULL, count INTEGER NOT NULL)",
		);
	}

	// fetch admits a socket the Worker has verified. The request is the Worker's own (src/index.ts).
	override async fetch(request: Request): Promise<Response> {
		const role = request.headers.get(HEADER.role);
		if (role === "pair" && request.headers.get(HEADER.list) !== null) {
			return this.listNetwork();
		}
		if (role === "pair" && request.headers.get(HEADER.open) !== null) {
			// A device with no code, for the machine that has pairing open. With several, the relay
			// doesn't choose: which machine a device is paired with is the person's to say, by opening
			// pairing on that one only.
			const open = this.openOwners();
			if (open.length === 0) {
				return new Response(null, { status: 401 });
			}
			if (open.length > 1) {
				return refuse(CLOSE.ambiguous);
			}
			return this.admitClient("pair", open[0]!, NO_DEVICE, { ticket: openKey(open[0]!), kind: KIND.openPair });
		}
		const id = request.headers.get(HEADER.machine) ?? "";
		if (fromHex(id, ID_SIZE) === null) {
			return new Response(null, { status: 400 });
		}
		if (role === "pair" && request.headers.get(HEADER.pass) !== null) {
			return this.admitPass(id);
		}
		if (role === "pair") {
			// A pairing socket names its machine by ID alone; which key that machine connected under
			// is in the ticket it registered.
			const hash = request.headers.get(HEADER.ticket) ?? "";
			const machine = this.ticketOwner(id, hash);
			if (machine === null) {
				return new Response(null, { status: 401 });
			}
			return this.admitClient("pair", machine, NO_DEVICE, { ticket: hash });
		}
		const key = request.headers.get(HEADER.key) ?? "";
		if (fromHex(key, KEY_SIZE) === null) {
			return new Response(null, { status: 400 });
		}
		const machine = owner(key, id);
		switch (role) {
			case "machine":
				return this.admitMachine(machine);
			case "device": {
				const device = request.headers.get(HEADER.device) ?? "";
				if (fromHex(device, ID_SIZE) === null) {
					return new Response(null, { status: 400 });
				}
				return this.admitClient("device", machine, device);
			}
			case "peer": {
				const target = request.headers.get(HEADER.target) ?? "";
				// A machine has no use for a socket to itself, and its frames would come back to it
				// as another machine's.
				if (fromHex(target, ID_SIZE) === null || target === id) {
					return new Response(null, { status: 400 });
				}
				return this.admitPeer(key, id, target);
			}
		}
		return new Response(null, { status: 400 });
	}

	override webSocketMessage(ws: WebSocket, message: string | ArrayBuffer): void {
		const peer = peerOf(ws);
		if (peer === null) {
			return;
		}
		if (typeof message === "string") {
			// The only text is the keepalive, which never gets here.
			this.drop(ws, peer, CLOSE.protocol);
		} else if (peer.role === "machine") {
			this.fromMachine(ws, peer, new Uint8Array(message));
		} else {
			this.fromClient(ws, peer, new Uint8Array(message));
		}
	}

	// The other end closed, or the connection broke. Closing in return completes the close for a
	// client that waits for it; where the runtime has answered already, it does nothing.
	override webSocketClose(ws: WebSocket): void {
		const peer = peerOf(ws);
		if (peer !== null) {
			this.drop(ws, peer, CLOSE.normal);
		}
	}

	override webSocketError(ws: WebSocket): void {
		this.webSocketClose(ws);
	}

	private admitMachine(machine: string): Response {
		const old = this.sockets(machineTag(machine));
		if (old.length === 0 && this.sockets(everyMachine).length >= LIMIT.machines) {
			return refuse(CLOSE.limit);
		}
		// A machine that connects again replaces itself. Its devices go too: the new socket was never
		// told of their connections, and a device that connects again starts a new handshake.
		for (const ws of old) {
			this.drop(ws, peerOf(ws)!, CLOSE.replaced);
		}
		const { 0: client, 1: server } = new WebSocketPair();
		this.ctx.acceptWebSocket(server, [everyMachine, machineTag(machine)]);
		server.serializeAttachment({ role: "machine", machine, conn: "", device: "", at: Date.now() } satisfies Peer);
		send(server, this.usageFrame());
		return upgraded(client);
	}

	// admitPass admits a pairing socket that will prove the network's passphrase to the machine with
	// an ID, if that machine takes it. Which key the machine connected under is in its network row.
	private admitPass(id: string): Response {
		const rows = this.ctx.storage.sql
			.exec<{ machine: string }>("SELECT machine FROM network WHERE machine LIKE ?", `%:${id}`)
			.toArray()
			.map((row) => row.machine);
		if (rows.length === 0) {
			return new Response(null, { status: 401 });
		}
		// A machine that took it and is away is refused as offline, by admitClient.
		const machine = rows.find((m) => this.sockets(machineTag(m)).length > 0) ?? rows[0]!;
		return this.admitClient("pair", machine, NO_DEVICE, { kind: KIND.passPair });
	}

	// admitPeer admits one machine's socket to another, which it names by ID alone: it learns the
	// other's key from their handshake. The other is told the key this one connected under, so that
	// it can check the handshake is with the machine the relay let in.
	private admitPeer(key: string, id: string, target: string): Response {
		const to = this.sockets(everyMachine)
			.map((ws) => peerOf(ws)!.machine)
			.find((m) => idOf(m) === target);
		if (to === undefined) {
			return refuse(CLOSE.offline);
		}
		return this.admitClient("peer", to, id, { kind: KIND.peer, key: fromHex(key, KEY_SIZE)! });
	}

	// listNetwork answers a socket that asks which machines take the network's passphrase: their
	// IDs, 16 bytes each and in order, in one frame, and then it is closed. A machine that is away is
	// left out, as it could not answer.
	private listNetwork(): Response {
		const rows = this.ctx.storage.sql.exec<{ machine: string }>("SELECT machine FROM network").toArray();
		const connected = rows.filter((row) => this.sockets(machineTag(row.machine)).length > 0);
		const ids = [...new Set(connected.map((row) => idOf(row.machine)))].sort();
		const frame = concat(...ids.map((id) => fromHex(id, ID_SIZE)!));
		const { 0: client, 1: server } = new WebSocketPair();
		server.accept();
		server.send(frame);
		server.close(CLOSE.normal);
		return upgraded(client);
	}

	// admitClient admits a device's socket, a pairing one or another machine's, and tells the machine
	// of it. A ticket is spent, and a passphrase pairing counted, only here, when its socket is really
	// admitted. Only another machine's open frame carries a key, the one that machine connected under.
	private admitClient(
		role: Client,
		machine: string,
		device: string,
		{
			ticket,
			kind = role === "pair" ? KIND.pair : KIND.device,
			key = new Uint8Array(0),
		}: { ticket?: string; kind?: number; key?: Uint8Array } = {},
	): Response {
		const host = this.sockets(machineTag(machine))[0];
		if (host === undefined) {
			return refuse(CLOSE.offline);
		}
		const now = Date.now();
		const others = this.sockets(clientsTag(role, machine)).filter((ws) => !this.outlived(ws, peerOf(ws)!, now));
		if (others.length >= CLIENT_LIMIT[role]) {
			return refuse(CLOSE.limit);
		}
		if (kind === KIND.passPair && !this.countPass(machine, now)) {
			return refuse(CLOSE.limit);
		}
		if (ticket !== undefined) {
			this.ctx.storage.sql.exec("UPDATE tickets SET uses = uses - 1 WHERE hash = ?", ticket);
			this.ctx.storage.sql.exec("DELETE FROM tickets WHERE uses <= 0");
		}
		const conn = crypto.getRandomValues(new Uint8Array(CONN_SIZE));
		const { 0: client, 1: server } = new WebSocketPair();
		this.ctx.acceptWebSocket(server, [clientsTag(role, machine), connTag(machine, toHex(conn))]);
		server.serializeAttachment({ role, machine, conn: toHex(conn), device, at: now } satisfies Peer);
		send(host, concat(new Uint8Array([FRAME.open]), conn, new Uint8Array([kind]), fromHex(device, ID_SIZE)!, key));
		return upgraded(client);
	}

	// countPass counts a passphrase pairing let in to a machine, and says whether the window had room
	// for it. The count is in storage, so that the object being evicted doesn't start it again.
	private countPass(machine: string, now: number): boolean {
		const sql = this.ctx.storage.sql;
		const row = sql.exec<FailureRow>("SELECT since, count FROM passes WHERE machine = ?", machine).toArray()[0];
		const counting = row !== undefined && now - row.since < LIMIT.passWindowMs;
		if (counting && row.count >= LIMIT.passPairs) {
			return false;
		}
		sql.exec(
			"INSERT OR REPLACE INTO passes (machine, since, count) VALUES (?, ?, ?)",
			machine,
			counting ? row.since : now,
			counting ? row.count + 1 : 1,
		);
		return true;
	}

	// A frame from a device, a pairing socket or another machine is a payload for its machine.
	private fromClient(ws: WebSocket, peer: Peer, payload: Uint8Array): void {
		const now = Date.now();
		if (this.outlived(ws, peer, now)) {
			return;
		}
		if (payload.length > LIMIT.payload || !this.take(peer.conn, now)) {
			this.drop(ws, peer, CLOSE.limit);
			return;
		}
		const host = this.sockets(machineTag(peer.machine))[0];
		if (host === undefined) {
			this.drop(ws, peer, CLOSE.offline);
			return;
		}
		send(host, concat(new Uint8Array([FRAME.data]), fromHex(peer.conn, CONN_SIZE)!, payload));
		this.forwarded();
	}

	private fromMachine(ws: WebSocket, peer: Peer, frame: Uint8Array): void {
		const body = frame.subarray(1);
		switch (frame[0]) {
			case FRAME.data: {
				if (body.length < CONN_SIZE) {
					break;
				}
				const payload = body.subarray(CONN_SIZE);
				if (payload.length > LIMIT.payload) {
					this.drop(ws, peer, CLOSE.limit);
					return;
				}
				// A connection that is not there has just closed, and the machine is being told so.
				const to = this.sockets(connTag(peer.machine, toHex(body.subarray(0, CONN_SIZE))))[0];
				if (to !== undefined && !this.outlived(to, peerOf(to)!, Date.now())) {
					send(to, payload);
					this.forwarded();
				}
				return;
			}
			case FRAME.close: {
				if (body.length !== CONN_SIZE) {
					break;
				}
				const to = this.sockets(connTag(peer.machine, toHex(body)))[0];
				if (to !== undefined) {
					// The machine asked for this, so it is not told of it.
					this.retire(to, peerOf(to)!, CLOSE.normal);
				}
				return;
			}
			case FRAME.ticket: {
				if (frame.length !== TICKET_FRAME_SIZE) {
					break;
				}
				const hash = toHex(body.subarray(1, 1 + HASH_SIZE));
				const view = new DataView(body.buffer, body.byteOffset + 1 + HASH_SIZE);
				// The expiry is 64 bits; a time too large for a number is as good as never.
				const expires = Math.min(Number(view.getBigUint64(0)), Number.MAX_SAFE_INTEGER);
				if (body[0] === TICKET.add) {
					this.addTicket(peer.machine, hash, expires, view.getUint8(8));
					return;
				}
				if (body[0] === TICKET.remove) {
					this.ctx.storage.sql.exec("DELETE FROM tickets WHERE hash = ? AND machine = ?", hash, peer.machine);
					return;
				}
				// Pairing opened to devices that hold no code, and closed again: a row of the machine's
				// own, with the window's end and how many connections it lets in.
				if (body[0] === TICKET.open) {
					// For a few minutes and a few connections at most, whatever the frame asks.
					const until = Math.min(expires, Math.floor(Date.now() / 1000) + LIMIT.openPairSeconds);
					this.ctx.storage.sql.exec("DELETE FROM tickets WHERE hash = ?", openKey(peer.machine));
					this.addTicket(peer.machine, openKey(peer.machine), until, Math.min(view.getUint8(8), LIMIT.openPairUses));
					return;
				}
				if (body[0] === TICKET.close) {
					// The window that ends at the frame's time, so that a late close for an earlier
					// window doesn't shut a newer one; a time of zero closes whatever is open.
					if (expires === 0) {
						this.ctx.storage.sql.exec("DELETE FROM tickets WHERE hash = ?", openKey(peer.machine));
					} else {
						this.ctx.storage.sql.exec("DELETE FROM tickets WHERE hash = ? AND expires = ?", openKey(peer.machine), expires);
					}
					return;
				}
				// Taking the network's passphrase, and no longer. The row stays while the machine is
				// away, but only a machine that is connected is listed or reached.
				if (body[0] === TICKET.networkOn) {
					this.ctx.storage.sql.exec("INSERT OR IGNORE INTO network (machine) VALUES (?)", peer.machine);
					return;
				}
				if (body[0] === TICKET.networkOff) {
					this.ctx.storage.sql.exec("DELETE FROM network WHERE machine = ?", peer.machine);
					return;
				}
				break;
			}
		}
		this.drop(ws, peer, CLOSE.protocol);
	}

	private addTicket(machine: string, hash: string, expires: number, uses: number): void {
		const sql = this.ctx.storage.sql;
		const now = Date.now();
		// Adding a ticket is the one time old rows are cleared, so the tables stay as small as the
		// number of pairings under way.
		sql.exec("DELETE FROM tickets WHERE expires <= ?", Math.floor(now / 1000));
		sql.exec("DELETE FROM failures WHERE since <= ?", now - LIMIT.failureWindowMs);
		sql.exec("DELETE FROM passes WHERE since <= ?", now - LIMIT.passWindowMs);
		// A machine replaces only its own ticket.
		sql.exec(
			`INSERT INTO tickets (hash, machine, expires, uses) VALUES (?, ?, ?, ?)
			 ON CONFLICT (hash) DO UPDATE SET expires = excluded.expires, uses = excluded.uses
			 WHERE tickets.machine = excluded.machine`,
			hash,
			machine,
			expires,
			uses,
		);
	}

	// ticketOwner returns the machine a pairing socket's ticket admits it to, or null: the machine
	// with that ID registered the ticket's hash, it has not expired and it has uses left. A right
	// ticket is always admitted. A wrong one is counted, and after too many for one machine the wrong
	// ones are no longer written down until the window has passed: the count is there to ration the
	// rows written, not to guard the ticket, which is 256 bits and is not going to be guessed.
	private ticketOwner(id: string, hash: string): string | null {
		const sql = this.ctx.storage.sql;
		const now = Date.now();
		const ticket = sql
			.exec<TicketRow>("SELECT machine, expires, uses FROM tickets WHERE hash = ?", hash)
			.toArray()[0];
		if (ticket !== undefined && idOf(ticket.machine) === id && ticket.expires > now / 1000 && ticket.uses > 0) {
			return ticket.machine;
		}
		const failed = sql.exec<FailureRow>("SELECT since, count FROM failures WHERE machine = ?", id).toArray()[0];
		const counting = failed !== undefined && now - failed.since < LIMIT.failureWindowMs;
		if (counting && failed.count >= LIMIT.failures) {
			return null;
		}
		// Only a machine that has a ticket out has anything to count: for any other, every ticket is
		// wrong, and nothing is kept.
		if (sql.exec("SELECT 1 FROM tickets WHERE machine LIKE ? LIMIT 1", `%:${id}`).toArray().length > 0) {
			sql.exec(
				"INSERT OR REPLACE INTO failures (machine, since, count) VALUES (?, ?, ?)",
				id,
				counting ? failed.since : now,
				counting ? failed.count + 1 : 1,
			);
		}
		return null;
	}

	// openOwners returns the connected machines that have pairing open to devices with no code.
	private openOwners(): string[] {
		const rows = this.ctx.storage.sql
			.exec<TicketRow & { hash: string }>(
				"SELECT hash, machine, expires, uses FROM tickets WHERE hash LIKE 'open:%' AND expires > ? AND uses > 0",
				Date.now() / 1000,
			)
			.toArray();
		return rows
			.filter((row) => row.hash === openKey(row.machine) && this.sockets(machineTag(row.machine)).length > 0)
			.map((row) => row.machine);
	}

	// sockets returns the sockets with a tag that the relay has not finished with.
	private sockets(tag: string): WebSocket[] {
		return this.ctx.getWebSockets(tag).filter((ws) => peerOf(ws) !== null);
	}

	// outlived closes a pairing socket that has been open longer than pairing may take, and says
	// whether it did. Nothing watches the clock for this: it is looked at when the socket comes up.
	private outlived(ws: WebSocket, peer: Peer, now: number): boolean {
		if (peer.role !== "pair" || now - peer.at <= LIMIT.pairLifetimeMs) {
			return false;
		}
		this.drop(ws, peer, CLOSE.limit);
		return true;
	}

	// drop closes a socket and tells whoever is at the other side of the relay: a machine's devices,
	// pairing sockets and other machines' sockets to it are closed as "machine offline", and a device's
	// machine gets a close frame. A machine's own sockets to other machines are connections of their
	// own, and stay.
	private drop(ws: WebSocket, peer: Peer, code: number): void {
		this.retire(ws, peer, code);
		if (peer.role === "machine") {
			for (const role of ["device", "pair", "peer"] as const) {
				for (const client of this.sockets(clientsTag(role, peer.machine))) {
					this.retire(client, peerOf(client)!, CLOSE.offline);
				}
			}
			this.flush();
			return;
		}
		const host = this.sockets(machineTag(peer.machine))[0];
		if (host !== undefined) {
			send(host, concat(new Uint8Array([FRAME.close]), fromHex(peer.conn, CONN_SIZE)!));
		}
	}

	// retire marks a socket as finished with, and closes it.
	private retire(ws: WebSocket, peer: Peer, code: number): void {
		this.buckets.delete(peer.conn);
		try {
			ws.serializeAttachment({ ...peer, gone: true } satisfies Peer);
			ws.close(code);
		} catch {
			// The socket has gone already, which is what was wanted.
		}
	}

	// take spends one token of a connection's bucket, and says whether there was one.
	private take(conn: string, now: number): boolean {
		const bucket = this.buckets.get(conn) ?? { tokens: LIMIT.burst, at: now };
		bucket.tokens = Math.min(LIMIT.burst, bucket.tokens + ((now - bucket.at) * LIMIT.perSecond) / 1000);
		bucket.at = now;
		this.buckets.set(conn, bucket);
		if (bucket.tokens < 1) {
			return false;
		}
		bucket.tokens--;
		return true;
	}

	// forwarded counts a frame. The count is written, and the machines told, once in so many frames:
	// a row written for every frame would use up the plan's writes.
	private forwarded(): void {
		if (++this.pending < LIMIT.flushEvery) {
			return;
		}
		this.flush();
		const frame = this.usageFrame();
		for (const machine of this.sockets(everyMachine)) {
			send(machine, frame);
		}
	}

	private flush(): void {
		if (this.pending === 0) {
			return;
		}
		this.ctx.storage.sql.exec(
			"INSERT INTO usage (day, frames) VALUES (?, ?) ON CONFLICT (day) DO UPDATE SET frames = frames + excluded.frames",
			today(),
			this.pending,
		);
		this.pending = 0;
	}

	private usageFrame(): Uint8Array {
		const day = today();
		const row = this.ctx.storage.sql.exec<{ frames: number }>("SELECT frames FROM usage WHERE day = ?", day).toArray()[0];
		const usage = JSON.stringify({ day, frames: (row?.frames ?? 0) + this.pending });
		return concat(new Uint8Array([FRAME.usage]), new TextEncoder().encode(usage));
	}
}

// owner names a machine by the relay key that vouches for it and its ID. idOf takes the ID back out.
function owner(key: string, id: string): string {
	return `${key}:${id}`;
}

// openKey is where the tickets table keeps a machine's open pairing: no hash looks like it.
function openKey(machine: string): string {
	return `open:${machine}`;
}

function idOf(machine: string): string {
	return machine.slice(machine.indexOf(":") + 1);
}

// peerOf reads who a socket is, or null when the relay has finished with it.
function peerOf(ws: WebSocket): Peer | null {
	const peer = ws.deserializeAttachment() as Peer | null;
	return peer === null || peer.gone ? null : peer;
}

// send does not fail for a socket that is closing: its close is on the way, and is handled then.
function send(ws: WebSocket, frame: Uint8Array): void {
	try {
		ws.send(frame);
	} catch {
		// See above.
	}
}

function upgraded(client: WebSocket): Response {
	return new Response(null, { status: 101, webSocket: client, headers: { "Sec-WebSocket-Protocol": SUBPROTOCOL } });
}

// refuse accepts a socket only to close it with a code, which is the one way to tell a browser why:
// it shows a page nothing of a refused upgrade.
function refuse(code: number): Response {
	const { 0: client, 1: server } = new WebSocketPair();
	server.accept();
	server.close(code);
	return upgraded(client);
}

// The day the quota counts by, which turns over at midnight UTC.
function today(): string {
	return new Date().toISOString().slice(0, 10);
}
