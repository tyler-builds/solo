// The relay's Worker: it answers /v1/info, checks the token of every socket, and hands the sockets it
// admits to the one Durable Object that forwards their frames. It keeps nothing and logs nothing.

import { parseHostKeys, parsePairToken, ticketHash, verifyMachineAuth, verifyVoucher, type Verdict } from "./auth";
import { fromBase64Url, toHex } from "./bytes";
import { HEADER, SUBPROTOCOL, type Env, type Role } from "./protocol";

export { Relay } from "./relay";

const routes: Record<string, Role> = {
	"/v1/machine": "machine",
	"/v1/device": "device",
	"/v1/pair": "pair",
};

export default {
	async fetch(request: Request, env: Env): Promise<Response> {
		const url = new URL(request.url);
		if (url.pathname === "/v1/info" && request.method === "GET") {
			return Response.json(
				{ solo: 1, bundle: Number(env.SOLO_BUNDLE), time: Date.now() },
				{ headers: { "Cache-Control": "no-store" } },
			);
		}
		const role = routes[url.pathname];
		if (role === undefined) {
			return status(404);
		}
		if (request.method !== "GET" || request.headers.get("Upgrade")?.toLowerCase() !== "websocket") {
			return new Response(null, { status: 426, headers: { Upgrade: "websocket" } });
		}
		const token = tokenOf(request.headers.get("Sec-WebSocket-Protocol"));
		if (token === null) {
			return status(400);
		}
		// What the tokens are signed for, and where the page that may connect was served from.
		const origin = `https://${url.host}`;
		// A browser says which page opened the socket, and only the relay's own page may. A machine is
		// a native program and sends no Origin.
		if (role !== "machine" && request.headers.get("Origin") !== origin) {
			return status(403);
		}
		const facts = await verified(role, token, origin, env);
		if (facts instanceof Response) {
			return facts;
		}
		// The Durable Object is told only what was verified here, in a request made here: nothing of
		// the client's request reaches it, the token least of all.
		const relay = env.RELAY.get(env.RELAY.idFromName("default"));
		return relay.fetch("https://relay/", { headers: { Upgrade: "websocket", [HEADER.role]: role, ...facts } });
	},
} satisfies ExportedHandler<Env>;

// tokenOf reads the token from the subprotocols a socket asks for: exactly solo.v1, then the token.
function tokenOf(header: string | null): Uint8Array | null {
	const asked = (header ?? "").split(",").map((p) => p.trim());
	if (asked.length !== 2 || asked[0] !== SUBPROTOCOL || asked[1] === "") {
		return null;
	}
	return fromBase64Url(asked[1]!);
}

// verified checks a token and returns the headers that tell the Durable Object what it proves, or the
// response that refuses it.
async function verified(
	role: Role,
	token: Uint8Array,
	origin: string,
	env: Env,
): Promise<Record<string, string> | Response> {
	const hostKeys = parseHostKeys(env.HOST_KEYS);
	switch (role) {
		case "machine": {
			const v = await verifyMachineAuth(origin, token, Date.now(), hostKeys);
			return v.ok ? { [HEADER.machine]: toHex(v.machineId) } : refused(v);
		}
		case "device": {
			const v = await verifyVoucher(origin, token, hostKeys);
			return v.ok ? { [HEADER.machine]: toHex(v.machineId), [HEADER.device]: toHex(v.deviceId) } : refused(v);
		}
		case "pair": {
			const pair = parsePairToken(token);
			if (pair === null) {
				return status(401);
			}
			return {
				[HEADER.machine]: toHex(pair.machineId),
				[HEADER.ticket]: toHex(await ticketHash(pair.ticket)),
			};
		}
	}
}

function refused(verdict: Verdict<unknown> & { ok: false }): Response {
	return status(verdict.why === "key" ? 403 : 401);
}

function status(code: number): Response {
	return new Response(null, { status: code });
}
