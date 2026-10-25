#!/bin/sh
# scan-dist.sh — privacy-scan exactly what a release ships (§9.1, §9.6, V9).
#
#   scripts/scan-dist.sh [--denylist FILE] [--allow-email ADDR]... [DIST]
#
# DIST defaults to ./dist. For every moox_<os>_<arch>.tar.gz in it:
#   - the archive LISTING (`tar -tvzf`): every entry must be owned by root:root
#     (tar records the build user's login otherwise), and the listing text is
#     scanned like a file (names, owners);
#   - each of the three binaries, as `strings` (privacy-scan.sh --binary);
#   - every other extracted file (privacy-scan.sh PATH mode: contents + names).
# Plus checksums.txt and moox.rb when present. The options are passed through
# to privacy-scan.sh (publish.sh and release-local.sh add the private denylist;
# CI has only the public patterns). Exit 0 clean, 1 on any hit, 2 usage.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
scan="$here/privacy-scan.sh"
opts=""
while [ $# -gt 0 ]; do
	case $1 in
	--denylist | --allow-email)
		[ $# -ge 2 ] || {
			echo "scan-dist: $1 needs a value" >&2
			exit 2
		}
		# Values are paths/addresses without spaces (checked here).
		case $2 in *[[:space:]]*)
			echo "scan-dist: $1 value has whitespace" >&2
			exit 2
			;;
		esac
		opts="$opts $1 $2"
		shift 2
		;;
	-h | --help)
		sed -n '2,15p' "$0"
		exit 0
		;;
	-*)
		echo "scan-dist: unknown option $1" >&2
		exit 2
		;;
	*) break ;;
	esac
done
dist=${1:-dist}
[ -d "$dist" ] || {
	echo "scan-dist: no dist dir: $dist" >&2
	exit 2
}

work=$(mktemp -d "${TMPDIR:-/tmp}/scan-dist.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
rc=0
n=0
for tgz in "$dist"/moox_*.tar.gz; do
	[ -f "$tgz" ] || continue
	n=$((n + 1))
	name=$(basename "$tgz" .tar.gz)
	mkdir "$work/$name"
	tar -tvzf "$tgz" >"$work/$name.listing"
	# bsdtar prints "owner group", GNU tar "owner/group".
	if grep -v -E '[[:space:]]root[[:space:]/]+root[[:space:]]' "$work/$name.listing" >"$work/$name.badowner"; then
		echo "owner  $tgz: entries not owned by root:root:"
		head -5 "$work/$name.badowner"
		rc=1
	fi
	# shellcheck disable=SC2086 # opts is a list of flag/value words without spaces
	"$scan" $opts "$work/$name.listing" || rc=1
	tar -xzf "$tgz" -C "$work/$name"
	for b in moox tt rpaster; do
		if [ ! -f "$work/$name/$b" ]; then
			echo "missing  $tgz has no $b at its root"
			rc=1
			continue
		fi
		# shellcheck disable=SC2086
		"$scan" $opts --binary "$work/$name/$b" || rc=1
		rm -f "$work/$name/$b"
	done
	# shellcheck disable=SC2086
	"$scan" $opts "$work/$name" || rc=1
done
[ "$n" -gt 0 ] || {
	echo "scan-dist: no moox_*.tar.gz in $dist" >&2
	exit 1
}
for f in "$dist/checksums.txt" "$dist/moox.rb"; do
	[ -f "$f" ] || continue
	# shellcheck disable=SC2086
	"$scan" $opts "$f" || rc=1
done
if [ "$rc" -eq 0 ]; then
	echo "scan-dist: clean ($n archives) in $dist"
fi
exit "$rc"
