#!/usr/bin/env bash
# share-tracked-only.test.sh — the release refuses a share/moox holding files
# version control does not track (scripts/share-tracked-only.sh, GoReleaser's
# `before` hook). The archives take share/moox whole, so an ignored agent state
# dir (.claude/.coaching/…) once reached a snapshot archive past privacy-scan
# --tree, which judges only what a commit would hold.
#
# Runs on a throwaway checkout in a temp dir; the real tree is only read (the
# guard over it must pass).
set -u
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"

pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/       /' | head -20; }

T=$(mktemp -d "${TMPDIR:-/tmp}/share-tracked.XXXXXX")
trap 'rm -rf "$T"' EXIT
R="$T/moox"
mkdir -p "$R/scripts" "$R/share/moox/plugins/p"
cp "$root/scripts/share-tracked-only.sh" "$R/scripts/"
printf 'set -g x 1\n' >"$R/share/moox/moox.conf"
printf 'p\n' >"$R/share/moox/plugins/p/p.tmux"
printf '/share/moox/VERSION\n.claude/\n' >"$R/.gitignore"
g() { git -C "$R" -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false "$@" >/dev/null 2>&1; }
g init -q && g add -A && g commit -q -m init || { echo "ABORT: cannot make the temp checkout"; exit 1; }

run() { out=$(sh "$R/scripts/share-tracked-only.sh" 2>&1); rc=$?; }

run
if [ "$rc" -eq 0 ]; then ok "only tracked files: passes"; else no "clean tree rc=$rc" "$out"; fi

printf '0.1.0\n' >"$R/share/moox/VERSION"
run
if [ "$rc" -eq 0 ]; then ok "the generated VERSION stamp is allowed"; else no "VERSION rc=$rc" "$out"; fi

mkdir -p "$R/share/moox/.claude/.coaching" "$R/share/moox/plugins/p/.claude"
printf '{}\n' >"$R/share/moox/.claude/.coaching/state.json"
printf 'x\n' >"$R/share/moox/plugins/p/.claude/notes.md"
run
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q 'share/moox/.claude/.coaching/state.json' &&
	printf '%s' "$out" | grep -q 'share/moox/plugins/p/.claude/notes.md'; then
	ok "an IGNORED file under share/moox is refused, and named"
else no "ignored stray rc=$rc" "$out"; fi
rm -rf "$R/share/moox/.claude" "$R/share/moox/plugins/p/.claude"

printf 'new\n' >"$R/share/moox/untracked.conf"
run
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q 'share/moox/untracked.conf'; then ok "an untracked, not ignored file is refused too"; else no "untracked rc=$rc" "$out"; fi
rm -f "$R/share/moox/untracked.conf"

rm -rf "$R/.git"
run
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'not a checkout'; then ok "no checkout (a source tarball build): nothing to compare, passes"; else no "no checkout rc=$rc" "$out"; fi

if grep -q 'scripts/share-tracked-only.sh' "$root/.goreleaser.yaml"; then ok "GoReleaser's before hook runs it"; else no "not wired into .goreleaser.yaml"; fi

out=$(sh "$root/scripts/share-tracked-only.sh" 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then ok "the real share/moox holds only tracked files"; else no "real tree rc=$rc" "$out"; fi

echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
