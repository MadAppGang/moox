#!/usr/bin/env bash
# hook-resume.test.sh — B3/B4 + C2: first-prompt resume in the zsh hook
# (HK_SHELL=bash runs the same contract against the bash hook; see
# hook-bash-resume.test.sh).
#
#   * MOOX_RESTORE_CONTENT is printed on its own and the file removed;
#   * the resume command is recorded FIRST (a save at any later instant stores
#     it), then the ack is created, then it runs;
#   * it runs as a CHILD: the shell is still there for the next typed line;
#   * no command inside the resumed line overwrites its recording;
#   * MOOX_RESUME* never reach a child process;
#   * one-shot: a second pending file is not consumed;
#   * a pane with contents but no command STAYS ARMED and later consumes a
#     pending file (cc_thaw's path, R2-H9).
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
SH="${HK_SHELL:-zsh}"
if [ "$SH" = zsh ] && ! command -v zsh >/dev/null 2>&1; then echo "SKIP: zsh not installed"; exit 0; fi
run() { if [ "$SH" = zsh ]; then hk_zsh "$@"; else hk_bash "$@"; fi; }
echo "=== $SH ==="

hk_moox_layout k
ENVW="$(hk_gate_env "$M_SOCK" "$M_STATE" "$M_LAUNCH" "$M_PENDING" "$M_PANES")"
mkdir -p "$M_STATE/restore/run1/ack"
ACK="$M_STATE/restore/run1/ack/row3"
CONTENT="$M_STATE/restore/run1/contents-3"
printf 'old line 1\nold line 2\n' > "$CONTENT"

# ── a. MOOX_RESUME + contents + ack ──────────────────────────────────────────
RESUME="cat \"$M_LAUNCH/5\" > \"$T/a-during\"; env > \"$T/a-childenv\"; true; echo inner > /dev/null"
# shellcheck disable=SC2086
run $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%5 \
  MOOX_RESUME="$RESUME" MOOX_RESTORE_CONTENT="$CONTENT" MOOX_RESUME_ACK="$ACK" > "$T/a-out" <<EOF
echo AFTER > "$T/a-after"
env > "$T/a-typedenv"
EOF
grep -q '^old line 1$' "$T/a-out" && grep -q '^old line 2$' "$T/a-out" \
  && ok "contents printed" || no "contents printed" "$(cat "$T/a-out")"
[ ! -e "$CONTENT" ] && ok "contents file removed" || no "contents file removed" "still there"
hk_eq "resume line recorded first, whole, and not overwritten by its inner commands" "$RESUME" "$(cat "$T/a-during" 2>/dev/null)"
[ -e "$ACK" ] && ok "ack created" || no "ack created" "missing"
hk_eq "ran as a child: the shell took the next typed line" "AFTER" "$(cat "$T/a-after" 2>/dev/null)"
if grep -q '^MOOX_RESUME' "$T/a-childenv" "$T/a-typedenv" 2>/dev/null; then
  no "MOOX_RESUME* never reach a child" "$(grep -h '^MOOX_RESUME' "$T/a-childenv" "$T/a-typedenv")"
else ok "MOOX_RESUME* never reach a child"; fi
if grep -q '^MOOX_RESTORE_CONTENT' "$T/a-childenv" 2>/dev/null; then
  no "MOOX_RESTORE_CONTENT never reaches a child" "inherited"
else ok "MOOX_RESTORE_CONTENT never reaches a child"; fi

# ── b. contents but no command: stays armed, later consumes a pending file ───
printf 'banner\n' > "$CONTENT"
rm -f "$M_LAUNCH"/*
# shellcheck disable=SC2086
run $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%6 MOOX_RESTORE_CONTENT="$CONTENT" > "$T/b-out" <<EOF
printf '%s\n' 'cat "$M_LAUNCH/6" > "$T/b-during"; echo LATE > "$T/b-late"' > "$M_PENDING/6"
true
EOF
grep -q '^banner$' "$T/b-out" && ok "banner printed (no command)" || no "banner printed (no command)" "$(cat "$T/b-out")"
hk_eq "stayed armed: a later pending file was consumed" "LATE" "$(cat "$T/b-late" 2>/dev/null)"
hk_eq "the pending line was recorded before it ran" \
  "cat \"$M_LAUNCH/6\" > \"$T/b-during\"; echo LATE > \"$T/b-late\"" "$(cat "$T/b-during" 2>/dev/null)"
[ ! -e "$M_PENDING/6" ] && ok "pending file removed" || no "pending file removed" "still there"

# ── c. one-shot: after a consume, a second pending file is left alone ────────
printf 'echo FIRST > %s\n' "$T/c-first" > "$M_PENDING/8"
# shellcheck disable=SC2086
run $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%8 >/dev/null <<EOF
printf 'echo SECOND > %s\n' "$T/c-second" > "$M_PENDING/8"
true
EOF
hk_eq "first pending file consumed" "FIRST" "$(cat "$T/c-first" 2>/dev/null)"
[ ! -e "$T/c-second" ] && [ -e "$M_PENDING/8" ] && ok "one-shot: the second is not consumed" \
  || no "one-shot: the second is not consumed" "it ran or vanished"

# ── d. the finished resumed command's recording is cleared ───────────────────
# shellcheck disable=SC2086
run $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%9 MOOX_RESUME="true" >/dev/null <<EOF
cat "$M_LAUNCH/9" > "$T/d-after"
EOF
hk_eq "after the resumed command exits, the next typed line owns the recording" \
  "cat \"$M_LAUNCH/9\" > \"$T/d-after\"" "$(cat "$T/d-after" 2>/dev/null)"
hk_gone "the resumed command's recording is gone once everything finished" "$M_LAUNCH/9"

hk_finish
