#!/bin/sh
# tap-publish.sh — put a release's rendered moox.rb into the Homebrew tap (§9.2, D16).
#
#   packaging/tap-publish.sh [--file moox.rb] [--dry-run] VERSION
#
# Without --file, downloads moox.rb from the GitHub release vVERSION of
# $MOOX_REPO (default MadAppGang/moox). Then writes it to Formula/moox.rb of
# $MOOX_TAP_REPO (default MadAppGang/homebrew-tap) through the contents API —
# no clone. Auth is whatever `gh` uses: GH_TOKEN (CI passes HOMEBREW_TAP_TOKEN,
# a fine-grained PAT with contents read/write on the tap only) or the local
# `gh auth` login. --dry-run prints the API call instead of making it.
set -eu

usage() {
	echo "usage: tap-publish.sh [--file moox.rb] [--dry-run] VERSION" >&2
	exit 2
}

file=""
dry=0
while [ $# -gt 0 ]; do
	case $1 in
	--file)
		[ $# -ge 2 ] || usage
		file=$2
		shift 2
		;;
	--dry-run)
		dry=1
		shift
		;;
	-h | --help)
		sed -n '2,13p' "$0"
		exit 0
		;;
	-*) usage ;;
	*) break ;;
	esac
done
[ $# -eq 1 ] || usage
version=${1#v}
repo=${MOOX_REPO:-MadAppGang/moox}
tap=${MOOX_TAP_REPO:-MadAppGang/homebrew-tap}
path=Formula/moox.rb

command -v gh >/dev/null 2>&1 || {
	echo "tap-publish: gh is required" >&2
	exit 1
}

work=$(mktemp -d "${TMPDIR:-/tmp}/tap-publish.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

if [ -z "$file" ]; then
	gh release download "v$version" -R "$repo" -p moox.rb -D "$work"
	file="$work/moox.rb"
fi
[ -s "$file" ] || {
	echo "tap-publish: empty or missing formula: $file" >&2
	exit 1
}
grep -q "version \"$version\"" "$file" || {
	echo "tap-publish: $file is not the formula for $version" >&2
	exit 1
}

content=$(base64 <"$file" | tr -d '\n')
if [ "$dry" -eq 1 ]; then
	# No network at all: not even the read of the current blob sha.
	printf 'would run: gh api -X PUT repos/%s/contents/%s -f message="moox %s" -f content=<%s bytes base64> [-f sha=<current blob>]\n' \
		"$tap" "$path" "$version" "${#content}"
	exit 0
fi
# The current blob sha is required to update an existing file; absent = create.
# G4-H1: on a 404 `gh api` prints the JSON error body to stdout WITHOUT applying
# --jq and exits 1, so the output is kept only when gh succeeded AND it is a blob
# sha (40 hex). A 404 means "create"; any other failure stops here.
sha=""
if out=$(gh api "repos/$tap/contents/$path" --jq .sha 2>"$work/get.err"); then
	case $out in
	*[!0-9a-f]* | '')
		echo "tap-publish: unexpected blob sha for $tap $path: $out" >&2
		exit 1
		;;
	esac
	[ "${#out}" -eq 40 ] || {
		echo "tap-publish: unexpected blob sha for $tap $path: $out" >&2
		exit 1
	}
	sha=$out
elif printf '%s\n' "$out" | cat - "$work/get.err" | grep -q -e '"status": *"404"' -e 'HTTP 404' -e 'Not Found'; then
	# A 404 is also what a private tap the token cannot see answers, and what a
	# wrong tap name answers (G4-2-L1). "Create" only when the repo itself is
	# visible; otherwise say so here instead of failing later in the PUT.
	if ! gh api "repos/$tap" --jq .full_name >/dev/null 2>"$work/repo.err"; then
		echo "tap-publish: cannot see the tap repo $tap (a wrong name, or a token without access):" >&2
		head -5 "$work/repo.err" >&2
		exit 1
	fi
	: # no Formula/moox.rb yet: the first publish creates it
else
	echo "tap-publish: cannot read $tap $path:" >&2
	printf '%s\n' "$out" | cat - "$work/get.err" | head -5 >&2
	exit 1
fi

set -- api -X PUT "repos/$tap/contents/$path" \
	-f message="moox $version" -f content="$content"
if [ -n "$sha" ]; then
	set -- "$@" -f sha="$sha"
fi
gh "$@" --jq .commit.sha
echo "tap-publish: $tap $path -> moox $version"
