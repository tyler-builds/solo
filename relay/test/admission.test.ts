// Who the Worker lets in, and what it answers everyone else.

import { exports } from "cloudflare:workers";
import { describe, expect, test } from "vitest";
import { fromBase64Url, toBase64Url } from "../src/bytes";
import { CLOSE, SUBPROTOCOL } from "../src/protocol";
import {
	connectMachine,
	flipped,
	hex,
	HOST,
	machineAuth,
	opened,
	ORIGIN,
	pairToken,
	random,
	Socket,
	unlisted,
	upgrade,
	voucher,
} from "./helpers";
import vectors from "./vectors.json";

const routes = ["/v1/machine", "/v1/device", "/v1/pair"];

describe("plain requests", () => {
	test("/v1/info says what is deployed, and is not cached", async () => {
		const before = Date.now();
		const response = await exports.default.fetch(`${ORIGIN}/v1/info`);
		expect(response.status).toBe(200);
		expect(response.headers.get("Cache-Control")).toBe("no-store");
		expect(response.headers.get("Set-Cookie")).toBeNull();
		expect(response.headers.get("Access-Control-Allow-Origin")).toBeNull();
		const info = await response.json<{ solo: number; bundle: number; time: number; network: number }>();
		expect(info).toEqual({ solo: 1, bundle: 7, time: expect.any(Number), network: 1 });
		expect(info.time).toBeGreaterThanOrEqual(before);
		expect(info.time).toBeLessThanOrEqual(Date.now());
	});

	test("anything else is not found", async () => {
		for (const path of ["/", "/index.html", "/v1", "/v1/info/", "/v1/machine/x", "/v2/info"]) {
			expect((await exports.default.fetch(`${ORIGIN}${path}`)).status, path).toBe(404);
		}
		expect((await exports.default.fetch(`${ORIGIN}/v1/info`, { method: "POST" })).status).toBe(404);
	});

	test.each(routes)("%s without an upgrade is refused", async (path) => {
		const response = await exports.default.fetch(`${ORIGIN}${path}`, {
			headers: { "Sec-WebSocket-Protocol": `${SUBPROTOCOL}, AAAA`, Origin: ORIGIN },
		});
		expect(response.status).toBe(426);
	});
});

describe("subprotocols", () => {
	test.each(routes)("%s needs solo.v1 and then a token, and nothing else", async (path) => {
		const token = toBase64Url(await machineAuth(random(16)));
		const wrong = [
			null,
			"",
			SUBPROTOCOL,
			token,
			`${token}, ${SUBPROTOCOL}`,
			`solo.v2, ${token}`,
			`${SUBPROTOCOL}, ${token}, extra`,
			`${SUBPROTOCOL}, `,
			`${SUBPROTOCOL}, ${token}=`,
			`${SUBPROTOCOL}, not+base64url`,
		];
		for (const protocols of wrong) {
			expect((await upgrade(path, null, { protocols })).status, String(protocols)).toBe(400);
		}
	});
});

describe("a machine", () => {
	test("is admitted with its token, with no Origin, and the token is not echoed", async () => {
		const response = await upgrade("/v1/machine", await machineAuth(random(16)));
		expect(response.status).toBe(101);
		expect(response.headers.get("Sec-WebSocket-Protocol")).toBe(SUBPROTOCOL);
		new Socket(response.webSocket!);
	});

	test("is admitted near the edges of the window", async () => {
		for (const offset of [-110_000, 110_000]) {
			const response = await upgrade("/v1/machine", await machineAuth(random(16), { at: Date.now() + offset }));
			expect(response.status).toBe(101);
			new Socket(response.webSocket!);
		}
	});

	test("is refused with a token that is not good", async () => {
		const id = random(16);
		const good = await machineAuth(id);
		const tokens: Record<string, Uint8Array> = {
			"altered signature": flipped(good, good.length - 1),
			"altered machine": flipped(good, 33),
			"wrong version": flipped(good, 0),
			truncated: good.subarray(0, good.length - 1),
			"too old": await machineAuth(id, { at: Date.now() - 125_000 }),
			"too new": await machineAuth(id, { at: Date.now() + 125_000 }),
			"for another relay": await machineAuth(id, { origin: "https://another.test" }),
			"for http": await machineAuth(id, { origin: `http://${HOST}` }),
			"a voucher": await voucher(id, random(16)),
			"the vector, which is not from now": fromBase64Url(vectors.machineAuth)!,
		};
		for (const [name, token] of Object.entries(tokens)) {
			expect((await upgrade("/v1/machine", token)).status, name).toBe(401);
		}
	});

	test("is refused when its key is not in HOST_KEYS", async () => {
		const token = await machineAuth(random(16), { signer: await unlisted() });
		expect((await upgrade("/v1/machine", token)).status).toBe(403);
	});
});

