// Pairing without a code: a device that was only given the relay's address is let through to whichever
// machine has pairing open, and to nobody when none has. What it then proves, it proves to the machine.

import { runInDurableObject } from "cloudflare:test";
import { describe, expect, test } from "vitest";
import { CLOSE, KIND, LIMIT } from "../src/protocol";
import {
	accepted,
	closePairing,
	connectMachine,
	dataFrame,
	hex,
	openPairing,
	openPairToken,
	opened,
	otherListed,
	relay,
	upgrade,
} from "./helpers";

const text = (s: string) => new TextEncoder().encode(s);
const knock = () => upgrade("/v1/pair", openPairToken);

describe("pairing without a code", () => {
	test("nobody is let in while no machine has pairing open", async () => {
		const machine = await connectMachine();
		expect((await knock()).status).toBe(401);
		expect(await machine.socket.idle()).toBe(true);
	});

	test("a device reaches the machine that opened pairing, and is told apart from one with a code", async () => {
		const machine = await connectMachine();
		const bystander = await connectMachine();
		await openPairing(machine);
		const device = await accepted(await knock());
		const open = await opened(machine);
		expect(open.kind).toBe(KIND.openPair);
		device.send(text("hello"));
		expect(await machine.socket.frame()).toEqual(dataFrame(open.conn, text("hello")));
		machine.socket.send(dataFrame(open.conn, text("welcome")));
		expect(await device.frame()).toEqual(text("welcome"));
		expect(await bystander.socket.idle()).toBe(true);
	});

	test("the window lets in as many as it says, then closes", async () => {
		const machine = await connectMachine();
		await openPairing(machine, 2);
		const one = await accepted(await knock());
		await opened(machine);
		one.close();
		expect(await one.closed).toBeDefined();
		await machine.socket.frame(); // its close
		const two = await accepted(await knock());
		await opened(machine);
		two.close();
		await machine.socket.frame();
		expect((await knock()).status).toBe(401);
	});

	test("a closed window, a past one, and one whose machine has gone let nobody in", async () => {
		const machine = await connectMachine();
		await openPairing(machine);
		await closePairing(machine);
		expect((await knock()).status).toBe(401);

		await openPairing(machine, 8, Date.now() / 1000 - 1);
		expect((await knock()).status).toBe(401);

		await openPairing(machine);
		machine.socket.close();
		expect(await machine.socket.closed).toBeDefined();
		expect((await knock()).status).toBe(401);
	});

	test("with two machines open the relay doesn't choose: the device is told, and reaches neither", async () => {
		const first = await connectMachine();
		const second = await connectMachine(undefined, otherListed);
		await openPairing(first);
		await openPairing(second);
		const refused = await accepted(await knock()); // let in only to be told why not
		expect(await refused.closed).toBe(CLOSE.ambiguous);
		expect(await first.socket.idle()).toBe(true);
		expect(await second.socket.idle()).toBe(true);
		// When one closes, the other's window is the only one.
		await closePairing(second);
		await accepted(await knock());
		expect((await opened(first)).kind).toBe(KIND.openPair);
	});

	test("a window is as long and lets in as many as the relay allows, whatever a machine asks", async () => {
		const machine = await connectMachine();
		await openPairing(machine, 255, Date.now() / 1000 + 100 * 365 * 86400);
		const row = await runInDurableObject(relay(), (_, state) =>
			state.storage.sql
				.exec<{ expires: number; uses: number }>(
					"SELECT expires, uses FROM tickets WHERE hash = ?",
					`open:${hex(machine.signer.public)}:${hex(machine.id)}`,
				)
				.one(),
		);
		expect(row.uses).toBe(LIMIT.openPairUses);
		expect(row.expires).toBeLessThanOrEqual(Date.now() / 1000 + LIMIT.openPairSeconds + 1);
	});

	test("a close for an earlier window leaves a newer one open; a close for no window in particular shuts it", async () => {
		const machine = await connectMachine();
		const earlier = Math.floor(Date.now() / 1000) + 60;
		const later = earlier + 120;
		await openPairing(machine, 8, earlier);
		await openPairing(machine, 8, later);
		await closePairing(machine, earlier);
		await accepted(await knock());
		await opened(machine);
		await closePairing(machine);
		expect((await knock()).status).toBe(401);
	});

	test("it needs the page's own origin, as every device's socket does", async () => {
		const machine = await connectMachine();
		await openPairing(machine);
		expect((await upgrade("/v1/pair", openPairToken, { origin: "https://elsewhere.example" })).status).toBe(403);
		expect((await upgrade("/v1/pair", openPairToken, { origin: null })).status).toBe(403);
		expect(await machine.socket.idle()).toBe(true);
	});
});
