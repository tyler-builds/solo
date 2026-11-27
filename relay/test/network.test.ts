// A network: machines that take one passphrase. A device asks the relay which machines they are,
// pairs with one by proving the passphrase to it, and the machines reach each other through the
// relay. The passphrase is never the relay's to see: it only lets the sockets through.

import { runInDurableObject } from "cloudflare:test";
import { describe, expect, test } from "vitest";
import { CLOSE, FRAME, KIND, LIMIT } from "../src/protocol";
import {
	accepted,
	closeFrame,
	connectMachine,
	connectPeer,
	dataFrame,
	flipped,
	hex,
	joinNetwork,
	leaveNetwork,
	listNetwork,
	machineAuth,
	networkToken,
	opened,
	openedPeer,
	otherListed,
	passToken,
	peerAuth,
	random,
	relay,
	Socket,
	unlisted,
	upgrade,
	type Machine,
} from "./helpers";

const text = (s: string) => new TextEncoder().encode(s);
const owner = (machine: Machine) => `${hex(machine.signer.public)}:${hex(machine.id)}`;

// closedWith opens a socket the relay accepts only to close, and returns the code.
async function closedWith(path: string, token: Uint8Array): Promise<number> {
	const response = await upgrade(path, token);
	expect(response.status).toBe(101);
	return new Socket(response.webSocket!).closed;
}

const pass = (machine: Machine) => upgrade("/v1/pair", passToken(machine.id));

// setPasses writes how many passphrase pairings a machine has been sent in its window, and when the
// window began.
async function setPasses(machine: Machine, count: number, since = Date.now()): Promise<void> {
	await runInDurableObject(relay(), (_, state) => {
		state.storage.sql.exec("INSERT OR REPLACE INTO passes (machine, since, count) VALUES (?, ?, ?)", owner(machine), since, count);
	});
}

describe("the list of the network", () => {
	test("is empty while no machine takes the passphrase", async () => {
		await connectMachine();
		expect(await listNetwork()).toEqual([]);
	});

	test("has the connected machines that take it, in order, and no others", async () => {
		const machines = [await connectMachine(), await connectMachine(), await connectMachine(undefined, otherListed)];
		const bystander = await connectMachine();
		for (const machine of machines) {
			await joinNetwork(machine);
		}
		// Taking it again changes nothing.
		await joinNetwork(machines[0]!);
		expect(await listNetwork()).toEqual(machines.map((m) => hex(m.id)).sort());

		await leaveNetwork(machines[1]!);
		expect(await listNetwork()).toEqual([hex(machines[0]!.id), hex(machines[2]!.id)].sort());

		// A machine that is away is not listed, but is again when it comes back.
		machines[2]!.socket.close();
		await machines[2]!.socket.closed;
		expect(await listNetwork()).toEqual([hex(machines[0]!.id)]);
		await connectMachine(machines[2]!.id, otherListed);
		expect(await listNetwork()).toEqual([hex(machines[0]!.id), hex(machines[2]!.id)].sort());

		// Asking tells no machine anything.
		expect(await machines[0]!.socket.idle()).toBe(true);
		expect(await bystander.socket.idle()).toBe(true);
	});

	test("needs the page's own origin, as every device's socket does", async () => {
		expect((await upgrade("/v1/pair", networkToken, { origin: "https://elsewhere.example" })).status).toBe(403);
		expect((await upgrade("/v1/pair", networkToken, { origin: null })).status).toBe(403);
	});
});

