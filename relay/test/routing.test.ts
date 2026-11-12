// Frames between a machine and its devices, and what happens when either goes.

import { evictDurableObject, runInDurableObject } from "cloudflare:test";
import { describe, expect, test } from "vitest";
import { CLOSE, FRAME, KIND, LIMIT } from "../src/protocol";
import {
	accepted,
	addTicket,
	closeFrame,
	connectDevice,
	connectMachine,
	connectPair,
	dataFrame,
	hex,
	opened,
	otherListed,
	random,
	relay,
	Socket,
	upgrade,
	voucher,
} from "./helpers";

const text = (s: string) => new TextEncoder().encode(s);

describe("routing", () => {
	test("a device and its machine exchange frames", async () => {
		const machine = await connectMachine();
		const deviceId = random(16);
		const device = await connectDevice(machine.id, deviceId);

		const open = await opened(machine);
		expect(open.kind).toBe(KIND.device);
		expect(hex(open.deviceId)).toBe(hex(deviceId));

		// To the machine: the device's payload, with its connection in front.
		device.send(text("from the device"));
		expect(await machine.socket.frame()).toEqual(dataFrame(open.conn, text("from the device")));
		// To the device: the payload alone.
		machine.socket.send(dataFrame(open.conn, text("from the machine")));
		expect(await device.frame()).toEqual(text("from the machine"));
		// An empty payload is a payload.
		device.send(new Uint8Array(0));
		expect(await machine.socket.frame()).toEqual(dataFrame(open.conn, new Uint8Array(0)));
		machine.socket.send(dataFrame(open.conn, new Uint8Array(0)));
		expect(await device.frame()).toEqual(new Uint8Array(0));
	});

	test("two devices are kept apart", async () => {
		const machine = await connectMachine();
		const first = await connectDevice(machine.id);
		const one = await opened(machine);
		const second = await connectDevice(machine.id);
		const two = await opened(machine);
		expect(hex(one.conn)).not.toBe(hex(two.conn));

		second.send(text("b"));
		first.send(text("a"));
		expect(await machine.socket.frame()).toEqual(dataFrame(two.conn, text("b")));
		expect(await machine.socket.frame()).toEqual(dataFrame(one.conn, text("a")));

		machine.socket.send(dataFrame(two.conn, text("for the second")));
		machine.socket.send(dataFrame(one.conn, text("for the first")));
		expect(await first.frame()).toEqual(text("for the first"));
		expect(await second.frame()).toEqual(text("for the second"));
		expect(await first.idle()).toBe(true);
		expect(await second.idle()).toBe(true);
	});

	test("two machines are kept apart", async () => {
		const a = await connectMachine();
		const b = await connectMachine();
		const deviceA = await connectDevice(a.id);
		const openA = await opened(a);
		const deviceB = await connectDevice(b.id);
		const openB = await opened(b);

		// A machine that names another machine's connection reaches nobody.
		b.socket.send(dataFrame(openA.conn, text("not yours")));
		b.socket.send(closeFrame(openA.conn));
		expect(await deviceA.idle()).toBe(true);

		deviceA.send(text("a"));
		deviceB.send(text("b"));
		expect(await a.socket.frame()).toEqual(dataFrame(openA.conn, text("a")));
		expect(await b.socket.frame()).toEqual(dataFrame(openB.conn, text("b")));
		expect(await a.socket.idle()).toBe(true);
		expect(await b.socket.idle()).toBe(true);
	});

	test("a device that closes is reported to the machine", async () => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		const open = await opened(machine);
		device.close();
		expect(await machine.socket.frame()).toEqual(closeFrame(open.conn));
		expect(await device.closed).toBe(1000);
		// Frames for a connection that has gone go nowhere, and are not an error.
		machine.socket.send(dataFrame(open.conn, text("too late")));
		machine.socket.send(closeFrame(open.conn));
		expect(await machine.socket.idle()).toBe(true);
	});

	test("a close frame from the machine closes the device, and is not echoed", async () => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		const open = await opened(machine);
		machine.socket.send(closeFrame(open.conn));
		expect(await device.closed).toBe(CLOSE.normal);
		expect(await machine.socket.idle()).toBe(true);
	});

	test("everything still works after the object has been evicted", async () => {
		const machine = await connectMachine();
		const first = await connectDevice(machine.id);
		const one = await opened(machine);
		await evictDurableObject(relay());

		first.send(text("after"));
		expect(await machine.socket.frame()).toEqual(dataFrame(one.conn, text("after")));
		machine.socket.send(dataFrame(one.conn, text("back")));
		expect(await first.frame()).toEqual(text("back"));

		await evictDurableObject(relay());
		const second = await connectDevice(machine.id);
		const two = await opened(machine);
		await evictDurableObject(relay());
		machine.socket.send(dataFrame(two.conn, text("to the second")));
		expect(await second.frame()).toEqual(text("to the second"));
		expect(await first.idle()).toBe(true);

		await evictDurableObject(relay());
		first.close();
		expect(await machine.socket.frame()).toEqual(closeFrame(one.conn));
		await evictDurableObject(relay());
		machine.socket.close();
		expect(await second.closed).toBe(CLOSE.offline);
	});
});

