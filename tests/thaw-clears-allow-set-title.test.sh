#!/usr/bin/env bash
# thaw-clears-allow-set-title.test.sh — R2-H7: a tombstone the moox engine
# restored has `allow-set-title off` (restore exec D), and respawn-pane keeps
# pane options, so cc_thaw.sh must clear it next to `allow-rename`; otherwise a
# thawed program's OSC 2 titles never reach pane_title.
#
# The freeze → restored-tombstone → thaw round trip needs the full fixture of
# the vendored thaw_roundtrip.sh (fake Claude trees on an isolated
# `-L … -f /dev/null` server, CC_TEST=1). This test runs it with the environment
# its runner uses and requires its two moox assertions, plus a static check that
# both thaw paths clear the option.
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/lib/hooklib.sh"
hk_setup
command -v tmux >/dev/null 2>&1 || { echo "SKIP: tmux not installed"; exit 0; }
SCRIPTS="$SHARE/plugins/tmux-claude-continuity/scripts"

n="$(grep -c 'set-option -pu -t .* allow-set-title' "$SCRIPTS/cc_thaw.sh")"
hk_eq "both thaw paths (pane and legacy window) clear allow-set-title" "2" "$n"

out="$(env -u TMUX -u TMUX_PANE -u MOOX_SOCKET -u MOOX_ACTIVE -u MOOX_STATE_DIR -u MOOX_STATE_KEY SHELL=/bin/sh \
  bash "$SHARE/plugins/tmux-claude-continuity/tests/thaw_roundtrip.sh" 2>&1)"
for want in "moox: thaw cleared allow-set-title on every pane" \
            "moox: an OSC 2 title from the thawed pane reaches pane_title"; do
  if printf '%s\n' "$out" | grep -qF "PASS: $want"; then ok "$want"
  else no "$want" "$(printf '%s\n' "$out" | grep -F "$want" | head -3)"; fi
done
summary="$(printf '%s\n' "$out" | grep -E 'Results: [0-9]+ passed' | tail -1)"
case "$summary" in *" 0 failed"*) ok "the whole round trip passes ($summary)" ;; *) no "the whole round trip passes" "$summary" ;; esac

hk_finish
