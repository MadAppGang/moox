#!/bin/sh
# Per-window Claude session tab colour — the MANUAL half.
#
#   claude-tab-colour.sh menu          open the picker
#   claude-tab-colour.sh set <token>   pin a colour on the current window
#   claude-tab-colour.sh auto          drop the pin, hand the window back
#
# No key of its own: the picker is the "Tab colour" row of the prefix+T theme
# menu, which theme.sh's menu() builds by invoking `menu` here. prefix+T is the
# whole appearance surface, and capital C — the obvious mnemonic — is already
# tmux's built-in `customize-mode -Z`.
#
# Tokens: red orange yellow green cyan blue purple pink default
#
# ---------------------------------------------------------------------------
# What this writes, and what it deliberately does not
#
# Two window options, both holding a TOKEN and never a hex:
#
#   @cc_colour_pin   the human's choice. Outranks whatever Claude says.
#   @cc_colour       the EFFECTIVE colour the status format actually reads.
#
# The token->hex chains live in conf/theme.conf (@cc_fill_current /
# @cc_fill_inactive) and resolve @thm_cc_* at draw time, so prefix+T re-skins a
# pinned tab for free. Nothing here may write a colour value — doing so would
# freeze whichever palette happened to be loaded, which is the exact bug the
# window-status-current-format comment in conf/theme.conf exists to document.
#
# `set -wu`, not `set -w … ""`, is how a pin is CLEARED. conf/theme.conf sets a
# global inheritance floor (@cc_colour default, @cc_colour_pin ""), and
# unsetting a window option drops it back to that floor. Writing an empty
# string instead would leave a window option standing that merely looks unset.
#
# ---------------------------------------------------------------------------
# Relationship to the reconciler
#
# cc_tab_reconcile.sh lives in the tmux-claude-continuity plugin — a SEPARATE
# git repo — and is the thing that colours tabs automatically from Claude's own
# session records. It may not be installed. So:
#
#   * every colour is applied HERE first, directly, so the prefix+T "Tab colour"
#     picker works on its own with no plugin at all;
#   * then the reconciler is invoked if present. It is idempotent and a pin
#     outranks everything else it knows, so it recomputes the same answer. This
#     is a fast path, not a second source of truth.
#   * "Auto" clears the pin and re-runs the reconciler, which is what hands the
#     window back to Claude. With no reconciler installed it unsets @cc_colour
#     too, so the tab simply goes uncoloured.
#
# It is invoked directly, as a child process — never via `tmux run-shell` from
# a script. See the run-shell note in conf/options.conf: a run-shell whose client has
# gone away by the time the job prints segfaulted the server and took all 16
# sessions with it (2026-08-16). Every run-shell this file puts into a menu
# entry therefore carries `>/dev/null 2>&1 || true`, the form proven to survive.

set -e

# Only a server's own job may run this: with $TMUX empty every bare `tmux`
# below would reach the user's DEFAULT server, whichever server the caller
# meant (I-1).
[ -n "${TMUX:-}" ] || exit 0

# Root-relative: this file lives in <moox share root>/scripts, the reconciler in
# the vendored continuity plugin beside it.
RECONCILE="$(cd "$(dirname "$0")/.." && pwd)/plugins/tmux-claude-continuity/scripts/cc_tab_reconcile.sh"

# --- helpers ---------------------------------------------------------------

# The window to act on. run-shell executes with the server's environment plus
# TMUX_PANE for the pane the binding fired from, so resolve through that rather
# than trusting "the current window" — with several clients attached to the
# same session those are not always the same window.
current_window() {
  if [ -n "${TMUX_PANE:-}" ]; then
    w=$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}' 2>/dev/null) || w=""
    if [ -n "$w" ]; then
      printf '%s' "$w"
      return 0
    fi
  fi
  tmux display-message -p '#{window_id}' 2>/dev/null || true
}

reconcile() {
  [ -x "$RECONCILE" ] || return 0
  "$RECONCILE" "$1" >/dev/null 2>&1 || true
}

# --- actions ---------------------------------------------------------------

