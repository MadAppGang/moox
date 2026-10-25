#!/usr/bin/env bash
# cc_common.sh — primitives shared by every window freeze/thaw script.
#
# Sourced, never executed. Provides the five things this feature must not
# re-implement per script: the option reader, the two logs, the durable-write
# protocol (§2.4), the mkdir lock with stale-owner recovery, and the ;TAG=
# field scanner that makes the on-disk format collapse-proof (§2.1).
#
# bash 3.2.57 only: no associative arrays, no mapfile/readarray, no ${v,,},
# no `base64 -w0`, no globstar, no `wait -n`.

# shellcheck disable=SC2086
# $TMUX_CMD is deliberately unquoted at every call site: tests drive these
# scripts with TMUX_CMD="tmux -L cc-test -f /dev/null", which must word-split
# (post_restore.sh:18 precedent, landmine L14).

[ -n "${_CC_COMMON_LOADED:-}" ] && return 0
_CC_COMMON_LOADED=1

# Which server every tmux call addresses (X9). Never the bare default: a script
# that falls back to plain `tmux` from a pane of another server (a scratch
# `-L` test server, an isolated MCP slot) mutates the user's real workspace.
#   1. an explicit TMUX_CMD wins (tests: "tmux -L cc-test -f /dev/null");
#   2. inside tmux, the server this process lives in: ${TMUX%%,*};
#   3. outside tmux (the daemon runs the heartbeat), the gate's MOOX_SOCKET;
#   4. otherwise `false`: every call fails like "no server", and the
#      server-mutating entry points exit 0 through _cc_require_server.
# `-u` always: without a UTF-8 locale (a hook started with a bare env) tmux
# rewrites control characters such as the US separator to `_` in -F output.
# Limitation: TMUX_CMD is word-split at its call sites, so a socket path that
# contains whitespace is not supported.
# _CC_TMUX_FROM records which rule chose TMUX_CMD (tmux | moox | explicit |
# none), so the participation checks below can name the server the commands
# really go to (G3-M2) without a fork in the common, derived, case.
if [ -z "${TMUX_CMD:-}" ]; then
  if [ -n "${TMUX:-}" ]; then
    TMUX_CMD="tmux -u -S ${TMUX%%,*}"; _CC_TMUX_FROM=tmux
  elif [ -n "${MOOX_SOCKET:-}" ]; then
    TMUX_CMD="tmux -u -S $MOOX_SOCKET"; _CC_TMUX_FROM=moox
  else
    TMUX_CMD="false"; _CC_TMUX_FROM=none
  fi
else
  _CC_TMUX_FROM=explicit
fi

# Entry points that change a server call this first: with no server to address
# they do nothing at all (exit 0), never the default-server fallback.
_cc_require_server() { [ "$TMUX_CMD" != "false" ]; }

# The socket path of the server TMUX_CMD addresses, into _CC_TARGET_SOCK
# (memoised; call it, then read the variable — `$( )` would lose the memo).
# Derived TMUX_CMDs cost nothing; only an explicit one (a test, an agent's
# `TMUX_CMD="tmux -L x"`) asks that server once. Returns 1 when unknown.
_cc_target_sock() {
  if [ -z "${_CC_TARGET_SOCK_DONE:-}" ]; then
    case "${_CC_TMUX_FROM:-explicit}" in
      tmux) _CC_TARGET_SOCK="${TMUX%%,*}" ;;
      moox) _CC_TARGET_SOCK="${MOOX_SOCKET:-}" ;;
      none) _CC_TARGET_SOCK="" ;;
      *)    _CC_TARGET_SOCK="$($TMUX_CMD display-message -p '#{socket_path}' 2>/dev/null)" ;;
    esac
    _CC_TARGET_SOCK_DONE=1
  fi
  [ -n "${_CC_TARGET_SOCK:-}" ]
}

# Is <socket path> the real login default server? Literal paths, never a
# basename: a scratch `-S /x/default` (or `-L default` under another
# TMUX_TMPDIR) is not the default server.
_cc_is_legacy_sock() {
  case "${1:-}" in
    "/tmp/tmux-${UID:-$(id -u)}/default"|"/private/tmp/tmux-${UID:-$(id -u)}/default") return 0 ;;
  esac
  return 1
}

# Does this process take part in moox continuity (R2-C5, R3-C4)? The check for
# processes that live in a pane (the Claude hooks, thaw, freeze, the popup).
#   env branch (any MOOX_* identity present): the gate's MOOX_SOCKET must name
#     the server this process runs in (a scratch server started from inside a
#     real pane inherits the real MOOX_* values; plan-review mandatory item)
#     AND, when TMUX_CMD was given explicitly, the server it addresses;
#     MOOX_ACTIVE=1, and the participation marker must still exist (a later
#     exclusion removes it, and already-running processes stop writing).
#   legacy branch (no MOOX_* at all): only on the real login default server
#     (G3-M1: before, ANY server passed, so a thaw on a scratch server wrote an
#     eval'd pending/<N> into the real server's legacy dir) and only while the
#     veto file is absent. CC_TEST=1 passes it: a test promises an isolated
#     `-L … -f /dev/null` server, which _cc_assert_isolation enforces, and a
#     real veto file in the tester's HOME must not silently skip the test.
# No fork unless TMUX_CMD is explicit: string compares and one stat.
_cc_participating() {
  if [ -n "${MOOX_STATE_DIR:-}" ] || [ -n "${MOOX_SOCKET:-}" ]; then
    [ -n "${MOOX_SOCKET:-}" ] && [ -n "${TMUX:-}" ] && [ "$MOOX_SOCKET" = "${TMUX%%,*}" ] \
      && [ "${MOOX_ACTIVE:-}" = "1" ] && [ -n "${MOOX_STATE_DIR:-}" ] && [ -e "$MOOX_STATE_DIR/active" ] \
      || return 1
    [ "${_CC_TMUX_FROM:-}" = tmux ] && return 0
    _cc_target_sock && [ "$_CC_TARGET_SOCK" = "$MOOX_SOCKET" ]
  else
    [ "${CC_TEST:-0}" = "1" ] && return 0
    _cc_on_legacy_default && [ ! -e "$HOME/.config/tmux-claude/moox-excluded" ]
  fi
}