describe("a machine that is not there", () => {
	test("a device with a good voucher is closed as offline", async () => {
		const response = await upgrade("/v1/device", await voucher(random(16), random(16)));
		expect(response.status).toBe(101);
		const device = new Socket(response.webSocket!);
		expect(await device.closed).toBe(CLOSE.offline);
	});

	test("its devices are closed as offline when it goes", async () => {
		const machine = await connectMachine();
		const bystander = await connectMachine();
		const first = await connectDevice(machine.id);
		const second = await connectDevice(machine.id);
		const other = await connectDevice(bystander.id);
		machine.socket.close();
		expect(await first.closed).toBe(CLOSE.offline);
		expect(await second.closed).toBe(CLOSE.offline);
		expect(await other.idle()).toBe(true);
	});
});

describe("a machine is its key and its ID", () => {
	test("another key that claims a machine's ID is another machine: it takes nothing over", async () => {
		const real = await connectMachine();
		const device = await connectDevice(real.id);
		const open = await opened(real);

		// Another of the user's machines, or an old one that was sold, claims the same ID.
		const impostor = await connectMachine(real.id, otherListed);
		expect(await real.socket.idle()).toBe(true); // not replaced, and told nothing
		device.send(text("for the real one"));
		expect(await real.socket.frame()).toEqual(dataFrame(open.conn, text("for the real one")));
		expect(await impostor.socket.idle()).toBe(true);

		// A device with the real machine's voucher never reaches the impostor, and a voucher the
		// impostor's key signed never reaches the real machine.
		const theirs = await accepted(await upgrade("/v1/device", await voucher(real.id, random(16), { signer: otherListed })));
		expect((await opened(impostor)).kind).toBe(KIND.device);
		expect(await real.socket.idle()).toBe(true);
		theirs.send(text("for the impostor"));
		expect((await impostor.socket.frame())[0]).toBe(FRAME.data);
		expect(await real.socket.idle()).toBe(true);

		// Nor can it close the real machine's connections by naming them.
		impostor.socket.send(closeFrame(open.conn));
		impostor.socket.send(dataFrame(open.conn, text("forged")));
		expect(await device.idle()).toBe(true);
	});

	test("a pairing socket reaches the machine that registered the ticket, not one that shares its ID", async () => {
		const real = await connectMachine();
		const impostor = await connectMachine(real.id, otherListed);
		const ticket = random(32);
		await addTicket(real, ticket);
		await connectPair(real.id, ticket);
		expect((await opened(real)).kind).toBe(KIND.pair);
		expect(await impostor.socket.idle()).toBe(true);
	});
});

describe("replacement", () => {
	test("a machine that connects again takes over from itself", async () => {
		const first = await connectMachine();
		const device = await connectDevice(first.id);
		await opened(first);

		const second = await connectMachine(first.id);
		expect(await first.socket.closed).toBe(CLOSE.replaced);
		// The new socket was never told of the old one's devices, so they start again.
		expect(await device.closed).toBe(CLOSE.offline);
		expect(await second.socket.idle()).toBe(true);

		const deviceId = random(16);
		const again = await connectDevice(first.id, deviceId);
		const open = await opened(second);
		expect(hex(open.deviceId)).toBe(hex(deviceId));
		again.send(text("to the new socket"));
		expect(await second.socket.frame()).toEqual(dataFrame(open.conn, text("to the new socket")));
		second.socket.send(dataFrame(open.conn, text("from the new socket")));
		expect(await again.frame()).toEqual(text("from the new socket"));
	});
});

describe("keepalive", () => {
	test("ping is answered with pong, on every kind of socket", async () => {
		const machine = await connectMachine();
		const device = await connectDevice(machine.id);
		await opened(machine);
		for (const socket of [machine.socket, device]) {
			socket.send("ping");
			expect(await socket.next()).toBe("pong");
		}
		// The keepalive is not forwarded.
		expect(await machine.socket.idle()).toBe(true);
	});
});

describe("usage", () => {
	test("the machine is told the day's count when it connects and every so many frames", async () => {
		const machine = await connectMachine();
		expect(machine.usage.day).toBe(new Date().toISOString().slice(0, 10));
		const device = await connectDevice(machine.id);
		const open = await opened(machine);

		// Frames from a machine are not limited, so they are the quick way to a thousand.
		for (let i = 0; i < LIMIT.flushEvery; i++) {
			machine.socket.send(dataFrame(open.conn, new Uint8Array([i & 0xff])));
		}
		const frame = await machine.socket.frame();
		expect(frame[0]).toBe(FRAME.usage);
		const usage = JSON.parse(new TextDecoder().decode(frame.subarray(1)));
		expect(usage).toEqual({ day: machine.usage.day, frames: machine.usage.frames + LIMIT.flushEvery });
		for (let i = 0; i < LIMIT.flushEvery; i++) {
			expect((await device.frame())[0]).toBe(i & 0xff);
		}

		// The count that has not been written is written when the machine goes.
		device.send(text("one more"));
		await machine.socket.frame();
		machine.socket.close();
		await device.closed;
		const stored = await runInDurableObject(relay(), (_, state) =>
			state.storage.sql.exec<{ frames: number }>("SELECT frames FROM usage WHERE day = ?", usage.day).one().frames,
		);
		expect(stored).toBe(usage.frames + 1);
		const next = await connectMachine();
		expect(next.usage).toEqual({ day: usage.day, frames: usage.frames + 1 });
	});
});
