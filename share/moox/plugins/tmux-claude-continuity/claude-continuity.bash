# claude-continuity.bash — launch recorder and first-prompt resume (bash twin, D19)
# shellcheck shell=bash
#
# Sourced from ~/.bashrc through share/moox/shell/moox.bash, which `moox setup`
# adds. Same contract as claude-continuity.zsh (read its header for the why):
# record the typed line to launch/<N> while it runs, clear it at the prompt,
# and on the first prompt of an engine-created pane print the saved contents,
# then record → ack → run the resume command. bash 3.2 (macOS /bin/bash) and
# later.
#
# bash has no preexec, so the recorder is a DEBUG trap armed at the end of every
# prompt. PROMPT_COMMAND becomes `_cc_disarm; <the user's own>; _cc_arm`, first
# and last, so nothing the prompt itself runs is ever recorded. The trap records
# the line from `history 1`, and ONLY if the history entry number moved since the
# prompt: a command history did not take (HISTCONTROL=ignorespace/ignoredups,
# HISTIGNORE, HISTSIZE=0) is not recorded at all, so a completed older command
# can never be replayed in its place (measured: Q9/Q9b). Every unknown case fails
# to "nothing recorded", never to a wrong line. Cost: two forks per typed
# command ($(history 1) here and in _cc_arm) plus the clear's `rm`; the zsh
# hook needs none.
#
# Source this file LAST in ~/.bashrc: a PROMPT_COMMAND assigned after it replaces
# the wrapper. With bash-preexec loaded first (__bp_imported), the hook uses its
# precmd_functions/preexec_functions instead of installing its own trap.

[[ -n "${TMUX_PANE:-}" ]] || return 0
[[ $- == *i* ]] || return 0
[[ -z "${_CC_BASH_LOADED:-}" ]] || return 0

_CC_MARKER='' _CC_VETO=''
if [[ -n "${MOOX_SOCKET:-}" ]]; then
  if [[ "$MOOX_SOCKET" != "${TMUX%%,*}" || "${MOOX_ACTIVE:-}" != 1 || -z "${MOOX_STATE_DIR:-}" \
        || -z "${MOOX_PENDING_DIR:-}" || -z "${MOOX_LAUNCH_DIR:-}" ]]; then
    # Inherited from another server, or this server is excluded (C1).
    unset MOOX_RESUME MOOX_RESTORE_CONTENT MOOX_RESUME_ACK _CC_MARKER _CC_VETO
    return 0
  fi
  _CC_MARKER="$MOOX_STATE_DIR/active"
  _CC_PENDING_DIR="$MOOX_PENDING_DIR"
  _CC_LAUNCH_DIR="$MOOX_LAUNCH_DIR"
else
  # Legacy path ONLY for the real login default server: the literal tmux paths,
  # never a basename (a scratch `-S /x/default` is not the default server).
  case "${TMUX%%,*}" in
    "/tmp/tmux-$UID/default"|"/private/tmp/tmux-$UID/default") ;;
    *) unset MOOX_RESUME MOOX_RESTORE_CONTENT MOOX_RESUME_ACK _CC_MARKER _CC_VETO; return 0 ;;
  esac
  _CC_VETO="$HOME/.config/tmux-claude/moox-excluded"
  _CC_PENDING_DIR="$HOME/.config/tmux-claude/pending"
  _CC_LAUNCH_DIR="$HOME/.config/tmux-claude/launch"
fi
_CC_BASH_LOADED=1
_CC_PENDING_FILE="${_CC_PENDING_DIR}/${TMUX_PANE#%}"
_CC_LAUNCH_FILE="${_CC_LAUNCH_DIR}/${TMUX_PANE#%}"
_cc_armed=
_cc_top=
_cc_resume_armed=1
_cc_trap_seen=

# The per-write participation test (R2-C5, R3-C4): exactly one path is set.
_cc_live() {
  if [[ -n "$_CC_MARKER" ]]; then [[ -e "$_CC_MARKER" ]]; return; fi
  [[ -n "$_CC_VETO" && ! -e "$_CC_VETO" ]]
}

