// Small helpers for the byte strings the relay handles: tokens, IDs and frames.

// fromBase64Url decodes unpadded base64url, the encoding of every token and key. It returns null for
// anything else, padding included.
export function fromBase64Url(text: string): Uint8Array | null {
	if (!/^[A-Za-z0-9_-]*$/.test(text) || text.length % 4 === 1) {
		return null;
	}
	const binary = atob(text.replaceAll("-", "+").replaceAll("_", "/"));
	const out = new Uint8Array(binary.length);
	for (let i = 0; i < out.length; i++) {
		out[i] = binary.charCodeAt(i);
	}
	return out;
}

export function toBase64Url(bytes: Uint8Array): string {
	let binary = "";
	for (const b of bytes) {
		binary += String.fromCharCode(b);
	}
	return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

export function toHex(bytes: Uint8Array): string {
	let out = "";
	for (const b of bytes) {
		out += b.toString(16).padStart(2, "0");
	}
	return out;
}

// fromHex decodes lower-case hex of exactly size bytes, and returns null for anything else.
export function fromHex(text: string, size: number): Uint8Array | null {
	if (text.length !== size * 2 || !/^[0-9a-f]*$/.test(text)) {
		return null;
	}
	const out = new Uint8Array(size);
	for (let i = 0; i < size; i++) {
		out[i] = parseInt(text.slice(i * 2, i * 2 + 2), 16);
	}
	return out;
}

// equal compares public values, so it does not need to take the same time for every input.
export function equal(a: Uint8Array, b: Uint8Array): boolean {
	return a.length === b.length && a.every((v, i) => v === b[i]);
}

export function concat(...parts: Uint8Array[]): Uint8Array {
	const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
	let at = 0;
	for (const p of parts) {
		out.set(p, at);
		at += p.length;
	}
	return out;
}
