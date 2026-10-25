#!/usr/bin/env bash
# hook-record.test.sh — B1/B2 (+R35, R37) for the zsh hook: preexec records the
# line exactly as TYPED (alias unexpanded, quoting intact), first line only,
# for every command, and precmd removes it when the shell is back at a prompt —
# so a finished one-shot (`terraform apply`) is never there to be replayed.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
command -v zsh >/dev/null 2>&1 || { echo "SKIP: zsh not installed"; exit 0; }

hk_moox_layout k
ENVW="$(hk_gate_env "$M_SOCK" "$M_STATE" "$M_LAUNCH" "$M_PENDING" "$M_PANES")"
L="$M_LAUNCH/5"

HK_ZSHRC="PS1=''
setopt noclobber
alias c='echo C'
terraform_apply_done() { : }
source '$SHARE/shell/moox.zsh'"
# shellcheck disable=SC2086
hk_zsh $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%5 >/dev/null <<EOF
c --name "My Session" > /dev/null; cat "$L" > "$T/r1"
cat "$L" > "$T/r2"; echo 'x
y' > /dev/null
terraform_apply_done
EOF
hk_eq "B1: recorded as typed (alias unexpanded, quoting intact, noclobber set)" \
  "c --name \"My Session\" > /dev/null; cat \"$L\" > \"$T/r1\"" "$(cat "$T/r1" 2>/dev/null)"
hk_eq "R35: first line only of a multi-line command" "cat \"$L\" > \"$T/r2\"; echo 'x" "$(cat "$T/r2" 2>/dev/null)"
hk_gone "B2/R37: a finished command leaves no launch/<N> at all" "$L"

hk_finish