describe("pairing with the passphrase", () => {
	test("reaches a machine that takes it, and is told apart from other pairings", async () => {
		const machine = await connectMachine();
		await joinNetwork(machine);
		const device = await accepted(await pass(machine));
		const open = await opened(machine);
		expect(open.kind).toBe(KIND.passPair);
		expect(hex(open.deviceId)).toBe("00".repeat(16));
		device.send(text("hello"));
		expect(await machine.socket.frame()).toEqual(dataFrame(open.conn, text("hello")));
		machine.socket.send(dataFrame(open.conn, text("welcome")));
		expect(await device.frame()).toEqual(text("welcome"));
		device.close();
		expect(await machine.socket.frame()).toEqual(closeFrame(open.conn));
	});

	test("is refused for a machine that doesn't take it, and for one that is away", async () => {
		const machine = await connectMachine();
		expect((await pass(machine)).status).toBe(401);
		expect((await upgrade("/v1/pair", passToken(random(16)))).status).toBe(401);
		await joinNetwork(machine);
		await leaveNetwork(machine);
		expect((await pass(machine)).status).toBe(401);
		expect(await machine.socket.idle()).toBe(true);

		await joinNetwork(machine);
		machine.socket.close();
		await machine.socket.closed;
		expect(await closedWith("/v1/pair", passToken(machine.id))).toBe(CLOSE.offline);
	});

	test("is refused for a token that is not one", async () => {
		const machine = await connectMachine();
		await joinNetwork(machine);
		expect((await upgrade("/v1/pair", passToken(machine.id).subarray(0, 16))).status).toBe(401);
		expect((await upgrade("/v1/pair", new Uint8Array([...passToken(machine.id), 0]))).status).toBe(401);
		expect(await machine.socket.idle()).toBe(true);
	});

	test("needs the page's own origin", async () => {
		const machine = await connectMachine();
		await joinNetwork(machine);
		expect((await upgrade("/v1/pair", passToken(machine.id), { origin: null })).status).toBe(403);
		expect(await machine.socket.idle()).toBe(true);
	});

	test("counts against the machine's pairing sockets", async () => {
		const machine = await connectMachine();
		await joinNetwork(machine);
		const first = await accepted(await pass(machine));
		await accepted(await pass(machine));
		expect(await closedWith("/v1/pair", passToken(machine.id))).toBe(CLOSE.limit);
		first.close();
		await first.closed;
		await accepted(await pass(machine));
	});

	test("lets in so many to a machine in a window, then no more until it has passed", async () => {
		const machine = await connectMachine();
		const other = await connectMachine();
		await joinNetwork(machine);
		await joinNetwork(other);
		await setPasses(machine, LIMIT.passPairs - 1);
		const last = await accepted(await pass(machine));
		await opened(machine);
		last.close();
		await machine.socket.frame();
		expect(await closedWith("/v1/pair", passToken(machine.id))).toBe(CLOSE.limit);
		expect(await machine.socket.idle()).toBe(true);
		// The count is each machine's own.
		await accepted(await pass(other));
		// A new window starts again from nothing.
		await setPasses(machine, LIMIT.passPairs, Date.now() - LIMIT.passWindowMs - 1);
		await accepted(await pass(machine));
		const row = await runInDurableObject(relay(), (_, state) =>
			state.storage.sql.exec<{ count: number }>("SELECT count FROM passes WHERE machine = ?", owner(machine)).one(),
		);
		expect(row.count).toBe(1);
	});

	test("one over the limit of sockets is not counted", async () => {
		const machine = await connectMachine();
		await joinNetwork(machine);
		await accepted(await pass(machine));
		await accepted(await pass(machine));
		expect(await closedWith("/v1/pair", passToken(machine.id))).toBe(CLOSE.limit);
		const row = await runInDurableObject(relay(), (_, state) =>
			state.storage.sql.exec<{ count: number }>("SELECT count FROM passes WHERE machine = ?", owner(machine)).one(),
		);
		expect(row.count).toBe(2);
	});
});

