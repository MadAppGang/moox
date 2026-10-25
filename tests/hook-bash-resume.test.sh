#!/usr/bin/env bash
# hook-bash-resume.test.sh — B3/B4 + C2 for the bash hook (D19, §7.1).
#
# Runs the shared resume contract (hook-resume.test.sh) against bash, then the
# bash-only cases: a resumed line failing mid-way under `set -e` leaves the
# shell alive (errexit is off around the eval, R2-M11), and none of the resumed
# line's own commands reach the recorder (resume runs before the re-arm, Q9b).
here="$(cd "$(dirname "$0")" && pwd)"
HK_SHELL=bash bash "$here/hook-resume.test.sh"
shared=$?

. "$here/lib/hooklib.sh"
hk_setup
echo "=== bash only ==="
hk_moox_layout k
ENVW="$(hk_gate_env "$M_SOCK" "$M_STATE" "$M_LAUNCH" "$M_PENDING" "$M_PANES")"

HK_BASHRC="PS1=''
set -e
. '$SHARE/shell/moox.bash'"
RESUME="false; echo after-false > \"$T/e-inner\"; cat \"$M_LAUNCH/4\" > \"$T/e-during\""
# shellcheck disable=SC2086
hk_bash $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%4 MOOX_RESUME="$RESUME" >/dev/null <<EOF
echo ALIVE > "$T/e-alive"
EOF
hk_eq "set -e: the resumed line ran past its failing command" "after-false" "$(cat "$T/e-inner" 2>/dev/null)"
hk_eq "set -e: the shell survived the resumed line" "ALIVE" "$(cat "$T/e-alive" 2>/dev/null)"
hk_eq "set -e: recorded once, whole" "$RESUME" "$(cat "$T/e-during" 2>/dev/null)"

hk_finish
mine=$?
[ "$shared" -eq 0 ] && [ "$mine" -eq 0 ]
