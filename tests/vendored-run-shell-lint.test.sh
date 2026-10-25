#!/usr/bin/env bash
# vendored-run-shell-lint.test.sh — G3-H2 / G3-M6: the run-shells in the
# vendored plugins' entry scripts (share/moox/plugins/**/*.tmux), read as
# LOGICAL lines: a backslash-newline continuation is joined first, so a
# run-shell split over several lines (tmux-sensible's `prefix R`) is seen whole
# — the same logical lines the Go conf-lint (internal/audit RunShellConf) reads,
# which pins that binding by the same joined text.
#
# Every run-shell found must be either
#   * silent and `|| true` as written, or
#   * a KEY BINDING listed below by its whole joined text — the plugins ship
#     unmodified, so those bindings are rewrapped silent on the server by
#     share/moox/scripts/silence-bindings.sh. This scan reads SOURCE text and
#     cannot see how tmux prints a binding (bare, '…' or "…") or whether the
#     rewrap happened; that is TestE2E_Conf_LoadedBindingsSilent and
#     TestE2E_SilenceBindings_RewrapsExactly (share/bindings_e2e_test.go), which
#     read every loaded binding back from list-keys (G3-2-H1).
# A run-shell that is not a binding (config time, a hook) gets no exemption,
# and a pin bump that changes or adds one fails here until it is reviewed.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
P="$SHARE/plugins"

# The reviewed bindings, "<plugin file>: <joined, whitespace-squeezed line>".
ALLOWED="$T/allowed"
cat > "$ALLOWED" <<'EOF'
tmux-fzf/main.tmux: tmux bind-key "$TMUX_FZF_LAUNCH_KEY" run-shell -b "$CURRENT_DIR/main.sh"
tmux-fzf-url/fzf-url.tmux: tmux bind-key "${note_flag[@]}" "$key" run -b "$cmd"
tmux-jump/tmux-jump.tmux: tmux bind-key -N "Jump to pane location in copy mode" $(get_tmux_option "@jump-key" "j") run-shell -b "$CURRENT_DIR/scripts/tmux-jump.sh"
tmux-sensible/sensible.tmux: tmux bind-key R run-shell " tmux source-file ${tmux_config} > /dev/null; tmux display-message 'Sourced ${tmux_config}!'"
tmux-yank/yank.tmux: tmux bind-key "$(yank_line_key)" run-shell -b "$SCRIPTS_DIR/copy_line.sh"
tmux-yank/yank.tmux: tmux bind-key "$(yank_pane_pwd_key)" run-shell -b "$SCRIPTS_DIR/copy_pane_pwd.sh"
EOF
# NOT a run-shell binding: tmux-open's copy-mode bindings pipe the selection
# to `xargs tmux run-shell -b '…'` inside the copy-pipe job, so the silencer
# cannot rewrap them. Tracked as G3-M4 (deferred: needs an edit of the plugin).
DEFERRED="$T/deferred"
cat > "$DEFERRED" <<'EOF'
tmux-open/open.tmux: echo "$(preserve_url_hash) | xargs -I {} tmux run-shell -b 'cd #{pane_current_path}; $command_string \"{}\" > /dev/null'"
tmux-open/open.tmux: echo "$(preserve_url_hash) | sed 's/\ /+/g' | xargs -I {} tmux run-shell -b 'cd #{pane_current_path}; $command_string $engine\"{}\" > /dev/null'"
EOF

# logical <file>: one line per logical line, continuations joined, runs of
# blanks squeezed to one space, comments dropped.
logical() {
  awk '
    { line = $0 }
    acc != "" { line = acc " " line; acc = "" }
    /\\$/ { acc = substr(line, 1, length(line) - 1); next }
    { gsub(/[ \t]+/, " ", line); sub(/^ /, "", line); sub(/ $/, "", line)
      if (line != "" && substr(line, 1, 1) != "#") print line }
    END { if (acc != "") print acc }' "$1"
}

FOUND="$T/found"; : > "$FOUND"
n=0
for f in "$P"/*/*.tmux "$P"/*/plugin/*.tmux; do
  [ -f "$f" ] || continue
  rel="${f#"$P"/}"
  while IFS= read -r l; do
    case "$l" in *run-shell*|*" run -b "*|*" run "\"*) ;; *) continue ;; esac
    n=$((n + 1))
    printf '%s: %s\n' "$rel" "$l" >> "$FOUND"
  done < <(logical "$f")
done
[ "$n" -gt 0 ] && ok "found $n run-shell logical lines in the vendored entry scripts" || no "found run-shell lines" "none: the scan saw nothing"

grep -q '^tmux-sensible/sensible.tmux: tmux bind-key R run-shell " tmux source-file' "$FOUND" \
  && ok "the multi-line sensible prefix R is read as ONE logical line" \
  || no "the multi-line sensible prefix R is read as ONE logical line" "$(grep sensible "$FOUND")"

silent='>[[:space:]]*/dev/null[[:space:]]+2>&1[[:space:]]*\|\|[[:space:]]*true'
while IFS= read -r e; do
  if printf '%s\n' "$e" | grep -Eq "$silent"; then ok "silent: ${e:0:110}"; continue; fi
  case "$e" in
    *set-hook*) no "a hook run-shell must be silent itself (no exemption)" "$e"; continue ;;
  esac
  if grep -Fxq -- "$e" "$ALLOWED"; then ok "reviewed binding (source text only; the loaded form is TestE2E_Conf_LoadedBindingsSilent's): ${e:0:70}"
  elif grep -Fxq -- "$e" "$DEFERRED"; then ok "known, deferred (G3-M4): ${e:0:90}"
  else no "an unreviewed loud run-shell in a vendored entry script" "$e"; fi
done < "$FOUND"

# Every reviewed entry still exists (a pin bump that drops one is reviewed too).
while IFS= read -r a; do
  grep -Fxq -- "$a" "$FOUND" || no "allowlist entry no longer present (review and remove)" "$a"
done < <(cat "$ALLOWED" "$DEFERRED")

# The runtime half is wired: plugins.conf runs the silencer after every plugin.
last="$(grep -n 'run-shell' "$SHARE/conf/plugins.conf" | grep -v ':[0-9]*:#' | tail -n 1)"
case "$last" in *silence-bindings.sh*'>/dev/null 2>&1 || true'*) ok "plugins.conf runs silence-bindings.sh last, itself silent" ;;
  *) no "plugins.conf runs silence-bindings.sh last, itself silent" "$last" ;; esac

hk_finish
