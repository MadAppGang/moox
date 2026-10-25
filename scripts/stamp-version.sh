#!/bin/sh
# stamp-version.sh VERSION — write share/moox/VERSION (the share root's version
# stamp that `moox doctor` compares with the binary's, §8.4, PR-M14).
#
# Run by GoReleaser's `before` hook with {{ .Version }} (no leading v). The file
# is generated, ignored by version control, and shipped inside every archive.
set -eu
[ $# -eq 1 ] || {
	echo "usage: stamp-version.sh VERSION" >&2
	exit 2
}
v=${1#v}
case $v in
'' | *[!0-9A-Za-z.+-]*)
	echo "stamp-version: not a version: $1" >&2
	exit 2
	;;
esac
root=$(cd "$(dirname "$0")/.." && pwd)
printf '%s\n' "$v" >"$root/share/moox/VERSION"
