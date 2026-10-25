#!/usr/bin/env bash
# on_stop.sh — Claude Code Stop hook
#
# Fires after every completed Claude turn. Updates the custom title in the
# per-pane sidecar file so that /rename changes are captured without needing
# to restart the session.
#
# Add to ~/.claude/settings.json alongside the SessionStart hook:
#
#   "Stop": [
#     {
#       "hooks": [
#         {
#           "type": "command",
#           "command": "bash <moox share root>/plugins/tmux-claude-continuity/scripts/on_stop.sh"
#         }
#       ]
#     }
#   ]

input="$(cat)"

# shellcheck source=lib/cc_common.sh
. "$(dirname "$0")/lib/cc_common.sh"

# ── Tab colour / name (lifecycle 6.2 and 6.3) ─────────────────────────────────
# This hook fires after EVERY completed turn, so it is already the thing that
# notices a rename mid-session — and it will be the thing that notices a colour
# change the day Claude ships one, at no new cost and with no new plumbing.
# cc_tab_reconcile.sh recomputes this pane's window from scratch and is
# idempotent, so firing it every turn is cheap and cannot drift.
#
# DELIBERATELY ABOVE THE EARLY EXITS BELOW, and this is the one thing in this
# file that is not simply appended. Every exit below gates on the POSITION-keyed
# sidecar (`#S-#I-#P`), which drifts when windows are renumbered, moved or
# restored — so for exactly the sessions whose sidecar went missing, the tab
# would stop refreshing and quietly keep a stale name or colour. The tab does
# not depend on that file and must not inherit its failure mode.
#
# TMUX_PANE is required: with an empty `-t` tmux resolves "the active pane of
# the current client", so a Claude running outside tmux would repaint whatever
# window the user happens to be looking at.
_cc_tab_reconcile="$(dirname "$0")/cc_tab_reconcile.sh"
# Only for a participating server (G3-M8): D7 gives an excluded or unmanaged
# server no slot [103] tab painting, so a Claude there must not paint it either.
if [ -n "${TMUX:-}" ] && [ -n "${TMUX_PANE:-}" ] && [ -x "$_cc_tab_reconcile" ] && _cc_participating; then
  "$_cc_tab_reconcile" "$TMUX_PANE" >/dev/null 2>&1 &
fi

# Skip claudish-spawned sessions
[ -n "$CLAUDISH_ACTIVE_MODEL_NAME" ] && exit 0

session_id="$(echo "$input" | jq -r '.session_id // empty' 2>/dev/null)"
[ -n "$session_id" ] || exit 0

cwd="$(echo "$input" | jq -r '.cwd // empty' 2>/dev/null)"
[ -n "$cwd" ] || exit 0

# C3: no pane, no position — and no tmux call at all. `display-message -t ""`
# resolves "the active pane of the current client", which is someone else's.
[ -n "${TMUX_PANE:-}" ] && [ -n "${TMUX:-}" ] || exit 0

# Only a participating server touches its position sidecars (PR-C12, R3-C4).
# _cc_participating also requires MOOX_SOCKET to name THIS server, so a scratch
# server started inside a real pane — which inherits the real MOOX_* values —
# cannot rewrite or remove the real server's sidecars. A pane with no MOOX_* at
# all is legacy and counts only on the real default server.
if [ -z "${MOOX_SOCKET:-}" ] && [ -z "${MOOX_STATE_DIR:-}" ]; then
  _cc_on_legacy_default || exit 0
fi
_cc_participating || exit 0

pane_key="$($TMUX_CMD display-message -t "$TMUX_PANE" -p '#S-#I-#P' 2>/dev/null)"
[ -n "$pane_key" ] || exit 0

# Per socket: the gate sets @claude-continuity-panes-dir for this server.
panes_dir="$(_cc_panes_dir)"

metadata_file="${panes_dir}/${pane_key}.session-id"
[ -f "$metadata_file" ] || exit 0

# Read customTitle from session JSONL (if it exists)
project_key="${cwd//\//-}"
jsonl="$HOME/.claude/projects/${project_key}/${session_id}.jsonl"

custom_title=""
if [ -f "$jsonl" ]; then
  custom_title="$(grep '"custom-title"' "$jsonl" 2>/dev/null | tail -1 | jq -r '.customTitle // empty' 2>/dev/null)"
fi

# Line 1: always the UUID (the resume token)
# Line 2: custom title if set (used for display/diagnostics only)
if [ -n "$custom_title" ]; then
  printf '%s\n%s\n' "$session_id" "$custom_title" > "$metadata_file"
else
  echo "$session_id" > "$metadata_file"
fi
