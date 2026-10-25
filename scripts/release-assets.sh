#!/bin/sh
# release-assets.sh — scan dist/, render moox.rb, create the GitHub release ONCE (§9.7, R3-H4).
#
#   scripts/release-assets.sh [--denylist FILE] [--dist DIR] [--no-create] vVERSION
#
# Runs after `goreleaser release --clean --skip=publish` (which builds dist/ and
# contacts no release API). In order, stopping at the first failure:
#   1. scripts/scan-dist.sh over DIST (public patterns; --denylist adds the
#      private list, which only release-local.sh has — CI does not);
#   2. packaging/render-formula.sh vVERSION DIST/checksums.txt DIST/moox.rb;
#   3. one `gh release create vVERSION <4 tarballs> checksums.txt moox.rb
#      -R $MOOX_REPO --verify-tag --title vVERSION` — the complete asset set in
#      one creation, so a failed scan or render publishes nothing.
# --no-create stops after step 2 (local checks). MOOX_REPO defaults to
# MadAppGang/moox.
set -eu

usage() {
	echo "usage: release-assets.sh [--denylist FILE] [--dist DIR] [--no-create] vVERSION" >&2
	exit 2
}

root=$(cd "$(dirname "$0")/.." && pwd)
dist="$root/dist"
deny=""
create=1
while [ $# -gt 0 ]; do
	case $1 in
	--denylist)
		[ $# -ge 2 ] || usage
		deny=$2
		shift 2
		;;
	--dist)
		[ $# -ge 2 ] || usage
		dist=$2
		shift 2
		;;
	--no-create)
		create=0
		shift
		;;
	-h | --help)
		sed -n '2,16p' "$0"
		exit 0
		;;
	-*) usage ;;
	*) break ;;
	esac
done
[ $# -eq 1 ] || usage
tag=v${1#v}
repo=${MOOX_REPO:-MadAppGang/moox}

if [ -n "$deny" ]; then
	"$root/scripts/scan-dist.sh" --denylist "$deny" "$dist"
else
	"$root/scripts/scan-dist.sh" "$dist"
fi
"$root/packaging/render-formula.sh" "$tag" "$dist/checksums.txt" "$dist/moox.rb"
echo "release-assets: rendered $dist/moox.rb for $tag"

if [ "$create" -eq 0 ]; then
	exit 0
fi
set --
for a in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
	set -- "$@" "$dist/moox_$a.tar.gz"
done
gh release create "$tag" "$@" "$dist/checksums.txt" "$dist/moox.rb" \
	-R "$repo" --verify-tag --title "$tag" --notes "moox $tag"
echo "release-assets: created release $tag on $repo"
