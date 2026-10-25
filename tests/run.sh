#!/usr/bin/env bash
# run.sh — run moox's shell black-box tests: every tests/*.test.sh, or the ones
# named as arguments. Prints one line per file and exits non-zero if any fails.
#
#   bash tests/run.sh                    # all
#   bash tests/run.sh hook-record.test.sh hook-bash-record.test.sh
#   bash tests/run.sh --except theme-blackbox.test.sh,theme-per-screen.test.sh
#
# A file listed in tests/known-failures.txt passes as "xfail" only when it fails
# with exactly the listed case ids (its "failed: <ids>" line); any other set of
# failures, including a listed case that now passes, is a FAIL (G4-M12).
set -u
here="$(cd "$(dirname "$0")" && pwd)"
known="$here/known-failures.txt"
except=","
if [ "${1:-}" = "--except" ]; then
  [ $# -ge 2 ] || { echo "usage: run.sh [--except a.test.sh,b.test.sh] [FILE...]" >&2; exit 2; }
  except=",$2,"
  shift 2
fi
if [ "$#" -gt 0 ]; then
  files=()
  for f in "$@"; do files+=("$here/${f##*/}"); done
else
  files=("$here"/*.test.sh)
fi

known_ids() { # known_ids FILE: the sorted case ids listed for FILE, or nothing
  [ -r "$known" ] || return 0
  awk -v f="$1" '$1 == f { for (i = 2; i <= NF; i++) print $i }' "$known" | sort -u
}

failed=""
for t in "${files[@]}"; do
  b="${t##*/}"
  case $except in *",$b,"*) printf '  skip  %s (--except)\n' "$b"; continue ;; esac
  out="$(bash "$t" 2>&1)"
  rc=$?
  line="$(printf '%s\n' "$out" | grep -E '^RESULT: ' | tail -1)"
  want="$(known_ids "$b")"
  if [ "$rc" -eq 0 ] && [ -z "$want" ]; then
    printf '  pass  %-48s %s\n' "$b" "${line:-}"
    continue
  fi
  if [ "$rc" -ne 0 ] && [ -n "$want" ]; then
    got="$(printf '%s\n' "$out" | grep -E '^failed: ' | tail -1 | sed 's/^failed: //' | tr ' ' '\n' | sed '/^$/d' | sort -u)"
    if [ "$got" = "$want" ]; then
      printf '  xfail %-48s known: %s\n' "$b" "$(printf '%s' "$want" | tr '\n' ' ')"
      continue
    fi
    printf '  FAIL  %-48s failures differ from known-failures.txt\n' "$b"
    new="$(comm -13 <(printf '%s\n' "$want") <(printf '%s\n' "$got") | tr '\n' ' ')"
    fixed="$(comm -23 <(printf '%s\n' "$want") <(printf '%s\n' "$got") | tr '\n' ' ')"
    [ -z "${new// /}" ] || printf '        new failures: %s\n' "$new"
    [ -z "${fixed// /}" ] || printf '        now passing (remove from known-failures.txt): %s\n' "$fixed"
    [ -n "$got" ] || printf '        (no "failed:" line; rc=%s)\n' "$rc"
  elif [ "$rc" -eq 0 ]; then
    printf '  FAIL  %-48s passes, but known-failures.txt lists: %s\n' "$b" "$(printf '%s' "$want" | tr '\n' ' ')"
  else
    printf '  FAIL  %-48s %s\n' "$b" "${line:-rc=$rc}"
    printf '%s\n' "$out" | grep -E 'FAIL|ABORT|^ +' | grep -v '^  ok ' | sed 's/^/        /' | head -20
  fi
  failed="$failed $b"
done
[ -z "$failed" ] || { echo "failing:$failed"; exit 1; }
