#!/bin/sh
# render-formula.sh — fill packaging/formula.rb.tmpl from a release's checksums.txt.
#
#   packaging/render-formula.sh [--url-base URL] VERSION CHECKSUMS OUT
#
# VERSION      the release version, with or without the leading v (v0.1.0 or 0.1.0)
# CHECKSUMS    GoReleaser's checksums.txt ("<sha256>  moox_<os>_<arch>.tar.gz")
# OUT          the formula to write (moox.rb), replaced atomically
# --url-base   where the four archives live; default
#              https://github.com/MadAppGang/moox/releases/download/v<VERSION>
#              (versioned, so the pinned sha256 values always match). The V8
#              local-tap test passes a file:// or http://127.0.0.1 URL.
#
# Refuses (exit 1, OUT untouched) when any of the four archive lines is missing
# or its hash is not 64 hex digits. Exit 2: usage.
set -eu

usage() {
	echo "usage: render-formula.sh [--url-base URL] VERSION CHECKSUMS OUT" >&2
	exit 2
}

base=""
while [ $# -gt 0 ]; do
	case $1 in
	--url-base)
		[ $# -ge 2 ] || usage
		base=$2
		shift 2
		;;
	-h | --help)
		sed -n '2,17p' "$0"
		exit 0
		;;
	-*) usage ;;
	*) break ;;
	esac
done
[ $# -eq 3 ] || usage
version=${1#v}
sums=$2
out=$3
case $version in
'' | *[!0-9A-Za-z.+-]*)
	echo "render-formula: not a version: $1" >&2
	exit 2
	;;
esac
[ -r "$sums" ] || {
	echo "render-formula: cannot read $sums" >&2
	exit 1
}
[ -n "$base" ] || base="https://github.com/MadAppGang/moox/releases/download/v$version"
base=${base%/}

tmpl="$(cd "$(dirname "$0")" && pwd)/formula.rb.tmpl"
[ -r "$tmpl" ] || {
	echo "render-formula: no template at $tmpl" >&2
	exit 1
}

sha_of() { # sha_of <asset>: the hash of exactly that file name, or nothing
	awk -v f="$1" '$2 == f || $2 == "*" f { print $1; exit }' "$sums"
}

missing=""
for pair in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
	h=$(sha_of "moox_$pair.tar.gz")
	case $h in
	????????????????????????????????????????????????????????????????) ;;
	*) h="" ;;
	esac
	case $h in *[!0-9a-f]*) h="" ;; esac
	if [ -z "$h" ]; then
		missing="$missing moox_$pair.tar.gz"
	fi
	eval "sha_$pair=\$h"
done
if [ -n "$missing" ]; then
	echo "render-formula: $sums has no valid sha256 for:$missing" >&2
	exit 1
fi

tmp="$out.tmp.$$"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
# shellcheck disable=SC2154 # sha_* are set through eval above
awk -v version="$version" -v base="$base" \
	-v da="$sha_darwin_arm64" -v di="$sha_darwin_amd64" \
	-v la="$sha_linux_arm64" -v li="$sha_linux_amd64" '
	# gsub treats & and \ in the replacement specially; a URL may carry either.
	function lit(s) { gsub(/\\/, "\\\\", s); gsub(/&/, "\\\\&", s); return s }
	BEGIN { base = lit(base) }
	{
		gsub(/\{\{VERSION\}\}/, version)
		gsub(/\{\{URL_BASE\}\}/, base)
		gsub(/\{\{SHA_DARWIN_ARM64\}\}/, da)
		gsub(/\{\{SHA_DARWIN_AMD64\}\}/, di)
		gsub(/\{\{SHA_LINUX_ARM64\}\}/, la)
		gsub(/\{\{SHA_LINUX_AMD64\}\}/, li)
		print
	}' "$tmpl" >"$tmp"
if grep -q '{{' "$tmp"; then
	echo "render-formula: unfilled placeholder left in the formula" >&2
	exit 1
fi
mv "$tmp" "$out"
trap - EXIT HUP INT TERM
