#!/usr/bin/env bash
# on-session-start-outside-tmux.test.sh — R34: a Claude running OUTSIDE tmux
# claims no pane. `display-message -t ""` does not fail: tmux reads an empty
# target as "the active pane of the current client", so a hook that asked
# tmux anyway would write the position sidecar of whatever pane the user is
# looking at (and repaint that window's tab).
#
#   no TMUX / no TMUX_PANE, a server's MOOX_* env inherited → no sidecar, no pane-resolving tmux call
#   TMUX set, TMUX_PANE unset                               → no sidecar, no pane-resolving tmux call
#   no TMUX and no MOOX_* at all (a plain terminal)          → no sidecar, no pane-resolving tmux call
#   in all three, the shared by-pid registry is still written (X12)
#   control: inside the pane (TMUX + TMUX_PANE)             → sidecar written, tmux asked
#
# Every tmux the hook (and the tab reconcile it starts) could run goes through
# a logging shim first on PATH. Isolation: a `tmux -f /dev/null -S $T/<name>`
# server, a scratch HOME, `env -i`.
. "$(dirname "$0")/lib/hooklib.sh"
. "$(dirname "$0")/lib/tmuxlib.sh"
hk_setup
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }
SCRIPTS="$SHARE/plugins/tmux-claude-continuity/scripts"
REAL_TMUX="$(command -v tmux)"
ts_start srv
ts set -g @claude-continuity-panes-dir "$T/st/panes"
mkdir -p "$T/st/panes" "$T/shim"
: > "$T/st/active"
LOG="$T/tmux-calls"
cat > "$T/shim/tmux" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "$LOG"
exec "$REAL_TMUX" "\$@"
EOF
chmod +x "$T/shim/tmux"
TOOLS="$T/shim:$(dirname "$(command -v jq)"):$HK_PATH"
LEGACY_PANES="$H/.config/tmux-claude/panes"
BYPID="$LEGACY_PANES/by-pid"

start() { # <label> [VAR=val ...]: one SessionStart, default 1 s tab delay kept
  local label="$1"; shift
  rm -rf "$T/st/panes"/* "$BYPID" "$LOG"
  printf '{"session_id":"sid-%s","cwd":"/nowhere"}' "$label" | env -i HOME="$H" PATH="$TOOLS" "$@" \
    bash "$SCRIPTS/on_session_start.sh"
  sleep 1.5 # past the tab reconcile's delay: a backgrounded reconcile would have run by now
}
moox_env() { printf '%s\n' MOOX_SOCKET="$TS_SOCK" MOOX_ACTIVE=1 MOOX_STATE_KEY="srv@00000000" \
  MOOX_STATE_DIR="$T/st" MOOX_PANES_DIR="$T/st/panes"; }
sidecars() { find "$T/st/panes" "$LEGACY_PANES" -maxdepth 1 -name '*.session-id' 2>/dev/null | sort | tr '\n' ' '; }
calls() { cat "$LOG" 2>/dev/null | tr '\n' ';'; }
# Calls that name a pane or window, or change anything. A global option read
# (the shared by-pid dir, `show-option -gqv @claude-continuity-…`) resolves
# no pane and is allowed.
pane_calls() { grep -v ' show-option -gqv @claude-continuity-[a-z-]*$' "$LOG" 2>/dev/null | tr '\n' ';'; }

# shellcheck disable=SC2046
start inherited $(moox_env)
hk_eq "outside tmux, MOOX_* inherited: no position sidecar" "" "$(sidecars)"
hk_eq "outside tmux, MOOX_* inherited: no pane-resolving tmux call" "" "$(pane_calls)"
hk_eq "outside tmux, MOOX_* inherited: by-pid entry written" "sid-inherited" "$(cat "$BYPID"/*.session-id 2>/dev/null | head -n1)"

# shellcheck disable=SC2046
start nopane $(moox_env) TMUX="$TS_SOCK,$TS_PID,0"
hk_eq "TMUX set, no TMUX_PANE: no position sidecar" "" "$(sidecars)"
hk_eq "TMUX set, no TMUX_PANE: no pane-resolving tmux call" "" "$(pane_calls)"
hk_eq "TMUX set, no TMUX_PANE: by-pid entry written" "sid-nopane" "$(cat "$BYPID"/*.session-id 2>/dev/null | head -n1)"

start plain
hk_eq "plain terminal: no position sidecar" "" "$(sidecars)"
hk_eq "plain terminal: no pane-resolving tmux call" "" "$(pane_calls)"
hk_eq "plain terminal: by-pid entry written" "sid-plain" "$(cat "$BYPID"/*.session-id 2>/dev/null | head -n1)"

# Control: the same hook inside the pane asks tmux (through the shim) for the
# pane's position and writes the sidecar, so the empty logs above are real.
# shellcheck disable=SC2046
start inside $(moox_env) TMUX="$TS_SOCK,$TS_PID,0" TMUX_PANE=%0
hk_eq "control, inside the pane: sidecar written" "sid-inside" "$(head -n1 "$T/st/panes/s-0-0.session-id" 2>/dev/null)"
case "$(calls)" in
  *"display-message -t %0"*) ok "control, inside the pane: tmux asked for %0 through the shim" ;;
  *) no "control, inside the pane: tmux asked for %0 through the shim" "calls: $(calls)" ;;
esac

hk_finish
