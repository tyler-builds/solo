// Pairing sockets, which a ticket the machine registered admits.

import { runInDurableObject } from "cloudflare:test";
import { describe, expect, test } from "vitest";
import { ticketHash } from "../src/auth";
import { CLOSE, KIND, LIMIT } from "../src/protocol";
import {
	addTicket,
	closeFrame,
	connectDevice,
	connectMachine,
	connectPair,
	dataFrame,
	hex,
	opened,
	pairToken,
	random,
	relay,
	removeTicket,
	Socket,
	upgrade,
} from "./helpers";

const text = (s: string) => new TextEncoder().encode(s);

// refused says whether the relay answers a pairing socket with 401.
async function refused(machineId: Uint8Array, ticket: Uint8Array): Promise<boolean> {
	const response = await upgrade("/v1/pair", pairToken(machineId, ticket));
	if (response.status === 101) {
		new Socket(response.webSocket!);
	}
	return response.status === 401;
}

// ticketRow reads what the relay has stored of a ticket.
async function ticketRow(ticket: Uint8Array): Promise<{ machine: string; uses: number } | undefined> {
	const hash = hex(await ticketHash(ticket));
	return runInDurableObject(relay(), (_, state) =>
		state.storage.sql.exec<{ machine: string; uses: number }>("SELECT machine, uses FROM tickets WHERE hash = ?", hash).toArray()[0],
	);
}

