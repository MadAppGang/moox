#!/usr/bin/env bash
# by-pid-dir-never-follows-panes-dir.test.sh — R3-C2: the by-pid registry is
# the option @claude-continuity-by-pid-dir if set, else the ONE shared path; it
# is never derived from the per-socket panes dir (`${panes_dir}/by-pid`).
#
# A scratch server sets @claude-continuity-panes-dir to a per-socket dir and
# @claude-continuity-by-pid-dir to a separate shared dir:
#   * on_session_start.sh writes the position sidecar under the first and the
#     by-pid entry (+ .start token) under the second;
#   * _cc_by_pid_dir resolves the second, and with the option unset the shared
#     default — never <panes>/by-pid;
#   * cc_freeze/cc_thaw/cc_popup take by_pid_dir from _cc_by_pid_dir, and no
#     vendored script derives it from a panes dir.
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/lib/hooklib.sh"
. "$here/lib/tmuxlib.sh"
hk_setup
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }
SCRIPTS="$SHARE/plugins/tmux-claude-continuity/scripts"
TOOLS="$(dirname "$(command -v tmux)"):$(dirname "$(command -v jq)"):$HK_PATH"
ts_start srv
PANES="$T/per-socket/panes"; SHARED="$T/shared/by-pid"
mkdir -p "$PANES"; : > "$T/per-socket/active"
ts set -g @claude-continuity-panes-dir "$PANES"
ts set -g @claude-continuity-by-pid-dir "$SHARED"

printf '{"session_id":"sid-1","cwd":"/nowhere"}' | env -i HOME="$H" PATH="$TOOLS" CC_TAB_RECONCILE_DELAY=0 \
  MOOX_SOCKET="$TS_SOCK" MOOX_ACTIVE=1 MOOX_STATE_DIR="$T/per-socket" MOOX_PANES_DIR="$PANES" \
  TMUX="$TS_SOCK,$TS_PID,0" TMUX_PANE=%0 bash "$SCRIPTS/on_session_start.sh"
hk_eq "sidecar under the per-socket panes dir" "sid-1" "$(head -n1 "$PANES/s-0-0.session-id" 2>/dev/null)"
hk_eq "by-pid entry under the shared dir" "sid-1" "$(head -n1 "$SHARED/$$.session-id" 2>/dev/null)"
[ -s "$SHARED/$$.start" ] && ok "start token beside it" || no "start token beside it" "missing"
[ ! -e "$PANES/by-pid" ] && ok "nothing under <panes>/by-pid" || no "nothing under <panes>/by-pid" "$(ls -R "$PANES/by-pid")"

resolve() { env -i HOME="$H" PATH="$TOOLS" TMUX="$TS_SOCK,$TS_PID,0" \
  bash -c '. "$1/lib/cc_common.sh"; _cc_by_pid_dir' _ "$SCRIPTS"; }
hk_eq "_cc_by_pid_dir: the option" "$SHARED" "$(resolve)"
ts set -gu @claude-continuity-by-pid-dir
hk_eq "_cc_by_pid_dir: option unset → the shared default, not <panes>/by-pid" \
  "$H/.config/tmux-claude/panes/by-pid" "$(resolve)"

for f in cc_freeze.sh cc_thaw.sh cc_popup.sh; do
  if grep -q '^by_pid_dir="$(_cc_by_pid_dir)"' "$SCRIPTS/$f"; then ok "$f takes by_pid_dir from _cc_by_pid_dir"
  else no "$f takes by_pid_dir from _cc_by_pid_dir" "$(grep -n 'by_pid_dir=' "$SCRIPTS/$f")"; fi
done
bad="$(grep -rn 'panes_dir}*/by-pid\|panes_dir/by-pid' "$SCRIPTS" 2>/dev/null | grep -v '^[^:]*:[0-9]*: *#')"
hk_eq "no vendored script derives by-pid from a panes dir" "" "$bad"

hk_finish
