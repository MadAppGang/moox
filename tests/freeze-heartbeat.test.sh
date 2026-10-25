#!/usr/bin/env bash
# freeze-heartbeat.test.sh — B23 / architecture §5.7: `cc_freeze.sh heartbeat`
# is pre_save.sh's per-save store block (namespace initialiser :201-216 plus
# :660-764), started by the moox daemon after each published save.
#   - under `set -u`, with _CC_STORE_NS absent from the environment
#   - a server that never froze anything: nothing is created, exit 0
#   - with a namespace: the ledger is ticked (seeded on its first tick), a
#     thawed entry whose sessions are not live is KEPT, and the sweep is
#     started (directly, detached) only with autofreeze on
#   - no `run-shell` anywhere on the heartbeat's path (its parent is a tool
#     client; `tmux run-shell` from one crashes tmux 3.7b)
# Scratch `-L moox-test-…` server with -f /dev/null; CC_TEST=1 so the script's
# own isolation assertion is on.
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/lib/hooklib.sh"
hk_setup
command -v tmux >/dev/null 2>&1 || { echo "SKIP: tmux not installed"; exit 0; }
SCRIPTS="$SHARE/plugins/tmux-claude-continuity/scripts"
SOCK="moox-test-cchb$$"
_t() { env -u TMUX tmux -L "$SOCK" -f /dev/null "$@"; }
SHELL=/bin/sh _t new-session -d -s s 'sleep 300' || { echo "ABORT: no scratch server"; exit 1; }
trap '_t kill-session -t s 2>/dev/null; rm -rf "$T"' EXIT INT TERM HUP

mkdir -p "$T/frozen" "$T/cc/by-pid"
for o in pending launch panes; do _t set -g "@claude-continuity-$o-dir" "$T/cc/$o"; done
_t set -g @claude-continuity-by-pid-dir "$T/cc/by-pid"
KEY="hbkey"
# The daemon exports THIS server's socket path with its key; a key paired with
# another server's socket is not trusted (G3-M2), so the fixture uses the real one.
SOCK_PATH="$(_t display-message -p '#{socket_path}')"
mkdir -p "$T/st"; : > "$T/st/active"     # the participation marker the daemon's env names

heartbeat() {
  env -u TMUX -u TMUX_PANE -u MOOX_SOCKET -u MOOX_ACTIVE -u MOOX_STATE_KEY -u MOOX_STATE_DIR -u MOOX_PANES_DIR -u MOOX_LAUNCH_DIR -u MOOX_PENDING_DIR -u MOOX_ROOT -u _CC_STORE_NS HOME="$H" PATH="$(dirname "$(command -v tmux)"):$HK_PATH" CC_TEST=1 \
    CC_FREEZE_DIR="$T/frozen" TMUX_CMD="tmux -L $SOCK -f /dev/null" CC_LOG_FILE="$T/log" \
    MOOX_STATE_KEY="$KEY" MOOX_SOCKET="$SOCK_PATH" MOOX_ACTIVE=1 MOOX_STATE_DIR="$T/st" \
    bash "$SCRIPTS/cc_freeze.sh" heartbeat
}

# 1. Never froze anything: no namespace, nothing created.
: > "$T/log"
heartbeat; rc=$?
hk_eq "no store: exit 0" "0" "$rc"
hk_eq "no store: nothing created under the freeze root" "" "$(ls -A "$T/frozen")"

# 2. A namespace with one thawed entry whose session is NOT live.
NS="$T/frozen/$KEY"
mkdir -p "$NS"
SID="11111111-2222-3333-4444-555555555555"
printf 'v\t1\nkey\tk1\nunit\tpane\nthawed_at\t2026-10-25T00:00:00Z\nsid\tprimary\t%s\n' "$SID" > "$NS/k1.state"
: > "$T/log"
heartbeat; rc=$?
hk_eq "store, autofreeze off: exit 0" "0" "$rc"
if ls "$NS"/*ledger* >/dev/null 2>&1 || grep -q 'LEDGER' "$T/log"; then ok "the ledger was ticked"
else no "the ledger was ticked" "$(ls -A "$NS"; cat "$T/log")"; fi
[ -f "$NS/k1.state" ] && ok "a thawed entry with no live session is kept" || no "a thawed entry with no live session is kept" "$(ls -A "$NS")"
if grep -q 'SWEEP' "$T/log"; then no "autofreeze off: no sweep started" "$(grep SWEEP "$T/log")"; else ok "autofreeze off: no sweep started"; fi
if grep -q 'unbound variable' "$T/log" 2>/dev/null; then no "set -u clean" "$(cat "$T/log")"; else ok "set -u clean"; fi

# 3. autofreeze on: the sweep is started, detached, by the heartbeat itself.
_t set -g @claude-continuity-autofreeze on
_t set -g @claude-continuity-autofreeze-idle 999d
: > "$T/log"
heartbeat; rc=$?
hk_eq "store, autofreeze on: exit 0" "0" "$rc"
i=0
while [ $i -lt 50 ] && ! grep -q 'SWEEP' "$T/log"; do sleep 0.1; i=$((i + 1)); done
grep -q 'SWEEP' "$T/log" && ok "autofreeze on: the sweep ran" || no "autofreeze on: the sweep ran" "$(cat "$T/log")"

# 4. No run-shell on the heartbeat's path (comments excluded).
body="$(awk '/^# ── heartbeat: /,/^# ── Entry /' "$SCRIPTS/cc_freeze.sh" | grep -v '^[[:space:]]*#')"
case "$body" in
  *run-shell*) no "no run-shell in the heartbeat section" "$(printf '%s\n' "$body" | grep run-shell)" ;;
  *)           ok "no run-shell in the heartbeat section" ;;
esac
hk_eq "the heartbeat section exists" "1" "$([ -n "$body" ] && echo 1)"

hk_finish
