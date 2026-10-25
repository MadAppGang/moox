#!/usr/bin/env bash
# tab-reconcile-foreign.test.sh — X5: cc_tab_reconcile.sh paints a window only
# for Claude records whose pid climbs UP (≤24 hops) to one of THIS server's
# pane_pids. Pane ids collide across servers (every server numbers from %0), so
# without the membership gate a Claude on server B naming `%0` painted server
# A's %0. A pid whose start time differs from its by-pid `<pid>.start` token is
# a recycled pid and counts as dead (PR-M16).
#
# Two scratch servers, A and B. The record's name reaches @cc_name with
# @cc_tab_name_source=session, which makes the paint observable without
# transcripts.
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/lib/hooklib.sh"
. "$here/lib/tmuxlib.sh"
hk_setup
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }
RECONCILE="$SHARE/plugins/tmux-claude-continuity/scripts/cc_tab_reconcile.sh"
ts_start B
B_PID="$(ts list-panes -t s -F '#{pane_pid}')"       # a Claude-like process living on B
ts_start A; SOCK_A="$TS_SOCK"
A_PID="$(ts list-panes -t s -F '#{pane_pid}')"
A_WIN="$(ts list-panes -t s -F '#{window_id}')"
ts set -g @cc_tab_name_source session
SESS="$T/sessions"; mkdir -p "$SESS" "$T/bypid"
ts set -g @claude-continuity-by-pid-dir "$T/bypid"

rec() { # pid name
  printf '{"pid":%s,"sessionId":"%08x-0000-4000-8000-000000000000","cwd":"/nowhere","tmux":"s:%s.%%0","name":"%s","nameSource":"user"}\n' \
    "$1" "$1" "$A_WIN" "$2" > "$SESS/$1.json"
}
reconcile_a() {
  env -i HOME="$H" PATH="$(dirname "$(command -v tmux)"):$(dirname "$(command -v jq)"):$HK_PATH" \
    CC_SESSIONS_DIR="$SESS" CC_LOG_FILE="$T/log" CC_PROJECTS_DIR="$T/projects" \
    CC_COLOUR_STAMP_DIR="$T/stamp" CC_COLOUR_CACHE_DIR="$T/cache" \
    TMUX_CMD="tmux -u -S $SOCK_A" bash "$RECONCILE" --all
}
name_a() { env -u TMUX tmux -S "$SOCK_A" show -wqv -t "$A_WIN" @cc_name; }

rec "$B_PID" "from-server-B"
reconcile_a
hk_eq "a live Claude on ANOTHER server naming %0 does not paint this server's %0" "" "$(name_a)"

rm -f "$SESS"/*.json
rec "$A_PID" "own-claude"
reconcile_a
hk_eq "a Claude descended from this server's pane paints" "own-claude" "$(name_a)"

env -u TMUX tmux -S "$SOCK_A" set -wu -t "$A_WIN" @cc_name
printf 'Thu_Jan__1_00:00:00_1970\n' > "$T/bypid/$A_PID.start"
reconcile_a
hk_eq "a pid whose start token differs (recycled) counts as dead" "" "$(name_a)"

hk_finish
