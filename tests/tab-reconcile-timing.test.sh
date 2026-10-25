#!/usr/bin/env bash
# tab-reconcile-timing.test.sh — B49: when the Claude hooks start the window-tab
# reconcile (cc_tab_reconcile.sh <pane>).
#
#   SessionStart: once, after a delay (default 1 s: Claude has not written
#     sessions/<pid>.json yet), in the background — the hook itself returns at once
#   Stop: on EVERY turn, and ABOVE the hook's early exits (no session_id, a
#     claudish child, a missing sidecar), so a drifted sidecar never freezes a tab
#   neither: without TMUX_PANE (an empty -t is "the active pane of the current
#     client") or on a server that does not take part (G3-M8)
#
# The scripts are copied into the temp dir with cc_tab_reconcile.sh replaced by
# a stub that records each call. Isolation: a `tmux -f /dev/null -S $T/<name>`
# server, a scratch HOME, `env -i`.
. "$(dirname "$0")/lib/hooklib.sh"
. "$(dirname "$0")/lib/tmuxlib.sh"
hk_setup
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }
cp -R "$SHARE/plugins/tmux-claude-continuity/scripts" "$T/scripts"
REC="$T/reconcile-calls"
cat > "$T/scripts/cc_tab_reconcile.sh" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "$REC"
EOF
chmod +x "$T/scripts/cc_tab_reconcile.sh"
TOOLS="$(dirname "$(command -v tmux)"):$(dirname "$(command -v jq)"):$HK_PATH"
ts_start srv
mkdir -p "$T/st/panes"
: > "$T/st/active"

hook() { # <script> <json> [VAR=val ...]
  local script="$1" json="$2"; shift 2
  printf '%s' "$json" | env -i HOME="$H" PATH="$TOOLS" \
    MOOX_SOCKET="$TS_SOCK" MOOX_ACTIVE=1 MOOX_STATE_KEY="srv@00000000" MOOX_STATE_DIR="$T/st" \
    MOOX_PANES_DIR="$T/st/panes" TMUX="$TS_SOCK,$TS_PID,0" "$@" bash "$T/scripts/$script"
}
calls() { [ -f "$REC" ] && wc -l < "$REC" | tr -d ' ' || echo 0; }
wait_calls() { # <n> <seconds>: wait until at least n calls are recorded
  local i=0
  while [ "$(calls)" -lt "$1" ] && [ "$i" -lt $(($2 * 10)) ]; do sleep 0.1; i=$((i + 1)); done
}

# ── SessionStart: delayed, backgrounded, once ────────────────────────────────
rm -f "$REC"
t0="$(date +%s)"
hook on_session_start.sh '{"session_id":"sid-a","cwd":"/nowhere"}' TMUX_PANE=%0
t1="$(date +%s)"
[ $((t1 - t0)) -le 1 ] && ok "SessionStart returns without waiting for the reconcile" \
  || no "SessionStart returns without waiting for the reconcile" "took $((t1 - t0)) s"
sleep 0.4
hk_eq "SessionStart: no reconcile before the 1 s delay" "0" "$(calls)"
wait_calls 1 5
hk_eq "SessionStart: one reconcile after the delay" "1" "$(calls)"
hk_eq "SessionStart: the reconcile names the hook's pane" "%0" "$(head -n1 "$REC" 2>/dev/null)"
sleep 1
hk_eq "SessionStart: exactly one reconcile" "1" "$(calls)"

rm -f "$REC"
hook on_session_start.sh '{"session_id":"sid-b","cwd":"/nowhere"}' TMUX_PANE=%0 CC_TAB_RECONCILE_DELAY=0
wait_calls 1 3
hk_eq "SessionStart: CC_TAB_RECONCILE_DELAY=0 reconciles at once" "1" "$(calls)"

# ── Stop: every turn, above the early exits ──────────────────────────────────
rm -f "$REC"
for turn in 1 2 3; do
  hook on_stop.sh "{\"session_id\":\"sid-$turn\",\"cwd\":\"/nowhere\"}" TMUX_PANE=%0
done
wait_calls 3 3
hk_eq "Stop: one reconcile per turn (3 turns, no sidecar on disk)" "3" "$(calls)"

rm -f "$REC"
hook on_stop.sh '{}' TMUX_PANE=%0                                                    # no session_id
hook on_stop.sh '{"session_id":"sid-c","cwd":"/nowhere"}' TMUX_PANE=%0 CLAUDISH_ACTIVE_MODEL_NAME=m
hook on_stop.sh '{"session_id":"sid-d"}' TMUX_PANE=%0                                # no cwd
wait_calls 3 3
hk_eq "Stop: reconciles even when the hook exits early (no sid, claudish, no cwd)" "3" "$(calls)"

# ── Neither: no pane, or a server that does not take part ────────────────────
rm -f "$REC"
hook on_session_start.sh '{"session_id":"sid-e","cwd":"/nowhere"}' CC_TAB_RECONCILE_DELAY=0
hook on_stop.sh '{"session_id":"sid-e","cwd":"/nowhere"}'
rm -f "$T/st/active"
hook on_session_start.sh '{"session_id":"sid-f","cwd":"/nowhere"}' TMUX_PANE=%0 CC_TAB_RECONCILE_DELAY=0
hook on_stop.sh '{"session_id":"sid-f","cwd":"/nowhere"}' TMUX_PANE=%0
sleep 1
hk_eq "no TMUX_PANE, or not participating: no reconcile" "0" "$(calls)"

hk_finish
