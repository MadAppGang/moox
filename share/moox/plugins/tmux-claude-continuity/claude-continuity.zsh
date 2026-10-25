# claude-continuity.zsh — launch recorder and first-prompt resume for tmux panes
#
# Vendored into moox (share/moox/plugins/tmux-claude-continuity). Sourced from
# ~/.zshrc through share/moox/shell/moox.zsh, which `moox setup` adds:
#   source "<moox share root>/shell/moox.zsh"
#
# Two jobs, both fork-free on the hot path:
#
#   record   preexec writes the line as TYPED to launch/<N> (N = pane number),
#            precmd removes it when the shell is back at a prompt. The file so
#            exists only while a command runs, which is exactly what a restore
#            should bring back; a finished `terraform apply` is never replayed.
#   resume   on the first prompt of a pane the moox engine created, print the
#            saved pane contents, then record → ack → run the resume command
#            (MOOX_RESUME, or a pending/<N> file written by cc_thaw.sh).
#
# Why a precmd hook instead of tmux send-keys: keystrokes sent into a shell that
# is still sourcing a heavy rc are dropped (typeahead flushed by prompt redraws).
# The first prompt is the first moment the shell is genuinely interactive.
#
# ── Which server's state this shell may touch (C1) ──────────────────────────
# Recordings and pending files are keyed by pane number alone, and every tmux
# server numbers its panes from %0. A shell on any OTHER server that writes into
# the real workspace's slots corrupts its restore: on 2026-10-02 an agent's
# scratch slot that happened to be %13 had its line replayed into the REAL %13
# on the next reboot. The rule is: trust the environment only if it names THIS
# server. The moox gate exports MOOX_SOCKET = the server's own #{socket_path},
# which tmux also puts, verbatim, into every pane's $TMUX; comparing the two
# strings costs no fork. A scratch server started from inside a real pane
# inherits the real MOOX_* values and fails that comparison, so it writes
# nothing.
#
# A shell with no MOOX_* at all (started before the first moox gate, or on a
# server moox does not manage) takes the legacy branch, and only on the real
# login default server, compared by realpath (`:A`, no fork), never by basename:
# a scratch `-S /x/default` is not the default server.
#
# ── Revocation reaches shells that already exist (R2-C5, R3-C4) ─────────────
# A running shell keeps the MOOX_ACTIVE=1 it inherited. So every record, clear
# and consume re-tests a FILE with `[[ -e ]]` (no fork): in the env branch the
# participation marker $MOOX_STATE_DIR/active (removed when the socket stops
# matching @moox-sockets), in the legacy branch the veto file
# $HOME/.config/tmux-claude/moox-excluded (written while the default socket is
# excluded). Exactly one of the two is set; a missing marker means "record
# nothing, read nothing, consume nothing".

# Only meaningful inside tmux, and only for interactive shells.
[[ -n "$TMUX_PANE" ]] || return 0
[[ -o interactive ]] || return 0

_CC_MARKER='' _CC_VETO=''
if [[ -n "$MOOX_SOCKET" ]]; then
  if [[ "$MOOX_SOCKET" != "${TMUX%%,*}" || "$MOOX_ACTIVE" != 1 || -z "$MOOX_STATE_DIR" ]]; then
    # Inherited from another server, or this server is excluded: record nothing,
    # read nothing, and never let a resume command reach a child process.
    unset MOOX_RESUME MOOX_RESTORE_CONTENT MOOX_RESUME_ACK _CC_MARKER _CC_VETO
    return 0
  fi
  _CC_MARKER="$MOOX_STATE_DIR/active"
  _CC_PENDING_DIR="$MOOX_PENDING_DIR"
  _CC_LAUNCH_DIR="$MOOX_LAUNCH_DIR"
  if [[ -z "$_CC_PENDING_DIR" || -z "$_CC_LAUNCH_DIR" ]]; then
    unset MOOX_RESUME MOOX_RESTORE_CONTENT MOOX_RESUME_ACK _CC_MARKER _CC_VETO _CC_PENDING_DIR _CC_LAUNCH_DIR
    return 0
  fi
else
  # Legacy path ONLY for the real login default server (realpath, no fork).
  if [[ "${${TMUX%%,*}:A}" != "${${:-/tmp}:A}/tmux-$UID/default" ]]; then
    unset MOOX_RESUME MOOX_RESTORE_CONTENT MOOX_RESUME_ACK _CC_MARKER _CC_VETO
    return 0
  fi
  _CC_VETO="$HOME/.config/tmux-claude/moox-excluded"
  _CC_PENDING_DIR="$HOME/.config/tmux-claude/pending"
  _CC_LAUNCH_DIR="$HOME/.config/tmux-claude/launch"