describe("a machine's socket to another", () => {
	test("reaches the other, which is told who it is from, and carries frames both ways", async () => {
		const from = await connectMachine();
		const to = await connectMachine(undefined, otherListed);
		const peer = await connectPeer(from, to.id);
		const open = await openedPeer(to);
		expect(open.kind).toBe(KIND.peer);
		expect(open.deviceId).toEqual(from.id);
		expect(open.key).toEqual(from.signer.public);

		peer.send(text("hello"));
		expect(await to.socket.frame()).toEqual(dataFrame(open.conn, text("hello")));
		to.socket.send(dataFrame(open.conn, text("welcome")));
		expect(await peer.frame()).toEqual(text("welcome"));
		// The machine it is from is told nothing: the socket is its own connection.
		expect(await from.socket.idle()).toBe(true);

		peer.close();
		expect(await to.socket.frame()).toEqual(closeFrame(open.conn));
	});

	test("is closed when the other machine closes it", async () => {
		const from = await connectMachine();
		const to = await connectMachine();
		const peer = await connectPeer(from, to.id);
		const open = await openedPeer(to);
		to.socket.send(closeFrame(open.conn));
		expect(await peer.closed).toBe(CLOSE.normal);
		expect(await to.socket.idle()).toBe(true);
	});

	test("is closed as offline when the other machine goes, and stays when its own does", async () => {
		const from = await connectMachine();
		const to = await connectMachine();
		const third = await connectMachine();
		const away = await connectPeer(from, to.id);
		await openedPeer(to);
		const staying = await connectPeer(from, third.id);
		const open = await openedPeer(third);

		to.socket.close();
		expect(await away.closed).toBe(CLOSE.offline);

		from.socket.close();
		await from.socket.closed;
		staying.send(text("still here"));
		expect(await third.socket.frame()).toEqual(dataFrame(open.conn, text("still here")));
	});

	test("is refused as offline when the other machine is not connected", async () => {
		const from = await connectMachine();
		expect(await closedWith("/v1/peer", await peerAuth(from.id, random(16)))).toBe(CLOSE.offline);
		expect(await from.socket.idle()).toBe(true);
	});

	test("is refused to the machine itself", async () => {
		const from = await connectMachine();
		expect((await upgrade("/v1/peer", await peerAuth(from.id, from.id))).status).toBe(400);
		expect(await from.socket.idle()).toBe(true);
	});

	test("is refused for a token that is not good, or whose key is not listed", async () => {
		const from = await connectMachine();
		const to = await connectMachine();
		const good = await peerAuth(from.id, to.id);
		expect((await upgrade("/v1/peer", flipped(good, 100))).status).toBe(401);
		expect((await upgrade("/v1/peer", flipped(good, good.length - 1))).status).toBe(401);
		expect((await upgrade("/v1/peer", await peerAuth(from.id, to.id, { at: Date.now() - 10 * 60 * 1000 }))).status).toBe(401);
		expect((await upgrade("/v1/peer", await peerAuth(from.id, to.id, { origin: "https://elsewhere.example" }))).status).toBe(401);
		// A machine's own token is not one for another machine, nor the other way round.
		expect((await upgrade("/v1/peer", await machineAuth(from.id))).status).toBe(401);
		expect((await upgrade("/v1/machine", good)).status).toBe(401);
		expect((await upgrade("/v1/peer", await peerAuth(from.id, to.id, { signer: await unlisted() }))).status).toBe(403);
		expect(await to.socket.idle()).toBe(true);
	});

	test("is refused when it comes from a page", async () => {
		const from = await connectMachine();
		const to = await connectMachine();
		const token = await peerAuth(from.id, to.id);
		for (const origin of [`https://relay.test`, "https://elsewhere.example"]) {
			expect((await upgrade("/v1/peer", token, { origin })).status).toBe(403);
		}
		expect(await to.socket.idle()).toBe(true);
	});

	test("a machine has at most 16 of them, counted apart from its devices and pairings", async () => {
		const to = await connectMachine();
		const from = await connectMachine();
		const sockets: Socket[] = [];
		for (let i = 0; i < LIMIT.peers; i++) {
			sockets.push(await connectPeer(from, to.id));
			await openedPeer(to);
		}
		expect(await closedWith("/v1/peer", await peerAuth(from.id, to.id))).toBe(CLOSE.limit);
		expect(await to.socket.idle()).toBe(true);
		await joinNetwork(to);
		await accepted(await pass(to));
		expect((await opened(to)).kind).toBe(KIND.passPair);

		sockets[0]!.close();
		expect((await to.socket.frame())[0]).toBe(FRAME.close);
		await connectPeer(from, to.id);
		expect((await openedPeer(to)).kind).toBe(KIND.peer);
	});

	test("has no lifetime, as a pairing socket has", async () => {
		const from = await connectMachine();
		const to = await connectMachine();
		const peer = await connectPeer(from, to.id);
		const open = await openedPeer(to);
		await runInDurableObject(relay(), (_, state) => {
			for (const ws of state.getWebSockets(`q:${owner(to)}`)) {
				const attached = ws.deserializeAttachment();
				ws.serializeAttachment({ ...attached, at: attached.at - LIMIT.pairLifetimeMs - 1 });
			}
		});
		peer.send(text("later"));
		expect(await to.socket.frame()).toEqual(dataFrame(open.conn, text("later")));
	});
});
