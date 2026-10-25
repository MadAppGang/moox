#!/usr/bin/env bash
# thaw-nudge.test.sh — B36/B37 on the path that keeps them: thaw's pending
# files. (The restore path retired both: env delivery has no race, §6.8;
# TestE2E_Restore_EnvDeliveryNoNudge.)
#
# In each thaw path of cc_thaw.sh (a pane entry, and a legacy window entry):
#   1. the pane is respawned to a FRESH shell before anything is queued — so
#      the shell that will read the pending file is never a busy foreground
#      program (what B36's 0.4 s shell check guarded on the restore path);
#   2. the pending file is written before the nudge;
#   3. the nudge is a bare Enter (`send-keys -t <pane> "" Enter`), suppressed
#      by CC_NO_NUDGE=1, sent before THAWED is reported;
#   4. no send-keys anywhere in thaw types a command (typeahead is dropped
#      while .zshrc loads; the command travels in the pending file).
# A static check: the full freeze → thaw round trip runs with CC_NO_NUDGE=1
# (thaw_roundtrip.sh, via thaw-clears-allow-set-title.test.sh), and the hook's
# consumption of a pending file at a prompt is tests/hook-resume.test.sh.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
THAW="$SHARE/plugins/tmux-claude-continuity/scripts/cc_thaw.sh"

# first_line <function> <ERE>: the first line number matching ERE inside the
# body of function <function> (from its `name() {` line to the next `^}`).
first_line() {
  awk -v fn="$1" -v re="$2" '
    $0 ~ "^" fn "\\(\\) \\{" { on = 1; next }
    on && /^}/ { exit }
    on && $0 ~ re { print NR; exit }' "$THAW"
}

for fn in __cc_thaw_pane_locked __cc_thaw_legacy_locked; do
  respawn="$(first_line "$fn" 'respawn-pane -k .*_cc_fresh_shell')"
  pending="$(first_line "$fn" '> "[$][{]pending_dir[}]/')"
  nudge="$(first_line "$fn" 'send-keys -t "[$][a-z]+" "" Enter')"
  thawed="$(first_line "$fn" '_cc_emit THAWED')"
  if [ -n "$respawn" ] && [ -n "$pending" ] && [ -n "$nudge" ] && [ -n "$thawed" ]; then
    ok "$fn: respawn, pending write, nudge and THAWED all present"
  else
    no "$fn: respawn, pending write, nudge and THAWED all present" \
      "respawn=$respawn pending=$pending nudge=$nudge thawed=$thawed"
    continue
  fi
  [ "$respawn" -lt "$pending" ] && ok "$fn: a fresh shell before the pending file (B36)" \
    || no "$fn: a fresh shell before the pending file (B36)" "respawn $respawn, pending $pending"
  [ "$pending" -lt "$nudge" ] && [ "$nudge" -lt "$thawed" ] && ok "$fn: pending file, then the nudge, then THAWED (B37)" \
    || no "$fn: pending file, then the nudge, then THAWED (B37)" "pending $pending, nudge $nudge, thawed $thawed"
  guard="$(awk -v a="$((nudge - 3))" -v b="$nudge" 'NR >= a && NR <= b' "$THAW" | grep -c 'CC_NO_NUDGE:-0}" != "1"')"
  [ "$guard" -ge 1 ] && ok "$fn: the nudge is suppressed by CC_NO_NUDGE=1" \
    || no "$fn: the nudge is suppressed by CC_NO_NUDGE=1" "no CC_NO_NUDGE guard just above line $nudge"
done

hk_eq "every send-keys in thaw is a bare Enter (never a typed command)" "" \
  "$(grep -n 'send-keys' "$THAW" | grep -v '^[0-9]*:[[:space:]]*#' | grep -vE 'send-keys -t "\$[a-z]+" "" Enter')"

hk_finish
