#!/bin/sh
# version.sh prints the version a build of Solo shows in Settings: VERSION when a release sets it,
# or else git's description of the commit without the tag's "v" ("0.6.0-3-gcb015ff" is three commits
# after 0.6.0, with "-dirty" when files it tracks are changed). Outside a git checkout it prints "dev".
set -eu

if [ -n "${VERSION:-}" ]; then
	v=$VERSION
else
	cd "$(dirname "$0")/.."
	v=$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
fi
# It goes into a linker flag and a Go string: nothing but what a version is made of.
case ${v:-dev} in
*[!0-9A-Za-z.+-]*) echo dev ;;
*) echo "${v:-dev}" ;;
esac