# The entry-point check for scripts that change a server or its continuity
# state (cc_freeze.sh, cc_thaw.sh, cc_popup.sh). Same as _cc_participating,
# plus the daemon's case: `tt serve` runs heartbeat/reclaim/ledger-seed outside
# tmux with this server's own MOOX_* contract, so TMUX_CMD was built from
# MOOX_SOCKET itself (or, given explicitly, addresses that same server) and
# the marker is what proves participation. Never true with no server to
# address.
_cc_script_participating() {
  _cc_require_server || return 1
  if [ -z "${TMUX:-}" ] && [ -n "${MOOX_SOCKET:-}" ]; then
    [ "${MOOX_ACTIVE:-}" = "1" ] && [ -n "${MOOX_STATE_DIR:-}" ] && [ -e "$MOOX_STATE_DIR/active" ] \
      && _cc_target_sock && [ "$_CC_TARGET_SOCK" = "$MOOX_SOCKET" ]
    return
  fi
  _cc_participating
}

# Is this process addressing the real login default server? The server
# TMUX_CMD names (inside a pane with a derived TMUX_CMD: ${TMUX%%,*}, no fork).
_cc_on_legacy_default() {
  _cc_target_sock || return 1
  _cc_is_legacy_sock "$_CC_TARGET_SOCK"
}

# A continuity directory (pending | launch | panes) for THIS server, or nothing.
#   1. the gate's @claude-continuity-<kind>-dir option on the server (a test
#      server sets it too);
#   2. the gate's MOOX_<KIND>_DIR, when this process participates by env;
#   3. the legacy ~/.config/tmux-claude/<kind> ONLY when the server addressed
#      is the real login default.
# Anything else prints nothing and returns 1 (G3-H1): with the options unset —
# any excluded, unmanaged or `-f /dev/null` server — the old `_cc_opt … $HOME/
# .config/tmux-claude/<kind>` default WAS the login default server's pane-number
# slots, so a freeze of %13 on a scratch server read and deleted the real %13's.
_cc_cont_dir() {
  local kind="$1" v envd
  v="$($TMUX_CMD show-option -gqv "@claude-continuity-$kind-dir" 2>/dev/null)"
  if [ -n "$v" ]; then printf '%s' "$v"; return 0; fi
  if [ -n "${MOOX_SOCKET:-}" ] || [ -n "${MOOX_STATE_DIR:-}" ]; then
    case "$kind" in
      pending) envd="${MOOX_PENDING_DIR:-}" ;;
      launch)  envd="${MOOX_LAUNCH_DIR:-}" ;;
      panes)   envd="${MOOX_PANES_DIR:-}" ;;
      *)       envd="" ;;
    esac
    if [ -n "$envd" ] && _cc_script_participating; then printf '%s' "$envd"; return 0; fi
    return 1
  fi
  if _cc_on_legacy_default; then printf '%s' "$HOME/.config/tmux-claude/$kind"; return 0; fi
  return 1
}

# ── Lock tuning (see the Locks section for what each one governs) ────────────
# All three are overridable from the environment so a test can compress the
# windows without editing the library. Defaults are the production values.
#
# STALE: an owner we cannot positively identify (a legacy 3-field owner file, or
#   a `ps` that would not answer) is reclaimed once past this age. It is NEVER
#   applied to an owner whose identity we CAN confirm — that is the naive
#   age-based timeout that steals a live worker's lock.
CC_LOCK_STALE_SECS="${CC_LOCK_STALE_SECS:-300}"
# ORPHAN GRACE: the mkdir→owner-write window. A lock directory with NO owner
#   file is either (a) a taker that is between mkdir(2) and its printf right
#   now, or (b) a taker that died in that window and will never come back.
#   (a) lasts microseconds — the owner write is fork-free by construction — so
#   anything ownerless for longer than this grace is (b) and is reclaimed.
CC_LOCK_ORPHAN_GRACE_SECS="${CC_LOCK_ORPHAN_GRACE_SECS:-5}"
# CEILING: the backstop. Nothing this feature does — freeze, thaw, sweep, save —
#   runs for a day, so a lock this old is a wedge whatever its owner file says.
CC_LOCK_MAX_AGE_SECS="${CC_LOCK_MAX_AGE_SECS:-86400}"

# ── Options ──────────────────────────────────────────────────────────────────
# Same two lines every script in this repo inlines (post_restore.sh:51-52),
# folded into one call because six scripts now need it. Args: <option> <default>
_cc_opt() {
  local v
  v="$($TMUX_CMD show-option -gqv "$1" 2>/dev/null)"
  printf '%s' "${v:-$2}"
}

# ── Clock ────────────────────────────────────────────────────────────────────
# CC_NOW freezes it so tombstone titles, frozen_at and idle ages are
# golden-file comparable (§3).
_cc_now() { printf '%s' "${CC_NOW:-$(date +%s)}"; }

