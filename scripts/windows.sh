#!/usr/bin/env bash
# Builds Solo for Windows with jamb package: dist/Solo-<version>-windows-amd64.zip and, when NSIS's
# makensis is installed, dist/Solo-<version>-windows-amd64-setup.exe, which installs it for the user.
# Both hold solo.exe (with its icon and version info) and the compiled sidecar, solo-agent.exe.
# Runs on Linux or macOS as well as Windows. Needs go and bun. VERSION defaults to jamb.json's.
# JAMB is the jamb command to run (default: the released one, through go run).
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
cp "$work/solo-agent.exe" "$app/"
# The version Settings shows (scripts/version.sh). jamb package links the app with its own flags, so
# it is set by a file of the copy instead of by the linker.
printf 'package main\n\nimport "github.com/tyler-builds/solo/internal/version"\n\nfunc init() { version.Version = "%s" }\n' \
	"$(scripts/version.sh)" >"$app/version_set.go"
installer=()
if command -v makensis >/dev/null; then
	installer=(--installer)
else
	echo "makensis not found: building the zip only (install NSIS for the installer)" >&2
fi
${JAMB:-go run github.com/tyler-builds/jamb/cmd/jamb@v0.4.3} package "${installer[@]}" "./$app"

zip=$dist/Solo-$version-windows-amd64.zip
cp "$app/dist/solo-$jv-windows-amd64.zip" "$zip"
echo "$zip ($(du -h "$zip" | cut -f1))"
if [ -f "$app/dist/solo-$jv-windows-amd64-setup.exe" ]; then
	setup=$dist/Solo-$version-windows-amd64-setup.exe
	cp "$app/dist/solo-$jv-windows-amd64-setup.exe" "$setup"
	echo "$setup ($(du -h "$setup" | cut -f1))"
fi
