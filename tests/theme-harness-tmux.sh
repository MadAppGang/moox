#!/bin/sh
# theme-harness-tmux.sh — the mandatory logging and guarding `tmux` of
# tests/theme-per-screen.test.sh (architecture.md §8.1).
#
# The harness copies this file to $D/X/bin/tmux for every case, with the three
# configuration lines below filled in and nothing else changed, and puts that
# bin dir first on the PATH of both private servers. Every tmux call that a
# hook job, Catppuccin, theme.sh or a screen makes therefore lands here first.
# (The harness's own calls do not: ti() and to() already name their socket and
# run the real tmux, so they cannot add noise to the counts below.)
#
# GUARD. A call runs only if
#   - its argv has -S PATH with PATH under H_ROOT (a screen's attach), or
#   - it has no -S and ${TMUX%%,*} is under H_ROOT (a hook job: tmux puts
#     TMUX in the environment of every job it starts).
# -L is always refused: it names a socket by label, which resolves under
# $TMUX_TMPDIR — or, where that is unset, in /private/tmp/tmux-UID, the live
# server's directory. A path with `..` is refused. Anything refused is logged
# REFUSED and exits 97 without running. A copy whose configuration lines are
# empty (this file run as is) refuses everything.
#
# LOG. $H_CASE/tmux.log gets one line per call, `epoch-ms pid status argv`,
# argv joined by spaces with newlines and tabs flattened. status is the exit
# status, `exec` for an attach (the screens are exec'ed, so the real client
# keeps this pid), PAUSED when a call starts waiting on a control, or REFUSED.
# $H_CASE/calls.tsv gets the same calls tab-separated with what the harness
# counts by: ms, pid, ppid, status, the canonical command words of the `;`
# chain (comma-separated: `set` is logged as set-option), the control that
# acted (fail.X, failonce.X, record, …) and argv.
#
# CONTROLS, files in $H_CASE. "A call with that subcommand" means any command
# of its `;` chain has that word, literally or by its canonical name, so
# pause.source-file also holds `tmux source x`.
#   pause.<sub>    while it exists, such a call logs PAUSED and waits (50 ms
#                  polls, 30 s at most, then PAUSE-TIMEOUT and it runs)
#   pause.match    while it exists, any call whose argv contains the file's
#                  first line waits the same way (an empty line matches nothing)
#   fail.<sub>     while it exists, such a call exits 1 without running
#   failonce.<sub> the next such call exits 1 without running; it claims the
#                  file by renaming it to failonce.<sub>.claimed.<pid>, and a
#                  rename is atomic, so exactly one call fails
#   record.match   while it exists, the stdout of a call whose argv contains
#                  the first line is also saved to out.<epoch-ms>.<pid>; it is
#                  written to a temp file first and then printed, so the exit
#                  status survives
# stdout and stderr otherwise pass through unchanged, and a call's exit status
# is the real tmux's.

# --- configuration: the harness fills in these three lines in each copy ------
H_ROOT=''
H_CASE=''
H_REAL_TMUX=''
# -----------------------------------------------------------------------------

set -f

ms=$(date +%s%N 2>/dev/null)
case $ms in
  *N|'') ms=$(perl -MTime::HiRes=time -e 'printf "%d", time() * 1000' 2>/dev/null) ;;
  *) ms=${ms%??????} ;;
esac

argv=$*
case $argv in
  *'
'*|*'	'*) argv=$(printf '%s' "$argv" | tr '\n\t' '  ') ;;
esac

csv=''     # canonical command words, comma-separated
words=''   # literal and canonical command words, space-separated

# log STATUS [CONTROL]
log() {
  [ -n "$H_CASE" ] && [ -d "$H_CASE" ] || return 0
  printf '%s %s %s %s\n' "$ms" "$$" "$1" "$argv" 2>/dev/null >> "$H_CASE/tmux.log"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$ms" "$$" "$PPID" "$1" "$csv" "${2:-}" "$argv" \
    2>/dev/null >> "$H_CASE/calls.tsv"
}

refuse() {
  log REFUSED "$1"
  printf 'theme-harness-tmux: REFUSED (%s): tmux %s\n' "$1" "$argv" >&2
  exit 97
}

under_root() {
  [ -n "$H_ROOT" ] || return 1
  case $1 in "$H_ROOT"/*) ;; *) return 1 ;; esac
  case $1 in */../*|*/..) return 1 ;; esac
  return 0
}