# ── Logging ──────────────────────────────────────────────────────────────────
# Path resolution is lazy: sourcing this file must not fork tmux for a script
# that never logs. An already-set LOG_FILE (post_restore.sh) wins, so sourcing
# these libs from an existing hook cannot redirect that hook's log.
_cc_log_path() {
  if [ -z "${_CC_LOG_PATH:-}" ]; then
    _CC_LOG_PATH="${CC_LOG_FILE:-${LOG_FILE:-}}"
    # The gate points @claude-continuity-log-file at the per-socket log dir
    # (§4.2a). Without it: MOOX_STATE_DIR only when that env names THIS server
    # (an inherited one belongs to another server, G3-M8), the default key's
    # dir only for the login default server (or a CC_TEST run, whose HOME is
    # scratch), and otherwise nowhere.
    if [ -z "$_CC_LOG_PATH" ]; then
      _CC_LOG_PATH="$($TMUX_CMD show-option -gqv @claude-continuity-log-file 2>/dev/null)"
    fi
    if [ -z "$_CC_LOG_PATH" ]; then
      if [ -n "${MOOX_STATE_DIR:-}" ] && _cc_target_sock && [ "$_CC_TARGET_SOCK" = "${MOOX_SOCKET:-}" ]; then
        _CC_LOG_PATH="$MOOX_STATE_DIR/log/claude-continuity.log"
      elif [ "${CC_TEST:-0}" = "1" ] || { [ -z "${MOOX_STATE_DIR:-}" ] && _cc_on_legacy_default; }; then
        _CC_LOG_PATH="${XDG_STATE_HOME:-$HOME/.local/state}/moox/sockets/default/log/claude-continuity.log"
      else
        _CC_LOG_PATH="/dev/null"
      fi
    fi
  fi
  printf '%s' "$_CC_LOG_PATH"
}

