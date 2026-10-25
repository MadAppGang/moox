#!/bin/sh
# share-tracked-only.sh [DIR] — refuse a share/moox that holds files version
# control does not track (GoReleaser's `before` hook, §9.1).
#
# The archives take share/moox as a DIRECTORY, so every file in it ships —
# ignored ones too. privacy-scan.sh --tree judges only what a commit would
# hold (tracked + untracked-not-ignored), so an ignored file there (an editor
# or agent state dir such as .claude/, a stray build output) would reach a
# public archive unscanned. The one generated file the archives must carry,
# share/moox/VERSION (stamp-version.sh), is allowed.
#
# DIR is the module root (default: this script's ..). Exit 0: only tracked
# files (or no checkout to compare with: a source tarball build). Exit 1: the
# stray files are listed. Exit 2: usage, or git failed.
set -eu
[ $# -le 1 ] || {
	echo "usage: share-tracked-only.sh [DIR]" >&2
	exit 2
}
root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
[ -d "$root/share/moox" ] || {
	echo "share-tracked-only: no share/moox under $root" >&2
	exit 2
}
if ! (cd "$root" && git rev-parse --is-inside-work-tree) >/dev/null 2>&1; then
	echo "share-tracked-only: $root is not a checkout; nothing to compare"
	exit 0
fi
# --others without --exclude-standard: untracked files, ignored ones included.
stray=$(cd "$root" && git ls-files --others -- share/moox) || {
	echo "share-tracked-only: git ls-files failed" >&2
	exit 2
}
stray=$(printf '%s\n' "$stray" | grep -v -x -e 'share/moox/VERSION' -e '' || true)
if [ -n "$stray" ]; then
	printf '%s\n' "$stray" | sed 's/^/share-tracked-only: not tracked, would ship: /'
	echo "share-tracked-only: move these out of share/moox (the archives take the whole directory)"
	exit 1
fi
echo "share-tracked-only: share/moox holds only tracked files"
