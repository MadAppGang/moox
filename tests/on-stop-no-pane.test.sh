#!/usr/bin/env bash
# on-stop-no-pane.test.sh — C3: with TMUX_PANE unset (a Claude outside tmux),
# on_stop.sh writes no position sidecar and makes NO tmux call at all —
# `display-message -t ""` would resolve "the active pane of the current client",
# which belongs to someone else. A `tmux` stub first on PATH logs every call.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
command -v jq >/dev/null 2>&1 || { echo "SKIP: jq not installed"; exit 0; }
SCRIPTS="$SHARE/plugins/tmux-claude-continuity/scripts"
mkdir -p "$T/stub" "$H/.config/tmux-claude/panes"
printf '#!/bin/sh\necho "tmux $*" >> "%s/tmux-calls"\necho s-1-1\n' "$T" > "$T/stub/tmux"
chmod +x "$T/stub/tmux"
printf 'old\n' > "$H/.config/tmux-claude/panes/s-1-1.session-id"
PATH_STUB="$T/stub:$(dirname "$(command -v jq)"):$HK_PATH"

for case in "no TMUX, no TMUX_PANE" "TMUX set, TMUX_PANE unset"; do
  rm -f "$T/tmux-calls"
  if [ "$case" = "no TMUX, no TMUX_PANE" ]; then
    printf '{"session_id":"x","cwd":"/nowhere"}' | env -i HOME="$H" PATH="$PATH_STUB" bash "$SCRIPTS/on_stop.sh"
  else
    printf '{"session_id":"x","cwd":"/nowhere"}' | env -i HOME="$H" PATH="$PATH_STUB" \
      TMUX="$HK_LEGACY_SOCK,1,0" bash "$SCRIPTS/on_stop.sh"
  fi
  [ ! -e "$T/tmux-calls" ] && ok "$case: no tmux call" || no "$case: no tmux call" "$(cat "$T/tmux-calls")"
  hk_eq "$case: the sidecar is untouched" "old" "$(cat "$H/.config/tmux-claude/panes/s-1-1.session-id")"
done

hk_finish