# Defined only if the sourcing script has not already defined it. post_restore.sh
# and pre_restore.sh each carry their own byte-identical copy; redefining theirs
# from a lib would move their log under this file's resolution rules.
if ! type -t _cc_log >/dev/null 2>&1; then
  _cc_log() {
    # Best-effort: never let logging failure abort a restore.
    local lf; lf="$(_cc_log_path)"
    { mkdir -p "$(dirname "$lf")" 2>/dev/null && \
      printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$1" >> "$lf"; } 2>/dev/null || true
  }
fi

# The freeze log is the SECOND durable copy of every session id this feature
# kills for (§2.6/H1c) — the copy that survives losing the whole store, and the
# reason no snapshot replica exists. Derived, not an option (post_restore.sh:696
# idiom). Mirrored into the main log so one `tail` still shows everything.
_cc_flog() {
  local lf ff
  lf="$(_cc_log_path)"
  ff="${CC_FREEZE_LOG:-${lf%.log}-freeze.log}"
  [ "$lf" = /dev/null ] && [ -z "${CC_FREEZE_LOG:-}" ] && ff=/dev/null
  { mkdir -p "$(dirname "$ff")" 2>/dev/null && \
    printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$1" >> "$ff"; } 2>/dev/null || true
  _cc_log "$1"
}

# ── base64 ───────────────────────────────────────────────────────────────────
# Encoding is `base64 | tr -d '\n'` — never -w0, which is GNU-only
# (pre_save.sh:344 precedent). Decoding tries -d then -D, because older macOS
# accepts only -D (post_restore.sh:536-540 precedent).
_cc_b64() {
  if [ "$#" -gt 0 ]; then printf '%s' "$1" | base64 2>/dev/null | tr -d '\n'
  else base64 2>/dev/null | tr -d '\n'; fi
}

_cc_unb64() {
  local out
  out="$(printf '%s' "${1:-}" | base64 -d 2>/dev/null)"
  [ -n "$out" ] || out="$(printf '%s' "${1:-}" | base64 -D 2>/dev/null)"
  printf '%s' "$out"
}

# ── The ;TAG= scanner ────────────────────────────────────────────────────────
# Every optional value in every file this feature owns is a ;PREFIX=-tagged
# token, scanned by PREFIX and never by position. That is what makes the format
# immune to L1/L6/L15: a tagged token is never empty (the tag itself is the
# floor), so a run of tabs can never collapse and shift the columns after it.
# Args: <record-line> <;TAG=>.  Returns 1 when the tag is absent.
_cc_tag() {
  local line="$1" tag="$2" f
  local IFS='	'
  set -f
  # shellcheck disable=SC2086
  set -- $line
  set +f
  for f in "$@"; do
    case "$f" in
      "$tag"*) printf '%s' "${f#"$tag"}"; return 0 ;;
    esac
  done
  return 1
}

# Same, but the value is base64 (every free-text value is — §2.1).
_cc_tag_b64() {
  local v
  v="$(_cc_tag "$1" "$2")" || return 1
  _cc_unb64 "$v"
}

# ── Shell quoting ────────────────────────────────────────────────────────────
# Single-quote a value for a command line tmux will hand to a shell. tmux runs a
# ONE-argument shell-command through $SHELL -c, so the tombstone command is
# assembled as one string with every path quoted — a space in $SHELL or in the
# store path cannot split it.
#
# Shared, not duplicated: cc_freeze.sh builds the tombstone command and
# cc_thaw.sh compares its own reconstruction against #{pane_start_command}, so
# any drift between two copies would make every tombstone look "busy" to its
# own thaw.
# The replacement is FOUR characters — quote, backslash, quote, quote — and in a
# double-quoted sed script that needs `\\\\`: the shell eats one layer and sed
# eats another. Written with one fewer backslash it emits `'''`, which does not
# round-trip through a shell at all ("unexpected EOF while looking for matching
# `''"), and cc_thaw.sh's byte-comparison against #{pane_start_command} would
# then read every tombstone as "the user is running something here". Verified by
# eval, both forms, before choosing this one.
_cc_shquote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

# ── Durable write (§2.4 / D3) ────────────────────────────────────────────────
# content on stdin → <dir>/tmp/<name>.<pid>.tmp → fsync → rename → sync.
# Nothing this feature kills for is killed before its record has been through
# this function AND been re-read off disk.
#
# NOT `dd if=$tmp of=$tmp conv=fsync` as the architecture sketches it: dd
# truncates its output file unless conv=notrunc, and with input and output the
# same path that yields a zero-byte "durable" state file — the exact data loss
# the protocol exists to prevent. Verified on this machine before choosing the
# two-file form. The extra file is written into the same tmp/ dir and removed.
_cc_atomic_write() {
  local dest="$1" dir base tmpdir raw tmp
  dir="$(dirname "$dest")"; base="$(basename "$dest")"
  tmpdir="$dir/tmp"
  mkdir -p "$tmpdir" 2>/dev/null || return 1
  raw="$tmpdir/$base.$$.raw"
  tmp="$tmpdir/$base.$$.tmp"
  cat > "$raw" || { rm -f "$raw"; return 1; }
  if ! dd if="$raw" of="$tmp" conv=fsync 2>/dev/null; then
    # No conv=fsync on this dd: copy and fall back to the filesystem-wide flush.
    cp "$raw" "$tmp" 2>/dev/null || { rm -f "$raw" "$tmp"; return 1; }
    sync
  fi
  rm -f "$raw"
  mv -f "$tmp" "$dest" || { rm -f "$tmp"; return 1; }
  # The directory-level flush. F_FULLFSYNC is unreachable from a POSIX shell;
  # the residual power-loss exposure is named in the architecture (F22).
  sync
  return 0
}

# Same protocol for a move that must be durable (archive, discard).
_cc_atomic_move() {
  mv -f "$1" "$2" 2>/dev/null || return 1
  sync
  return 0
}

# ── Locks (§2.4) ─────────────────────────────────────────────────────────────
# mkdir(2) mutex plus an owner file. FOUR TAB-separated fields, none ever empty:
#
#     <owner-pid> <TAB> <tmux-server-pid> <TAB> <epoch> <TAB> <start-token>
#
# THE LOCK ROOT IS NOT A LOCK. `cc_store_lock_root` is `<ns>/.lock`, and
# `cc_store_ns_dir` creates it with `mkdir -p` on EVERY store access — so that
# directory exists permanently, holds no owner file, and is created by a path
# that never goes through `_cc_lock_take`. It is the CONTAINER; the locks are
# its children. This was mistaken for a wedged, ownerless lock on the live
# machine (found "held and empty", twice, and rmdir'd by hand). Two consequences
# are encoded below: `_cc_lock_acquire` refuses a lock NAME that could resolve
# back to the root, and no reclaim may ever `rm -rf` the root itself — doing so
# would delete every live sibling lock in one call.
#
# ── HOW A DEAD OWNER IS TOLD FROM A LIVE ONE ─────────────────────────────────
# Not by age. An age-based timeout steals the lock from a worker that is simply
# slow, which is worse than the wedge it fixes. Identity is established in three
# escalating steps, and only a failure of one of them permits a reclaim:
#
#   1. kill -0 <owner-pid>          — is anything at all running under that pid?
#   2. start-token match            — is it the SAME process, or has the pid been
#                                     recycled? `ps -o lstart=` of the owner pid
#                                     is compared byte-for-byte with the token
#                                     recorded when the lock was taken. This is
#                                     what makes step 1 trustworthy: pids wrap,
#                                     and after a reboot a dead worker's pid is
#                                     very likely alive again as something else.
#   3. tmux server-pid match        — a lock stamped by a different tmux server
#                                     than the one we are talking to cannot be
#                                     protecting anything in this server's world.
#
# An owner that passes all three is CONFIRMED LIVE and is never stolen from, at
# any age below the CEILING. Everything else — missing owner file, non-numeric
# pid, dead pid, recycled pid, foreign server, unparseable epoch, an owner form
# we cannot confirm that is older than STALE, or any lock past the CEILING — is
# reclaimable.
#
# ── THE mkdir→owner-write WINDOW ─────────────────────────────────────────────
# mkdir(2) is the mutex; the owner file is written immediately after. A worker
# killed in between leaves a lock directory with no owner file. That window is
# closed from both ends:
#
#   * It is made vanishingly small. Every value the owner record needs — the
#     tmux server pid (a `tmux display-message` round trip, measured at 100-600
#     ms on this loaded machine), the clock, this process's start token — is
#     resolved BEFORE the mkdir. Between mkdir(2) and the printf there is now
#     exactly one shell builtin and no fork, no tmux call, and no `date`.
#   * A lock that IS ownerless is reclaimable once it is older than the orphan
#     grace, so even a kill landing inside that window cannot wedge the store.
#     Below the grace it is treated as BUSY, because a taker may legitimately be
#     mid-write — the grace is what keeps property "never steal from a live
#     worker" true for the one instant when the owner file does not yet exist.
#   * If the owner write FAILS (full disk, read-only store, anything), the lock
#     is released rather than held: an ownerless lock is never left behind on
#     purpose. The old code ignored the write's status entirely.

# The process identity token: `ps -o lstart=` for one pid. Single-pid `ps` is
# ~20 ms; the multi-pid form is the one that cost 7-12 s on this machine, and it
# is not used here. Whitespace is squeezed to '_' so the token can never contain
# a TAB and can never be empty — '-' is the floor (§2.1: never an empty field).
_cc_proc_start_token() {
  local t
  t="$(ps -o lstart= -p "$1" 2>/dev/null | tr -s '[:space:]' '_')"
  case "${t:-}" in ''|'_') printf '%s' '-' ;; *) printf '%s' "$t" ;; esac
}