# The number of the newest history entry ("" when history holds nothing).
_cc_hnum() {
  local l
  l=$(HISTTIMEFORMAT='' builtin history 1)
  l=${l#"${l%%[! ]*}"}
  printf '%s' "${l%%[!0-9]*}"
}

_claude_continuity_record() {
  local line="$1"
  [[ -n "$line" ]] || return 0
  _cc_live || return 0
  [[ -d "$_CC_LAUNCH_DIR" ]] || mkdir -p "$_CC_LAUNCH_DIR" 2>/dev/null || return 0
  # First line only: a continuation line is not part of the command to replay.
  printf '%s\n' "${line%%$'\n'*}" >| "$_CC_LAUNCH_FILE" 2>/dev/null || :
  return 0
}

# Records the line history just took, if it took one. Shared by the DEBUG trap
# and the bash-preexec path.
_cc_record_from_history() {
  local l n
  l=$(HISTTIMEFORMAT='' builtin history 1)
  n=${l#"${l%%[! ]*}"}
  n=${n%%[!0-9]*}
  [[ -n "$n" && "$n" != "$_cc_top" ]] || return 0
  l=${l#"${l%%[! ]*}"}; l=${l#"$n"}; l=${l#"${l%%[! ]*}"}
  _claude_continuity_record "$l"
}

_cc_debug() {
  # _cc_disarm is first in PROMPT_COMMAND: seeing it proves our trap is live.
  if [[ "$BASH_COMMAND" == _cc_disarm ]]; then _cc_trap_seen=1; return 0; fi
  [[ "$_cc_armed" == 1 ]] || return 0
  [[ -n "${COMP_LINE-}" ]] && return 0
  _cc_armed=
  _cc_record_from_history
  return 0
}

# First in PROMPT_COMMAND. Keeps $? for the user's own PROMPT_COMMAND.
_cc_disarm() {
  local rc=$?
  _cc_armed=
  return $rc
}

# The §4.4 sequence, once, before the recorder is re-armed, so no command inside
# the resumed line can reach the recorder.
_cc_resume() {
  local cmd="" f="${MOOX_RESTORE_CONTENT:-}" ack="${MOOX_RESUME_ACK:-}" e=
  unset MOOX_RESTORE_CONTENT MOOX_RESUME_ACK
  if [[ -n "$f" && -f "$f" ]]; then
    printf '%s\n' "$(<"$f")"
    command rm -f -- "$f" 2>/dev/null
  fi
  if [[ -n "${MOOX_RESUME:-}" ]]; then
    cmd="$MOOX_RESUME"
    unset MOOX_RESUME
  elif [[ -f "$_CC_PENDING_FILE" ]]; then
    cmd="$(<"$_CC_PENDING_FILE")"
    command rm -f -- "$_CC_PENDING_FILE" 2>/dev/null
  else
    return 0                      # no command yet: stay armed for a pending file
  fi
  [[ -n "$cmd" ]] || return 0
  _cc_resume_armed=
  _claude_continuity_record "$cmd"
  if [[ -n "$ack" ]]; then : >| "$ack" 2>/dev/null || :; fi
  # A resumed line that fails must not end a shell running under `set -e`.
  case $- in *e*) e=1; set +e ;; esac
  eval "$cmd"
  if [[ -n "$e" ]]; then set -e; fi
  # The resumed command has finished: clear its recording now, as zsh's precmd
  # does right after its resume returns.
  if _cc_live && [[ -e "$_CC_LAUNCH_FILE" ]]; then command rm -f -- "$_CC_LAUNCH_FILE" 2>/dev/null || :; fi
  return 0
}

# Last in PROMPT_COMMAND: clear the finished command's recording, run a pending
# resume, then arm the recorder for the next typed line.
_cc_arm() {
  local rc=$?
  if _cc_live; then
    # B2: the command finished, so its recording goes. Removed, not truncated,
    # so a finished one-shot leaves NO launch/<N> behind (R2-C6); one fork, and
    # only when a recording exists (once per typed command).
    if [[ -e "$_CC_LAUNCH_FILE" ]]; then command rm -f -- "$_CC_LAUNCH_FILE" 2>/dev/null || :; fi
    if [[ -n "$_cc_resume_armed" ]]; then _cc_resume; fi
  elif [[ -n "$_cc_resume_armed" ]]; then
    unset MOOX_RESUME MOOX_RESTORE_CONTENT MOOX_RESUME_ACK
  fi
  _cc_trap_seen=   # set again when our trap fires for the next _cc_disarm (_CC_RETRAP)
  _cc_top=$(_cc_hnum)
  _cc_armed=1
  return $rc
}

_cc_preexec() {
  [[ "$_cc_armed" == 1 ]] || return 0
  _cc_armed=
  _cc_record_from_history
  return 0
}

# The `trap` command that installs our DEBUG trap chained with the current
# one (never replacing it), or nothing when ours is already in place.
#
# The user's trap is never parsed, split or eval'd here (G3-H3): `trap -p`
# prints `trap -- <BODY> DEBUG` with BODY already one shell-quoted word, so it
# is cut out by parameter expansion and glued, still quoted, after our own
# quoted `_cc_debug` and a quoted newline: `trap -- '_cc_debug'$'\n'<BODY> DEBUG`
# is one word for the eval that installs it. Joined by a NEWLINE, not `; `
# (G3-H4): a body that ends in `;`, a newline or a comment stays valid. Any
# other `trap -p` shape is left alone rather than guessed at.
#
# It must be EVALUATED AT TOP LEVEL, never inside a function or a sourced file:
# bash restores a pre-existing DEBUG trap when the function or file that changed
# it returns (measured on 3.2). So it runs from PROMPT_COMMAND (_CC_RETRAP),
# which also re-chains it if a later rc line or the user replaces the trap.
_cc_trap_cmd() {
  local cur body
  cur="$(trap -p DEBUG)"
  case "$cur" in *_cc_debug*) return 0 ;; esac
  if [[ -z "$cur" ]]; then
    printf 'trap -- _cc_debug DEBUG'
    return 0
  fi
  body="${cur#trap -- }"
  body="${body% DEBUG}"
  [[ "trap -- $body DEBUG" == "$cur" && "$body" == \'*\' ]] || return 0
  printf '%s%s DEBUG' "trap -- '_cc_debug'\$'\\n'" "$body"
}
# Our trap fired for _cc_disarm (first in PROMPT_COMMAND) iff it is installed;
# only when it did not is there a fork, to (re)install it.
# shellcheck disable=SC2016
_CC_RETRAP='[[ -n "$_cc_trap_seen" ]] || eval "$(_cc_trap_cmd)"'

if [[ -n "${__bp_imported:-}" ]]; then
  # bash-preexec owns the DEBUG trap and PROMPT_COMMAND; register with it.
  preexec_functions+=(_cc_preexec)
  precmd_functions+=(_cc_arm)
else
  # An array PROMPT_COMMAND runs element by element only on bash >= 5.1; older
  # bash runs element 0 alone, so there the string form below (which assigns
  # element 0) is the one that runs.
  if [[ "$(declare -p PROMPT_COMMAND 2>/dev/null)" == "declare -a"* ]] \
     && (( BASH_VERSINFO[0] > 5 || (BASH_VERSINFO[0] == 5 && BASH_VERSINFO[1] >= 1) )); then
    PROMPT_COMMAND=(_cc_disarm ${PROMPT_COMMAND[@]+"${PROMPT_COMMAND[@]}"} "$_CC_RETRAP" _cc_arm)
  else
    # Joined by NEWLINES, never `; ` (G3-H4): a PROMPT_COMMAND that already
    # ends in `;` (`history -a;`, vte.sh, iTerm2, direnv), a newline or a
    # comment would otherwise give `;;` or `\n;` — a syntax error at every
    # prompt that also skips _CC_RETRAP and _cc_arm. Each piece on its own line
    # stays valid whatever the user's text ends with.
    # shellcheck disable=SC2128,SC2178
    _cc_pc="${PROMPT_COMMAND:-}" _cc_nl=$'\n'
    # shellcheck disable=SC2128,SC2178
    PROMPT_COMMAND="_cc_disarm${_cc_nl}${_cc_pc:+$_cc_pc$_cc_nl}${_CC_RETRAP}${_cc_nl}_cc_arm"
    unset _cc_pc _cc_nl
  fi
  # NOT installed here: the first prompt's _CC_RETRAP does it, at top level,
  # before the first typed line. Inside a sourced file bash hides an existing
  # DEBUG trap from `trap -p` (no functrace), so _cc_trap_cmd would see none
  # and print a bare `trap -- _cc_debug DEBUG`. bash 3.2 restores the user's
  # trap when the file returns, which masked this; bash 5.x KEEPS a DEBUG trap
  # the sourced file set, so the user's trap was replaced and lost (measured
  # on 5.0–5.3 in docker, iter-2 G3).
fi
