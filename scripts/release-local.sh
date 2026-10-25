#!/bin/sh
# release-local.sh — the fallback release when GitHub Actions does not run (§9.7 step 5).
#
#   scripts/release-local.sh --denylist FILE [--src DIR] [--no-tap] vVERSION
#
# Builds from the PUBLISHED orphan commit, never from a development checkout:
# `git clone --depth 1 --branch vVERSION https://github.com/$MOOX_REPO` into a
# temp dir (--src DIR uses an existing clean copy instead; tests use it). One
# sequence that publishes once, last (R3-H4):
#   (a) `gh release view vVERSION` must FAIL — an existing (partial) release is
#       never uploaded into; the script stops and prints the delete command;
#   (b) GORELEASER_CURRENT_TAG=vVERSION goreleaser release --clean --skip=publish
#       (no --snapshot: the version is the tag, so `brew test` matches);
#   (c)-(e) scripts/release-assets.sh --denylist FILE: scan dist/ with the public
#       patterns AND the private denylist, render moox.rb, one `gh release create`
#       with all six assets;
#   (f) packaging/tap-publish.sh vVERSION (skipped with --no-tap).
# MOOX_REPO defaults to MadAppGang/moox. Needs gh (authenticated) and goreleaser.
set -eu

usage() {
	echo "usage: release-local.sh --denylist FILE [--src DIR] [--no-tap] vVERSION" >&2
	exit 2
}

deny=""
src=""
tap=1
while [ $# -gt 0 ]; do
	case $1 in
	--denylist)
		[ $# -ge 2 ] || usage
		deny=$2
		shift 2
		;;
	--src)
		[ $# -ge 2 ] || usage
		src=$2
		shift 2
		;;
	--no-tap)
		tap=0
		shift
		;;
	-h | --help)
		sed -n '2,20p' "$0"
		exit 0
		;;
	-*) usage ;;
	*) break ;;
	esac
done
[ $# -eq 1 ] || usage
[ -n "$deny" ] || {
	echo "release-local: --denylist is required (the private list, outside the public tree)" >&2
	exit 2
}
[ -r "$deny" ] || {
	echo "release-local: cannot read denylist $deny" >&2
	exit 2
}
deny=$(cd "$(dirname "$deny")" && pwd)/$(basename "$deny")
tag=v${1#v}
repo=${MOOX_REPO:-MadAppGang/moox}

# (a) never upload into an existing release.
if gh release view "$tag" -R "$repo" >/dev/null 2>&1; then
	echo "release-local: release $tag already exists on $repo; not uploading into it." >&2
	echo "  inspect it, then: gh release delete $tag -R $repo" >&2
	exit 1
fi

if [ -z "$src" ]; then
	work=$(mktemp -d "${TMPDIR:-/tmp}/moox-release.XXXXXX")
	trap 'rm -rf "$work"' EXIT HUP INT TERM
	git clone --quiet --depth 1 --branch "$tag" "https://github.com/$repo" "$work/moox"
	src="$work/moox"
fi
cd "$src"

# (b) build only: contacts no release API.
GORELEASER_CURRENT_TAG=$tag goreleaser release --clean --skip=publish

# (c)-(e) scan (public + private), render, create once.
./scripts/release-assets.sh --denylist "$deny" "$tag"

# (f) the tap formula.
if [ "$tap" -eq 1 ]; then
	./packaging/tap-publish.sh "$tag"
else
	echo "release-local: tap not published (--no-tap); run: packaging/tap-publish.sh $tag"
fi