# Memoised: a process's own start time cannot change, and the socket a running
# process is attached to cannot change either.
_cc_lock_self_token() {
  [ -n "${_CC_LOCK_SELF_TOKEN:-}" ] || _CC_LOCK_SELF_TOKEN="$(_cc_proc_start_token $$)"
  printf '%s' "$_CC_LOCK_SELF_TOKEN"
}
_cc_lock_server_pid() {
  [ -n "${_CC_LOCK_SERVER_PID:-}" ] || _CC_LOCK_SERVER_PID="$(_cc_server_pid)"
  printf '%s' "$_CC_LOCK_SERVER_PID"
}

# Real-clock mtime, in seconds. BSD stat first, GNU second. Used ONLY for the
# orphan grace, and deliberately against the REAL clock rather than _cc_now:
# an mtime is stamped by the kernel, so comparing it to a CC_NOW the test froze
# would produce a nonsense age.
_cc_mtime() {
  local m
  m="$(stat -f %m "$1" 2>/dev/null)"
  case "${m:-}" in ''|*[!0-9]*) m="$(stat -c %Y "$1" 2>/dev/null)" ;; esac
  case "${m:-}" in ''|*[!0-9]*) return 1 ;; esac
  printf '%s' "$m"
}

# Age of a path against the real clock. Unknowable ⇒ "older than the grace",
# which fails towards reclaimability for a directory that has no owner file.
_cc_path_age() {
  local m n a
  m="$(_cc_mtime "$1")" || { printf '%s' "$((CC_LOCK_ORPHAN_GRACE_SECS + 1))"; return 0; }
  n="$(date +%s 2>/dev/null)"
  case "${n:-}" in ''|*[!0-9]*) printf '%s' "$((CC_LOCK_ORPHAN_GRACE_SECS + 1))"; return 0 ;; esac
  a=$((n - m)); [ "$a" -lt 0 ] && a=0
  printf '%s' "$a"
}

# Args: <lock-dir> <server-pid> <epoch> <start-token>. Every value is supplied
# by the caller and already resolved: THIS FUNCTION MUST NOT FORK. It runs in
# the mkdir→owner window, and every fork added here widens the window that
# produces an ownerless lock.
_cc_lock_owner_write() {
  # The braces matter: `cmd > f 2>/dev/null` applies the redirections in order,
  # so a FAILING `> f` is reported to the still-original stderr — the old form
  # both swallowed the reason (its `2>/dev/null`) AND leaked the message into
  # whatever hook was running. Wrapping puts the whole redirection under it, and
  # the reason is recovered by the caller's LOCK-OWNER-WRITE-FAILED log line.
  { printf '%s\t%s\t%s\t%s\n' "$$" "${2:-0}" "${3:-0}" "${4:--}" > "$1/owner"; } 2>/dev/null || return 1
  [ -s "$1/owner" ] || return 1
  return 0
}

# Args: <lock-dir> <server-pid> <epoch> <start-token>. 0 = held, 1 = not held
# (and nothing left behind).
_cc_lock_take() {
  if ! _cc_lock_owner_write "$1" "$2" "$3" "$4"; then
    # Never hold what we cannot stamp. An unstamped lock is the wedge.
    rmdir "$1" 2>/dev/null || rm -rf "$1" 2>/dev/null
    _cc_log "LOCK-OWNER-WRITE-FAILED $1 — released rather than held ownerless"
    return 1
  fi
  _CC_LOCK_LAST="$1"
  # Only when the caller has no handler of its own. Installing one
  # unconditionally would silently REPLACE the caller's cleanup — cc_freeze.sh
  # and cc_thaw.sh both remove a work directory there, and both already release
  # locks from it. (Observed: a test's own kill-server trap was clobbered this
  # way and its tmux server outlived the run.)
  [ -n "$(trap -p EXIT)" ] || trap '_cc_lock_release_all' EXIT HUP INT TERM
  return 0
}

