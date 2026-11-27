// The tokens Go made (internal/remote/secure/vectors_test.go), checked here as Go checks them.

import { describe, expect, test } from "vitest";
import {
	MACHINE_AUTH_WINDOW_MS,
	parseHostKeys,
	parsePairToken,
	ticketHash,
	verifyMachineAuth,
	verifyPeerAuth,
	verifyVoucher,
} from "../src/auth";
import { fromBase64Url, toBase64Url, toHex } from "../src/bytes";
import vectors from "./vectors.json";

const hostKeys = parseHostKeys(vectors.hostKeys);
const token = (text: string) => fromBase64Url(text)!;
const auth = token(vectors.machineAuth);
const voucher = token(vectors.voucher);
const peer = token(vectors.peerAuth);
// A key the list could hold in place of the machine's.
const otherKeys = [new Uint8Array(32).fill(7)];

describe("the machine's token", () => {
	test("is accepted, and names the machine", async () => {
		const v = await verifyMachineAuth(vectors.origin, auth, vectors.nowMs, hostKeys);
		expect(v.ok && toHex(v.machineId)).toBe(vectors.machineId);
	});

	test.each(Object.entries(vectors.rejectAuth))("is refused: %s", async (_, text) => {
		expect(await verifyMachineAuth(vectors.origin, token(text), vectors.nowMs, hostKeys)).toMatchObject({ ok: false });
	});

	test("is refused for another origin", async () => {
		expect(await verifyMachineAuth(vectors.otherOrigin, auth, vectors.nowMs, hostKeys)).toEqual({ ok: false, why: "token" });
	});

	test("is accepted only within the window", async () => {
		expect(vectors.windowMs).toBe(MACHINE_AUTH_WINDOW_MS);
		for (const offset of [-vectors.windowMs, vectors.windowMs]) {
			expect(await verifyMachineAuth(vectors.origin, auth, vectors.nowMs + offset, hostKeys)).toMatchObject({ ok: true });
		}
		for (const offset of [-vectors.windowMs - 1, vectors.windowMs + 1, -86_400_000, 86_400_000]) {
			expect(await verifyMachineAuth(vectors.origin, auth, vectors.nowMs + offset, hostKeys)).toEqual({ ok: false, why: "token" });
		}
	});

	test("is refused when its key is not in the list", async () => {
		expect(await verifyMachineAuth(vectors.origin, auth, vectors.nowMs, otherKeys)).toEqual({ ok: false, why: "key" });
		expect(await verifyMachineAuth(vectors.origin, auth, vectors.nowMs, [])).toEqual({ ok: false, why: "key" });
		// Another machine's token is signed well, by a key the list does not have.
		const other = token(vectors.rejectAuth["another machine's"]);
		expect(await verifyMachineAuth(vectors.origin, other, vectors.nowMs, hostKeys)).toEqual({ ok: false, why: "key" });
		expect(await verifyMachineAuth(vectors.origin, other, vectors.nowMs, [other.slice(1, 33)])).toMatchObject({ ok: true });
	});
});

describe("the machine's token for another machine", () => {
	test("is accepted, and names the machine and the other", async () => {
		const v = await verifyPeerAuth(vectors.origin, peer, vectors.nowMs, hostKeys);
		expect(v.ok && [toHex(v.machineId), toBase64Url(v.key), toHex(v.target)]).toEqual([
			vectors.machineId,
			vectors.machineKey,
			vectors.peerTarget,
		]);
	});

	test.each(Object.entries(vectors.rejectPeer))("is refused: %s", async (_, text) => {
		expect(await verifyPeerAuth(vectors.origin, token(text), vectors.nowMs, hostKeys)).toMatchObject({ ok: false });
	});

	test("is refused for another origin", async () => {
		expect(await verifyPeerAuth(vectors.otherOrigin, peer, vectors.nowMs, hostKeys)).toEqual({ ok: false, why: "token" });
	});

	test("is accepted only within the window", async () => {
		for (const offset of [-vectors.windowMs, vectors.windowMs]) {
			expect(await verifyPeerAuth(vectors.origin, peer, vectors.nowMs + offset, hostKeys)).toMatchObject({ ok: true });
		}
		for (const offset of [-vectors.windowMs - 1, vectors.windowMs + 1]) {
			expect(await verifyPeerAuth(vectors.origin, peer, vectors.nowMs + offset, hostKeys)).toEqual({ ok: false, why: "token" });
		}
	});

	test("is refused when its key is not in the list", async () => {
		expect(await verifyPeerAuth(vectors.origin, peer, vectors.nowMs, otherKeys)).toEqual({ ok: false, why: "key" });
		const other = token(vectors.rejectPeer["another machine's"]);
		expect(await verifyPeerAuth(vectors.origin, other, vectors.nowMs, hostKeys)).toEqual({ ok: false, why: "key" });
		expect(await verifyPeerAuth(vectors.origin, other, vectors.nowMs, [other.slice(1, 33)])).toMatchObject({ ok: true });
	});

	test("is not a machine's own token, nor the other way round", async () => {
		expect(await verifyMachineAuth(vectors.origin, peer, vectors.nowMs, hostKeys)).toEqual({ ok: false, why: "token" });
		expect(await verifyPeerAuth(vectors.origin, auth, vectors.nowMs, hostKeys)).toEqual({ ok: false, why: "token" });
	});
});

describe("the voucher", () => {
	test("is accepted, and names the machine and the device", async () => {
		const v = await verifyVoucher(vectors.origin, voucher, hostKeys);
		expect(v.ok && [toHex(v.machineId), toHex(v.deviceId)]).toEqual([vectors.machineId, vectors.deviceId]);
	});

	test.each(Object.entries(vectors.rejectVoucher))("is refused: %s", async (_, text) => {
		expect(await verifyVoucher(vectors.origin, token(text), hostKeys)).toEqual({ ok: false, why: "token" });
	});

	test("is refused for another origin", async () => {
		expect(await verifyVoucher(vectors.otherOrigin, voucher, hostKeys)).toEqual({ ok: false, why: "token" });
	});

	test("is refused when its key is not in the list", async () => {
		expect(await verifyVoucher(vectors.origin, voucher, otherKeys)).toEqual({ ok: false, why: "key" });
		expect(await verifyVoucher(vectors.origin, voucher, [])).toEqual({ ok: false, why: "key" });
	});
});

describe("the pairing token", () => {
	test("names the machine and carries the ticket the hash is of", async () => {
		const pair = parsePairToken(token(vectors.pairToken))!;
		expect(toHex(pair.machineId)).toBe(vectors.machineId);
		expect(toHex(await ticketHash(pair.ticket))).toBe(vectors.ticketHash);
	});

	test("is refused when it is not one", () => {
		const good = token(vectors.pairToken);
		expect(parsePairToken(good.subarray(1))).toBeNull();
		expect(parsePairToken(new Uint8Array([2, ...good.subarray(1)]))).toBeNull();
		expect(parsePairToken(auth)).toBeNull();
	});
});

describe("the list of keys", () => {
	test("is read with spaces, and without the entries that are not keys", () => {
		const key = vectors.machineKey;
		const other = toBase64Url(otherKeys[0]!);
		expect(parseHostKeys(`${key}, ${other}`).map(toBase64Url)).toEqual([key, other]);
		expect(parseHostKeys(`,${key},,not a key,AAAA,${key}=`).map(toBase64Url)).toEqual([key]);
		expect(parseHostKeys("")).toEqual([]);
	});
});
