// What the tests see of the Worker: its bindings, the test's signing key (vitest.config.ts), and its
// exports, which the tests call it through.
import type { Env as RelayEnv } from "../src/protocol";

declare global {
	namespace Cloudflare {
		interface Env extends RelayEnv {
			// The private half, as PKCS #8 in base64url, of the second key that HOST_KEYS allows.
			TEST_KEY: string;
			// And of the third.
			TEST_KEY_2: string;
		}
		interface GlobalProps {
			mainModule: typeof import("../src/index");
			durableNamespaces: "Relay";
		}
	}
}
