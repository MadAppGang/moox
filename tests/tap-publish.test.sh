#!/usr/bin/env bash
# tap-publish.test.sh — packaging/tap-publish.sh reads the current blob sha safely (G4-H1).
#
# A fake `gh` on PATH plays the contents API the way gh 2.x does:
#   404     — the JSON error body on STDOUT (--jq is not applied), "HTTP 404" on
#             stderr, exit 1: the first publish must PUT with NO sha.
#   exists  — the blob sha (40 hex) on stdout, exit 0: the PUT carries it.
#   401     — an auth error, exit 1: stop, no PUT at all.
#   garbage — exit 0 but not a sha: stop, no PUT.
#   norepo  — the contents read AND the repo read 404 (a private tap the token
#             cannot see, or a wrong name): stop, no PUT (G4-2-L1).
# Nothing is contacted.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
tp="$here/../packaging/tap-publish.sh"

pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/       /' | head -20; }

T=$(mktemp -d "${TMPDIR:-/tmp}/tap-publish.XXXXXX")
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin"
cat >"$T/bin/gh" <<'EOF'
#!/bin/sh
echo "FAKE gh $*" >>"$FAKE_LOG"
case "$*" in
*"-X PUT"*) echo 0123456789abcdef0123456789abcdef01234567; exit 0 ;;
*/contents/*) ;;
"api repos/"*)
	if [ "$FAKE_MODE" = norepo ]; then
		printf '{"message":"Not Found","status":"404"}'
		echo "gh: Not Found (HTTP 404)" >&2
		exit 1
	fi
	echo MadAppGang/homebrew-tap; exit 0 ;;
esac
case $FAKE_MODE in
404 | norepo)
	printf '{"message":"Not Found","documentation_url":"https://docs.github.com/rest/repos/contents#get-repository-content","status":"404"}'
	echo "gh: Not Found (HTTP 404)" >&2
	exit 1 ;;
exists) echo 3b18e512dba79e4c8300dd08aeb37f8e728b8dad; exit 0 ;;
401)
	printf '{"message":"Bad credentials","status":"401"}'
	echo "gh: Bad credentials (HTTP 401)" >&2
	exit 1 ;;
garbage) echo '{"weird":true}'; exit 0 ;;
esac
exit 0
EOF
chmod +x "$T/bin/gh"
printf 'class Moox < Formula\n  version "0.1.0"\nend\n' >"$T/moox.rb"

run() { # run MODE — output in $out, status in $rc, gh calls in $T/log
	: >"$T/log"
	out=$(env PATH="$T/bin:$PATH" FAKE_LOG="$T/log" FAKE_MODE="$1" "$tp" --file "$T/moox.rb" 0.1.0 2>&1)
	rc=$?
	put=$(grep -e '-X PUT' "$T/log" || true)
}

echo "=== first publish: the file does not exist yet (404 body on stdout) ==="
run 404
if [ "$rc" -eq 0 ] && [ -n "$put" ] && ! printf '%s' "$put" | grep -q 'sha='; then
	ok "404: PUT without a sha (create)"
else
	no "404: rc=$rc" "$out
$(cat "$T/log")"
fi
if printf '%s' "$put" | grep -q 'Not Found\|status'; then no "the 404 body leaked into the PUT" "$put"; else ok "404: the error body is not sent as anything"; fi

echo "=== update: the file exists ==="
run exists
if [ "$rc" -eq 0 ] && printf '%s' "$put" | grep -q 'sha=3b18e512dba79e4c8300dd08aeb37f8e728b8dad'; then
	ok "exists: PUT carries the current blob sha"
else
	no "exists: rc=$rc" "$out
$(cat "$T/log")"
fi

echo "=== any other read failure stops before the PUT ==="
run 401
if [ "$rc" -ne 0 ] && [ -z "$put" ]; then ok "401: exit $rc, no PUT"; else no "401: rc=$rc" "$out
$(cat "$T/log")"; fi
run norepo
if [ "$rc" -ne 0 ] && [ -z "$put" ] && printf '%s' "$out" | grep -q 'cannot see'; then ok "a tap the token cannot see: exit $rc, no PUT"; else no "norepo: rc=$rc" "$out
$(cat "$T/log")"; fi
run garbage
if [ "$rc" -ne 0 ] && [ -z "$put" ]; then ok "a non-sha answer: exit $rc, no PUT"; else no "garbage: rc=$rc" "$out
$(cat "$T/log")"; fi

echo "=== --dry-run contacts nothing ==="
: >"$T/log"
out=$(env PATH="$T/bin:$PATH" FAKE_LOG="$T/log" FAKE_MODE=404 "$tp" --dry-run --file "$T/moox.rb" 0.1.0 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && [ ! -s "$T/log" ]; then ok "--dry-run: no gh call"; else no "dry-run rc=$rc" "$out"; fi

echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
