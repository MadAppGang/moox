#!/usr/bin/env bash
# release-local-order.test.sh — scripts/release-local.sh publishes once, last (§9.7 step 5, R3-H4).
#
# Fake `gh` and `goreleaser` on PATH record every call. Asserts the order
#   gh release view (must fail) -> goreleaser release --clean --skip=publish
#   -> scan-dist -> render-formula -> exactly ONE gh release create -> tap
# and that an existing release, or a privacy hit, stops before any create.
# Runs on a temp copy of the scripts (--src); nothing is contacted.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"

pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/       /' | head -30; }

T=$(mktemp -d "${TMPDIR:-/tmp}/release-order.XXXXXX")
trap 'rm -rf "$T"' EXIT

# Root-owned tar entries, whichever tar this is.
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
	tarown=(--owner=root --group=root)
else
	tarown=(--uname root --gname root --uid 0 --gid 0)
fi

mkfake() { # mkfake DIR — the fake tools and a source copy
	local d=$1
	mkdir -p "$d/bin" "$d/src"
	cp -R "$root/scripts" "$root/packaging" "$d/src/"
	cat >"$d/bin/gh" <<'EOF'
#!/bin/sh
echo "FAKE gh $*" >>"$FAKE_LOG"
case "$1 $2" in
"release view") [ -n "${FAKE_RELEASE_EXISTS:-}" ] && exit 0; exit 1 ;;
"release create") exit 0 ;;
"release download")
	while [ $# -gt 0 ]; do [ "$1" = -D ] && cp "$FAKE_SRC/dist/moox.rb" "$2/moox.rb"; shift; done
	exit 0 ;;
"api -X") exit 0 ;;
"api repos/MadAppGang/homebrew-tap")
	# The tap repo itself is visible (G4-2-L1: a 404 on the file means
	# "create" only then).
	echo MadAppGang/homebrew-tap; exit 0 ;;
"api "*)
	# The read of a Formula/moox.rb that does not exist yet, as gh 2.x does it:
	# the 404 JSON body on stdout (no --jq), exit 1 (G4-H1).
	printf '{"message":"Not Found","status":"404"}'
	echo "gh: Not Found (HTTP 404)" >&2
	exit 1 ;;
esac
exit 0
EOF
	cat >"$d/bin/goreleaser" <<'EOF'
#!/bin/sh
echo "FAKE goreleaser $* (GORELEASER_CURRENT_TAG=$GORELEASER_CURRENT_TAG)" >>"$FAKE_LOG"
rm -rf dist
mkdir -p dist/stage/share/moox
for b in moox tt rpaster; do printf 'binary %s\n' "$b" >dist/stage/$b; done
printf 'conf\n' >dist/stage/share/moox/moox.conf
[ -z "${FAKE_LEAK:-}" ] || printf '%s\n' "$FAKE_LEAK" >dist/stage/share/moox/leak.txt
for a in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
	(cd dist/stage && tar $FAKE_TAROWN -czf ../moox_$a.tar.gz moox tt rpaster share)
done
(cd dist && for f in moox_*.tar.gz; do
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$f"; else shasum -a 256 "$f"; fi
done >checksums.txt)
rm -rf dist/stage
EOF
	chmod +x "$d/bin/gh" "$d/bin/goreleaser"
	printf '# test denylist\nNEVER-PUBLISH-THIS\n' >"$d/deny.txt"
}

run() { # run DIR [ENV=VAL...] — output in $out, status in $rc
	local d=$1
	shift
	: >"$d/log"
	out=$(env "$@" PATH="$d/bin:$PATH" FAKE_LOG="$d/log" FAKE_SRC="$d/src" FAKE_TAROWN="${tarown[*]}" \
		"$d/src/scripts/release-local.sh" --denylist "$d/deny.txt" --src "$d/src" v0.1.0 2>&1)
	rc=$?
}

order_of() { # order_of FILE PATTERN — line number of the first match in the merged trace
	grep -n -m1 -e "$2" "$1" | cut -d: -f1
}

echo "=== happy path ==="
D="$T/ok"
mkfake "$D"
# Make the trace ordered: the fakes append to the log; route the scripts' stdout there too.
: >"$D/log"
out=$(env PATH="$D/bin:$PATH" FAKE_LOG="$D/log" FAKE_SRC="$D/src" FAKE_TAROWN="${tarown[*]}" \
	sh -c '"$1" --denylist "$2" --src "$3" v0.1.0 2>&1 | while IFS= read -r l; do printf "%s\n" "$l" >>"$FAKE_LOG"; done; exit 0' \
	_ "$D/src/scripts/release-local.sh" "$D/deny.txt" "$D/src")
