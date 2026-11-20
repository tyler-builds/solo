#!/bin/sh
# e2e.sh runs remote access end to end in a real browser (e2e/remote.cjs): a phone-sized page pairs
# with a demo machine through the relay that runs on this machine, uses it, sends it a 6 MB file and
# checks the bytes that arrived, loses and regains it, comes back after a reload, and is removed.
# Screenshots go to dist/e2e.
#
# It needs the web client built (scripts/web.sh; built here when dist/web is missing), node, and
# Playwright's Chromium (cd e2e && bun install && bunx playwright install chromium). A machine with a
# GPU draws at full speed; without one the page is slow, and still passes.
#
# e2e.sh paste runs e2e/paste.cjs instead: large Markdown pasted into the composer of the web client,
# over generated data, typed after and sent, without the page stopping or running out of memory.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
[ -f dist/web/app.wasm ] || scripts/web.sh
mkdir -p dist/e2e
[ -d e2e/node_modules ] || (cd e2e && bun install)
if [ "${1:-}" = paste ]; then
	cd e2e
	WEB="$root/dist/web" node paste.cjs "$root/dist/e2e"
	exit
fi
go build -o dist/e2e/solo-localrelay ./cmd/solo-localrelay
cd e2e
RELAY="$root/dist/e2e/solo-localrelay" WEB="$root/dist/web" node remote.cjs "$root/dist/e2e"
