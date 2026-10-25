#!/usr/bin/env bash
# on-session-start-skips.test.sh — B48: the SessionStart hook skips ephemeral
# Claude sessions and nothing else.
#
#   agent_type in the hook JSON (a --agent subagent)           → nothing written
#   the invoking claude ran with -p / --print / --output-format → nothing written
#   CLAUDISH_ACTIVE_MODEL_NAME set (every claudish pane has it) → NOT skipped
#   a normal session                                           → by-pid entry + position sidecar
#   no TMUX_PANE                                               → by-pid entry only, no sidecar
#
# The hook reads the invoking claude's argv as `ps -o command= -p $PPID`, so
# each case runs it from a fake `claude` script that stays the hook's parent.
# Isolation: a `tmux -f /dev/null -S $T/<name>` server (tmuxlib.sh), a scratch
# HOME, `env -i`; nothing outside the temp dir is written.
. "$(dirname "$0")/lib/hooklib.sh"
. "$(dirname "$0")/lib/tmuxlib.sh"
hk_setup
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }
SCRIPTS="$SHARE/plugins/tmux-claude-continuity/scripts"
TOOLS="$(dirname "$(command -v tmux)"):$(dirname "$(command -v jq)"):$HK_PATH"
ts_start srv
mkdir -p "$T/st/panes" "$T/bin"
: > "$T/st/active"
BYPID="$H/.config/tmux-claude/panes/by-pid"

# The fake claude: runs the hook as its child (not exec), so $PPID is this
# script and its argv is what the hook inspects.
cat > "$T/bin/claude" <<EOF
#!/bin/bash
bash "$SCRIPTS/on_session_start.sh"
rc=\$?
exit \$rc
EOF
chmod +x "$T/bin/claude"

start() { # <json> [VAR=val ...] -- [claude args...]
  local json="$1"; shift
  local envs=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
  [ "${1:-}" = "--" ] && shift
  rm -rf "$T/st/panes"/* "$BYPID"
  printf '%s' "$json" | env -i HOME="$H" PATH="$TOOLS" CC_TAB_RECONCILE_DELAY=0 \
    MOOX_SOCKET="$TS_SOCK" MOOX_ACTIVE=1 MOOX_STATE_KEY="srv@00000000" MOOX_STATE_DIR="$T/st" \
    MOOX_PANES_DIR="$T/st/panes" TMUX="$TS_SOCK,$TS_PID,0" ${envs[@]+"${envs[@]}"} "$T/bin/claude" "$@"
}
bypid() { cat "$BYPID"/*.session-id 2>/dev/null | head -n1; }
sidecar() { head -n1 "$T/st/panes/s-0-0.session-id" 2>/dev/null; }
written() { find "$T/st/panes" "$BYPID" -type f 2>/dev/null | sort | tr '\n' ' '; }

# Control: a normal interactive session writes both, so the skips below are
# not vacuous.
start '{"session_id":"sid-plain","cwd":"/nowhere"}' TMUX_PANE=%0 --
hk_eq "normal session: by-pid entry" "sid-plain" "$(bypid)"
hk_eq "normal session: position sidecar" "sid-plain" "$(sidecar)"
hk_eq "normal session: the by-pid key is the invoking claude" "1" "$(find "$BYPID" -name '*.session-id' | wc -l | tr -d ' ')"

start '{"session_id":"sid-agent","cwd":"/nowhere","agent_type":"general-purpose"}' TMUX_PANE=%0 --
hk_eq "agent_type set: nothing written" "" "$(written)"

for args in "-p hello" "--print" "--output-format json" "--model x -p hello"; do
  # shellcheck disable=SC2086
  start '{"session_id":"sid-print","cwd":"/nowhere"}' TMUX_PANE=%0 -- $args
  hk_eq "claude $args: nothing written" "" "$(written)"
done

# A flag that merely starts with p, or -p inside a word, is not a print run.
start '{"session_id":"sid-pflag","cwd":"/nowhere"}' TMUX_PANE=%0 -- --permission-mode plan
hk_eq "claude --permission-mode plan: not skipped" "sid-pflag" "$(bypid)"

start '{"session_id":"sid-claudish","cwd":"/nowhere"}' TMUX_PANE=%0 CLAUDISH_ACTIVE_MODEL_NAME=some-model --
hk_eq "CLAUDISH_ACTIVE_MODEL_NAME set: by-pid entry still written" "sid-claudish" "$(bypid)"
hk_eq "CLAUDISH_ACTIVE_MODEL_NAME set: sidecar still written" "sid-claudish" "$(sidecar)"

start '{"session_id":"sid-nopane","cwd":"/nowhere"}' --
hk_eq "no TMUX_PANE: by-pid entry written" "sid-nopane" "$(bypid)"
hk_eq "no TMUX_PANE: no position sidecar" "" "$(find "$T/st/panes" -type f | tr '\n' ' ')"

start '{"cwd":"/nowhere"}' TMUX_PANE=%0 --
hk_eq "no session_id: nothing written" "" "$(written)"

hk_finish
