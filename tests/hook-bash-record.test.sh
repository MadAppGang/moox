#!/usr/bin/env bash
# hook-bash-record.test.sh — B1/B2 for the bash hook (D19, §7.1, Q9b): the
# DEBUG-trap recorder records history's text for the running line, and ONLY
# when history advanced, so launch/<N> never holds a completed command.
#
# Each line below snapshots the recording WHILE IT RUNS. A line that history
# does not take (ignorespace, HISTIGNORE, ignoredups, HISTSIZE=0) must see an
# EMPTY recording — never the previous, completed command.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
echo "=== $HK_BASH ($("$HK_BASH" -c 'echo $BASH_VERSION')) ==="

hk_moox_layout k
ENVW="$(hk_gate_env "$M_SOCK" "$M_STATE" "$M_LAUNCH" "$M_PENDING" "$M_PANES")"
L="$M_LAUNCH/5"

HK_BASHRC="PS1=''
HISTFILE='$T/hist'
HISTCONTROL=ignoreboth
HISTIGNORE='secret*'
secret_cat() { cat \"\$1\"; }
terraform_apply_done() { :; }
. '$SHARE/shell/moox.bash'"
# shellcheck disable=SC2086
hk_bash $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%5 >/dev/null <<EOF
terraform_apply_done; cat "$L" > "$T/s1"

 cat "$L" > "$T/s2"
secret_cat "$L" > "$T/s3"
cat "$L" >> "$T/s5"
cat "$L" >> "$T/s5"
echo a > /dev/null; cat "$L" > "$T/s6"
terraform_apply_done
EOF
hk_eq "recorded: the running line" "terraform_apply_done; cat \"$L\" > \"$T/s1\"" "$(cat "$T/s1" 2>/dev/null)"
hk_absent "ignorespace after an empty Enter: nothing (not the completed line)" "$T/s2"
hk_absent "HISTIGNORE: nothing" "$T/s3"
hk_eq "ignoredups: the duplicate is not recorded" "cat \"$L\" >> \"$T/s5\"" "$(cat "$T/s5" 2>/dev/null)"
hk_eq "a list arrives whole" "echo a > /dev/null; cat \"$L\" > \"$T/s6\"" "$(cat "$T/s6" 2>/dev/null)"
hk_gone "R2-C6: a finished one-shot leaves NO launch/<N> behind (not even empty)" "$L"

HK_BASHRC="PS1=''
HISTSIZE=0
. '$SHARE/shell/moox.bash'"
# shellcheck disable=SC2086
hk_bash $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%6 >/dev/null <<EOF
cat "$M_LAUNCH/6" > "$T/s7"
EOF
hk_absent "HISTSIZE=0: nothing" "$T/s7"

# R2-C6, the replay class: a one-shot finishes, the shell sits at its prompt
# (an empty Enter, then idle) and nothing may remain that a save could replay.
# The probe line starts with a space (ignorespace), so it records nothing
# itself: it sees exactly what a save would see at that idle prompt.
HK_BASHRC="PS1=''
HISTFILE='$T/hist2'
HISTCONTROL=ignorespace
terraform_apply() { :; }
. '$SHARE/shell/moox.bash'"
# shellcheck disable=SC2086
hk_bash $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%7 >/dev/null <<EOF
terraform_apply
cat "$M_LAUNCH/7" > "$T/s8"

 [ -e "$M_LAUNCH/7" ] && echo present > "$T/s9"
EOF
hk_eq "while it ran, the next line saw only itself" "cat \"$M_LAUNCH/7\" > \"$T/s8\"" "$(cat "$T/s8" 2>/dev/null)"
hk_gone "R2-C6: after terraform_apply finished and an empty Enter, launch/7 does not exist" "$T/s9"
hk_gone "R2-C6: and not after the shell ends either" "$M_LAUNCH/7"

hk_finish
