#!/usr/bin/env bash
# popup-no-resurrect-reads.test.sh — I11: cc_popup.sh (and every vendored
# continuity script) reads tmux options and the moox/continuity stores only —
# never resurrect's directory, its `last` link or a tmux_resurrect_* file.
# (Before moox, the popup and post_restore resolved @resurrect-dir and `last`.)
#
#   static:  no non-comment line of the plugin's scripts, libs or shell hooks
#            names resurrect (@resurrect-dir, tmux_resurrect_, resurrect/last);
#            a negative control proves the scan finds a planted read
#   dynamic: `cc_popup.sh --list` on a participating scratch server prints the
#            same tree (bar the idle and rss columns) with a broken resurrect
#            tree and @resurrect-dir set as without them
#
# Isolation: a `tmux -f /dev/null -S $T/<name>` server, a scratch HOME,
# `env -i`; the freeze store lands under the scratch HOME.
. "$(dirname "$0")/lib/hooklib.sh"
. "$(dirname "$0")/lib/tmuxlib.sh"
hk_setup
P="$SHARE/plugins/tmux-claude-continuity"

resurrect_reads() { awk 'tolower($0) ~ /resurrect/ && !/^[ \t]*#/ { print FILENAME ":" FNR ": " $0 }' "$@"; }

files=()
while IFS= read -r f; do files+=("$f"); done < <(
  find "$P/scripts" -type f \( -name '*.sh' -o -name '*.awk' \) | sort
  printf '%s\n' "$P/claude-continuity.zsh" "$P/claude-continuity.bash")
hk_eq "cc_popup.sh: no resurrect read" "" "$(resurrect_reads "$P/scripts/cc_popup.sh")"
hk_eq "no vendored continuity script reads resurrect (${#files[@]} files)" "" "$(resurrect_reads "${files[@]}")"
printf '# resurrect in a comment\ndir="$(tmux show -gqv @resurrect-dir)"\nls "$dir/last"\n' > "$T/planted.sh"
hk_eq "negative control: the scan finds the planted read" "2" \
  "$(resurrect_reads "$T/planted.sh" | awk -F: '{ print $2 }')"

command -v jq >/dev/null 2>&1 || { echo "SKIP dynamic: jq not installed"; hk_finish; exit $?; }
ts_start srv
ts new-window -d -t s: 'sleep 600'
mkdir -p "$T/st/panes"
: > "$T/st/active"
TOOLS="$(dirname "$(command -v tmux)"):$(dirname "$(command -v jq)"):$HK_PATH"
list() {
  env -i HOME="$H" PATH="$TOOLS" TERM=dumb MOOX_SOCKET="$TS_SOCK" MOOX_ACTIVE=1 MOOX_STATE_KEY="srv@00000000" \
    MOOX_STATE_DIR="$T/st" MOOX_PANES_DIR="$T/st/panes" \
    bash "$P/scripts/cc_popup.sh" --list >"$T/list.out" 2>"$T/list.err"
  local rc=$?
  cut -f1-4,7- "$T/list.out" # minus idle_seconds and rss_bytes, which move on their own
  return "$rc"
}
before="$(list)"; rc_before=$?
hk_eq "--list on a participating server exits 0" "0" "$rc_before"
[ -n "$before" ] && ok "--list prints the tree ($(printf '%s\n' "$before" | wc -l | tr -d ' ') rows)" \
  || no "--list prints the tree" "empty; stderr: $(head -3 "$T/list.err")"

for d in "$H/.local/share/tmux/resurrect" "$T/res"; do
  mkdir -p "$d"
  printf 'garbage\tnot\ta\tsnapshot\n' > "$d/tmux_resurrect_20260101T000000.txt"
  ln -s tmux_resurrect_20991231T235959.txt "$d/last"
done
ts set -g @resurrect-dir "$T/res"
after="$(list)"; rc_after=$?
hk_eq "--list with a broken resurrect tree still exits 0" "0" "$rc_after"
hk_eq "--list prints the same tree with or without resurrect's files" "$before" "$after"

hk_finish
