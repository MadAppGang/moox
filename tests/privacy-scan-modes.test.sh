#!/usr/bin/env bash
# privacy-scan-modes.test.sh — privacy-scan.sh --tree and --binary (§9.1 hooks, §9.6).
#
# Leaks are assembled at run time so this file holds none (the tree scan reads it).
set -u
here="$(cd "$(dirname "$0")" && pwd)"
scan="$here/../scripts/privacy-scan.sh"

pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/       /' | head -12; }
expect() { # expect NAME WANT_RC ARGS...
	local n=$1 want=$2
	shift 2
	local out rc
	out=$("$scan" "$@" 2>&1)
	rc=$?
	if [ "$rc" -eq "$want" ]; then ok "$n (exit $rc)"; else no "$n: want $want, got $rc" "$out"; fi
}

T=$(mktemp -d "${TMPDIR:-/tmp}/scan-modes.XXXXXX")
trap 'rm -rf "$T"' EXIT
home_path="/Use""rs/somebody/project"
at='@'

echo "=== --tree in a checkout: what a commit would hold ==="
if command -v git >/dev/null 2>&1; then
	export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
	R="$T/repo"
	mkdir -p "$R/bin" "$R/sub"
	(cd "$R" && git init -q .)
	printf 'clean\n' >"$R/tracked.txt"
	printf '/bin/\n*.log\n' >"$R/.gitignore"
	printf '%s\n' "$home_path" >"$R/bin/devbinary"
	printf '%s\n' "$home_path" >"$R/sub/run.log"
	(cd "$R" && git add -A)
	expect "ignored files (dev binaries, logs) are not scanned" 0 --tree "$R"
	printf '%s\n' "$home_path" >"$R/sub/new.txt"
	expect "an untracked, not-ignored file IS scanned" 1 --tree "$R"
	rm "$R/sub/new.txt"
	printf '%s\n' "$home_path" >"$R/tracked.txt"
	expect "a tracked file's work-tree content is scanned" 1 --tree "$R"
	printf 'clean\n' >"$R/tracked.txt"
	mkdir -p "$R/x${at}example.org"
	printf 'a\n' >"$R/x${at}example.org/f"
	expect "a file NAME is scanned too" 1 --tree "$R"
else
	echo "  skip (no git)"
fi

echo "=== --tree outside a checkout ==="
D="$T/plain"
mkdir -p "$D/dist" "$D/bin" "$D/src"
printf '%s\n' "$home_path" >"$D/dist/moox"
printf '%s\n' "$home_path" >"$D/bin/moox"
printf 'ok\n' >"$D/src/a"
expect "dist/ and bin/ are skipped" 0 --tree "$D"
printf '%s\n' "$home_path" >"$D/src/a"
expect "everything else is scanned" 1 --tree "$D"

echo "=== --binary ==="
B="$T/bin.dat"
{
	head -c 4096 /dev/urandom
	printf 'moox-restore-req-%smoox-restore-res-reflect.Value.Elem' "$at"
	head -c 4096 /dev/urandom
} >"$B"
expect "packed Go string constants are not addresses" 0 --binary "$B"
{ cat "$B"; printf 'xxfirst.last%sgmail.comyy' "$at"; } >"$T/bin2.dat"
expect "a real address packed between other strings blocks" 1 --binary "$T/bin2.dat"
{ cat "$B"; printf '%s/go/src/main.go' "$home_path"; } >"$T/bin3.dat"
expect "a build-host path blocks" 1 --binary "$T/bin3.dat"
{ cat "$B"; printf 'tok=sk-''ant-abcdefabcdef'; } >"$T/bin4.dat"
expect "a token shape blocks" 1 --binary "$T/bin4.dat"
expect "--binary needs exactly one file" 2 --binary "$B" "$B"
expect "--tree needs a directory" 2 --tree "$B"

echo "=== Claude project slugs and /home names (G4-L1) ==="
L="$T/slugs"
mkdir -p "$L"
printf 'see ~/.claude/projects/-Use''rs-alice-src-app/x.jsonl\n' >"$L/a.txt"
expect "a -Users-<name>- slug blocks" 1 "$L/a.txt"
printf 'see ~/.claude/projects/-ho''me-alice-src/x.jsonl\n' >"$L/b.txt"
expect "a -home-<name>- slug blocks" 1 "$L/b.txt"
printf 'see -ho''me-user-src, -Use''rs-someone and set-home-dir\n' >"$L/c.txt"
expect "placeholder slugs (-home-user-, -Users-someone) and set-home-dir pass" 0 "$L/c.txt"
printf 'see -Use''rs-someonelse-src\n' >"$L/d.txt"
expect "a name that only starts like a placeholder blocks" 1 "$L/d.txt"
N="$T/names-home"
mkdir -p "$N/ho""me/somebody" "$N/ok/ho""me/user"
printf 'x\n' >"$N/ho""me/somebody/f"
printf 'x\n' >"$N/ok/ho""me/user/f"
expect "a /home/<name> FILE NAME blocks" 1 "$N"
expect "a /home/user file name passes" 0 "$N/ok"

echo "=== the denylist fails CLOSED (G4-H4) ==="
F="$T/deny-tree"
mkdir -p "$F"
printf 'a line with Secret-Client-Name in it\n' >"$F/a.txt"
printf 'Secret-Client-Name\n' >"$T/deny-ok"
expect "control: a good denylist reports its hit" 1 --denylist "$T/deny-ok" "$F"
# Patterns that every grep refuses to compile (G4-2-H2): GNU grep accepts
# `a{1` and `*leading-star` as literal-ish EREs, so they were not malformed on
# Linux. Each one is checked against the grep the scanner will run, so a grep
# that accepts it fails here by name instead of as a scanner bug.
for bad in '(unclosed' '[unclosed' '[z-a]'; do
	printf 'x\n' | grep -E -e "$bad" >/dev/null 2>&1
	if [ $? -lt 2 ]; then
		no "precondition: this grep ($(grep --version 2>&1 | head -n 1)) compiles '$bad'; pick a pattern every grep rejects"
		continue
	fi
	printf '# names\nSecret-Client-Name\n%s\n' "$bad" >"$T/deny-bad"
	out=$("$scan" --denylist "$T/deny-bad" "$F" 2>&1)
	rc=$?
	if [ "$rc" -eq 2 ] && ! printf '%s' "$out" | grep -q 'privacy-scan: clean'; then
		ok "malformed line '$bad' next to a hitting one: exit 2, never clean"
	else
		no "malformed line '$bad': want exit 2 and no 'clean', got $rc" "$out"
	fi
done
printf '# only comments\n\n' >"$T/deny-empty"
expect "a denylist with no patterns is refused" 2 --denylist "$T/deny-empty" "$F"
expect "a missing denylist is refused" 2 --denylist "$T/no-such-denylist" "$F"
if [ "$(id -u)" -ne 0 ]; then
	printf 'clean\n' >"$F/unreadable.txt"
	chmod 000 "$F/unreadable.txt"
	out=$("$scan" "$F" 2>&1)
	rc=$?
	chmod 644 "$F/unreadable.txt"
	if [ "$rc" -eq 2 ] && ! printf '%s' "$out" | grep -q 'privacy-scan: clean'; then
		ok "an unreadable file: exit 2, never clean"
	else
		no "unreadable file: want exit 2, got $rc" "$out"
	fi
	rm -f "$F/unreadable.txt"
fi

echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
