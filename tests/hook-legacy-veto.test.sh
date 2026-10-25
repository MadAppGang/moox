#!/usr/bin/env bash
# hook-legacy-veto.test.sh — R3-C4 / R2-C5: revocation reaches shells that
# already exist, in ONE running shell, for zsh and bash.
#
#   legacy branch (no MOOX_* env, the real login default socket as a string):
#     no veto → recorded; veto file written → nothing; veto removed → recorded.
#   env branch: marker present → recorded; marker removed by deactivation →
#     nothing, and a planted pending/<N> is not consumed.
#
# The hook tests a file with `[[ -e ]]` at every record/clear/consume (no fork),
# so the change takes effect on the very next command of the same shell.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup

LEG="$H/.config/tmux-claude"
VETO="$LEG/moox-excluded"

for SH in zsh bash; do
  echo "=== $SH ==="
  if [ "$SH" = zsh ] && ! command -v zsh >/dev/null 2>&1; then echo "SKIP zsh"; continue; fi
  run() { if [ "$SH" = zsh ]; then hk_zsh "$@"; else hk_bash "$@"; fi; }
  rm -rf "$LEG" "$T"/d-* "$T/state"
  mkdir -p "$LEG/launch" "$LEG/pending"

  run TMUX="$HK_LEGACY_SOCK,1,0" TMUX_PANE=%7 >/dev/null <<EOF
cat "$LEG/launch/7" > "$T/d-1"
: > "$VETO"
cat "$LEG/launch/7" > "$T/d-2"
rm -f "$VETO"
cat "$LEG/launch/7" > "$T/d-3"
EOF
  hk_eq "$SH legacy, no veto: recorded" "cat \"$LEG/launch/7\" > \"$T/d-1\"" "$(cat "$T/d-1" 2>/dev/null)"
  # Excluded: the new line is not recorded (and, like record and consume, the
  # clear is gated too, so the line before it may still sit there).
  hk_ne "$SH legacy, veto written: the new line is not recorded" "cat \"$LEG/launch/7\" > \"$T/d-2\"" "$(cat "$T/d-2" 2>/dev/null)"
  hk_eq "$SH legacy, veto removed: recorded again" "cat \"$LEG/launch/7\" > \"$T/d-3\"" "$(cat "$T/d-3" 2>/dev/null)"

  hk_moox_layout k
  printf 'echo PENDING-RAN > %s\n' "$T/d-pending" > "$M_PENDING/7"
  # shellcheck disable=SC2046
  run $(hk_gate_env "$M_SOCK" "$M_STATE" "$M_LAUNCH" "$M_PENDING" "$M_PANES") \
    TMUX="$M_SOCK,9,0" TMUX_PANE=%7 >/dev/null <<EOF
cat "$M_LAUNCH/7" > "$T/d-4"
rm -f "$M_STATE/active"
cat "$M_LAUNCH/7" > "$T/d-5"
EOF
  # The planted pending file is consumed on the FIRST prompt (marker present),
  # before any of the typed lines; re-plant one after removal to prove the
  # excluded shell no longer consumes.
  hk_eq "$SH env, marker present: pending consumed on the first prompt" "PENDING-RAN" "$(cat "$T/d-pending" 2>/dev/null)"
  hk_eq "$SH env, marker present: recorded" "cat \"$M_LAUNCH/7\" > \"$T/d-4\"" "$(cat "$T/d-4" 2>/dev/null)"
  hk_ne "$SH env, marker removed: the new line is not recorded" "cat \"$M_LAUNCH/7\" > \"$T/d-5\"" "$(cat "$T/d-5" 2>/dev/null)"

  # A shell that starts while excluded consumes nothing.
  rm -f "$T/d-pending"
  printf 'echo PENDING-RAN > %s\n' "$T/d-pending" > "$M_PENDING/7"
  # shellcheck disable=SC2046
  run $(hk_gate_env "$M_SOCK" "$M_STATE" "$M_LAUNCH" "$M_PENDING" "$M_PANES") \
    TMUX="$M_SOCK,9,0" TMUX_PANE=%7 >/dev/null <<EOF
true
EOF
  [ ! -e "$T/d-pending" ] && [ -e "$M_PENDING/7" ] && ok "$SH env, no marker: planted pending/<N> not consumed" \
    || no "$SH env, no marker: planted pending/<N> not consumed" "ran=$([ -e "$T/d-pending" ] && echo yes) file=$([ -e "$M_PENDING/7" ] && echo kept || echo gone)"

  # A scratch server whose socket is named default, no env: the hook stays off.
  rm -rf "$LEG/launch"; mkdir -p "$LEG/launch"
  run TMUX="$T/default,1,0" TMUX_PANE=%7 >/dev/null <<EOF
cat "$LEG/launch/7" > "$T/d-6"
EOF
  hk_absent "$SH scratch …/default, no env: hook off" "$T/d-6"
done

hk_finish
