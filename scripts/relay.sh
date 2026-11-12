#!/bin/sh
# relay.sh builds the relay, relay/, into one ES module, dist/relay/worker.js, which is what Solo
# uploads to Cloudflare. It checks the types first. The module imports cloudflare:workers, which the
# Workers runtime provides, and nothing else.
#
# The relay's tests are not run here: cd relay && bun run test.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

echo "== install relay's tools"
(cd relay && bun install --frozen-lockfile)

echo "== typecheck relay"
(cd relay && bun run typecheck)

echo "== build dist/relay/worker.js"
mkdir -p dist/relay
bun build relay/src/index.ts --format esm --target browser --external cloudflare:workers \
	--outfile dist/relay/worker.js
ls -l dist/relay/worker.js