describe("a device", () => {
	test("is admitted with a voucher and the relay's origin", async () => {
		const machine = await connectMachine();
		const response = await upgrade("/v1/device", await voucher(machine.id, random(16)));
		expect(response.status).toBe(101);
		expect(response.headers.get("Sec-WebSocket-Protocol")).toBe(SUBPROTOCOL);
		new Socket(response.webSocket!);
	});

	test("is refused without the relay's origin", async () => {
		const token = await voucher(random(16), random(16));
		for (const origin of [null, "https://another.test", `http://${HOST}`, `${ORIGIN}:8443`, "null"]) {
			expect((await upgrade("/v1/device", token, { origin })).status, String(origin)).toBe(403);
		}
	});

	test("is refused with a voucher that is not good", async () => {
		const machine = random(16);
		const good = await voucher(machine, random(16));
		const tokens: Record<string, Uint8Array> = {
			"altered signature": flipped(good, good.length - 1),
			"altered device": flipped(good, 49),
			"altered machine": flipped(good, 33),
			truncated: good.subarray(0, good.length - 1),
			"for another relay": await voucher(machine, random(16), { origin: "https://another.test" }),
			"a machine's token": await machineAuth(machine),
		};
		for (const [name, token] of Object.entries(tokens)) {
			expect((await upgrade("/v1/device", token)).status, name).toBe(401);
		}
	});

	test("is refused when the voucher's key is not in HOST_KEYS", async () => {
		const token = await voucher(random(16), random(16), { signer: await unlisted() });
		expect((await upgrade("/v1/device", token)).status).toBe(403);
	});

	test("is admitted with the voucher Go made, at the origin Go made it for", async () => {
		const host = new URL(vectors.origin).host;
		const response = await upgrade("/v1/device", fromBase64Url(vectors.voucher)!, { host });
		expect(response.status).toBe(101);
		// The vector's machine is not connected.
		expect(await new Socket(response.webSocket!).closed).toBe(CLOSE.offline);
		// The same voucher at this relay's other address is for another origin.
		expect((await upgrade("/v1/device", fromBase64Url(vectors.voucher)!)).status).toBe(401);
	});
});

describe("a pairing socket", () => {
	test("is refused without the relay's origin", async () => {
		const token = pairToken(random(16), random(32));
		for (const origin of [null, "https://another.test"]) {
			expect((await upgrade("/v1/pair", token, { origin })).status, String(origin)).toBe(403);
		}
	});

	test("is refused with a token that is not one, or a ticket nobody registered", async () => {
		const good = pairToken(random(16), random(32));
		const tokens: Record<string, Uint8Array> = {
			"unknown ticket": good,
			"wrong version": flipped(good, 0),
			truncated: good.subarray(0, good.length - 1),
			"a voucher": await voucher(random(16), random(16)),
			"the vector, which nobody registered": fromBase64Url(vectors.pairToken)!,
		};
		for (const [name, token] of Object.entries(tokens)) {
			expect((await upgrade("/v1/pair", token)).status, name).toBe(401);
		}
	});
});

describe("the Worker's word to the Durable Object", () => {
	test("cannot be forged by a client", async () => {
		const target = await connectMachine();
		const other = await connectMachine();
		const device = random(16);
		const forged = {
			"X-Solo-Role": "machine",
			"X-Solo-Machine": hex(other.id),
			"X-Solo-Device": hex(random(16)),
			"X-Solo-Ticket": hex(random(32)),
		};
		const response = await upgrade("/v1/device", await voucher(target.id, device), { headers: forged });
		expect(response.status).toBe(101);
		new Socket(response.webSocket!);
		// The socket is the voucher's device, at the voucher's machine.
		const open = await opened(target);
		expect([open.kind, hex(open.deviceId)]).toEqual([1, hex(device)]);
		expect(await other.socket.idle()).toBe(true);

		// A pairing socket that claims to be verified already is still asked for its ticket.
		const pair = await upgrade("/v1/pair", pairToken(target.id, random(32)), { headers: forged });
		expect(pair.status).toBe(401);
		// And a request with no token at all is not let through for its headers.
		const bare = await upgrade("/v1/machine", null, { protocols: SUBPROTOCOL, headers: forged });
		expect(bare.status).toBe(400);
		expect(await target.socket.idle()).toBe(true);
		expect(await other.socket.idle()).toBe(true);
	});
	test("a page can't connect as a machine: a machine sends no Origin", async () => {
		const res = await upgrade("/v1/machine", await machineAuth(random(16)), { origin: ORIGIN });
		expect(res.status).toBe(403);
	});
});