# Args: <lock-root> <name>. Returns 0 when held, 1 when BUSY (caller reports
# BUSY and exits 0 having done nothing — an occupied lock is not an error).
_cc_lock_acquire() {
  local root="$1" name="$2" dir now spid tok
  # A name that resolves back to the root, or out of it, would make the reclaim
  # below `rm -rf` the container of every sibling lock.
  case "${name:-}" in
    ''|.|..|*/*) _cc_log "LOCK-BAD-NAME '${name:-}' under '$root' — refusing"; return 1 ;;
  esac
  dir="$root/$name"
  _CC_LOCK_ROOT="$root"
  mkdir -p "$root" 2>/dev/null || return 1

  # EVERY fork this acquisition needs happens HERE, before the mutex exists.
  now="$(_cc_now)"
  spid="$(_cc_lock_server_pid)"
  tok="$(_cc_lock_self_token)"

  if mkdir "$dir" 2>/dev/null; then
    # ── the window ── (one builtin, no fork)
    if [ -n "${CC_LOCK_FAIL_AFTER:-}" ] && [ "$CC_LOCK_FAIL_AFTER" = "mkdir" ]; then
      # Test escape (§3): die exactly inside the window, the way a SIGKILLed
      # worker does, so the ownerless state under test is produced by the REAL
      # acquisition path and not by a hand-made directory.
      kill -9 $$
    fi
    _cc_lock_take "$dir" "$spid" "$now" "$tok" && return 0
    return 1
  fi
  _cc_lock_reclaim "$dir" "$name" "$now" "$spid" "$tok"
}

# The reclaim decision. Args: <dir> <name> <now> <server-pid> <start-token>.
# Returns 0 only if the lock was reclaimed AND retaken.
_cc_lock_reclaim() {
  local dir="$1" name="$2" now="$3" spid="$4" tok="$5"
  local owner="" opid ospid oepoch otok age why="" live_tok

  # `read < file` is a builtin: no `cat`, no fork, on a path that runs whenever
  # the lock is contended.
  [ -f "$dir/owner" ] && IFS= read -r owner < "$dir/owner" 2>/dev/null

  if [ -z "$owner" ]; then
    # Ownerless: the mkdir→owner-write window, or a directory that never went
    # through this protocol at all.
    age="$(_cc_path_age "$dir")"
    if [ "$age" -lt "$CC_LOCK_ORPHAN_GRACE_SECS" ]; then
      return 1    # BUSY: a taker may be mid-write this instant. Never steal it.
    fi
    why="no owner file after ${age}s (crashed inside the mkdir→owner window)"
  else
    IFS='	' read -r opid ospid oepoch otok <<EOF
$owner
EOF
    case "${oepoch:-}" in
      ''|*[!0-9]*) age=$((CC_LOCK_MAX_AGE_SECS + 1)) ;;
      *) age=$((now - oepoch)) ;;
    esac
    [ "$age" -lt 0 ] && age=0

    case "${opid:-}" in
      ''|*[!0-9]*)
        why="owner field '${opid:-}' is not a pid" ;;
      *)
        if ! kill -0 "$opid" 2>/dev/null; then
          why="owner pid $opid is dead"
        else
          case "${otok:-}" in
            ''|'-')
              # A legacy three-field owner file, or a `ps` that would not answer
              # when the lock was taken. Identity is UNCONFIRMABLE — and this is
              # the ONLY branch in which age decides anything.
              #
              # THE AGE RULES LIVE HERE AND NOWHERE ELSE. The stale window and
              # the ceiling both apply to a lock we cannot positively identify,
              # and neither applies to one we can: "a lock held by a LIVE worker
              # must never be stolen" is unconditional, and an age-based rule
              # layered on top of a confirmed-live owner is precisely the naive
              # timeout this rewrite exists to remove. (Caught by
              # tests/save_lock_mutex.sh [3] on the first run of this code: a
              # 24 h ceiling applied unconditionally stole a live holder's lock
              # the moment the clock was pushed forward.)
              if [ "$age" -gt "$CC_LOCK_MAX_AGE_SECS" ]; then
                why="unidentifiable owner pid $opid, ${age}s old — past the ${CC_LOCK_MAX_AGE_SECS}s ceiling"
              elif [ "$age" -gt "$CC_LOCK_STALE_SECS" ]; then
                why="owner pid $opid cannot be identified (no start token) and the lock is ${age}s old"
              fi ;;
            *)
              live_tok="$(_cc_proc_start_token "$opid")"
              [ "$live_tok" != "$otok" ] && \
                why="pid $opid was RECYCLED (starts '$live_tok', lock recorded '$otok')" ;;
          esac
          if [ -z "$why" ]; then
            case "${ospid:-}" in
              ''|*[!0-9]*|0) : ;;
              *) case "$spid" in
                   0|"$ospid") : ;;
                   *) why="lock belongs to tmux server $ospid, we are talking to $spid" ;;
                 esac ;;
            esac
          fi
        fi ;;
    esac
  fi

  # CONFIRMED LIVE. Not stolen, not logged as an anomaly: an occupied lock is
  # normal operation and the caller reports BUSY.
  [ -n "$why" ] || return 1

  # Belt and braces before an `rm -rf`: the root is the container of every
  # sibling lock and must never be removed as if it were one.
  case "$dir" in ''|*/) return 1 ;; esac
  [ "$dir" = "${_CC_LOCK_ROOT:-}" ] && return 1
  [ "$dir" = "$(dirname "$dir")" ] && return 1

  _cc_log "LOCK-RECLAIM $name: $why"
  rm -rf "$dir" 2>/dev/null
  if mkdir "$dir" 2>/dev/null; then
    _cc_lock_take "$dir" "$spid" "$now" "$tok" && return 0
  fi
  return 1
}

# Releases one lock by path (default: the most recently taken). Locks nest —
# the sweep holds `sweep` while each window freeze holds `freeze-<id>` — so the
# caller passes the path it took rather than trusting a single global.
_cc_lock_release() {
  local d="${1:-${_CC_LOCK_LAST:-}}"
  [ -n "$d" ] || return 0
  # The root is not a lock (see the section header): releasing it would delete
  # every sibling lock that is currently held.
  [ "$d" = "${_CC_LOCK_ROOT:-}" ] && return 0
  rm -rf "$d" 2>/dev/null
  [ "$d" = "${_CC_LOCK_LAST:-}" ] && _CC_LOCK_LAST=""
  return 0
}

