#!/usr/bin/env bash
# Builds dist/Solo-<version>-windows-amd64.zip: solo.exe (with its icon and version info, by jamb
# package) and the compiled sidecar, solo-agent.exe, beside it. Runs on Linux or macOS as well as
# Windows. Needs go and bun. VERSION (MAJOR.MINOR.PATCH) defaults to jamb.json's.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
dist=$root/dist
mkdir -p "$dist"

bun=$(command -v bun || echo "$HOME/.bun/bin/bun")
[ -x "$bun" ] || { echo "bun not found: install it from https://bun.sh" >&2; exit 1; }

version=${VERSION:-$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' cmd/solo/jamb.json)}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "building sidecar"
(cd sidecar && "$bun" install --frozen-lockfile >/dev/null &&
	"$bun" build src/index.ts --compile --target=bun-windows-x64 --outfile "$work/solo-agent.exe" >/dev/null)

echo "building solo $version"
# jamb package reads jamb.json from the app's folder, and the folder must be in this module: build
# from a copy there with the release's version.
app=cmd/solo/winbuild
trap 'rm -rf "$work" "$root/$app"' EXIT
rm -rf "$app" && mkdir -p "$app"
cp cmd/solo/*.go cmd/solo/icon.png "$app/"
jv=${version%%-*} # Windows version info takes MAJOR.MINOR.PATCH only, no pre-release suffix
sed "s/\"version\": *\"[^\"]*\"/\"version\": \"$jv\"/" cmd/solo/jamb.json >"$app/jamb.json"
go run github.com/tyler-builds/jamb/cmd/jamb@v0.4.2 package "./$app"

zip=$dist/Solo-$version-windows-amd64.zip
cp "$app/dist/solo-$jv-windows-amd64.zip" "$zip"
(cd "$work" && zip -q -j "$zip" solo-agent.exe)
echo "$zip ($(du -h "$zip" | cut -f1))"