fi
_CC_PENDING_FILE="${_CC_PENDING_DIR}/${TMUX_PANE#%}"
_CC_LAUNCH_FILE="${_CC_LAUNCH_DIR}/${TMUX_PANE#%}"

# The per-write participation test. Exactly one of the two paths is set.
_cc_live() {
  if [[ -n "$_CC_MARKER" ]]; then [[ -e "$_CC_MARKER" ]]; return; fi
  [[ -n "$_CC_VETO" && ! -e "$_CC_VETO" ]]
}

# Fork-free unlink when zsh/files is available; a fork otherwise (rare paths only).
zmodload -F zsh/files b:zf_rm 2>/dev/null
if (( $+builtins[zf_rm] )); then
  _cc_rm() { zf_rm -f -- "$@" 2>/dev/null }
else
  _cc_rm() { command rm -f -- "$@" 2>/dev/null }
fi

# ── Record the launch command, exactly as typed ──────────────────────────────
# zsh's preexec $1 is the line as TYPED, before alias expansion and with quoting
# intact (`c --name "My Session"`), which `ps` can never reconstruct. EVERY
# command is recorded, with no "is this a Claude launcher" test: the save keeps a
# recording only for a pane still running something, so any launcher works.
# First line only: a continuation line is not part of the command to replay.
_claude_continuity_record() {
  emulate -L zsh
  local line="$1"
  [[ -n "$line" ]] || return 0
  _cc_live || return 0
  [[ -d "$_CC_LAUNCH_DIR" ]] || mkdir -p "$_CC_LAUNCH_DIR" 2>/dev/null || return 0
  print -r -- "${line%%$'\n'*}" >| "$_CC_LAUNCH_FILE" 2>/dev/null
}

_claude_continuity_preexec() { _claude_continuity_record "$1" }

# ── First-prompt resume (C2) ─────────────────────────────────────────────────
# Order is load-bearing:
#   0. contents/banner (MOOX_RESTORE_CONTENT) are printed by this function on
#      their own; zsh runs preexec only for lines read from the editor, never for
#      commands inside a hook function, so the print is never recorded.
#   1. record the command FIRST, so a save at any later instant stores it;
#   2. create the durable ack (MOOX_RESUME_ACK) with a builtin; the LAUNCH
#      verdict reads it, and the clear below never removes it;
#   3. run it as a CHILD of this shell (eval, never exec), so quitting Claude
#      drops to the same interactive shell a typed `claude` would.
# The function disarms only after it has consumed a COMMAND: a restored plain
# shell pane stays armed for a later pending file (cc_thaw.sh writes one and
# respawns the pane). Until then the per-prompt cost is one file stat.
_claude_continuity_resume() {
  if ! _cc_live; then
    unset MOOX_RESUME MOOX_RESTORE_CONTENT MOOX_RESUME_ACK
    return 0
  fi
  local cmd="" f="$MOOX_RESTORE_CONTENT" ack="$MOOX_RESUME_ACK"
  unset MOOX_RESTORE_CONTENT MOOX_RESUME_ACK
  if [[ -n "$f" && -f "$f" ]]; then
    print -r -- "$(<"$f")"
    _cc_rm "$f"
  fi
  if [[ -n "$MOOX_RESUME" ]]; then
    cmd="$MOOX_RESUME"
    unset MOOX_RESUME
  elif [[ -f "$_CC_PENDING_FILE" ]]; then
    cmd="$(<"$_CC_PENDING_FILE")"
    _cc_rm "$_CC_PENDING_FILE"
  else
    return 0
  fi
  [[ -n "$cmd" ]] || return 0
  add-zsh-hook -d precmd _claude_continuity_resume 2>/dev/null
  unfunction _claude_continuity_resume 2>/dev/null
  _claude_continuity_record "$cmd"
  [[ -n "$ack" ]] && : >| "$ack" 2>/dev/null
  eval "${cmd}"
}

# ── Clear the recording when the command finishes ────────────────────────────
# Runs after _claude_continuity_resume in the same precmd, so a resumed Claude
# keeps its recording for the whole time it runs (the resume blocks in eval).
_claude_continuity_clear() {
  _cc_live || return 0
  [[ -s "$_CC_LAUNCH_FILE" ]] || return 0
  if (( $+builtins[zf_rm] )); then
    zf_rm -f -- "$_CC_LAUNCH_FILE" 2>/dev/null
  else
    : >| "$_CC_LAUNCH_FILE" 2>/dev/null   # fork-free; an empty recording counts as absent (B15)
  fi
}

autoload -Uz add-zsh-hook
add-zsh-hook precmd _claude_continuity_resume
add-zsh-hook precmd _claude_continuity_clear
add-zsh-hook preexec _claude_continuity_preexec
