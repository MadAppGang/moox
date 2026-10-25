#!/usr/bin/env bash
# claude-hook-excluded-server.test.sh — X4 / PR-C12: position sidecars are
# written only by a participating server, into ITS per-socket panes dir; the
# shared by-pid registry is written always (X12), with a start token (PR-M16).
#
#   participating (own MOOX_SOCKET, MOOX_ACTIVE=1, marker) → $MOOX_PANES_DIR
#   MOOX_ACTIVE=0                                          → no sidecar anywhere
#   marker removed (excluded after the shell started)      → no sidecar anywhere
#   no MOOX_* on a non-default server                      → no sidecar anywhere
. "$(dirname "$0")/lib/hooklib.sh"
. "$(dirname "$0")/lib/tmuxlib.sh"
hk_setup
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }
SCRIPTS="$SHARE/plugins/tmux-claude-continuity/scripts"
TOOLS="$(dirname "$(command -v tmux)"):$(dirname "$(command -v jq)"):$HK_PATH"
ts_start srv
mkdir -p "$T/st/panes"
LEGACY_PANES="$H/.config/tmux-claude/panes"
BYPID="$LEGACY_PANES/by-pid"

start() { # <label> [VAR=val ...]: one SessionStart for the scratch pane %0
  local label="$1"; shift
  printf '{"session_id":"sid-%s","cwd":"/nowhere"}' "$label" | env -i HOME="$H" PATH="$TOOLS" \
    CC_TAB_RECONCILE_DELAY=0 TMUX="$TS_SOCK,$TS_PID,0" TMUX_PANE=%0 "$@" bash "$SCRIPTS/on_session_start.sh"
}
sidecars() { find "$T/st/panes" "$LEGACY_PANES" -maxdepth 1 -name '*.session-id' 2>/dev/null | sort; }
env_own() { printf '%s\n' MOOX_SOCKET="$TS_SOCK" MOOX_STATE_KEY="srv@00000000" MOOX_STATE_DIR="$T/st" MOOX_PANES_DIR="$T/st/panes"; }

: > "$T/st/active"
# shellcheck disable=SC2046
start part $(env_own) MOOX_ACTIVE=1
hk_eq "participating: sidecar in MOOX_PANES_DIR" "sid-part" "$(head -n1 "$T/st/panes/s-0-0.session-id" 2>/dev/null)"
[ ! -e "$LEGACY_PANES/s-0-0.session-id" ] && ok "participating: nothing in the legacy panes dir" || no "participating: nothing in the legacy panes dir" "written"
start_tok="$(cat "$BYPID/$$.start" 2>/dev/null)"
case "$start_tok" in ''|-|*' '*|_*|*_) no "by-pid <pid>.start token is normalised" "<$start_tok>" ;; *) ok "by-pid <pid>.start token is normalised ($start_tok)" ;; esac

for c in inactive nomarker noenv; do
  rm -f "$T/st/panes"/*.session-id "$BYPID"/*.session-id
  # shellcheck disable=SC2046
  case $c in
    inactive) start "$c" $(env_own) MOOX_ACTIVE=0 ;;
    nomarker) rm -f "$T/st/active"; start "$c" $(env_own) MOOX_ACTIVE=1 ;;
    noenv)    start "$c" ;;
  esac
  hk_eq "$c: no position sidecar anywhere" "" "$(sidecars)"
  hk_eq "$c: by-pid entry still written" "sid-$c" "$(head -n1 "$BYPID/$$.session-id" 2>/dev/null)"
done

hk_finish