# The EXIT/HUP/INT/TERM path: drop every lock this pid owns, identified by the
# owner file rather than by a remembered list of paths (a path may contain a
# space; a glob handles that and a word-split list would not).
_cc_lock_release_all() {
  local d
  [ -n "${_CC_LOCK_ROOT:-}" ] || return 0
  for d in "$_CC_LOCK_ROOT"/*; do
    [ -d "$d" ] || continue
    case "$(head -n 1 "$d/owner" 2>/dev/null)" in
      "$$	"*) rm -rf "$d" 2>/dev/null ;;
    esac
  done
  _CC_LOCK_LAST=""
  return 0
}

# ── Durations (§7) ───────────────────────────────────────────────────────────
# 2d, 36h, 90m, 45s, or bare seconds. Anything else fails, and every caller
# treats a failure as "no threshold" ⇒ no freeze (NFR1).
_cc_duration_secs() {
  local s="${1:-}" n u
  case "$s" in ''|*[!0-9dhms]*) return 1 ;; esac
  case "$s" in *[dhms]) n="${s%?}"; u="${s#"$n"}" ;; *) n="$s"; u="s" ;; esac
  case "$n" in ''|*[!0-9]*) return 1 ;; esac
  case "$u" in
    d) printf '%s' "$((n * 86400))" ;;
    h) printf '%s' "$((n * 3600))" ;;
    m) printf '%s' "$((n * 60))" ;;
    s) printf '%s' "$n" ;;
    *) return 1 ;;
  esac
}

# ── Server / namespace identity (§2.2) ───────────────────────────────────────
_cc_server_pid() {
  local p
  p="$($TMUX_CMD display-message -p '#{pid}' 2>/dev/null)"
  case "${p:-}" in ''|*[!0-9]*) printf '%s' '0' ;; *) printf '%s' "$p" ;; esac
}

_cc_socket_path() { $TMUX_CMD display-message -p '#{socket_path}' 2>/dev/null; }

# The store is namespaced by socket so a second tmux server — the user has had
# one by accident — cannot reach this one's entries even before the
# server_pid guard runs.
#
# Under moox the namespace is the gate's state key (MOOX_STATE_KEY: "default"
# for the login server, "<name>@<hash>" otherwise), so the shell and Go's
# Paths.FreezeDir name the same <root>/<key> (R2-C2). The key is trusted only
# when MOOX_SOCKET names the server this process talks to: inside tmux that is
# ${TMUX%%,*}; outside tmux (the daemon's heartbeat) TMUX_CMD was built from
# MOOX_SOCKET itself — and in both cases the server TMUX_CMD addresses.
#
# Without a trusted key the namespace is Go's sockets.Key rule, never the bare
# socket basename (G3-M2): "default" ONLY for the literal login default socket,
# every other socket "<base>@<hex8(sha256(canonical dir))>" — so `-S /x/default`
# or `-L default` under another TMUX_TMPDIR can never land in the real
# frozen/default store. With no sha256 tool the suffix is "@x": still never
# the real key, only less unique.
_cc_socket_ns() {
  local sp ns dir cdir h
  if [ -n "${MOOX_STATE_KEY:-}" ] && [ -n "${MOOX_SOCKET:-}" ]; then
    if { [ -z "${TMUX:-}" ] || [ "${TMUX%%,*}" = "$MOOX_SOCKET" ]; } \
       && _cc_target_sock && [ "$_CC_TARGET_SOCK" = "$MOOX_SOCKET" ]; then
      # Verbatim, as Go joins it (sockets.For): a socket basename never holds
      # '/', and '.'/'..' are refused rather than rewritten.
      case "$MOOX_STATE_KEY" in
        */*|.|..) ;;
        *) printf '%s' "$MOOX_STATE_KEY"; return 0 ;;
      esac
    fi
  fi
  sp="$(_cc_socket_path)"
  [ -n "$sp" ] || { printf '%s' 'unknown@x'; return 0; }
  if _cc_is_legacy_sock "$sp"; then printf '%s' default; return 0; fi
  dir="$(dirname "$sp")"
  cdir="$(cd "$dir" 2>/dev/null && pwd -P)" || cdir=""
  [ -n "$cdir" ] || cdir="$dir"
  if command -v shasum >/dev/null 2>&1; then
    h="$(printf '%s' "$cdir" | shasum -a 256 2>/dev/null | cut -c1-8)"
  elif command -v sha256sum >/dev/null 2>&1; then
    h="$(printf '%s' "$cdir" | sha256sum 2>/dev/null | cut -c1-8)"
  fi
  case "${h:-}" in [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;; *) h=x ;; esac
  ns="$(printf '%s' "${sp##*/}" | tr -c 'A-Za-z0-9._-' '_')"
  printf '%s@%s' "${ns:-sock}" "$h"
}

# Root of the store. No path component may contain the substring "claude":
# these paths appear in the tombstone pane's argv, and post_restore.sh:428
# skips any row whose command contains "claude" — which is what makes an OLD
# plugin ignore a tombstone instead of arming it (§2.2, internal C7).
_cc_store_root() {
  if [ -n "${CC_FREEZE_DIR:-}" ]; then printf '%s' "$CC_FREEZE_DIR"; return 0; fi
  _cc_opt @claude-continuity-freeze-dir "$HOME/.config/tmux-cc/frozen"
}

