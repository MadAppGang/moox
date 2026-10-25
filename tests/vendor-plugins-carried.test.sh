#!/usr/bin/env bash
# vendor-plugins-carried.test.sh — a carried plugin is never re-vendored over (G4-M9).
#
# Runs scripts/vendor-plugins.sh on a temp COPY of the tree pieces it reads
# (scripts/, share/moox/plugins/PINS, share/moox/plugins/<carried>/): --carried
# is refused, and asking for the carried plugin by name skips it untouched.
# Nothing is cloned: the --from dir holds no checkout, so a fetch would fail.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"

pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/       /' | head -20; }

T=$(mktemp -d "${TMPDIR:-/tmp}/vendor-carried.XXXXXX")
trap 'rm -rf "$T"' EXIT
R="$T/moox"
mkdir -p "$R/share/moox/plugins" "$T/from"
cp -R "$root/scripts" "$R/"
cp "$root/share/moox/plugins/PINS" "$R/share/moox/plugins/PINS"
carried=$(awk '!/^#/ && $6 == "carried" { print $1; exit }' "$R/share/moox/plugins/PINS")
[ -n "$carried" ] || { echo "ABORT: PINS has no carried plugin"; exit 1; }
mkdir -p "$R/share/moox/plugins/$carried"
printf 'moox edit C1\n' >"$R/share/moox/plugins/$carried/edited.sh"

out=$(bash "$R/scripts/vendor-plugins.sh" --carried --from "$T/from" 2>&1)
rc=$?
if [ "$rc" -eq 2 ] && printf '%s' "$out" | grep -q 'THIRD_PARTY.md'; then ok "--carried is refused (exit 2) and points at THIRD_PARTY.md"; else no "--carried rc=$rc" "$out"; fi

out=$(bash "$R/scripts/vendor-plugins.sh" --only "$carried" --from "$T/from" 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "$carried is carried" &&
	[ "$(cat "$R/share/moox/plugins/$carried/edited.sh")" = "moox edit C1" ]; then
	ok "--only $carried: skipped, its moox edits untouched"
else
	no "--only $carried rc=$rc" "$out"
fi

echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
