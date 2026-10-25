#!/usr/bin/env bash
# hook-env-dirs.test.sh — C1: the continuity hooks (zsh and bash) take their
# directories from the gate's MOOX_* env with no fork, and trust that env ONLY
# when MOOX_SOCKET names the server the shell runs in (${TMUX%%,*}).
#
#   own server        → records into MOOX_LAUNCH_DIR while the command runs, clears after
#   inherited env     → a shell on ANOTHER server holding the real MOOX_* values
#                       writes nothing, consumes nothing, and MOOX_RESUME never
#                       reaches a child (R17, X1, PR-H8)
#   MOOX_ACTIVE=0     → nothing
#   scratch "default" → a `-S <scratch>/default` server with no env is not the
#                       login default: nothing (realpath/literal compare, not basename)
#   legacy            → no env on the real login default: today's dirs
#
# No tmux server is started: $TMUX is a fake string the hooks only compare.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup

hk_moox_layout real
REAL_SOCK="$M_SOCK" REAL_STATE="$M_STATE" REAL_LAUNCH="$M_LAUNCH" REAL_PENDING="$M_PENDING" REAL_PANES="$M_PANES"
LEGACY_LAUNCH="$H/.config/tmux-claude/launch"

for SH in zsh bash; do
  echo "=== $SH ==="
  run() { if [ "$SH" = zsh ]; then hk_zsh "$@"; else hk_bash "$@"; fi; }
  if [ "$SH" = zsh ] && ! command -v zsh >/dev/null 2>&1; then echo "SKIP zsh"; continue; fi
  rm -rf "${REAL_LAUNCH:?}"/* "$H/.config" "$T"/during-* "$T"/child-*

  # 1. own server: the env names this server.
  # shellcheck disable=SC2046
  run $(hk_gate_env "$REAL_SOCK" "$REAL_STATE" "$REAL_LAUNCH" "$REAL_PENDING" "$REAL_PANES") \
    TMUX="$REAL_SOCK,4242,0" TMUX_PANE=%5 >/dev/null <<EOF
cat "\$MOOX_LAUNCH_DIR/5" > "$T/during-own"
true
EOF
  hk_eq "$SH own server: recorded the typed line while it ran" \
    'cat "$MOOX_LAUNCH_DIR/5" > "'"$T"'/during-own"' "$(cat "$T/during-own" 2>/dev/null)"
  hk_absent "$SH own server: cleared at the prompt" "$REAL_LAUNCH/5"
  [ ! -e "$LEGACY_LAUNCH" ] && ok "$SH own server: legacy dir untouched" || no "$SH own server: legacy dir untouched" "$(ls -la "$LEGACY_LAUNCH")"

  # 2. inherited env: a scratch server started from inside a real pane. Its
  #    panes carry the REAL server's MOOX_* (marker present!) but $TMUX names
  #    the scratch socket. (bash clears by truncation, so start from an empty dir.)
  rm -f "${REAL_LAUNCH:?}"/*
  # shellcheck disable=SC2046
  run $(hk_gate_env "$REAL_SOCK" "$REAL_STATE" "$REAL_LAUNCH" "$REAL_PENDING" "$REAL_PANES") \
    MOOX_RESUME="echo RESUMED > $T/child-resumed" \
    TMUX="$T/scratch-sock,999,0" TMUX_PANE=%5 >/dev/null <<EOF
env > "$T/child-env"
sleep 0
EOF
  [ -z "$(ls -A "$REAL_LAUNCH")" ] && ok "$SH inherited env: the real launch/ stays empty" \
    || no "$SH inherited env: the real launch/ stays empty" "$(ls -A "$REAL_LAUNCH")"
  [ ! -e "$T/child-resumed" ] && ok "$SH inherited env: MOOX_RESUME not consumed" || no "$SH inherited env: MOOX_RESUME not consumed" "it ran"
  if grep -q '^MOOX_RESUME=' "$T/child-env" 2>/dev/null; then
    no "$SH inherited env: MOOX_RESUME never reaches a child" "$(grep '^MOOX_RESUME=' "$T/child-env")"
  else ok "$SH inherited env: MOOX_RESUME never reaches a child"; fi

  # 3. MOOX_ACTIVE=0 (this server is excluded).
  run MOOX_SOCKET="$REAL_SOCK" MOOX_ACTIVE=0 MOOX_STATE_DIR="$REAL_STATE" \
    MOOX_LAUNCH_DIR="$REAL_LAUNCH" MOOX_PENDING_DIR="$REAL_PENDING" \
    TMUX="$REAL_SOCK,4242,0" TMUX_PANE=%5 >/dev/null <<EOF
cat "\$MOOX_LAUNCH_DIR/5" > "$T/during-inactive"
EOF
  hk_absent "$SH MOOX_ACTIVE=0: nothing recorded" "$T/during-inactive"

  # 4. A scratch server whose socket is NAMED default, no env at all.
  run TMUX="$T/default,77,0" TMUX_PANE=%5 >/dev/null <<EOF
cat "$LEGACY_LAUNCH/5" > "$T/during-scratchdefault"
EOF
  hk_absent "$SH scratch -S …/default: nothing recorded" "$T/during-scratchdefault"
  [ ! -e "$LEGACY_LAUNCH" ] && ok "$SH scratch -S …/default: legacy dir not created" || no "$SH scratch -S …/default: legacy dir not created" "exists"

  # 5. Legacy: no env, the real login default socket (a string only).
  run TMUX="$HK_LEGACY_SOCK,1,0" TMUX_PANE=%5 >/dev/null <<EOF
cat "$LEGACY_LAUNCH/5" > "$T/during-legacy"
EOF
  hk_eq "$SH legacy default (no env): today's launch dir" \
    "cat \"$LEGACY_LAUNCH/5\" > \"$T/during-legacy\"" "$(cat "$T/during-legacy" 2>/dev/null)"
  hk_absent "$SH legacy default: cleared at the prompt" "$LEGACY_LAUNCH/5"
done

hk_finish
