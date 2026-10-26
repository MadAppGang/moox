#!/usr/bin/env bash
# run_all.sh — run the vendored continuity tests that moox keeps.
#
# Every test drives its own `tmux -L <scratch> -f /dev/null` server through
# TMUX_CMD and sets CC_TEST=1 where it touches a store. Each one runs with TMUX
# and every MOOX_* variable removed from its environment, so a script that
# forgot TMUX_CMD falls to `false` (cc_common.sh: no server, nothing done) and
# can never reach the server this runner was started from. The runner itself
# never contacts any tmux server.
#
# The tests for the resurrect-era scripts (pre_save, pre_restore, post_restore,
# verify_launch) were removed with those scripts: moox's Go engine replaces
# them and carries its own tests (moox/internal/engine/**). The hook tests moved
# to moox/tests/hook-*.test.sh.
set -u
CD="$(cd "$(dirname "$0")" && pwd)"
TOTP=0; TOTF=0; FAILING=""
# A per-test ceiling: GNU `timeout`, else coreutils' `gtimeout` (Homebrew), else
# none. macOS ships neither, and a bare `timeout` there fails every test before
# it starts ("command not found" and no result line, as in CI on macos-latest).
# (An array; the ${CEIL[@]+…} form keeps an empty one legal under set -u on bash 3.2.)
CEIL=()
if command -v timeout >/dev/null 2>&1; then CEIL=(timeout 900)
elif command -v gtimeout >/dev/null 2>&1; then CEIL=(gtimeout 900); fi
[ "${#CEIL[@]}" -gt 0 ] || printf '  (no timeout/gtimeout on PATH: tests run without a time limit)\n'
for t in "$CD"/*.sh; do
  b="$(basename "$t")"
  case "$b" in run_all.sh|_*) continue ;; esac
  out="$(env -u TMUX -u TMUX_PANE -u MOOX_SOCKET -u MOOX_ACTIVE -u MOOX_STATE_KEY -u MOOX_STATE_DIR \
    -u MOOX_PANES_DIR -u MOOX_LAUNCH_DIR -u MOOX_PENDING_DIR -u MOOX_ROOT -u MOOX_BIN SHELL=/bin/sh \
    ${CEIL[@]+"${CEIL[@]}"} bash "$t" 2>&1)"; rc=$?
  line="$(printf '%s' "$out" | grep -iE '(RESULT|Results):[[:space:]]+[0-9]+ passed' | tail -1)"
  p="$(printf '%s' "$line" | awk '{for(i=2;i<=NF;i++) if($i ~ /^passed/) {print $(i-1); exit}}')"
  f="$(printf '%s' "$line" | awk '{for(i=2;i<=NF;i++) if($i ~ /^failed/) {print $(i-1); exit}}')"
  if [ -z "$line" ]; then
    printf '  %-30s NO RESULT LINE (exit %s); its last lines:\n' "$b" "$rc"; FAILING="$FAILING $b(noresult)"
    printf '%s\n' "$out" | tail -20 | sed 's/^/      | /'
  elif [ "${f:-0}" -gt 0 ]; then
    printf '  %-30s %3s passed, %3s FAILED\n' "$b" "${p:-?}" "${f:-?}"; FAILING="$FAILING $b($f)"
  else
    printf '  %-30s %3s passed\n' "$b" "${p:-?}"
  fi
  TOTP=$((TOTP + ${p:-0})); TOTF=$((TOTF + ${f:-0}))
done
printf '  total: %s passed, %s failed\n' "$TOTP" "$TOTF"
[ -n "$FAILING" ] && printf '  failing:%s\n' "$FAILING"
[ "$TOTF" -eq 0 ] && [ -z "$FAILING" ]
