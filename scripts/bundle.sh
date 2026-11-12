#!/bin/sh
# bundle.sh builds what Solo deploys to Cloudflare, the relay and the web client, and puts them where
# the app's build picks them up (internal/remote/bundle/files). Run it before building a Solo that can
# set up remote access; scripts/appimage.sh does.
#
# The version is the time of the build, so a newer build's bundle replaces an older one's on the
# relay, and never the reverse.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
scripts/web.sh "$root/dist/web"
scripts/relay.sh

dest=internal/remote/bundle/files
rm -rf "$dest/web" "$dest/worker.js" "$dest/version"
mkdir -p "$dest/web"
cp -R dist/web/. "$dest/web/"
cp dist/relay/worker.js "$dest/worker.js"
date +%s >"$dest/version"
echo "bundle $(cat "$dest/version"): $(du -sh "$dest" | cut -f1) in $dest"
