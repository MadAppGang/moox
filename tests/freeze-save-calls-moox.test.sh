#!/usr/bin/env bash
# freeze-save-calls-moox.test.sh — C5 / I10 / B26: cc_freeze.sh and cc_thaw.sh
# save through `moox save --wait 30s --reason …` (no tmux-resurrect), and a
# save result never undoes the freeze or thaw (architecture §5.5):
#   0 → SAVE-DONE   75 → SAVE-DEFERRED   5/4/1/124 → SAVE-FAILED <code> + @moox_status
#
# A stub `moox` on MOOX_BIN records its argv and exits with the code under test.
# The freeze runs its real `save` entry point (the freeze-store lock, then the
# save) against a scratch `-L moox-test-…` server with -f /dev/null and
# CC_TEST=1, so the script's own isolation assertion is on.
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/lib/hooklib.sh"
hk_setup
command -v tmux >/dev/null 2>&1 || { echo "SKIP: tmux not installed"; exit 0; }
SCRIPTS="$SHARE/plugins/tmux-claude-continuity/scripts"
SOCK="moox-test-ccsave$$"
_t() { env -u TMUX tmux -L "$SOCK" -f /dev/null "$@"; }
SHELL=/bin/sh _t new-session -d -s s 'sleep 300' || { echo "ABORT: no scratch server"; exit 1; }
trap '_t kill-session -t s 2>/dev/null; rm -rf "$T"' EXIT INT TERM HUP

mkdir -p "$T/bin" "$T/frozen" "$T/cc/by-pid"
# `moox save` gets -S <this server's socket> (G3-M2): with $TMUX unset it would
# otherwise resolve to the user's default server.
SOCK_PATH="$(_t display-message -p '#{socket_path}')"
# CC_TEST's isolation contract: every continuity dir the script resolves must
# be a scratch one, not the (scratch-HOME) default.
for o in pending launch panes; do _t set -g "@claude-continuity-$o-dir" "$T/cc/$o"; done
_t set -g @claude-continuity-by-pid-dir "$T/cc/by-pid"
cat > "$T/bin/moox" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "$T/moox-argv"
exit \$(cat "$T/rc")
EOF
chmod +x "$T/bin/moox"

freeze_save() {
  env -u TMUX -u TMUX_PANE -u MOOX_SOCKET -u MOOX_ACTIVE -u MOOX_STATE_KEY -u MOOX_STATE_DIR -u MOOX_PANES_DIR -u MOOX_LAUNCH_DIR -u MOOX_PENDING_DIR -u MOOX_ROOT HOME="$H" PATH="$(dirname "$(command -v tmux)"):$HK_PATH" CC_TEST=1 CC_FREEZE_DIR="$T/frozen" \
    TMUX_CMD="tmux -L $SOCK -f /dev/null" MOOX_BIN="$T/bin" CC_LOG_FILE="$T/log" \
    bash "$SCRIPTS/cc_freeze.sh" save
}

for rc in 0 75 5 4 1 124; do
  : > "$T/log"; rm -f "$T/moox-argv"; printf '%s' "$rc" > "$T/rc"
  _t set -gu @moox_status
  freeze_save; frc=$?
  hk_eq "rc=$rc: cc_freeze.sh save still exits 0" "0" "$frc"
  hk_eq "rc=$rc: moox called once with the contract argv, -S naming THIS server" "save -S $SOCK_PATH --wait 30s --reason freeze: manual" "$(cat "$T/moox-argv" 2>/dev/null)"
  case $rc in
    0)  want="SAVE-DONE reason=freeze: manual" ;;
    75) want="SAVE-DEFERRED reason=freeze: manual" ;;
    *)  want="SAVE-FAILED $rc reason=freeze: manual" ;;
  esac
  grep -q "$want" "$T/log" && ok "rc=$rc: logged $want" || no "rc=$rc: logged $want" "$(cat "$T/log")"
  st="$(_t show -gqv @moox_status)"
  case $rc in
    0|75) hk_eq "rc=$rc: @moox_status untouched" "" "$st" ;;
    *)    case "$st" in *"save failed ($rc"*) ok "rc=$rc: @moox_status reports the failure" ;;
                        *) no "rc=$rc: @moox_status reports the failure" "<$st>" ;; esac ;;
  esac
done

# No resurrect anywhere on the save path.
if grep -n 'tmux-resurrect/scripts/save.sh\|resurrect.*save\.sh' "$SCRIPTS/cc_freeze.sh" "$SCRIPTS/cc_thaw.sh" | grep -v ':[0-9]*: *#'; then
  no "no resurrect save.sh on the freeze/thaw save path" "see above"
else ok "no resurrect save.sh on the freeze/thaw save path"; fi

# thaw's request: `moox save … --reason thaw`, detached, through the same helper.
grep -q '( _cc_moox_save thaw ) >/dev/null 2>&1 &' "$SCRIPTS/cc_thaw.sh" \
  && ok "cc_thaw.sh requests 'moox save … --reason thaw' detached" \
  || no "cc_thaw.sh requests 'moox save … --reason thaw' detached" "not found"

# No moox binary at all: logged, nothing fails.
: > "$T/log"
env -u TMUX -u TMUX_PANE -u MOOX_SOCKET -u MOOX_ACTIVE -u MOOX_STATE_KEY -u MOOX_STATE_DIR -u MOOX_PANES_DIR -u MOOX_LAUNCH_DIR -u MOOX_PENDING_DIR -u MOOX_ROOT HOME="$H" PATH="$(dirname "$(command -v tmux)"):$HK_PATH" CC_TEST=1 CC_FREEZE_DIR="$T/frozen" \
  TMUX_CMD="tmux -L $SOCK -f /dev/null" MOOX_BIN="$T/nowhere" CC_LOG_FILE="$T/log" bash "$SCRIPTS/cc_freeze.sh" save
grep -q 'SAVE-UNAVAILABLE' "$T/log" && ok "no moox binary: SAVE-UNAVAILABLE logged" || no "no moox binary: SAVE-UNAVAILABLE logged" "$(cat "$T/log")"

hk_finish