set_colour() {
  case "$1" in
    red|orange|yellow|green|cyan|blue|purple|pink|default) ;;
    *) echo "claude-tab-colour.sh: unknown colour '$1'" >&2; exit 1 ;;
  esac
  win=$(current_window)
  [ -n "$win" ] || exit 0
  tmux set -w -t "$win" @cc_colour_pin "$1"
  tmux set -w -t "$win" @cc_colour "$1"
  reconcile "$win"
  tmux refresh-client -S 2>/dev/null || true
}

auto_colour() {
  win=$(current_window)
  [ -n "$win" ] || exit 0
  tmux set -wu -t "$win" @cc_colour_pin 2>/dev/null || true
  if [ -x "$RECONCILE" ]; then
    "$RECONCILE" "$win" >/dev/null 2>&1 || true
  else
    tmux set -wu -t "$win" @cc_colour 2>/dev/null || true
  fi
  tmux refresh-client -S 2>/dev/null || true
}

# --- interactive picker ----------------------------------------------------
#
# Same idiom as theme.sh's menu(): a native display-menu rather than fzf or a
# popup shell, so it needs no dependency and opens instantly, and each entry
# applies on the spot — the menu doubles as a preview. Pick green, look at the
# tab, pick Auto back.
#
# The dot marks the pin in force. Each row also carries a swatch drawn with
# #{E:@thm_cc_cur_<token>}, resolved when the menu is drawn, so the swatches
# follow the light/dark palette like everything else.
#
# Entries run $0 by absolute path because run-shell executes with the server's
# environment, not an interactive shell's, and $0 as invoked may be relative.

entry() { # $1 token, $2 label — coloured swatch
  if [ "$cur" = "$1" ]; then _d='●'; else _d=' '; fi
  printf '%s #[bg=#{E:@thm_cc_cur_%s}]   #[default] %s' "$_d" "$1" "$2"
}

plain() { # $1 token ("" for Auto), $2 label — no swatch, aligned with entry()
  if [ "$cur" = "$1" ]; then _d='●'; else _d=' '; fi
  printf '%s     %s' "$_d" "$2"
}

menu() {
  win=$(current_window)
  [ -n "$win" ] || exit 0
  # -wqv, not -wv: MEASURED, `show -wv` on an option that was never set prints
  # "invalid option" and exits 1. -q makes it silent with exit 0.
  cur=$(tmux show -wqv -t "$win" @cc_colour_pin 2>/dev/null || true)
  self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")

  tmux display-menu -T "#[align=centre] claude tab colour " -x C -y C \
    "$(entry red    Red)"    r "run-shell '$self set red >/dev/null 2>&1 || true'" \
    "$(entry orange Orange)" o "run-shell '$self set orange >/dev/null 2>&1 || true'" \
    "$(entry yellow Yellow)" y "run-shell '$self set yellow >/dev/null 2>&1 || true'" \
    "$(entry green  Green)"  g "run-shell '$self set green >/dev/null 2>&1 || true'" \
    "$(entry cyan   Cyan)"   c "run-shell '$self set cyan >/dev/null 2>&1 || true'" \
    "$(entry blue   Blue)"   b "run-shell '$self set blue >/dev/null 2>&1 || true'" \
    "$(entry purple Purple)" v "run-shell '$self set purple >/dev/null 2>&1 || true'" \
    "$(entry pink   Pink)"   p "run-shell '$self set pink >/dev/null 2>&1 || true'" \
    ""                                                                             \
    "$(plain default None)"  n "run-shell '$self set default >/dev/null 2>&1 || true'" \
    "$(plain '' 'Auto (Claude decides)')" a "run-shell '$self auto >/dev/null 2>&1 || true'" \
    ""                                                                             \
    "Cancel"                 q ""
}

case "${1:-menu}" in
  menu) menu ;;
  set)  set_colour "${2:-}" ;;
  auto) auto_colour ;;
  *) echo "usage: claude-tab-colour.sh [menu|set <colour>|auto]" >&2; exit 1 ;;
esac
