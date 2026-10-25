#!/usr/bin/env bash
# on_session_start.sh — Claude Code SessionStart hook
#
# Fires once when a Claude Code session starts or resumes. Writes the
# session_id to a per-pane sidecar file keyed by stable tmux pane identity
# (session-window-pane index), which survives tmux-resurrect restore.
#
# Skips subagent sessions (e.g. claudish-spawned instances) by detecting:
#   1. agent_type field in the SessionStart JSON (set when --agent flag is used)
#   2. ephemeral headless/print runs (-p, --print, --output-format), detected on
#      the invoking claude's own argv. NOT CLAUDISH_ACTIVE_MODEL_NAME — see below.
#
# Add to ~/.claude/settings.json:
#
#   "hooks": {
#     "SessionStart": [
#       {
#         "hooks": [
#           {
#             "type": "command",
#             "command": "bash <moox share root>/plugins/tmux-claude-continuity/scripts/on_session_start.sh"
#           }
#         ]
#       }
#     ]
#   }

input="$(cat)"

# shellcheck source=lib/cc_common.sh
. "$(dirname "$0")/lib/cc_common.sh"

# ── Skip subagent sessions ────────────────────────────────────────────────────

# Signal 1: agent_type is set → spawned with --agent flag (Task tool subagents).
agent_type="$(echo "$input" | jq -r '.agent_type // empty' 2>/dev/null)"
[ -n "$agent_type" ] && exit 0

# Signal 2: headless / print sessions are ephemeral — an MCP create_session, a
# `-p` one-shot, or a JSON-output run. They never occupy a tmux pane as its
# foreground program, so they must not claim a pane's resume slot. Detect via the
# INVOKING claude's own argv ($PPID is the claude that spawned this hook).
#
# NOTE: we intentionally do NOT skip on CLAUDISH_ACTIVE_MODEL_NAME. That env var
# is set on EVERY claude claudish spawns — including the MAIN interactive pane
# session — so skipping on it dropped the user's real claudish panes from restore
# entirely. The main claudish session runs claude interactively (no -p) and IS a
# restorable pane; only the ephemeral shapes above should be skipped.
_cc_claude_args="$(ps -o command= -p "$PPID" 2>/dev/null)"
case " $_cc_claude_args " in
  *" -p "*|*" --print "*|*" --output-format "*) exit 0 ;;
esac

# ── Capture session ID ────────────────────────────────────────────────────────

session_id="$(echo "$input" | jq -r '.session_id // empty' 2>/dev/null)"
[ -n "$session_id" ] || exit 0

# TMUX_PANE gates the POSITION-keyed write only — never the PID-keyed one below,
# which is what the snapshot enrichment actually reads.
#
# `display-message -t ""` does NOT fail: tmux treats an empty target as "the
# active pane of the current client". So a Claude session running OUTSIDE tmux
# (a plain iTerm/Terminal shell) resolves a pane key anyway and overwrites the
# record of whatever pane the user happens to be looking at. Observed: a
# non-tmux session claimed the position key of a live pane in a different
# project. Restore does not resume from these files, so the blast radius is
# diagnostics — but wrong data is worse than no data when debugging a restore.
#
# Participation gate for the position sidecar (PR-C12, R2-C5, R3-C4). Position
# keys (`#S-#I-#P`) collide across servers, so the sidecar is written only for a
# server that takes part, and only into ITS directory:
#   * MOOX_SOCKET set: only if it names this server, MOOX_ACTIVE=1 and the
#     participation marker still exists (_cc_participating), into
#     $MOOX_PANES_DIR (per socket). A scratch server started inside a real pane
#     inherits the real MOOX_* values and is refused by the socket compare.
#   * no MOOX_* at all (a pane from before the cutover): only on the real login
#     default server and only while the legacy veto file is absent, into the
#     legacy panes/ dir.
# The by-pid registry further down is written always (X12).
pane_key=""
panes_dir=""
if [ -n "${TMUX_PANE:-}" ] && [ -n "${TMUX:-}" ]; then
  if [ -n "${MOOX_SOCKET:-}" ] || [ -n "${MOOX_STATE_DIR:-}" ]; then
    _cc_participating && panes_dir="${MOOX_PANES_DIR:-}"
  elif _cc_on_legacy_default && _cc_participating; then
    panes_dir="$HOME/.config/tmux-claude/panes"
  fi
  if [ -n "$panes_dir" ]; then
    pane_key="$($TMUX_CMD display-message -t "$TMUX_PANE" -p '#S-#I-#P' 2>/dev/null)"
  fi
