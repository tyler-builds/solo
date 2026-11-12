#!/bin/sh
# web.sh [OUT] builds Solo's web client, cmd/solo-web, into the directory OUT (dist/web by default):
# index.html and its scripts, app.wasm, and the fonts jamb fetches. Serve OUT with any static file
# server and open it in a browser.
#
# The build uses TinyGo, as every jamb web build does (see jamb's docs/web.md), from its official
# image in docker or podman (CONTAINER picks one), with the settings of jamb's scripts/web/tinygo.sh:
# goroutines through asyncify with jamb's wasm-opt wrapper, no panic unwinding, and 1 MB stacks. The
# module cache is shared with the container read-only, so run `go mod download` first. MEMORY caps the
# container's memory (3g by default): the build needs about 2 GB, and a cap stops it, not the machine,
# when memory is short.
#
# The names of the app's functions go to app.names, which the page fetches only to show a stack.
#
# The build's files must belong to the user who runs the script. Under rootless podman (or rootless
# docker) the container's root is that user, so TinyGo runs as root; under rootful docker, as on CI,
# root is the host's root, so TinyGo runs as the user's own uid and gid instead.
set -eu

image=docker.io/tinygo/tinygo:0.44.0@sha256:7998bda3b044521b650a6a671229712b13f59a29ae373c60669907122e8d42f7
root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/dist/web}
mkdir -p "$out"
out=$(cd "$out" && pwd)
engine=${CONTAINER:-$(command -v docker >/dev/null 2>&1 && echo docker || echo podman)}
jamb=$(cd "$root" && go list -m -f '{{.Dir}}' github.com/tyler-builds/jamb)
modcache=$(go env GOMODCACHE)
cache=${TINYGO_CACHE:-$HOME/.cache/jamb-tinygo}
tools=$(mktemp -d)
trap 'rm -rf "$tools"' EXIT
mkdir -p "$cache"

# rootless succeeds when the engine maps the container's root to the user: podman reports it in
# Host.Security.Rootless, docker among its security options.
rootless() {
	case $("$engine" info --format '{{.Host.Security.Rootless}}' 2>/dev/null) in
	true) return 0 ;;
	false) return 1 ;;
	esac
	"$engine" info --format '{{.SecurityOptions}}' 2>/dev/null | grep -q rootless
}
if rootless; then
	user=0:0
else
	user=$(id -u):$(id -g)
fi

# The module cache's files are not executable; TinyGo runs this one.
cp "$jamb/scripts/web/wasm-opt.sh" "$tools/wasm-opt.sh"
chmod 755 "$tools/wasm-opt.sh"

echo "== tinygo build cmd/solo-web"
"$engine" run --rm --user "$user" --memory="${MEMORY:-3g}" --memory-swap="${MEMORY:-3g}" --security-opt label=disable \
	-v "$root:/src:ro" -v "$modcache:/gomod:ro" -v "$out:/out" -v "$cache:/tmp/.cache" -v "$tools:/tools:ro" \
	-e GOMODCACHE=/gomod -e GOPROXY=off -e GOFLAGS=-buildvcs=false \
	-e HOME=/tmp -e XDG_CACHE_HOME=/tmp/.cache -e GOCACHE=/tmp/.cache/go-build \
	-e WASMOPT=/tools/wasm-opt.sh -e NAMES=/out/app.names \
	-w /src "$image" \
	tinygo build -target wasm -no-debug -scheduler=asyncify -panic-unwind=none -stack-size=1MB \
	-o /out/app.wasm ./cmd/solo-web
"$engine" run --rm "$image" cat /usr/local/tinygo/targets/wasm_exec.js >"$out/wasm_exec.js"

cp "$root"/cmd/solo-web/web/* "$out/"
cp "$jamb/scripts/web/jamb.js" "$out/jamb.js"
chmod u+w "$out"/*
# The fonts a jamb page fetches (jamb's internal/text/fonts_js.go); its script writes them.
(cd "$jamb" && GOWORK=off go run scripts/web/fonts.go "$out/fonts")
ls -l "$out/app.wasm"
echo "built $out"
