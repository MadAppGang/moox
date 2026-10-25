#!/usr/bin/env bash
# hook-bash-coexist.test.sh — the bash hook next to the user's own prompt
# machinery (D19, §7.1, K15):
#   * a user PROMPT_COMMAND string keeps running, still sees the command's $?,
#     and is never recorded;
#   * a user PROMPT_COMMAND array (bash >= 5.1) keeps running, the same way;
#   * an existing user DEBUG trap is chained, never replaced;
#   * with bash-preexec loaded first (__bp_imported), the hook registers with
#     precmd_functions/preexec_functions instead of installing its own trap.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
BV="$("$HK_BASH" -c 'echo "${BASH_VERSINFO[0]}${BASH_VERSINFO[1]}"')"
echo "=== $HK_BASH (bash $BV) ==="

hk_moox_layout k
ENVW="$(hk_gate_env "$M_SOCK" "$M_STATE" "$M_LAUNCH" "$M_PENDING" "$M_PANES")"
L="$M_LAUNCH/5"

# ── user PROMPT_COMMAND (string) + user DEBUG trap ───────────────────────────
HK_BASHRC="PS1=''
userpc() { echo \"pc rc=\$?\" >> '$T/pc'; }
PROMPT_COMMAND=userpc
trap 'echo dbg >> \"$T/dbg\"' DEBUG
. '$SHARE/shell/moox.bash'"
# shellcheck disable=SC2086
hk_bash $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%5 >/dev/null <<EOF
cat "$L" > "$T/c1"
false
EOF
hk_eq "string PROMPT_COMMAND: recording is the typed line" "cat \"$L\" > \"$T/c1\"" "$(cat "$T/c1" 2>/dev/null)"
grep -q 'pc rc=1' "$T/pc" 2>/dev/null && ok "user PROMPT_COMMAND still runs and sees \$? of the command" \
  || no "user PROMPT_COMMAND still runs and sees \$? of the command" "$(cat "$T/pc" 2>/dev/null)"
[ -s "$T/dbg" ] && ok "user DEBUG trap still fires (chained)" || no "user DEBUG trap still fires (chained)" "no output"
if grep -rq 'userpc\|_cc_arm\|_cc_disarm' "$M_LAUNCH" 2>/dev/null; then
  no "the prompt's own commands are never recorded" "$(cat "$M_LAUNCH"/* 2>/dev/null)"
else ok "the prompt's own commands are never recorded"; fi

# ── user PROMPT_COMMAND (array, bash >= 5.1) ─────────────────────────────────
if [ "$BV" -ge 51 ]; then
  HK_BASHRC="PS1=''
  PROMPT_COMMAND=('echo one >> \"$T/arr\"' 'echo two >> \"$T/arr\"')
  . '$SHARE/shell/moox.bash'"
  # shellcheck disable=SC2086
  hk_bash $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%5 >/dev/null <<EOF
cat "$L" > "$T/c2"
EOF
  hk_eq "array PROMPT_COMMAND: recorded" "cat \"$L\" > \"$T/c2\"" "$(cat "$T/c2" 2>/dev/null)"
  grep -q one "$T/arr" && grep -q two "$T/arr" && ok "array PROMPT_COMMAND: both user elements run" \
    || no "array PROMPT_COMMAND: both user elements run" "$(cat "$T/arr" 2>/dev/null)"
else
  echo "  skip array PROMPT_COMMAND (needs bash >= 5.1; set HK_BASH)"
fi

# ── bash-preexec loaded first ────────────────────────────────────────────────
# A minimal stand-in with bash-preexec's public contract: it owns PROMPT_COMMAND
# and the DEBUG trap, runs precmd_functions before each prompt and
# preexec_functions once per typed line with the history line as $1.
cat > "$T/bp.sh" <<'BP'
__bp_imported=defined
precmd_functions=()
preexec_functions=()
__bp_ready=
__bp_precmd() { local rc=$? f; for f in "${precmd_functions[@]}"; do (exit $rc); "$f"; done; __bp_ready=1; }
__bp_preexec() {
  [ -n "$__bp_ready" ] || return 0
  [ "$BASH_COMMAND" = __bp_precmd ] && return 0
  __bp_ready=
  local l f; l=$(HISTTIMEFORMAT= builtin history 1); l=${l#"${l%%[! ]*}"}; l=${l#*[0-9] }; l=${l#"${l%%[! ]*}"}
  for f in "${preexec_functions[@]}"; do "$f" "$l"; done
}
PROMPT_COMMAND=__bp_precmd
trap '__bp_preexec' DEBUG
BP
HK_BASHRC="PS1=''
HISTCONTROL=ignorespace
. '$T/bp.sh'
. '$SHARE/shell/moox.bash'"
# shellcheck disable=SC2086
hk_bash $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%5 >/dev/null <<EOF
cat "$L" > "$T/c3"
trap -p DEBUG > "$T/trap"

 cat "$L" > "$T/c4"
EOF
hk_eq "bash-preexec: recorded through preexec_functions" "cat \"$L\" > \"$T/c3\"" "$(cat "$T/c3" 2>/dev/null)"
hk_absent "bash-preexec: a line history did not take is not recorded" "$T/c4"
# Read INSIDE the shell (G4-M4): moox.bash must leave bash-preexec's own DEBUG trap.
hk_eq "bash-preexec: its DEBUG trap is still __bp_preexec" "trap -- '__bp_preexec' DEBUG" "$(cat "$T/trap" 2>/dev/null)"
hk_absent "bash-preexec: the finished command's recording is cleared" "$L"

hk_finish