# canon WORD: sets cw to tmux's full command name for WORD (the aliases this
# harness and theme.sh can meet; anything else is kept as is).
canon() {
  case $1 in
    attach) cw=attach-session ;;
    capturep) cw=capture-pane ;;
    detach) cw=detach-client ;;
    display) cw=display-message ;;
    has) cw=has-session ;;
    if) cw=if-shell ;;
    killp) cw=kill-pane ;;
    killw) cw=kill-window ;;
    ls) cw=list-sessions ;;
    lsc) cw=list-clients ;;
    lsp) cw=list-panes ;;
    lsw) cw=list-windows ;;
    menu) cw=display-menu ;;
    new) cw=new-session ;;
    neww) cw=new-window ;;
    refresh) cw=refresh-client ;;
    respawnp) cw=respawn-pane ;;
    run) cw=run-shell ;;
    selectp) cw=select-pane ;;
    selectw) cw=select-window ;;
    send) cw=send-keys ;;
    set) cw=set-option ;;
    setenv) cw=set-environment ;;
    setw) cw=set-window-option ;;
    show) cw=show-options ;;
    showenv) cw=show-environment ;;
    showw) cw=show-window-options ;;
    source) cw=source-file ;;
    splitw) cw=split-window ;;
    switchc) cw=switch-client ;;
    *) cw=$1 ;;
  esac
}

# --- parse: tmux's global flags (getopt clusters included), then the chain --
has_S=0 bad_S=0 has_L=0 want='' inopts=1 expect=1
for a do
  if [ "$inopts" = 1 ]; then
    if [ -n "$want" ]; then
      [ "$want" = S ] && { under_root "$a" || bad_S=1; }
      want=''
      continue
    fi
    case $a in
      --) inopts=0; continue ;;
      -?*)
        c=${a#-}
        while [ -n "$c" ]; do
          f=${c%"${c#?}"}
          c=${c#?}
          case $f in
            S)
              has_S=1
              if [ -n "$c" ]; then under_root "$c" || bad_S=1; else want=S; fi
              break ;;
            L) has_L=1; [ -n "$c" ] || want=L; break ;;
            c|f|T) [ -n "$c" ] || want=$f; break ;;
          esac
        done
        continue ;;
      *) inopts=0 ;;
    esac
  fi
  if [ "$expect" = 1 ]; then
    w=${a%;}
    canon "$w"
    words="$words $w $cw"
    csv="${csv:+$csv,}$cw"
    expect=0
  fi
  case $a in
    ';') expect=1 ;;
    *\\\;) ;;
    *\;) expect=1 ;;
  esac
done
[ "$want" = S ] && bad_S=1

# --- guard -------------------------------------------------------------------
[ -n "$H_ROOT" ] && [ -n "$H_CASE" ] && [ -n "$H_REAL_TMUX" ] || refuse "unconfigured wrapper"
case $H_ROOT in /?*) ;; *) refuse "root '$H_ROOT' is not an absolute path" ;; esac
if [ "$has_L" = 1 ]; then
  refuse "-L names a socket by label"
elif [ "$has_S" = 1 ]; then
  [ "$bad_S" = 0 ] || refuse "-S outside $H_ROOT"
else
  under_root "${TMUX%%,*}" || refuse "no -S, and \$TMUX is not a socket under $H_ROOT"
fi

# --- screens: exec'ed, so the real client keeps this pid ----------------------
for w in $words; do
  case $w in
    attach-session)
      log exec
      exec "$H_REAL_TMUX" "$@" ;;
  esac
done

# --- controls ------------------------------------------------------------------

# has_control PREFIX: a PREFIX.<word> file exists for one of the chain's words;
# sets ctl to its name.
has_control() {
  for w in $words; do
    if [ -e "$H_CASE/$1.$w" ]; then
      ctl="$1.$w"
      return 0
    fi
  done
  return 1
}

# matches_file FILE: FILE exists and argv contains its non-empty first line.
matches_file() {
  [ -e "$1" ] || return 1
  pat=''
  IFS= read -r pat 2>/dev/null < "$1"
  [ -n "$pat" ] || return 1
  case $argv in *"$pat"*) return 0 ;; esac
  return 1
}

held=0 polls=0
while has_control pause || matches_file "$H_CASE/pause.match"; do
  if [ "$held" = 0 ]; then
    held=1
    log PAUSED "${ctl:-pause.match}"
  fi
  if [ "$polls" -ge 600 ]; then
    log PAUSE-TIMEOUT
    break
  fi
  polls=$((polls + 1))
  sleep 0.05
done

if has_control fail; then
  log 1 "$ctl"
  printf 'theme-harness-tmux: injected failure (%s)\n' "$ctl" >&2
  exit 1
fi
for w in $words; do
  if [ -e "$H_CASE/failonce.$w" ] && mv "$H_CASE/failonce.$w" "$H_CASE/failonce.$w.claimed.$$" 2>/dev/null; then
    log 1 "failonce.$w"
    printf 'theme-harness-tmux: injected failure (failonce.%s)\n' "$w" >&2
    exit 1
  fi
done

# --- run -----------------------------------------------------------------------
if matches_file "$H_CASE/record.match"; then
  tmp="$H_CASE/.out.$ms.$$"
  "$H_REAL_TMUX" "$@" > "$tmp"
  rc=$?
  cat "$tmp"
  mv "$tmp" "$H_CASE/out.$ms.$$"
  log "$rc" record
  exit "$rc"
fi

"$H_REAL_TMUX" "$@"
rc=$?
log "$rc"
exit "$rc"