t="$D/log"
v=$(order_of "$t" 'FAKE gh release view v0.1.0')
b=$(order_of "$t" 'FAKE goreleaser release --clean --skip=publish (GORELEASER_CURRENT_TAG=v0.1.0)')
s=$(order_of "$t" 'scan-dist: clean (4 archives)')
r=$(order_of "$t" 'release-assets: rendered')
c=$(order_of "$t" 'FAKE gh release create v0.1.0')
p=$(order_of "$t" 'FAKE gh api -X PUT repos/MadAppGang/homebrew-tap/contents/Formula/moox.rb')
if [ -n "$v" ] && [ -n "$b" ] && [ -n "$s" ] && [ -n "$r" ] && [ -n "$c" ] && [ -n "$p" ] &&
	[ "$v" -lt "$b" ] && [ "$b" -lt "$s" ] && [ "$s" -lt "$r" ] && [ "$r" -lt "$c" ] && [ "$c" -lt "$p" ]; then
	ok "view -> build --skip=publish -> scan -> render -> create -> tap"
else
	no "order v=$v b=$b s=$s r=$r c=$c p=$p" "$(cat "$t")"
fi
n=$(grep -c 'FAKE gh release create' "$t")
if [ "$n" -eq 1 ]; then ok "exactly one gh release create"; else no "create count $n"; fi
line=$(grep 'FAKE gh release create' "$t")
okassets=1
for a in moox_darwin_arm64.tar.gz moox_darwin_amd64.tar.gz moox_linux_arm64.tar.gz moox_linux_amd64.tar.gz checksums.txt moox.rb; do
	case $line in *"/$a"*) ;; *) okassets=0 ;; esac
done
case $line in *--verify-tag*) ;; *) okassets=0 ;; esac
if [ "$okassets" -eq 1 ]; then ok "the one create carries all six assets and --verify-tag"; else no "create args" "$line"; fi
if grep -q 'version "0.1.0"' "$D/src/dist/moox.rb" && ! grep -q '{{' "$D/src/dist/moox.rb"; then ok "moox.rb rendered for 0.1.0"; else no "rendered formula" "$(head -20 "$D/src/dist/moox.rb")"; fi
if grep -q -- '--snapshot' "$t"; then no "a release build must not use --snapshot"; else ok "no --snapshot in the release build"; fi

echo "=== an existing release stops everything ==="
D="$T/exists"
mkfake "$D"
run "$D" FAKE_RELEASE_EXISTS=1
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'gh release delete v0.1.0' && ! grep -q 'goreleaser\|release create' "$D/log"; then ok "existing release: no build, no create, prints the delete command"; else no "existing release" "rc=$rc $out $(cat "$D/log")"; fi

echo "=== a privacy hit stops before create ==="
D="$T/leak"
mkfake "$D"
run "$D" FAKE_LEAK="NEVER-PUBLISH-THIS"
if [ "$rc" -ne 0 ] && grep -q 'goreleaser' "$D/log" && ! grep -q 'release create' "$D/log" && printf '%s' "$out" | grep -q 'denylist'; then ok "denylist hit: built, scanned, nothing created"; else no "denylist hit" "rc=$rc $out $(cat "$D/log")"; fi
D="$T/leak2"
mkfake "$D"
# A macOS home path, assembled so this file does not hold one.
run "$D" FAKE_LEAK="/Use""rs/somebody/src"
if [ "$rc" -ne 0 ] && ! grep -q 'release create' "$D/log"; then ok "public-pattern hit: nothing created"; else no "public hit" "rc=$rc $out"; fi

echo "=== --denylist is mandatory ==="
D="$T/nodeny"
mkfake "$D"
out=$(env PATH="$D/bin:$PATH" FAKE_LOG="$D/log" "$D/src/scripts/release-local.sh" --src "$D/src" v0.1.0 2>&1)
rc=$?
if [ "$rc" -eq 2 ] && [ ! -s "$D/log" ]; then ok "no --denylist: usage error, nothing called"; else no "no denylist" "rc=$rc $out"; fi

echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