describe("pairing", () => {
	test("a ticket admits one socket, which reaches the machine", async () => {
		const machine = await connectMachine();
		const ticket = random(32);
		await addTicket(machine, ticket);
		expect(await ticketRow(ticket)).toEqual({ machine: `${hex(machine.signer.public)}:${hex(machine.id)}`, uses: 1 });

		const pair = await connectPair(machine.id, ticket);
		const open = await opened(machine);
		expect(open.kind).toBe(KIND.pair);
		expect(hex(open.deviceId)).toBe("00".repeat(16));
		pair.send(text("hello"));
		expect(await machine.socket.frame()).toEqual(dataFrame(open.conn, text("hello")));
		machine.socket.send(dataFrame(open.conn, text("welcome")));
		expect(await pair.frame()).toEqual(text("welcome"));

		// Its one use is spent, and nothing is kept of it.
		expect(await ticketRow(ticket)).toBeUndefined();
		expect(await refused(machine.id, ticket)).toBe(true);
		pair.close();
		expect(await machine.socket.frame()).toEqual(closeFrame(open.conn));
	});

	test("a ticket's uses are counted down", async () => {
		const machine = await connectMachine();
		const ticket = random(32);
		await addTicket(machine, ticket, 2);
		await connectPair(machine.id, ticket);
		expect(await ticketRow(ticket)).toMatchObject({ uses: 1 });
		await connectPair(machine.id, ticket);
		expect(await ticketRow(ticket)).toBeUndefined();
		expect(await refused(machine.id, ticket)).toBe(true);
	});

	test("adding a ticket again replaces it", async () => {
		const machine = await connectMachine();
		const ticket = random(32);
		await addTicket(machine, ticket, 1, Date.now() / 1000 - 5);
		expect(await refused(machine.id, ticket)).toBe(true);
		await addTicket(machine, ticket, 2);
		expect(await ticketRow(ticket)).toMatchObject({ uses: 2 });
		await connectPair(machine.id, ticket);
	});

	test("a wrong ticket is refused", async () => {
		const machine = await connectMachine();
		await addTicket(machine, random(32));
		expect(await refused(machine.id, random(32))).toBe(true);
	});

	test("an expired ticket is refused, and cleared when the next is added", async () => {
		const machine = await connectMachine();
		const ticket = random(32);
		await addTicket(machine, ticket, 1, Date.now() / 1000 - 1);
		expect(await refused(machine.id, ticket)).toBe(true);
		expect(await ticketRow(ticket)).toBeDefined();
		await addTicket(machine, random(32));
		expect(await ticketRow(ticket)).toBeUndefined();
	});

	test("a removed ticket is refused", async () => {
		const machine = await connectMachine();
		const other = await connectMachine();
		const ticket = random(32);
		await addTicket(machine, ticket);
		// Only the machine that added a ticket removes it.
		await removeTicket(other, ticket);
		expect(await ticketRow(ticket)).toBeDefined();
		await removeTicket(machine, ticket);
		expect(await ticketRow(ticket)).toBeUndefined();
		expect(await refused(machine.id, ticket)).toBe(true);
	});

	test("a ticket for another machine is refused, and cannot be taken over", async () => {
		const machine = await connectMachine();
		const other = await connectMachine();
		const ticket = random(32);
		await addTicket(other, ticket);
		expect(await refused(machine.id, ticket)).toBe(true);
		await addTicket(machine, ticket, 5);
		expect(await ticketRow(ticket)).toEqual({ machine: `${hex(other.signer.public)}:${hex(other.id)}`, uses: 1 });
		expect(await refused(machine.id, ticket)).toBe(true);
		await connectPair(other.id, ticket);
	});

	test("a ticket is not spent while its machine is away", async () => {
		const machine = await connectMachine();
		const ticket = random(32);
		await addTicket(machine, ticket);
		machine.socket.close();
		await machine.socket.closed;

		const response = await upgrade("/v1/pair", pairToken(machine.id, ticket));
		expect(response.status).toBe(101);
		expect(await new Socket(response.webSocket!).closed).toBe(CLOSE.offline);
		expect(await ticketRow(ticket)).toMatchObject({ uses: 1 });

		const back = await connectMachine(machine.id);
		await connectPair(machine.id, ticket);
		expect((await opened(back)).kind).toBe(KIND.pair);
	});

	test("after too many wrong tickets the wrong ones stop being counted, and the right one still works", async () => {
		const machine = await connectMachine();
		const bystander = await connectMachine();
		const ticket = random(32);
		const other = random(32);
		await addTicket(machine, ticket, 3);
		await addTicket(bystander, other);

		for (let i = 0; i < LIMIT.failures - 1; i++) {
			expect(await refused(machine.id, random(32))).toBe(true);
		}
		// One short of the limit, the right ticket still works.
		await connectPair(machine.id, ticket);
		expect(await refused(machine.id, random(32))).toBe(true);
		// At the limit wrong tickets are still refused, and the right one is still let in: someone who
		// only knows the machine's ID can't keep its owner from pairing.
		expect(await refused(machine.id, random(32))).toBe(true);
		await connectPair(machine.id, ticket);
		expect(await ticketRow(ticket)).toMatchObject({ uses: 1 });
		// Another machine's pairing is not affected.
		await connectPair(bystander.id, other);

		const failures = () =>
			runInDurableObject(relay(), (_, state) =>
				state.storage.sql.exec<{ count: number }>("SELECT count FROM failures WHERE machine = ?", hex(machine.id)).one().count,
			);
		// Refusals past the limit are not written.
		expect(await failures()).toBe(LIMIT.failures);

		// Ten minutes later.
		await runInDurableObject(relay(), (_, state) => {
			state.storage.sql.exec("UPDATE failures SET since = since - ? WHERE machine = ?", LIMIT.failureWindowMs, hex(machine.id));
		});
		await connectPair(machine.id, ticket);
		expect(await refused(machine.id, random(32))).toBe(true);
		expect(await failures()).toBe(1);
	});

	test("wrong tickets for a machine with none out are not counted", async () => {
		const machine = await connectMachine();
		for (let i = 0; i < 3; i++) {
			expect(await refused(machine.id, random(32))).toBe(true);
			expect(await refused(random(16), random(32))).toBe(true);
		}
		const rows = await runInDurableObject(relay(), (_, state) =>
			state.storage.sql.exec("SELECT 1 FROM failures WHERE machine = ?", hex(machine.id)).toArray().length,
		);
		expect(rows).toBe(0);
	});

	test("a pairing socket is closed when it has been open too long", async () => {
		const machine = await connectMachine();
		const ticket = random(32);
		await addTicket(machine, ticket, 4);
		const first = await connectPair(machine.id, ticket);
		const one = await opened(machine);
		const second = await connectPair(machine.id, ticket);
		const two = await opened(machine);
		const device = await connectDevice(machine.id);
		const three = await opened(machine);

		// Ten minutes later, for every socket: only pairing ones have a lifetime.
		await runInDurableObject(relay(), (_, state) => {
			for (const ws of state.getWebSockets()) {
				const peer = ws.deserializeAttachment();
				ws.serializeAttachment({ ...peer, at: peer.at - LIMIT.pairLifetimeMs - 1 });
			}
		});
		// Its own frame closes the first, and is not forwarded.
		first.send(text("late"));
		expect(await first.closed).toBe(CLOSE.limit);
		expect(await machine.socket.frame()).toEqual(closeFrame(one.conn));
		// A frame from the machine closes the second, and is not delivered.
		machine.socket.send(dataFrame(two.conn, text("late")));
		expect(await second.closed).toBe(CLOSE.limit);
		expect(await machine.socket.frame()).toEqual(closeFrame(two.conn));
		// The device and the machine carry on.
		device.send(text("still here"));
		expect(await machine.socket.frame()).toEqual(dataFrame(three.conn, text("still here")));
	});

	test("a pairing socket that has been open too long does not hold a place", async () => {
		const machine = await connectMachine();
		const ticket = random(32);
		await addTicket(machine, ticket, 3);
		const stale = [await connectPair(machine.id, ticket), await connectPair(machine.id, ticket)];
		await runInDurableObject(relay(), (_, state) => {
			for (const ws of state.getWebSockets(`p:${hex(machine.signer.public)}:${hex(machine.id)}`)) {
				const peer = ws.deserializeAttachment();
				ws.serializeAttachment({ ...peer, at: peer.at - LIMIT.pairLifetimeMs - 1 });
			}
		});
		await connectPair(machine.id, ticket);
		for (const socket of stale) {
			expect(await socket.closed).toBe(CLOSE.limit);
		}
	});
});