fi

[ -n "$pane_key" ] && mkdir -p "$panes_dir"

# ── Look up customTitle from session JSONL (set via /title or session rename) ──
cwd="$(echo "$input" | jq -r '.cwd // empty' 2>/dev/null)"
custom_title=""
if [ -n "$cwd" ]; then
  project_key="${cwd//\//-}"
  jsonl="$HOME/.claude/projects/${project_key}/${session_id}.jsonl"
  if [ -f "$jsonl" ]; then
    custom_title="$(grep -m1 '"custom-title"' "$jsonl" 2>/dev/null | jq -r '.customTitle // empty' 2>/dev/null)"
  fi
fi

# Line 1: always the UUID (the resume token)
# Line 2: custom title if set (used for display/diagnostics only)
if [ -n "$pane_key" ]; then
  if [ -n "$custom_title" ]; then
    printf '%s\n%s\n' "$session_id" "$custom_title" > "${panes_dir}/${pane_key}.session-id"
  else
    echo "$session_id" > "${panes_dir}/${pane_key}.session-id"
  fi
fi

# ── PID-keyed sidecar ─────────────────────────────────────────────────────────
# The position-keyed file above (session-window-pane index) drifts when tmux
# windows are renumbered, moved, or swapped between save and restore. The
# PID-keyed file is unambiguous for the lifetime of the Claude process and is
# the primary source for the moox save's session-id climb (sidmap).
#
# $PPID is the Claude process that invoked this hook.
#
# The registry is shared across servers and NEVER derived from panes_dir, which
# is per socket (R3-C2). Each entry carries a <pid>.start token so a reader can
# tell a recycled pid from the Claude that wrote it (PR-M16).
by_pid_dir="$(_cc_by_pid_dir)"
mkdir -p "$by_pid_dir"

if [ -n "$custom_title" ]; then
  printf '%s\n%s\n' "$session_id" "$custom_title" > "${by_pid_dir}/${PPID}.session-id"
else
  echo "$session_id" > "${by_pid_dir}/${PPID}.session-id"
fi
_cc_start_token "$PPID" > "${by_pid_dir}/${PPID}.start" 2>/dev/null || true

# ── Tab colour / name (lifecycle 6.1) ─────────────────────────────────────────
# Announce "this session exists" to the ONE consumer that paints the tmux window
# tab. This script stays ignorant of tabs: cc_tab_reconcile.sh recomputes the
# whole answer for this pane's window from the session records plus a live pane
# inventory. It is idempotent, so a duplicate announcement costs nothing — which
# is what makes it safe to fire this from here AND from the Stop hook AND from
# tmux's own pane hooks.
#
# Backgrounded and fully redirected: a Claude hook must never block on it, and a
# hook that writes to stdout confuses the client.
#
# TMUX_PANE is REQUIRED, not optional. A Claude session running outside tmux
# still resolves a target with an empty `-t`, because tmux reads that as "the
# active pane of the current client" — which is how a non-tmux session once
# claimed a live pane's sidecar (see the note further up this file). The same
# trap would have it repaint whatever window the user happens to be looking at.
#
# The delay is deliberate: at SessionStart Claude has not necessarily written
# ~/.claude/sessions/<pid>.json yet, and reconciling before the record exists
# paints the window as "no Claude here". The Stop hook repairs that within one
# turn regardless; the delay just makes the first attempt the correct one.
_cc_tab_reconcile="$(dirname "$0")/cc_tab_reconcile.sh"
# Only for a participating server (G3-M8): D7 gives an excluded or unmanaged
# server no slot [103] tab painting, so a Claude there must not paint it either.
if [ -n "${TMUX:-}" ] && [ -n "${TMUX_PANE:-}" ] && [ -x "$_cc_tab_reconcile" ] && _cc_participating; then
  ( sleep "${CC_TAB_RECONCILE_DELAY:-1}"
    "$_cc_tab_reconcile" "$TMUX_PANE" ) >/dev/null 2>&1 &
fi
