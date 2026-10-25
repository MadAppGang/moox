#!/usr/bin/env bash
# no-bare-tmux.test.sh — X9: every tmux call in the vendored continuity plugin
# goes through $TMUX_CMD. A bare `tmux …` (no -S/-L) talks to whatever server
# $TMUX or the default socket names — from a daemon or a hook outside the pane,
# that is the user's real server.
#
#   * no command-position `tmux` in any script, lib or shell hook of
#     share/moox/plugins/tmux-claude-continuity (comments and messages aside);
#   * TMUX_CMD is built from ${TMUX%%,*} inside a pane, else from $MOOX_SOCKET,
#     else it is `false` — never a default-socket fallback;
#   * a negative control: the detector finds bare calls planted in a scratch file.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
P="$SHARE/plugins/tmux-claude-continuity"

# bare_tmux FILE...: "file:line: text" for each command-position tmux. A line
# whose first non-blank character is # is a comment. Command position: the
# start of the line, or after $( ` ( { ; & | or a shell keyword.
bare_tmux() {
  awk '
    /^[ \t]*#/ { next }
    /(^|\$\(|[`({;&|]|(^|[ \t])(then|do|else|exec|command|time|!))[ \t]*tmux([ \t]|$)/ {
      print FILENAME ":" FNR ": " $0
    }' "$@"
}

files=()
while IFS= read -r f; do files+=("$f"); done < <(
  find "$P/scripts" -type f \( -name '*.sh' -o -name '*.awk' \) | sort
  printf '%s\n' "$P/claude-continuity.zsh" "$P/claude-continuity.bash")
[ "${#files[@]}" -ge 10 ] && ok "scanned ${#files[@]} vendored continuity files" \
  || no "scanned the vendored continuity files" "only ${#files[@]} found under $P"

found="$(bare_tmux "${files[@]}")"
hk_eq "no bare tmux call in the vendored continuity plugin" "" "$found"

lib="$P/scripts/lib/cc_common.sh"
hk_eq "inside a pane TMUX_CMD names \${TMUX%%,*}" "1" \
  "$(grep -cF 'TMUX_CMD="tmux -u -S ${TMUX%%,*}"' "$lib")"
hk_eq "outside a pane TMUX_CMD names \$MOOX_SOCKET" "1" \
  "$(grep -cF 'TMUX_CMD="tmux -u -S $MOOX_SOCKET"' "$lib")"
hk_eq "with neither, TMUX_CMD is false (no default fallback)" "1" \
  "$(grep -cE '^[[:space:]]*TMUX_CMD="false"' "$lib")"
hk_eq "no TMUX_CMD is ever a bare tmux" "" \
  "$(grep -nE 'TMUX_CMD="?tmux"?([[:space:]]*;|[[:space:]]*$)' "${files[@]}")"

# Negative control: the same detector on planted bare calls.
cat > "$T/planted.sh" <<'EOF'
# tmux in a comment is fine
printf 'this tmux server does not take part\n'
tmux display-message -p '#{pane_id}'
x="$(tmux show -gqv @a)"
[ -n "$x" ] && tmux set -g @b 1
if true; then tmux refresh-client -S; fi
$TMUX_CMD set -g @c 1
EOF
hk_eq "negative control: the detector finds all 4 planted bare calls" "3 4 5 6" \
  "$(bare_tmux "$T/planted.sh" | awk -F: '{ printf "%s%s", sep, $2; sep = " " }')"

hk_finish
