// What the relay closes a socket for.

import { describe, expect, test } from "vitest";
import { CLOSE, FRAME, LIMIT, TICKET } from "../src/protocol";
import {
	addTicket,
	closeFrame,
	connectDevice,
	connectMachine,
	connectPair,
	dataFrame,
	hex,
	machineAuth,
	opened,
	pairToken,
	random,
	Socket,
	upgrade,
	voucher,
} from "./helpers";

// closedWith opens a socket the relay accepts only to close, and returns the code.
async function closedWith(path: string, token: Uint8Array): Promise<number> {
	const response = await upgrade(path, token);
	expect(response.status).toBe(101);
	return new Socket(response.webSocket!).closed;
}

describe("frame size", () => {
	test("a device's frame may be 65536 bytes, and no more", async () => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		const open = await opened(machine);
		const largest = random(LIMIT.payload);
		device.send(largest);
		expect(await machine.socket.frame()).toEqual(dataFrame(open.conn, largest));

		device.send(new Uint8Array(LIMIT.payload + 1));
		expect(await device.closed).toBe(CLOSE.limit);
		expect(await machine.socket.frame()).toEqual(closeFrame(open.conn));
	});

	test("a machine's payload may be 65536 bytes, and no more", async () => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		const open = await opened(machine);
		const largest = random(LIMIT.payload);
		machine.socket.send(dataFrame(open.conn, largest));
		expect(await device.frame()).toEqual(largest);

		machine.socket.send(dataFrame(open.conn, new Uint8Array(LIMIT.payload + 1)));
		expect(await machine.socket.closed).toBe(CLOSE.limit);
		expect(await device.closed).toBe(CLOSE.offline);
	});
});

describe("how many sockets", () => {
	test("a machine has at most 16 devices", async () => {
		const machine = await connectMachine();
		const other = await connectMachine();
		const devices: Socket[] = [];
		for (let i = 0; i < LIMIT.devices; i++) {
			devices.push(await connectDevice(machine.id));
			await opened(machine);
		}
		expect(await closedWith("/v1/device", await voucher(machine.id, random(16)))).toBe(CLOSE.limit);
		// The machine is told nothing of a device that was not admitted.
		expect(await machine.socket.idle()).toBe(true);
		// The limit is each machine's own.
		await connectDevice(other.id);

		// A place is free again when a device goes.
		devices[0]!.close();
		expect((await machine.socket.frame())[0]).toBe(FRAME.close);
		await connectDevice(machine.id);
		expect((await machine.socket.frame())[0]).toBe(FRAME.open);
	});

	test("a machine has at most 2 pairing sockets, and one over the limit does not spend the ticket", async () => {
		const machine = await connectMachine();
		const ticket = random(32);
		await addTicket(machine, ticket, 3);
		const first = await connectPair(machine.id, ticket);
		await connectPair(machine.id, ticket);
		expect(await closedWith("/v1/pair", pairToken(machine.id, ticket))).toBe(CLOSE.limit);
		// Pairing sockets and devices are counted apart.
		await connectDevice(machine.id);

		first.close();
		await first.closed;
		await connectPair(machine.id, ticket);
	});

	test("the relay has at most 8 machines", async () => {
		const machines = [];
		for (let i = 0; i < LIMIT.machines; i++) {
			machines.push(await connectMachine());
		}
		expect(await closedWith("/v1/machine", await machineAuth(random(16)))).toBe(CLOSE.limit);
		// A machine that is already connected may still replace itself.
		const again = await connectMachine(machines[0]!.id);
		expect(await machines[0]!.socket.closed).toBe(CLOSE.replaced);
		// And a place is free again when one goes.
		again.socket.close();
		await again.socket.closed;
		await connectMachine();
	});
});

describe("how many frames", () => {
	test("a device that sends faster than its bucket fills is closed", async () => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		const quiet = await connectDevice(machine.id);
		const open = await opened(machine);
		const other = await opened(machine);

		const sent = 2 * LIMIT.burst;
		for (let i = 0; i < sent; i++) {
			device.send(new Uint8Array([i & 0xff]));
		}
		expect(await device.closed).toBe(CLOSE.limit);

		// The machine got the burst, in order, and then the close.
		let forwarded = 0;
		for (;;) {
			const frame = await machine.socket.frame();
			if (frame[0] === FRAME.close) {
				expect(frame).toEqual(closeFrame(open.conn));
				break;
			}
			expect(frame).toEqual(dataFrame(open.conn, new Uint8Array([forwarded & 0xff])));
			forwarded++;
		}
		expect(forwarded).toBeGreaterThanOrEqual(LIMIT.burst);
		expect(forwarded).toBeLessThan(sent);

		// Each socket has its own bucket.
		quiet.send(new Uint8Array([1]));
		expect(await machine.socket.frame()).toEqual(dataFrame(other.conn, new Uint8Array([1])));
	});

	test("a machine's frames are not limited", async () => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		const open = await opened(machine);
		for (let i = 0; i < 3 * LIMIT.burst; i++) {
			machine.socket.send(dataFrame(open.conn, new Uint8Array([i & 0xff])));
		}
		for (let i = 0; i < 3 * LIMIT.burst; i++) {
			expect((await device.frame())[0]).toBe(i & 0xff);
		}
	});
});

describe("protocol errors", () => {
	test("a text frame other than ping closes a device", async () => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		const open = await opened(machine);
		device.send("hello");
		expect(await device.closed).toBe(CLOSE.protocol);
		expect(await machine.socket.frame()).toEqual(closeFrame(open.conn));
	});

	test("a text frame other than ping closes a machine", async () => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		machine.socket.send("pong");
		expect(await machine.socket.closed).toBe(CLOSE.protocol);
		expect(await device.closed).toBe(CLOSE.offline);
	});

	const conn = new Uint8Array(8);
	const ticket = [FRAME.ticket, TICKET.add, ...new Uint8Array(32 + 8), 1];
	const malformed: Record<string, number[]> = {
		"an unknown type": [9, ...conn],
		"type zero": [0, ...conn],
		"an empty frame": [],
		"an open frame, which only the relay sends": [FRAME.open, ...conn, 1, ...new Uint8Array(16)],
		"a usage frame, which only the relay sends": [FRAME.usage, 0x7b, 0x7d],
		"a data frame with half a connection": [FRAME.data, 1, 2, 3, 4],
		"a close frame with half a connection": [FRAME.close, 1, 2, 3, 4],
		"a close frame with more than a connection": [FRAME.close, ...conn, 0],
		"a ticket frame that is short": ticket.slice(0, -1),
		"a ticket frame that is long": [...ticket, 0],
		"a ticket frame with an unknown operation": [FRAME.ticket, 9, ...ticket.slice(2)],
	};
	test.each(Object.entries(malformed))("%s closes a machine", async (_, bytes) => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		machine.socket.send(new Uint8Array(bytes));
		expect(await machine.socket.closed).toBe(CLOSE.protocol);
		expect(await device.closed).toBe(CLOSE.offline);
		// The machine may connect again.
		const again = await connectMachine(machine.id);
		expect(hex(again.id)).toBe(hex(machine.id));
	});
});
