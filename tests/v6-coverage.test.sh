#!/usr/bin/env bash
# v6-coverage.test.sh — V6 (architecture §6.7, §12.3): every behaviour and
# regression id of the research inventory maps to a component and to tests
# that EXIST, checked by machine against the real test functions and files.
#
# Inputs (committed): tests/v6-ids.txt (the id inventory) and
# tests/v6-mapping.tsv (ids → component → tests). Fails unless
#   1. every inventory id appears in the mapping's ids column, and the mapping
#      names no id the inventory lacks;
#   2. every row names at least one test, or says exactly "RETIRED: §6.8";
#   3. every Go name (Test*/Benchmark*/Fuzz*) is a top-level test function that
#      `go test -list` reports — both modules (moox, tests/blackbox), every
#      package, with the tmuxe2e, tmuxlive and realdata build tags, so a
#      tag-gated test counts — and every tests/*.test.sh name is a file here.
# Optional cross-checks, when the session documents are at hand:
#   MOOX_V6_RESEARCH_DIR=<dir with continuity-behaviours.md, resurrect-behaviours.md>
#     re-derives the inventory with §6.7's rule and requires it to equal v6-ids.txt;
#   MOOX_V6_ARCH=<architecture.md> requires §6.7's tables to name the same
#     ids and tests, row for row, as v6-mapping.tsv.
# A negative control runs the name check on a planted mapping with one real
# and one invented test of each kind.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
IDS="$HERE/v6-ids.txt"
MAP="$HERE/v6-mapping.tsv"
ID_RE='\b(RR|B|R|X|I|C|S|T|G|H|U)[0-9]+\b'
command -v go >/dev/null 2>&1 || { echo "SKIP: go not installed"; exit 0; }

rows() { grep -v '^#' "$MAP" | grep -v '^[[:space:]]*$'; }
inventory() { grep -v '^#' "$IDS" | grep -v '^[[:space:]]*$' | sort -u; }
mapped() { rows | cut -f1 | tr ',' '\n' | grep -oE "$ID_RE" | sort -u; }

# ── 1. ids ───────────────────────────────────────────────────────────────────
n_ids="$(inventory | wc -l | tr -d ' ')"
[ "$n_ids" -gt 0 ] && ok "inventory: $n_ids ids" || no "inventory: ids" "none in $IDS"
hk_eq "every inventory id is mapped" "" "$(comm -23 <(inventory) <(mapped) | tr '\n' ' ')"
hk_eq "the mapping names no id outside the inventory" "" "$(comm -13 <(inventory) <(mapped) | tr '\n' ' ')"

if [ -n "${MOOX_V6_RESEARCH_DIR:-}" ]; then
  F="$MOOX_V6_RESEARCH_DIR"
  derived="$({ grep -oE '\b(B|R|X|I|C)[0-9]+\b' "$F/continuity-behaviours.md"
               grep -oE '\b(S|T|G|H|U)[0-9]+\b' "$F/resurrect-behaviours.md"
               grep -oE '\bR[0-9]+\b' "$F/resurrect-behaviours.md" | sed 's/^R/RR/'; } | sort -u)"
  hk_eq "the inventory equals the ids §6.7's rule derives from $F" "$(inventory)" "$derived"
fi

# ── 2. every row has tests or is retired ─────────────────────────────────────
bad_rows() { # a function: bash 3.2 cannot parse case inside $(…)
  rows | while IFS="$(printf '\t')" read -r ids _ tests _; do
    case "$tests" in
      "RETIRED: §6.8") ;;
      *RETIRED*) printf '%s(RETIRED must be the whole column) ' "$ids" ;;
      "") printf '%s(no test) ' "$ids" ;;
    esac
  done
}
hk_eq "every row names a test or is RETIRED: §6.8" "" "$(bad_rows)"
retired="$(rows | cut -f3 | grep -c '^RETIRED: §6.8$')"

