import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";
import vectors from "./test/vectors.json" with { type: "json" };

// The tests run in workerd, against the Worker and its Durable Object as wrangler.jsonc sets them up.
// HOST_KEYS allows two keys: the one the vectors were made with, and one made here, whose private half
// the tests get (TEST_KEY) to sign tokens of their own with.
export default defineConfig(async () => {
	const pair = (await crypto.subtle.generateKey("Ed25519", true, ["sign", "verify"])) as CryptoKeyPair;
	const exported = async (format: "raw" | "pkcs8", key: CryptoKey) => {
		const bytes = new Uint8Array((await crypto.subtle.exportKey(format, key)) as ArrayBuffer);
		return btoa(String.fromCharCode(...bytes)).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
	};
	return {
		plugins: [
			cloudflareTest({
				wrangler: { configPath: "./wrangler.jsonc" },
				miniflare: {
					bindings: {
						HOST_KEYS: `${vectors.hostKeys}, ${await exported("raw", pair.publicKey)}`,
						SOLO_BUNDLE: "7",
						TEST_KEY: await exported("pkcs8", pair.privateKey),
					},
				},
			}),
		],
		test: {
			include: ["test/**/*.test.ts"],
			// One file at a time: they share the one Durable Object, and the machine is short of memory.
			fileParallelism: false,
		},
	};
});