# ── Saves go through moox (C5, I10, B26; architecture §5.5) ──────────────────
# The engine owns saving: freeze and thaw ask THIS server's moox daemon for a
# save with `moox save --wait 30s` (moox finds the server from $TMUX). The
# binary is $MOOX_BIN (exported by the gate), else the server's @moox-bin, for
# panes started before the cutover. The freeze store was already written and is
# the source of truth for frozen windows, so a failed save NEVER aborts or rolls
# back a freeze or thaw; it is logged, and a hard failure is shown in the
# status line:
#   0              done (published, unchanged or pristine)
#   75             SAVE-DEFERRED: a restore or another save owns the server; the
#                  server stays dirty and the next debounced save catches up
#   5 4 1 124 …    SAVE-FAILED <code>, and @moox_status says so
# CC_SAVE_CMD overrides the whole call (tests). Args: <reason>
_cc_moox_save() {
  local reason="${1:-freeze}" bin rc=0
  if [ -n "${CC_SAVE_CMD:-}" ]; then
    sh -c "$CC_SAVE_CMD" >/dev/null 2>&1 || rc=$?
  else
    bin="${MOOX_BIN:-}"
    [ -n "$bin" ] || bin="$($TMUX_CMD show -gqv @moox-bin 2>/dev/null)"
    if [ -z "$bin" ] || [ ! -x "$bin/moox" ]; then
      _cc_log "SAVE-UNAVAILABLE reason=$reason: no moox binary (MOOX_BIN / @moox-bin)"
      return 0
    fi
    # -S names the server TMUX_CMD addresses (G3-M2): left to itself `moox
    # save` resolves $MOOX_TMUX_SOCKET > $TMUX > "default", so with $TMUX unset
    # (the daemon, an agent's explicit TMUX_CMD) it would save the user's
    # default server instead of this one.
    if ! _cc_target_sock; then
      _cc_log "SAVE-UNAVAILABLE reason=$reason: the server's socket path is unknown"
      return 0
    fi
    "$bin/moox" save -S "$_CC_TARGET_SOCK" --wait 30s --reason "$reason" >/dev/null 2>&1 || rc=$?
  fi
  case "$rc" in
    0)  _cc_log "SAVE-DONE reason=$reason" ;;
    75) _cc_log "SAVE-DEFERRED reason=$reason (restoring or busy; the next debounced save catches up)" ;;
    *)  _cc_log "SAVE-FAILED $rc reason=$reason"
        $TMUX_CMD set -g @moox_status "save failed ($rc, $reason)" >/dev/null 2>&1 || true ;;
  esac
  return 0
}

# ── Continuity directories (§4.2, R3-C2) ─────────────────────────────────────
# The by-pid registry is SHARED across servers (keyed by pid, X12). One rule:
# the option @claude-continuity-by-pid-dir if set (test servers only), else the
# one shared path. It is NEVER derived from the panes dir, which the gate makes
# per socket; `${panes_dir}/by-pid` would split the registry per server.
_cc_by_pid_dir() { _cc_opt @claude-continuity-by-pid-dir "$HOME/.config/tmux-claude/panes/by-pid"; }

# Position sidecars are per socket: the gate sets this option per server.
_cc_panes_dir() { _cc_opt @claude-continuity-panes-dir "$HOME/.config/tmux-claude/panes"; }

# The process start token shared with Go (procStartToken, PR-M16): the C-locale
# `ps -o lstart=` of ONE pid, every run of whitespace turned into a single '_',
# leading and trailing '_' removed; '-' when ps has no answer.
#   "Sat Oct 25 01:02:03 2026\n" -> "Sat_Oct_25_01:02:03_2026"
_cc_start_token() {
  local t
  t="$(LC_ALL=C ps -o lstart= -p "$1" 2>/dev/null | tr -s '[:space:]' '_')"
  t="${t#_}"; t="${t%_}"
  case "${t:-}" in '') printf '%s' '-' ;; *) printf '%s' "$t" ;; esac
}

# A string tmux will not format-expand, for titles and names the shell sets
# (select-pane -T, rename-window), so a thawed title comes back byte-identical
# (R2-L5). Same rule as Go's tmuxtarget.Literal: every run of '#' is doubled
# unless it immediately precedes '[' (tmux copies a `#…#[` run verbatim, as the
# start of a style).
_cc_lit() {
  case "$1" in *'#'*) ;; *) printf '%s' "$1"; return 0 ;; esac
  printf '%s' "$1" | awk '
    { s = $0; out = ""
      while (match(s, /#+/)) {
        run = substr(s, RSTART, RLENGTH); rest = substr(s, RSTART + RLENGTH)
        out = out substr(s, 1, RSTART - 1) run
        if (substr(rest, 1, 1) != "[") out = out run
        s = rest
      }
      printf "%s%s", (NR > 1 ? "\n" : ""), out s }'
}

# ── Test isolation as a runtime assertion (§3, NFR6) ─────────────────────────
# CC_TEST=1 means "I promise I am not touching the live world". This checks the
# promise instead of trusting it: a test that forgets -f /dev/null makes tmux
# source the user's .tmux.conf and continuum restores the real layout into the
# test server. That has already happened on this machine once.
# Any extra paths a caller has just resolved from tmux options are checked too:
# with `-f /dev/null` those options are UNSET, so they fall back to the live
# ~/.config/tmux-claude defaults and a test would delete the user's real pending
# and launch sidecars. Observed while building this feature; asserted since.
_cc_assert_isolation() {
  [ "${CC_TEST:-0}" = "1" ] || return 0
  local bad="" p
  case "${CC_FREEZE_DIR:-}" in
    '') bad="CC_FREEZE_DIR is unset" ;;
    "$HOME/.config/tmux-cc"*) bad="CC_FREEZE_DIR points inside the live store" ;;
  esac
  case " $TMUX_CMD " in *" -L "*) ;; *) bad="${bad:+$bad; }TMUX_CMD has no -L <socket>" ;; esac
  case "$TMUX_CMD" in *"-f /dev/null"*) ;; *) bad="${bad:+$bad; }TMUX_CMD has no -f /dev/null" ;; esac
  for p in "$@"; do
    case "$p" in
      "$HOME/.config/tmux-claude"*|"$HOME/.tmux/"*) bad="${bad:+$bad; }'$p' is a LIVE plugin directory" ;;
    esac
  done
  [ -z "$bad" ] && return 0
  printf 'cc: CC_TEST=1 isolation contract violated: %s\n' "$bad" >&2
  _cc_log "ISOLATION-VIOLATION $bad"
  exit 2
}