# ── 3. names against the real tests ──────────────────────────────────────────
LIST="$T/go-tests"
golist() { # <module dir>: the top-level test names of every package, all tags
  if ! (cd "$1" && go test -list '.*' -tags 'tmuxe2e tmuxlive realdata' ./...) >"$T/golist.out" 2>"$T/golist.err"; then
    printf '%s: %s\n' "$1" "$(grep -v '^ok' "$T/golist.out" "$T/golist.err" | head -5 | tr '\n' ' ')" >> "$T/golist.fail"
  fi
  grep -E '^(Test|Benchmark|Fuzz|Example)[A-Za-z0-9_]*$' "$T/golist.out"
}
{ golist "$ROOT"; golist "$ROOT/tests/blackbox"; } > "$LIST.raw"
sort -u "$LIST.raw" > "$LIST"
n_go="$(wc -l < "$LIST" | tr -d ' ')"
hk_eq "go test -list succeeds in both modules" "" "$(cat "$T/golist.fail" 2>/dev/null)"
[ "$n_go" -gt 1000 ] && ok "go test -list: $n_go test functions in both modules" \
  || no "go test -list: test functions in both modules" "only $n_go"

check_names() { # <mapping file>: one line per name that is not a real test
  grep -v '^#' "$1" | cut -f1,3 | while IFS="$(printf '\t')" read -r ids tests; do
    [ "$tests" = "RETIRED: §6.8" ] && continue
    for n in $tests; do
      case "$n" in
        Test*|Benchmark*|Fuzz*) grep -qxF "$n" "$LIST" || printf 'missing Go test %s (%s)\n' "$n" "$ids" ;;
        tests/*.test.sh) [ -f "$ROOT/$n" ] || printf 'missing shell test %s (%s)\n' "$n" "$ids" ;;
        *) printf 'not a test name: %s (%s)\n' "$n" "$ids" ;;
      esac
    done
  done
}
missing="$(check_names "$MAP")"
n_names="$(rows | cut -f3 | grep -v '^RETIRED' | tr ' ' '\n' | grep -c .)"
hk_eq "every named test exists ($n_names names, $retired retired rows)" "" "$missing"

printf 'X1\tc\tTestPaths_DistinctPerKey TestNoSuchTest_V6Control tests/v6-coverage.test.sh tests/no-such.test.sh x.sh\tn\tg\n' > "$T/planted.tsv"
hk_eq "negative control: the name check reports exactly the 3 invented names" \
  "missing Go test TestNoSuchTest_V6Control (X1)|missing shell test tests/no-such.test.sh (X1)|not a test name: x.sh (X1)" \
  "$(check_names "$T/planted.tsv" | paste -sd '|' -)"

# ── optional: architecture §6.7 says the same ────────────────────────────────
if [ -n "${MOOX_V6_ARCH:-}" ]; then
  norm_tsv() {
    rows | while IFS="$(printf '\t')" read -r ids _ tests _; do
      i="$(printf '%s\n' "$ids" | grep -oE "$ID_RE" | sort | paste -sd, -)"
      t="$(printf '%s\n' "$tests" | sed 's/^RETIRED: §6.8$/RETIRED/' | tr ' ' '\n' | sort | paste -sd' ' -)"
      printf '%s\t%s\n' "$i" "$t"
    done | sort
  }
  norm_arch() {
    awk '/^### 6\.7 /{on=1} /^### 6\.8 /{on=0} on && /^\| / && !/^\| IDs/ && !/^\|---/' "$MOOX_V6_ARCH" \
      | while IFS= read -r line; do
          ids="$(printf '%s\n' "$line" | awk -F'|' '{print $2}')"
          test="$(printf '%s\n' "$line" | cut -d'|' -f4-)"
          i="$(printf '%s\n' "$ids" | grep -oE "$ID_RE" | sort | paste -sd, -)"
          t="$( { printf '%s\n' "$test" | grep -oE '`(Test|Benchmark|Fuzz)[A-Za-z0-9_]+`|`tests/[A-Za-z0-9_.-]+\.test\.sh`' | tr -d '`'
                  printf '%s\n' "$test" | grep -qF 'RETIRED: §6.8' && echo RETIRED; } | sort | paste -sd' ' -)"
          printf '%s\t%s\n' "$i" "$t"
        done | sort
  }
  hk_eq "architecture §6.7 names the same ids and tests as v6-mapping.tsv" "" \
    "$(diff <(norm_arch) <(norm_tsv) | grep '^[<>]' | head -20)"
fi

echo "V6: ids=$n_ids rows=$(rows | wc -l | tr -d ' ') names=$n_names retired=$retired go-tests=$n_go"
hk_finish
