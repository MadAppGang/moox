#!/usr/bin/env bash
# participating-requires-own-socket.test.sh — regression for the plan-review
# mandatory item: `_cc_participating` must also require
# MOOX_SOCKET == ${TMUX%%,*}.
#
# The case: a scratch tmux server started from inside a REAL pane inherits the
# real server's MOOX_SOCKET / MOOX_STATE_DIR / MOOX_PANES_DIR, and the real
# participation marker exists. Before the fix, a Claude in that scratch server
# passed `_cc_participating` (marker present) and its on_stop.sh rewrote — and
# its on_session_start.sh wrote — the REAL server's position sidecars, which are
# keyed by `#S-#I-#P` and so collide across servers.
#
# Asserted: (1) _cc_participating is false for that env and true for the
# server's own env; (2) on_stop.sh leaves the real sidecar byte-identical and
# on_session_start.sh writes no sidecar into the real panes dir; (3) the
# control — the same scripts with the scratch server's OWN env — do write.
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/lib/hooklib.sh"
. "$here/lib/tmuxlib.sh"
hk_setup
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }
SCRIPTS="$SHARE/plugins/tmux-claude-continuity/scripts"
TOOLS="$(dirname "$(command -v tmux)"):$(dirname "$(command -v jq)"):$HK_PATH"

ts_start scratch                       # the scratch server the Claude runs in
hk_moox_layout real                    # the REAL server's exported layout (marker present)
# The real server is the login default (key "default"), so its position sidecars
# live in the legacy panes dir, which is also every script's fallback default.
M_PANES="$H/.config/tmux-claude/panes"; mkdir -p "$M_PANES"
KEY="s-0-0"                            # #S-#I-#P of the scratch pane: collides by construction
printf 'real-sid\nReal Title\n' > "$M_PANES/$KEY.session-id"
BEFORE="$(cat "$M_PANES/$KEY.session-id")"

inherited() { # run a command with the REAL env inside the SCRATCH server's pane
  env -i HOME="$H" PATH="$TOOLS" CC_TAB_RECONCILE_DELAY=0 \
    MOOX_SOCKET="$M_SOCK" MOOX_ACTIVE=1 MOOX_STATE_KEY=default MOOX_STATE_DIR="$M_STATE" \
    MOOX_PANES_DIR="$M_PANES" MOOX_LAUNCH_DIR="$M_LAUNCH" MOOX_PENDING_DIR="$M_PENDING" \
    TMUX="$TS_SOCK,$TS_PID,0" TMUX_PANE=%0 "$@"
}

# ── 1. the predicate itself ──────────────────────────────────────────────────
p_inh="$(inherited bash -c '. "$1/lib/cc_common.sh"; _cc_participating && echo yes || echo no' _ "$SCRIPTS")"
hk_eq "_cc_participating: inherited env (MOOX_SOCKET names another server) → no" "no" "$p_inh"
p_own="$(env -i HOME="$H" PATH="$TOOLS" MOOX_SOCKET="$TS_SOCK" MOOX_ACTIVE=1 MOOX_STATE_DIR="$M_STATE" \
  TMUX="$TS_SOCK,$TS_PID,0" bash -c '. "$1/lib/cc_common.sh"; _cc_participating && echo yes || echo no' _ "$SCRIPTS")"
hk_eq "_cc_participating: the server's own env, marker present → yes" "yes" "$p_own"
p_notmux="$(env -i HOME="$H" PATH="$TOOLS" MOOX_SOCKET="$M_SOCK" MOOX_ACTIVE=1 MOOX_STATE_DIR="$M_STATE" \
  bash -c '. "$1/lib/cc_common.sh"; _cc_participating && echo yes || echo no' _ "$SCRIPTS")"
hk_eq "_cc_participating: env present but not inside tmux → no" "no" "$p_notmux"

# ── 2. the Claude hooks with the inherited env ───────────────────────────────
printf '{"session_id":"scratch-sid","cwd":"/nowhere"}' | inherited bash "$SCRIPTS/on_stop.sh"
hk_eq "on_stop.sh: the real sidecar is byte-identical" "$BEFORE" "$(cat "$M_PANES/$KEY.session-id")"
rm -f "$M_PANES/$KEY.session-id"
printf '{"session_id":"scratch-sid","cwd":"/nowhere"}' | inherited bash "$SCRIPTS/on_session_start.sh"
[ ! -e "$M_PANES/$KEY.session-id" ] && ok "on_session_start.sh: no sidecar in the real panes dir" \
  || no "on_session_start.sh: no sidecar in the real panes dir" "$(cat "$M_PANES/$KEY.session-id")"
BYPID="$H/.config/tmux-claude/panes/by-pid"
[ -n "$(ls "$BYPID"/*.session-id 2>/dev/null)" ] && ok "on_session_start.sh: the shared by-pid entry is still written (X12)" \
  || no "on_session_start.sh: the shared by-pid entry is still written (X12)" "none in $BYPID"

# ── 3. control: the scratch server's own env does write ──────────────────────
mkdir -p "$T/own/panes"; : > "$T/own/active"
own() {
  env -i HOME="$H" PATH="$TOOLS" CC_TAB_RECONCILE_DELAY=0 \
    MOOX_SOCKET="$TS_SOCK" MOOX_ACTIVE=1 MOOX_STATE_KEY="scratch@00000000" MOOX_STATE_DIR="$T/own" \
    MOOX_PANES_DIR="$T/own/panes" TMUX="$TS_SOCK,$TS_PID,0" TMUX_PANE=%0 "$@"
}
printf '{"session_id":"own-sid","cwd":"/nowhere"}' | own bash "$SCRIPTS/on_session_start.sh"
hk_eq "control: on_session_start.sh writes the own server's sidecar" "own-sid" "$(head -n1 "$T/own/panes/$KEY.session-id" 2>/dev/null)"
ts set -g @claude-continuity-panes-dir "$T/own/panes"
printf 'stale\n' > "$T/own/panes/$KEY.session-id"
printf '{"session_id":"own-sid-2","cwd":"/nowhere"}' | own bash "$SCRIPTS/on_stop.sh"
hk_eq "control: on_stop.sh rewrites the own server's sidecar" "own-sid-2" "$(head -n1 "$T/own/panes/$KEY.session-id" 2>/dev/null)"

hk_finish
