#!/usr/bin/env bash
# run-known-failures.test.sh — tests/run.sh passes a known-failing file only on
# EXACTLY its listed case ids (G4-M12). Runs a copy of run.sh in a temp dir
# against fake test files; nothing else is run.
set -u
here="$(cd "$(dirname "$0")" && pwd)"

pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/       /' | head -20; }

T=$(mktemp -d "${TMPDIR:-/tmp}/run-known.XXXXXX")
trap 'rm -rf "$T"' EXIT

fake() { # fake NAME RC FAILED_IDS: a test file that prints "failed: IDS" and exits RC
	printf '#!/bin/sh\necho "RESULT: x"\n[ -z "%s" ] || echo "failed: %s"\nexit %s\n' "$3" "$3" "$2" >"$T/$1"
}
case_run() { # case_run NAME WANT_RC: run.sh in $T; output in $out
	local n=$1 want=$2 rc
	out=$(bash "$T/run.sh" 2>&1)
	rc=$?
	if [ "$rc" -eq "$want" ]; then ok "$n (exit $rc)"; else no "$n: want $want, got $rc" "$out"; fi
}

cp "$here/run.sh" "$T/run.sh"
printf '# comment\nknown.test.sh B7 B9\n' >"$T/known-failures.txt"
fake plain.test.sh 0 ""

fake known.test.sh 1 "B9 B7"
case_run "exactly the known ids: xfail, run passes" 0
printf '%s' "$out" | grep -q 'xfail known.test.sh' && ok "reported as xfail" || no "xfail line" "$out"

fake known.test.sh 1 "B7 B9 T1"
case_run "a new failure next to the known ones: FAIL" 1
printf '%s' "$out" | grep -q 'new failures: T1' && ok "names the new failure" || no "new failure line" "$out"

fake known.test.sh 1 "B7"
case_run "a known case now passing: FAIL until the list is updated" 1
printf '%s' "$out" | grep -q 'now passing (remove from known-failures.txt): B9' && ok "names the fixed case" || no "fixed line" "$out"

fake known.test.sh 0 ""
case_run "a listed file that fully passes: FAIL" 1

fake known.test.sh 1 ""
case_run "a listed file failing with no failed: line: FAIL" 1

fake known.test.sh 1 "B7 B9"
fake plain.test.sh 1 ""
case_run "an unlisted file failing: FAIL" 1

fake plain.test.sh 1 ""
out=$(bash "$T/run.sh" --except plain.test.sh 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'skip  plain.test.sh'; then ok "--except skips a file"; else no "--except rc=$rc" "$out"; fi

echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
