#!/usr/bin/env bash
# Builds dist/Solo-x86_64.AppImage: the Go app, the compiled Bun sidecar and the SDK's claude binary.
# Needs go, bun, mksquashfs and magick. The AppImage runtime is downloaded once into
# ~/.cache/solo-build, or taken from $APPIMAGE_RUNTIME.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
app_id=io.github.tyler_builds.solo
dist=$root/dist
appdir=$dist/Solo.AppDir
out=$dist/Solo-x86_64.AppImage

bun=$(command -v bun || echo "$HOME/.bun/bin/bun")
[ -x "$bun" ] || { echo "bun not found: install it from https://bun.sh" >&2; exit 1; }
export PATH="$(dirname "$bun"):$PATH" # package scripts call bun by name

runtime=${APPIMAGE_RUNTIME:-$HOME/.cache/solo-build/runtime-x86_64}
if [ ! -f "$runtime" ]; then
	mkdir -p "$(dirname "$runtime")"
	curl -fsSL -o "$runtime.part" \
		https://github.com/AppImage/type2-runtime/releases/download/continuous/runtime-x86_64
	mv "$runtime.part" "$runtime"
fi

echo "building sidecar"
(cd sidecar && "$bun" install --frozen-lockfile >/dev/null && "$bun" run build >/dev/null)
claude=sidecar/node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude
[ -x "$claude" ] || { echo "missing $claude: run bun install in sidecar/" >&2; exit 1; }

echo "building solo"
rm -rf "$appdir"
mkdir -p "$appdir/usr/bin" "$appdir/usr/share/applications" "$appdir/usr/share/icons/hicolor/256x256/apps"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$appdir/usr/bin/solo" ./cmd/solo
cp bin/solo-agent "$appdir/usr/bin/solo-agent"
cp "$claude" "$appdir/usr/bin/claude"

magick -background none -density 384 internal/gui/brand/solo.svg -resize 256x256 \
	"$appdir/usr/share/icons/hicolor/256x256/apps/$app_id.png"
cp "$appdir/usr/share/icons/hicolor/256x256/apps/$app_id.png" "$appdir/$app_id.png"
ln -s "$app_id.png" "$appdir/.DirIcon"

cat >"$appdir/usr/share/applications/$app_id.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Solo
Comment=Code with Claude agents
Exec=solo
Icon=$app_id
Categories=Development;
Terminal=false
StartupWMClass=$app_id
EOF
ln -s "usr/share/applications/$app_id.desktop" "$appdir/$app_id.desktop"

cat >"$appdir/AppRun" <<'EOF'
#!/bin/sh
here=$(dirname "$(readlink -f "$0")")
exec "$here/usr/bin/solo" "$@"
EOF
chmod +x "$appdir/AppRun"

echo "packing"
rm -f "$out" "$dist/solo.squashfs"
mksquashfs "$appdir" "$dist/solo.squashfs" -root-owned -noappend -comp zstd -quiet
cat "$runtime" "$dist/solo.squashfs" >"$out"
chmod +x "$out"
rm "$dist/solo.squashfs"
echo "$out ($(du -h "$out" | cut -f1))"
