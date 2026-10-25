#!/usr/bin/env bash
# cc_freeze.sh — freeze PANES (persist them, reclaim their process trees, and
# respawn each one IN PLACE as a tombstone) and sweep for idle candidates.
#
#   cc_freeze.sh freeze [--reason manual|auto] [--force] [--no-save] <target>...
#   cc_freeze.sh sweep  [--dry-run]
#     <target> ::= "%12"                 one pane
#                | "session:index.pane"  one pane
#                | "@37"                 every pane of that window
#                | "session:index"       every pane of that window
#                | "session:"            every pane of every window of it
#
# THE ATOM IS THE PANE. A window freeze is a loop over its panes and a session
# freeze is a loop over its windows; there is no separate window kill path and
# the window is never restructured — no pane is destroyed, no layout is captured
# or replayed, and `#{window_layout}`, the pane count and every pane id are
# unchanged by a freeze.
#
# This is the ONLY code path in this feature that may kill a process, and it may
# do it only against a set that was captured once, classified once, persisted
# once, re-read off disk once, and re-verified pid-by-pid immediately before the
# first signal. Every other path — save-time, restore-time, popup rendering —
# has a worst case of "log loudly and leave the pane awake".
#
# ── stdout is a contract ─────────────────────────────────────────────────────
# One ATOM line per pane acted on, six TAB-separated fields, none ever empty:
#
#   FROZE|ALREADY|REFUSED|FAILED|PARTIAL|BUSY|CHECK-OK <TAB> session:window.pane
#     <TAB> key-or-"-" <TAB> panes <TAB> sids <TAB> reason
#
# Plus one CONTAINER line whenever the target named a window or a session, so a
# caller can tell "all froze" from "some froze" without re-deriving it:
#
#   WINDOW  <TAB> session:window <TAB> ALL|PARTIAL|NONE
#     <TAB> frozen <TAB> refused <TAB> failed <TAB> busy <TAB> considered
#   SESSION <TAB> session        <TAB> ALL|PARTIAL|NONE  <TAB> … (same five counts)
#
# `frozen` counts FROZE + ALREADY + PARTIAL — panes that are frozen NOW. The
# verdict is ALL when frozen == considered, NONE when frozen == 0, else PARTIAL.
# Column 1 tells the line types apart; a container line is never an atom line.
#
# ONE WORD FOR ONE CONCEPT: **PARTIAL always means "the thing this line names is
# only partly done"** — on an atom line, the pane froze but not every process in
# it died; on a container line, some of its atoms froze and some did not; and it
# is the same word cc_popup.sh's tree and the design delta use for a container
# that is neither AWAKE nor FROZEN. It replaced `SOME` here, which was a second
# word for the container case and nothing else. The two levels never collide in
# parsing: column 1 identifies the line type before column 3 is read.
#
# ── Exit codes — the COMPLETE set (§3, §3.1.11) ──────────────────────────────
#   0  every atom in the invocation is frozen (FROZE/ALREADY), or there was
#      nothing to do, or the only non-successes were BUSY (an occupied lock is
#      not an error — §3.1.14)
#   1  usage / bad arguments
#   2  NOTHING froze and at least one atom FAILED (or ended PARTIAL)
#   3  NOTHING froze and the only refusals were safety rails — a decision
#   4  PARTIAL SUCCESS: at least one atom froze AND at least one did not
#
# 4 IS NOT A FAILURE, and a caller must not write `cc_freeze.sh … || die`. It
# says the operation half-happened: the panes that froze STAY frozen (a partial
# operation is never rolled back — rolling one back would thaw a pane the user
# asked to freeze), and the container line reports exactly which. It is distinct
# from 2 on purpose: "some froze" and "none froze" are different facts and a
# caller that has to re-parse stdout to tell them apart is a caller that will
# get it wrong. Read the WINDOW/SESSION line for the counts.
#
# `sweep` is exempt from all of the above: it exits 0 whenever it RAN, however
# many windows it declined to touch (§3.3), because it runs unattended from
# `run-shell -b` where a non-zero exit reads as a failed hook — and it is
# UNANIMOUS, so it never produces a partial outcome to report in the first
# place.

# shellcheck disable=SC2086
# $TMUX_CMD must word-split so tests can drive this against an isolated socket.

set -u

CURRENT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck source=./lib/cc_store.sh
. "$CURRENT_DIR/lib/cc_store.sh"     # pulls cc_relaunch → cc_proc → cc_common

_cc_assert_isolation

_CC_SWEEP_CAP=5          # a bug can never freeze 45 windows at once
_CC_HARD_RC=0            # usage / infrastructure failures, not per-atom verdicts
_CC_WORK=""
_CC_CHECK_ONLY=0         # 1 = evaluate every rail, then stop before persisting

# Per-atom outcome counters. Every atom line goes through _cc_emit, so the
# container summaries and the exit code cannot disagree with stdout.
_CC_N_OK=0; _CC_N_REFUSED=0; _CC_N_FAILED=0; _CC_N_BUSY=0
_CC_LAST_N=0             # panes considered by the last container loop
# Check-only accumulators (the sweep's dry run reads these instead of re-parsing
# its own stdout). _CC_C_ALREADY is deliberately NEITHER ok nor bad: a pane that
# is already frozen is not a candidate and not a refusal — see _cc_emit.
_CC_C_OK=0; _CC_C_BAD=0; _CC_C_REASON=""; _CC_C_SIDS=0; _CC_C_RSS=0; _CC_C_ALREADY=0

_cc_usage() {
  printf 'usage: cc_freeze.sh freeze [--reason manual|auto] [--force] [--no-save] <target>...\n' >&2
  printf '       cc_freeze.sh sweep [--dry-run]\n' >&2
  printf '       cc_freeze.sh save                  (serialised moox save)\n' >&2
  printf '       cc_freeze.sh reclaim <key> <pane_id> <window_id>   (moox restore, step 9)\n' >&2
  printf '       cc_freeze.sh ledger-seed           (moox restore, step 10)\n' >&2
  printf '       cc_freeze.sh heartbeat             (moox daemon, after each published save)\n' >&2
  printf '       <target> ::= %%12 | session:index.pane | @37 | session:index | session:\n' >&2
}

_cc_cleanup() {
  [ -n "${_CC_WORK:-}" ] && rm -rf "$_CC_WORK" 2>/dev/null
  _cc_lock_release_all
}
trap _cc_cleanup EXIT HUP INT TERM

# Every field is non-empty: an omitted reason is "-", never "" (§2.1).
# This is also the ONLY place an atom verdict is counted.
_cc_emit() {
  local verb="$1" reason="${6:--}" sids="${5:-0}"
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$verb" "${2:--}" "${3:--}" "${4:-0}" "$sids" "$reason"
  if [ "$_CC_CHECK_ONLY" = "1" ]; then
    case "$verb" in
      CHECK-OK)
        _CC_C_OK=$((_CC_C_OK + 1))
        case "$sids" in ''|*[!0-9]*) ;; *) _CC_C_SIDS=$((_CC_C_SIDS + sids)) ;; esac
        case "$reason" in ''|*[!0-9]*) ;; *) _CC_C_RSS=$((_CC_C_RSS + reason)) ;; esac ;;
      # ALREADY is a THIRD outcome, not a refusal. Under the pane atom a window
      # may hold one frozen pane and two awake ones, and the awake two are
      # perfectly good candidates; counting the frozen one as BAD made the whole
      # window read "already-frozen" and the sweep never offered them. The
      # already-frozen pane is simply not part of the operation: it is neither a
      # candidate (_CC_C_OK) nor an objection (_CC_C_BAD), and it contributes
      # neither sids nor rss to the preview because nothing of it would be
      # reclaimed.
      ALREADY)
        _CC_C_ALREADY=$((_CC_C_ALREADY + 1)) ;;
      *)
        _CC_C_BAD=$((_CC_C_BAD + 1))
        [ -n "$_CC_C_REASON" ] || _CC_C_REASON="$reason" ;;
    esac
    return 0
  fi
  case "$verb" in
    FROZE|ALREADY) _CC_N_OK=$((_CC_N_OK + 1)) ;;
    PARTIAL)       _CC_N_OK=$((_CC_N_OK + 1)); _CC_N_FAILED=$((_CC_N_FAILED + 1)) ;;
    REFUSED)       _CC_N_REFUSED=$((_CC_N_REFUSED + 1)) ;;
    FAILED)        _CC_N_FAILED=$((_CC_N_FAILED + 1)) ;;
    BUSY)          _CC_N_BUSY=$((_CC_N_BUSY + 1)) ;;
  esac
  return 0
}

# The documented exit contract, computed once, from the counters.
_cc_exit_code() {
  [ "$_CC_HARD_RC" != "0" ] && { printf '%s' "$_CC_HARD_RC"; return 0; }
  if [ "$_CC_N_FAILED" -gt 0 ] || [ "$_CC_N_REFUSED" -gt 0 ]; then
    # PARTIAL counts in BOTH ok and failed, so a lone PARTIAL exits 2 (state is
    # durable, a pid survived) rather than claiming the mixed-outcome code.
    if [ "$_CC_N_OK" -gt "$_CC_N_FAILED" ] || \
       { [ "$_CC_N_OK" -gt 0 ] && [ "$_CC_N_REFUSED" -gt 0 ]; }; then printf '4'; return 0; fi
    [ "$_CC_N_FAILED" -gt 0 ] && { printf '2'; return 0; }
    printf '3'; return 0
  fi
  printf '0'
}

# ── Participation (G3-H1) ─────────────────────────────────────────────────────
# Every verb below changes a server or its continuity state: freeze and sweep
# kill processes and respawn panes, reclaim/ledger-seed/heartbeat/save mutate
# the store. Only a server that takes part may be touched — the same gate the
# hooks and cc_thaw.sh apply (_cc_participating: MOOX_SOCKET == the server this
# runs in, MOOX_ACTIVE=1, the marker; legacy only on the real login default),
# plus the daemon's own MOOX_* contract outside tmux. On any other server this
# is a no-op that exits 0 and reads, writes and deletes nothing. Usage and help
# still answer everywhere.
case "${1:-}" in
  freeze|sweep|reclaim|ledger-seed|heartbeat|save)
    if ! _cc_script_participating; then
      [ -t 2 ] && printf 'cc_freeze: this tmux server does not take part in moox continuity; nothing done\n' >&2
      exit 0
    fi ;;
esac

# The pane-number slots, for THIS server only (_cc_cont_dir): never the login
# default server's ~/.config/tmux-claude/<kind> unless this IS that server. A
# dir that cannot be resolved is empty, and every use below is skipped.
pending_dir="$(_cc_cont_dir pending)"
launch_dir="$(_cc_cont_dir launch)"
panes_dir="$(_cc_cont_dir panes)"
# Shared registry, never derived from the per-socket panes dir (R3-C2).
by_pid_dir="$(_cc_by_pid_dir)"
# Under CC_TEST these must be isolated too: a `-f /dev/null` server has no
# @claude-continuity-* options, so they silently default to the LIVE sidecar
# directories and this script deletes pane-keyed files in them.
_cc_assert_isolation "$pending_dir" "$launch_dir" "$panes_dir" "$by_pid_dir"

# ── Targets ──────────────────────────────────────────────────────────────────
# A pane is resolved to a %pane_id on THIS server. No other identity exists: a
# state file is inert until a live pane carries its key (D1), so there is no
# matcher here to mis-resolve anything onto a neighbour.
#
# The level is read off the SYNTAX, not guessed from what happens to resolve:
# "work:2" is a window even when the window has one pane, so it still reports a
# WINDOW summary, and "work:2.1" is a pane even though it names the same thing.
_cc_target_level() {
  local rest
  case "$1" in
    %*) printf 'pane' ;;
    @*) printf 'window' ;;
    *:) printf 'session' ;;
    *:*) rest="${1##*:}"
         case "$rest" in *.*) printf 'pane' ;; *) printf 'window' ;; esac ;;
    *) printf 'unknown' ;;
  esac
}

# session_name is LAST in every -F below because it is the field most likely to
# contain something exotic; a tab inside it can then only truncate itself.
_cc_all_panes() {
  $TMUX_CMD list-panes -a -F \
    '#{pane_id}	#{pane_index}	#{pane_pid}	#{window_id}	#{window_index}	#{window_activity}	#{session_name}' \
    2>/dev/null
}

_cc_resolve_panes() {
  local t="$1" all
  all="$(_cc_all_panes)"
  case "$t" in
    %*)  printf '%s\n' "$all" | awk -F'\t' -v p="$t" '$1 == p { print $1 }' ;;
    @*)  printf '%s\n' "$all" | awk -F'\t' -v w="$t" '$4 == w { print $1 }' ;;
    *:)  printf '%s\n' "$all" | awk -F'\t' -v s="${t%:}" '$7 == s { print $1 }' ;;
    *:*.*) printf '%s\n' "$all" | awk -F'\t' -v s="${t%%:*}" -v i="${t##*:}" \
             'BEGIN { split(i, a, ".") } $7 == s && $5 == a[1] && $2 == a[2] { print $1 }' ;;
    *:*) printf '%s\n' "$all" | awk -F'\t' -v s="${t%%:*}" -v i="${t##*:}" \
             '$7 == s && $5 == i { print $1 }' ;;
    *) return 1 ;;
  esac
}

_cc_resolve_windows() {
  local t="$1" all
  all="$($TMUX_CMD list-windows -a -F '#{window_id}	#{window_index}	#{session_name}' 2>/dev/null)"
  case "$t" in
    @*)  printf '%s\n' "$all" | awk -F'\t' -v w="$t" '$1 == w { print $1 }' ;;
    *:)  printf '%s\n' "$all" | awk -F'\t' -v s="${t%:}" '$3 == s { print $1 }' ;;
    *:*) printf '%s\n' "$all" | awk -F'\t' -v s="${t%%:*}" -v i="${t##*:}" '$3 == s && $2 == i { print $1 }' ;;
    *) return 1 ;;
  esac
}

# Everything the pane atom needs, in two calls: one structured (safe to split on
# tabs) and one per free-text field (a path or a title may contain anything).
_cc_pane_info() {
  local line
  line="$(_cc_all_panes | awk -F'\t' -v p="$1" '$1 == p { print; exit }')"
  [ -n "$line" ] || return 1
  IFS='	' read -r P_ID P_IDX P_PID W_ID W_IDX W_ACT W_SESS <<EOF
$line
EOF
  W_NAME="$($TMUX_CMD display-message -p -t "$W_ID" '#{window_name}' 2>/dev/null)"
  P_CWD="$($TMUX_CMD display-message -p -t "$P_ID" '#{pane_current_path}' 2>/dev/null)"
  P_TITLE="$($TMUX_CMD display-message -p -t "$P_ID" '#{pane_title}' 2>/dev/null)"
  P_TARGET="$W_SESS:$W_IDX.$P_IDX"
  return 0
}

# _cc_tombstone_shell lives in lib/cc_proc.sh and _cc_shquote in lib/cc_common.sh:
# cc_thaw.sh must rebuild a BYTE-IDENTICAL tombstone command, and two copies of
# either would drift.

_cc_scrape_sid() {
  awk -v c="$1" 'BEGIN {
    n = split(c, t, " ")
    for (i = 1; i < n; i++) {
      w = t[i]; sub(/=.*$/, "", w)
      if (w == "--resume" || w == "-r" || w == "--session-id") {
        u = t[i + 1]; sub(/^.*=/, "", u)
        if (length(u) == 36 && u ~ /^[0-9a-fA-F-]+$/) { print u; exit }
      }
    }
  }'
}

# sid<TAB><pane_index><TAB>;CLAUDE_SID=…  — ;CLASS= is last and mandatory, so no
# record can end in an omissible field (§2.1).
_cc_emit_sid() {
  local idx="$1" sid="$2" role="$3" pid="$4" replay="$5" model="$6" class="$7"
  printf 'sid\t%s\t;CLAUDE_SID=%s\t;ROLE=%s\t;PID=%s' "$idx" "$sid" "$role" "$pid"
  [ -n "$replay" ] && printf '\t;REPLAY=%s' "$(_cc_b64 "$replay")"
  [ -n "$model" ]  && printf '\t;MODEL=%s' "$(_cc_b64 "$model")"
  printf '\t;CLASS=%s\n' "$class"
}

# ── The gate's non-vacuity rail (H1a), shared by BOTH kill paths ─────────────
# Args: <capture file> <claude count> <sid count> <target> <fresh|resume>
# Prints the refusal itself and returns 1, so no caller can "check" it and then
# carry on: the only thing a caller may do with a 1 is stop.
#
# The witness is derived from the captured ARGVs' exec tokens, never from the
# class column the counter reads. Two independent sources are the whole point —
# a single classification miss must not be able to zero the numerator, the
# denominator and the guard together.
_cc_gate_nonvacuous() {
  local cap="$1" n="$2" sids="$3" tgt="$4" which="$5" witness
  case "${n:-}" in ''|*[!0-9]*) n=0 ;; esac
  case "${sids:-}" in ''|*[!0-9]*) sids=0 ;; esac
  witness="$(cc_proc_claude_exec_pids "$cap" | tr '\n' ' ')"
  case "$witness" in
    *[0-9]*) ;;
    *) return 0 ;;   # no claude-family argv in the capture at all: 0 is a fact
  esac
  [ "$n" -gt 0 ] && [ "$sids" -gt 0 ] && return 0
  _cc_emit REFUSED "$tgt" - 1 "$sids" no-sid-for-live-claude
  _cc_log "REFUSE $tgt no-sid-for-live-claude ($which): the exec-token witness sees claude-family pids [$witness], but claude_procs=$n sid_count=$sids — captured classes: $(awk -F'\t' '{ print $3 "(" $5 ")" }' "$cap" 2>/dev/null | tr '\n' ' ')"
  return 1
}

# ── freeze: the PANE atom ────────────────────────────────────────────────────
# The lock is per PANE, and cc_thaw.sh takes the same name, so two operations on
# one pane serialise while the other panes of the window are untouched.
_cc_freeze_pane() {
  local pane="$1" mylock
  if ! _cc_pane_info "$pane"; then
    _cc_emit FAILED "$pane" - 0 0 target-unresolvable
    return 0
  fi
  # A held lock is not an error: BUSY, exit 0, nothing done (§3.1.14).
  if ! _cc_lock_acquire "$(cc_store_lock_root)" "freeze-p${pane#%}"; then
    _cc_emit BUSY "$P_TARGET" - 0 0 lock-held
    return 0
  fi
  mylock="$_CC_LOCK_LAST"
  __cc_freeze_pane_locked
  _cc_lock_release "$mylock"
}

__cc_freeze_pane_locked() {
  local claim wclaim tkey state key reuse="" resuming=0
  local work psf capf panef rootf allf smf panelines sidlines proclines livef rootlive sidpidf
  local now srv ns idle rss primary_cwd spawn_cwd pane_ct sid_ct claude_procs
  local audit offender reason dup_owner used_pids survivors banner title tsh tcmd
  local c_idx c_depth c_pid c_ppid c_class c_cmd rec tagv sid cpid clpid pcmd class ppid_rec pid_rec typed
  local launcher_argv replay model launcher_read cp ssid sowner sidx arch sidcsv livepid roe

  claim="$($TMUX_CMD show-option -pqv -t "$P_ID" @cc-frozen 2>/dev/null)"
  wclaim="$($TMUX_CMD show-option -wqv -t "$W_ID" @cc-frozen 2>/dev/null)"

  # ── Idempotency, corroborated rather than assumed (§3.1.1) ─────────────────
  # ALREADY requires all THREE: the claim, a state file that verifies, and this
  # pane's title carrying that key. Every other combination is a half-state and
  # is handled as such.
  if [ -n "$claim" ]; then
    state="$(cc_store_path "$claim")"
    if cc_store_verify "$state"; then
      case "$P_TITLE" in
        "❄ FROZEN $claim "*)
          _cc_emit ALREADY "$P_TARGET" "$claim" \
            "$(cc_store_scalar "$state" pane_count)" "$(cc_store_scalar "$state" sid_count)" already-frozen
          return 0 ;;
      esac
      # Durable state, pane not a tombstone: a crash after persist, before the
      # kill (F6/α). Resume under the SAME key. The state file records geometry
      # and sids, not the process set, so "resume from the kill step" is
      # implemented as a fresh capture + gate + re-persist under that key —
      # which keeps the invariant that matters: nothing is signalled that is not
      # already on disk and re-verified.
      reuse="$claim"
      _cc_log "FREEZE-RESUME $P_TARGET key=$claim (state verified, pane is not a tombstone)"
    else
      # A tombstone with no readable state is never "close enough": kill
      # nothing, change nothing, and say so (ext #11).
      _cc_emit FAILED "$P_TARGET" "$claim" 0 0 claimed-without-verified-state
      return 0
    fi
  else
    case "$P_TITLE" in
      '❄ FROZEN '*)
        tkey="$(printf '%s' "$P_TITLE" | awk '{ print $3 }')"
        state="$(cc_store_path "$tkey")"
        if [ -n "$tkey" ] && cc_store_verify "$state"; then
          if [ "$(cc_store_unit "$state")" = "pane" ]; then
            # A pane entry whose claim rode a WINDOW option across a reboot:
            # post_restore.sh re-claims by title at window level. Converge the
            # claim onto the pane it actually describes — a repair, not a
            # rewrite: no file is touched and nothing is destroyed.
            $TMUX_CMD set-option -p -t "$P_ID" @cc-frozen "$tkey" 2>/dev/null
            _cc_log "MIGRATED-CLAIM $P_TARGET key=$tkey: window option → pane option"
            _cc_emit ALREADY "$P_TARGET" "$tkey" \
              "$(cc_store_scalar "$state" pane_count)" "$(cc_store_scalar "$state" sid_count)" already-frozen
          else
            _cc_emit ALREADY "$P_TARGET" "$tkey" \
              "$(cc_store_scalar "$state" pane_count)" "$(cc_store_scalar "$state" sid_count)" legacy-window-entry
          fi
          return 0
        fi
        _cc_emit FAILED "$P_TARGET" - 0 0 tombstone-without-state
        return 0 ;;
    esac
    # A LEGACY window entry claims every pane of its window, even one whose ❄
    # title was lost. Freezing "into" it would write a second entry over a
    # window the old model still owns.
    if [ -n "$wclaim" ]; then
      state="$(cc_store_path "$wclaim")"
      if cc_store_verify "$state" && [ "$(cc_store_unit "$state")" = "window" ]; then
        _cc_emit ALREADY "$P_TARGET" "$wclaim" \
          "$(cc_store_scalar "$state" pane_count)" "$(cc_store_scalar "$state" sid_count)" legacy-window-entry
        return 0
      fi
    fi
  fi

  # Pins stay a WINDOW gesture (that is what the popup pins); a pinned window
  # refuses every one of its panes.
  if cc_pin_is "$W_ID"; then
    _cc_emit REFUSED "$P_TARGET" - 0 0 pinned
    return 0
  fi

  # ── Capture — one ps, this pane's tree, one climb ──────────────────────────
  work="$_CC_WORK/p${P_ID#%}"
  rm -rf "$work" 2>/dev/null
  mkdir -p "$work" || { _cc_emit FAILED "$P_TARGET" - 0 0 workdir-failed; return 0; }
  psf="$work/ps"; capf="$work/cap"; panef="$work/panes"; rootf="$work/roots"
  allf="$work/allpanes"; smf="$work/sidmap"; panelines="$work/panelines"; sidlines="$work/sidlines"

  if ! cc_proc_ps_snapshot "$psf"; then
    _cc_emit FAILED "$P_TARGET" - 0 0 ps-failed
    return 0
  fi
  # The pane table is ONE row: the atom's own descendant tree is the only tree
  # this operation may touch. Its siblings are not captured, not classified, not
  # signalled and not respawned.
  printf '%s\t%s\t%s\t%s\t%s\n' "$P_IDX" "$P_ID" "$P_PID" "$P_CWD" "$P_TITLE" > "$panef"

  # ── Resume from the kill step, on the PERSISTED capture (§3.1.1, F6) ───────
  # A verified state file for a pane that is not a tombstone means a crash
  # between the durable write and the first signal. The recovery is NOT a fresh
  # capture: the whole guarantee of §3.1.7 is that the set which gets signalled
  # is the set that passed the gate, re-verified pid-by-pid against the world as
  # it is NOW. Re-capturing would silently adopt whatever appeared in between —
  # which is exactly the divergence stale-capture exists to refuse — so the
  # process set is read back out of the state file and K0 runs against that.
  if [ -n "$reuse" ] && cc_store_capture_file "$(cc_store_path "$reuse")" "$capf" && [ -s "$capf" ]; then
    resuming=1
    key="$reuse"
    state="$(cc_store_path "$key")"
    banner="$(cc_store_banner_path "$key")"
    awk -F'\t' '$2 == 0 { print $1 "\t-\t" $3 }' "$capf" > "$rootf"
    pane_ct="$(cc_store_scalar "$state" pane_count)"
    sid_ct="$(cc_store_scalar "$state" sid_count)"
    now="$(cc_store_scalar "$state" frozen_at)"
    primary_cwd="$(_cc_unb64 "$(cc_store_scalar "$state" primary_cwd)")"
    _cc_log "FREEZE-RESUME $P_TARGET key=$key: re-verifying the persisted capture ($(awk 'END { print NR + 0 }' "$capf") pids), not re-capturing"
  fi

  if [ "$resuming" = "0" ]; then
  awk -F'\t' 'BEGIN { OFS = "\t" } { print $1, $2, $3 }' "$panef" > "$rootf"
  cc_proc_capture "$psf" "$rootf" > "$capf"

  # The climb runs over EVERY pane on the server, not just this one: a session
  # id that another live pane already owns must be visible as a duplicate here,
  # or freezing this pane would let a later thaw put a second Claude on a
  # transcript a live pane is still using (§3.1.6).
  $TMUX_CMD list-panes -a -F '#{pane_pid}	#S:#I.#P	#{pane_id}' 2>/dev/null > "$allf"
  set -- "$by_pid_dir"/*.session-id
  [ -f "$1" ] || set --
  cc_proc_sidmap "$allf" "$psf" "$@" > "$smf"

  : > "$panelines"; : > "$sidlines"
  used_pids=" "; dup_owner=""; primary_cwd=""; class=shell

  rec="$(awk -F'\t' -v t=";TARGET=$P_TARGET" '$1 == t { print; exit }' "$smf")"
  sid=""; cpid=""; clpid="-"
  if [ -n "$rec" ]; then
    # Assign only when the tag is PRESENT: `x="$(f)" || x=""` would clear a dup,
    # and a lost dup is a recorded sid that a live pane still owns.
    tagv="$(_cc_tag "$rec" ';DUP=')" && dup_owner="$tagv"
    sid="$(_cc_tag "$rec" ';SID=')" || sid=""
    cpid="$(_cc_tag "$rec" ';PID=')" || cpid=""
    clpid="$(_cc_tag "$rec" ';CLPID=')" || clpid="-"
  fi

  class=shell
  awk -F'\t' -v i="$P_IDX" '$1 == i && $5 == "CLAUDE"   { f = 1 } END { exit !f }' "$capf" && class=claude
  awk -F'\t' -v i="$P_IDX" '$1 == i && $5 == "CLAUDISH" { f = 1 } END { exit !f }' "$capf" && class=claudish

  pcmd="$(cc_proc_pane_cmd "$P_IDX" "$capf")"
  [ -n "$pcmd" ] || pcmd="$(cc_proc_pane_root_cmd "$P_IDX" "$capf")"
  if [ -n "$cpid" ]; then pid_rec="$cpid"; else pid_rec="$P_PID"; fi
  ppid_rec="$(cc_proc_ppid_of "$pid_rec" "$capf")"
  case "${ppid_rec:-}" in ''|*[!0-9]*) ppid_rec=0 ;; esac

  typed=""
  [ -n "$launch_dir" ] && [ -f "${launch_dir}/${P_ID#%}" ] && typed="$(_cc_b64 < "${launch_dir}/${P_ID#%}")"

  { printf 'pane\t%s\t;CWD=%s\t;TITLE=%s\t;CMD=%s\t;PID=%s\t;PPID=%s' \
      "$P_IDX" "$(_cc_b64 "$P_CWD")" "$(_cc_b64 "$P_TITLE")" "$(_cc_b64 "$pcmd")" "$pid_rec" "$ppid_rec"
    [ -n "$typed" ] && printf '\t;TYPED=%s' "$typed"
    printf '\t;CLASS=%s\n' "$class"
  } >> "$panelines"

  primary_cwd="$P_CWD"

  if [ -n "$sid" ]; then
    replay=""; model=""; launcher_read=1
    if [ "$clpid" != "-" ] && [ -n "$clpid" ]; then
      launcher_argv="$(cc_proc_cmd_of "$clpid" "$capf")"
      if [ -n "$launcher_argv" ]; then
        replay="$(_cc_claudish_replay "$launcher_argv" "$cpid")"
        model="$(_cc_claudish_model "$cpid")"
      else
        launcher_read=0
      fi
    fi
    # An empty replay is indistinguishable downstream from "not a claudish
    # pane", and relaunching a claudish pane as a bare `claude --resume`
    # replays the transcript against the real Anthropic API — wrong account,
    # wrong model. Refuse rather than record something that cannot be thawed
    # correctly.
    if [ "$class" = "claudish" ] && { [ "$launcher_read" = "0" ] || [ -z "$replay" ]; }; then
      _cc_emit REFUSED "$P_TARGET" - 1 0 no-replay-for-claudish
      _cc_log "REFUSE $P_TARGET no-replay-for-claudish clpid=$clpid"
      return 0
    fi
    _cc_emit_sid "$P_IDX" "$sid" primary "$cpid" "$replay" "$model" "$class" >> "$sidlines"
    used_pids="$used_pids$cpid "
  fi

  # ── Secondary sessions: a pane hosting more than one Claude ────────────────
  # The climb elects the SHALLOWEST session per pane and discards the rest, so a
  # second Claude in one pane would otherwise be killed with no record. It is
  # recovered here from its own by-pid sidecar or its own argv, recorded with
  # ;ROLE=secondary, counted by the gate, and never auto-resumed (§2.3).
  for cp in $(cc_proc_claude_pids "$capf"); do
    case "$used_pids" in *" $cp "*) continue ;; esac
    ssid=""
    [ -f "${by_pid_dir}/${cp}.session-id" ] && ssid="$(head -n 1 "${by_pid_dir}/${cp}.session-id" 2>/dev/null)"
    [ -n "$ssid" ] || ssid="$(_cc_scrape_sid "$(cc_proc_cmd_of "$cp" "$capf")")"
    case "${ssid:-}" in
      '') continue ;;
      *) _cc_is_safe_token "$ssid" || continue ;;
    esac
    [ "${#ssid}" -eq 36 ] || continue
    # Already ours, or already someone else's live pane's: either way it is not
    # attributable to this pane and the gate must fail rather than record it.
    if grep -q ";CLAUDE_SID=$ssid	" "$sidlines" 2>/dev/null; then continue; fi
    sowner="$(awk -F'\t' -v s=";SID=$ssid" '{ for (i = 2; i <= NF; i++) if ($i == s) { print $1; exit } }' "$smf")"
    if [ -n "$sowner" ]; then
      dup_owner="$ssid@${sowner#;TARGET=}"
      continue
    fi
    sidx="$(awk -F'\t' -v p="$cp" '$3 == p { print $1; exit }' "$capf")"
    replay=""; model=""
    clpid="$(cc_proc_claudish_ancestor "$cp" "$capf")"
    if [ -n "$clpid" ]; then
      launcher_argv="$(cc_proc_cmd_of "$clpid" "$capf")"
      [ -n "$launcher_argv" ] && replay="$(_cc_claudish_replay "$launcher_argv" "$cp")"
      model="$(_cc_claudish_model "$cp")"
    fi
    _cc_emit_sid "$sidx" "$ssid" secondary "$cp" "$replay" "$model" \
      "$([ -n "$clpid" ] && printf 'claudish' || printf 'claude')" >> "$sidlines"
    used_pids="$used_pids$cp "
  done

  pane_ct=1
  sid_ct="$(awk 'END { print NR + 0 }' "$sidlines")"
  claude_procs="$(cc_proc_claude_count "$capf")"

  # ── GATE 1: every live Claude must map to a recorded session id ────────────
  # Per PROCESS, not per pane, and --force does not override it, ever. This is
  # the rail that converts "a silent, undetectable loss" into "an action that
  # does not happen" (L3/H1a). Ambiguity refuses THIS PANE and only this pane.
  if [ -n "$dup_owner" ]; then
    _cc_emit REFUSED "$P_TARGET" - "$pane_ct" "$sid_ct" "no-sid-for-live-claude:dup=${dup_owner#*@}"
    _cc_log "REFUSE $P_TARGET no-sid-for-live-claude dup=$dup_owner"
    return 0
  fi
  if [ "$sid_ct" != "$claude_procs" ]; then
    _cc_emit REFUSED "$P_TARGET" - "$pane_ct" "$sid_ct" no-sid-for-live-claude
    _cc_log "REFUSE $P_TARGET no-sid-for-live-claude sids=$sid_ct claude_procs=$claude_procs pids=$(cc_proc_claude_pids "$capf" | tr '\n' ',')"
    return 0
  fi
  # ── GATE 1b: the count must be POSITIVE, not merely equal (H1a) ────────────
  # `0 == 0` is not a passing state, it is an unanswered question. The witness
  # comes from a genuinely different source than the counter — the EXEC TOKENS
  # of the captured argvs (cc_proc_claude_exec_pids) rather than the class
  # column cc_proc_claude_pids reads — so one classification miss can no longer
  # zero the numerator, the denominator AND the guard at the same time, which is
  # exactly how a live `claude --mcp-config …/mcp-servers.json` was pruned into
  # invisibility and killed with sid_count 0.
  #
  # Asserted, not inferred: if ANY claude-family process was captured, both the
  # count and the recorded sids must be positive, whatever the arithmetic above
  # concluded. False positives here (a wrapper argv that mentions claude in exec
  # position) cost a REFUSAL — the safe direction, NFR1 — and never a kill.
  if ! _cc_gate_nonvacuous "$capf" "$claude_procs" "$sid_ct" "$P_TARGET" fresh; then
    return 0
  fi

  # ── GATE 2: nothing unsafe in this pane's descendant set ───────────────────
  audit="$(cc_proc_audit "$capf")"
  if [ -n "$audit" ]; then
    printf '%s\n' "$audit" | while IFS='	' read -r reason offender cmd; do
      _cc_log "UNSAFE $P_TARGET $reason pid=$offender cmd=$cmd"
    done
    if [ "$FORCE" != "1" ]; then
      reason="$(printf '%s\n' "$audit" | head -1 | cut -f1)"
      _cc_emit REFUSED "$P_TARGET" - "$pane_ct" "$sid_ct" "$reason"
      return 0
    fi
    _cc_log "FORCED $P_TARGET overriding $(printf '%s\n' "$audit" | awk 'END { print NR }') unsafe process(es)"
  fi

  # ── The dry run stops HERE, having evaluated every rail ────────────────────
  # A dry run that disagrees with the real run is worse than no dry run
  # (FR3.5): the whole point is that the user can trust the preview before
  # anything is killed. So the preview walks the same capture, the same sid
  # gate and the same audit, and only then declines to act.
  if [ "$_CC_CHECK_ONLY" = "1" ]; then
    _cc_emit CHECK-OK "$P_TARGET" - "$pane_ct" "$sid_ct" \
      "$(cc_proc_rss_sum "$(awk -F'\t' '{ print $3 }' "$capf" | tr '\n' ' ')")"
    return 0
  fi

  if [ "${CC_FAIL_AFTER:-}" = "capture" ]; then
    _cc_emit FAILED "$P_TARGET" - "$pane_ct" "$sid_ct" fail-injected-capture
    return 0
  fi

  # ── Persist ────────────────────────────────────────────────────────────────
  now="$(_cc_now)"
  srv="$(_cc_server_pid)"
  ns="$(_cc_socket_ns)"
  key="$(cc_store_mint_key)" || { _cc_emit FAILED "$P_TARGET" - "$pane_ct" "$sid_ct" mint-failed; return 0; }
  state="$(cc_store_path "$key")"
  banner="$(cc_store_banner_path "$key")"

  # The banner path is the tombstone's argv. If it contained "claude", an OLD
  # post_restore.sh would fail to skip the row at :428 and would arm a Claude
  # relaunch in the tombstone (internal C7). The default store root exists to
  # make that impossible; refuse if it has been configured away.
  case "$banner" in
    *claude*)
      _cc_emit FAILED "$P_TARGET" - "$pane_ct" "$sid_ct" store-path-contains-claude
      _cc_log "REFUSE $P_TARGET store path contains 'claude': $banner"
      return 0 ;;
  esac

  idle="$(cc_ledger_last "$W_ID")"
  case "${idle:-}" in
    ''|*[!0-9]*) case "$W_ACT" in ''|*[!0-9]*) idle="$now" ;; *) idle="$W_ACT" ;; esac ;;
  esac
  idle=$((now - idle))
  [ "$idle" -lt 0 ] && idle=0
  rss="$(cc_proc_rss_sum "$(awk -F'\t' '{ print $3 }' "$capf" | tr '\n' ' ')")"

  # The captured process set, persisted so that a resume re-verifies THIS set
  # rather than adopting whatever the world looks like on the second attempt
  # (§3.1.1's "using the persisted capture" and §3.1.7's K0 — without it the
  # stale-capture rail is simply unreachable after a crash). These records are
  # internal to the store; no resurrect snapshot gains a line type from them (D4).
  proclines="$work/proclines"
  : > "$proclines"
  while IFS='	' read -r c_idx c_depth c_pid c_ppid c_class c_cmd; do
    [ -n "$c_pid" ] || continue
    printf 'proc\t%s\t;PANE=%s\t;DEPTH=%s\t;PPID=%s\t;CLASS=%s\t;CMD=%s\n' \
      "$c_pid" "$c_idx" "$c_depth" "$c_ppid" "$c_class" "$(_cc_b64 "$c_cmd")" >> "$proclines"
  done < "$capf"

  # `unit pane` is the entry TYPE, and it is line 2 for a reason: everything
  # that reads this store has to tell a pane entry from a a97bff0 WINDOW entry
  # before it does anything else (cc_store_unit). There is no `layout` and no
  # `active_pane` — the window is never restructured, so there is nothing to
  # replay and L11 has ceased to exist.
  {
    printf 'v\t1\n'
    printf 'unit\tpane\n'
    printf 'key\t%s\n' "$key"
    printf 'frozen_at\t%s\n' "$now"
    printf 'reason\t%s\n' "$REASON"
    printf 'idle_at_freeze\t%s\n' "$idle"
    printf 'socket\t%s\n' "$ns"
    printf 'server_pid\t%s\n' "$srv"
    printf 'window_id\t%s\n' "$W_ID"
    printf 'pane_id\t%s\n' "$P_ID"
    printf 'pane_index\t%s\n' "$P_IDX"
    printf 'session\t%s\n' "$(_cc_b64 "$W_SESS")"
    printf 'window_index\t%s\n' "$W_IDX"
    printf 'window_name\t%s\n' "$(_cc_b64 "$W_NAME")"
    printf 'pane_count\t%s\n' "$pane_ct"
    printf 'sid_count\t%s\n' "$sid_ct"
    printf 'claude_procs\t%s\n' "$claude_procs"
    printf 'primary_cwd\t%s\n' "$(_cc_b64 "$primary_cwd")"
    printf 'rss_at_freeze\t%s\n' "$rss"
    cat "$panelines"
    cat "$sidlines"
    cat "$proclines"
    printf 'end\t1\n'
  } | cc_store_write "$key"

  # Re-read the RENAMED file off disk. Verifying the buffer we just wrote would
  # checksum our own belief (internal C4).
  if ! cc_store_verify "$state"; then
    rm -f "$state" 2>/dev/null
    _cc_emit FAILED "$P_TARGET" "$key" "$pane_ct" "$sid_ct" state-verify-failed
    return 0
  fi

  # CC_NO_KILL=1 stops the sequence HERE, with the capture durable and the pane
  # untouched. The escape means "perform no irreversible act", and the tombstone
  # respawn is one: `respawn-pane -k` kills the pane's foreground process, so
  # respawning under CC_NO_KILL would destroy exactly what the escape exists to
  # preserve. No claim is written either — nothing was destroyed, so nothing
  # needs a claim to be recoverable, and the pane is left in precisely the state
  # the caller handed us.
  if [ "${CC_NO_KILL:-0}" = "1" ]; then
    _cc_flog "FROZE-DRY $P_TARGET key=$key panes=$pane_ct sids=$(cc_store_sids "$state" | tr '\n' ',' | sed 's/,$//') CC_NO_KILL=1: capture durable, pane left intact"
    _cc_emit FROZE "$P_TARGET" "$key" "$pane_ct" "$sid_ct" "$REASON"
    return 0
  fi

  # ── CLAIM, still before the first signal ───────────────────────────────────
  # The claim is part of the write-ahead sequence, not of the cleanup. A state
  # file is INERT until a live pane carries its key (D1) and there is no matcher
  # to find it again — so a capture that is durable but unclaimed is a file
  # holding session ids that nothing will ever resume, that §9 never
  # garbage-collects, and that makes §3.1.1's resume-from-kill branch (and with
  # it the stale-capture rail) unreachable. Claim first; only then destroy.
  $TMUX_CMD set-option -p -t "$P_ID" @cc-frozen "$key" 2>/dev/null

  if [ "${CC_FAIL_AFTER:-}" = "persist" ]; then
    _cc_emit FAILED "$P_TARGET" "$key" "$pane_ct" "$sid_ct" fail-injected-persist
    return 0
  fi

  fi   # ── end of the capture → gate → persist → claim path ───────────────────

  # ── The resume path's gates (§3.1.1, D2) ───────────────────────────────────
  # Everything below this point is the ONE code path that kills, and it is
  # reached by two routes. The rails above ran on the fresh route only: the
  # resume route's capture, gate and persist happened in an EARLIER invocation,
  # against a world that has had every second since to move. D2 says the kill
  # path re-asserts the invariants itself rather than trusting that a caller —
  # or a past self — checked them, so it asks the same questions again here.
  #
  # They are asked against a FRESH capture of the LIVE pane, because the old
  # capture can only re-answer the old question: a Claude started in this pane
  # after the interruption is a descendant of the same root but is absent from
  # the persisted set, and is invisible to every check derived from it. That
  # fresh capture is a WITNESS ONLY — it is never killed from and never
  # persisted. The kill set is still $capf, the persisted one (§3.1.7).
  if [ "$resuming" = "1" ]; then
    livef="$work/live"; rootlive="$work/rootslive"; sidpidf="$work/sidpids"
    awk -F'\t' 'BEGIN { OFS = "\t" } { print $1, $2, $3 }' "$panef" > "$rootlive"
    cc_proc_capture "$psf" "$rootlive" > "$livef"
    cc_store_sid_pids "$state" > "$sidpidf"

    # GATE 1 — every live claude/claudish process must map to a sid this entry
    # ALREADY records. This is the rail C2 skipped: with it, the new session in
    # the pane refuses the freeze instead of dying unrecorded.
    for cp in $(cc_proc_claude_pids "$livef"); do
      grep -qx -- "$cp" "$sidpidf" 2>/dev/null && continue
      _cc_emit REFUSED "$P_TARGET" - 1 "$sid_ct" no-sid-for-live-claude
      _cc_log "REFUSE $P_TARGET no-sid-for-live-claude (resume): live claude pid $cp [$(cc_proc_cmd_of "$cp" "$livef")] is recorded by no sid in key=$key"
      return 0
    done
    if ! _cc_gate_nonvacuous "$livef" "$(cc_proc_claude_count "$livef")" "$sid_ct" "$P_TARGET" resume; then
      return 0
    fi

    # GATE 2 — and nothing unsafe may have appeared in the live set either.
    audit="$(cc_proc_audit "$livef")"
    if [ -n "$audit" ]; then
      printf '%s\n' "$audit" | while IFS='	' read -r reason offender cmd; do
        _cc_log "UNSAFE $P_TARGET $reason pid=$offender cmd=$cmd (resume)"
      done
      if [ "$FORCE" != "1" ]; then
        reason="$(printf '%s\n' "$audit" | head -1 | cut -f1)"
        _cc_emit REFUSED "$P_TARGET" - "$pane_ct" "$sid_ct" "$reason"
        return 0
      fi
      _cc_log "FORCED $P_TARGET overriding $(printf '%s\n' "$audit" | awk 'END { print NR }') unsafe process(es) (resume)"
    fi

    # The dry run stops HERE too. FR3.5's "a dry run that disagrees with the
    # real run is worse than no dry run" is violated far more cheaply by a dry
    # run that PERFORMS one: the kill ladder is a few lines below.
    if [ "$_CC_CHECK_ONLY" = "1" ]; then
      _cc_emit CHECK-OK "$P_TARGET" - "$pane_ct" "$sid_ct" "$(cc_store_scalar "$state" rss_at_freeze)"
      return 0
    fi

    # And so does CC_NO_KILL. It means "perform no irreversible act" — and it
    # was inert on this path precisely because the check sat inside the branch
    # while the `respawn-pane -k` it guards sat outside it. The capture is
    # already durable and already claimed; the pane is left exactly as it was
    # handed to us.
    if [ "${CC_NO_KILL:-0}" = "1" ]; then
      _cc_flog "FROZE-DRY $P_TARGET key=$key panes=$pane_ct sids=$(cc_store_sids "$state" | tr '\n' ',' | sed 's/,$//') CC_NO_KILL=1: resume stopped before the tombstone, pane left intact"
      _cc_emit FROZE "$P_TARGET" "$key" "$pane_ct" "$sid_ct" "$REASON"
      return 0
    fi
  fi

  # Validated for the RESPAWN only; the true cwd stays in the state file, so a
  # deleted directory cannot make the pane un-thawable (internal H-e).
  [ -n "$primary_cwd" ] || primary_cwd="$P_CWD"
  spawn_cwd="$primary_cwd"
  [ -d "$spawn_cwd" ] || spawn_cwd="$HOME"
  [ "$resuming" = "1" ] && rss="$(cc_store_scalar "$state" rss_at_freeze)"

  # ── K0: the world must not have moved ──────────────────────────────────────
  # The root is the CAPTURED root *and* the pid the pane has right now: a pane
  # whose process was replaced since the capture is a stale capture, not a tree
  # to reclaim.
  cc_proc_ps_snapshot "$work/ps2" || true
  livepid="$($TMUX_CMD display-message -p -t "$P_ID" '#{pane_pid}' 2>/dev/null)"
  survivors="$(cc_proc_reverify "$capf" "$work/ps2" \
                "$(cut -f3 "$rootf" | tr '\n' ' ') ${livepid:-}")"

  # ── K0b: the PANE must be the captured pane (§3.1.7) ───────────────────────
  # The set that gets signalled is the set that passed the gate, and the pane
  # that gets respawned is the pane that was captured — by pane id AND by root
  # pid, checked before the first irreversible act.
  if [ -z "$livepid" ]; then
    survivors="${survivors}
vanished-pane $P_ID"
  elif ! awk -F'\t' -v p="$livepid" '$2 == 0 && $3 == p { f = 1 } END { exit !f }' "$capf"; then
    survivors="${survivors}
uncaptured-pane-pid $P_ID($livepid)"
  fi

  if [ -n "$(printf '%s' "$survivors" | tr -d ' \n')" ]; then
    # The capture is void, so the claim that points at it must go too —
    # otherwise the pane carries a key whose file no longer exists, which is
    # the "claimed-without-verified-state" half-state (§3.1.1) and a hard
    # FAILED on the next attempt.
    #
    # The FILE, however, is only ours to delete when THIS invocation wrote it.
    # On the resume path it was written by an earlier attempt that never reached
    # the kill — so it never reached the freeze log either, and it is therefore
    # the ONLY record of its session ids anywhere. D2 forbids destroying that
    # automatically: archive it instead, so the ids stay on disk while the entry
    # stops being an active claim.
    if [ "$resuming" = "1" ]; then
      sidcsv="$(cc_store_sids "$state" | tr '\n' ',' | sed 's/,$//')"
      _cc_flog "STALE-CAPTURE $P_TARGET key=$key sids=${sidcsv:--} — the resumed capture is void; the entry is archived, NOT deleted"
      if arch="$(cc_store_archive "$key")"; then
        _cc_log "ARCHIVED $P_TARGET key=$key -> $arch"
      else
        _cc_log "ARCHIVE-FAILED $P_TARGET key=$key: state file LEFT IN PLACE — it is the only record of [${sidcsv:--}]"
      fi
    else
      # Written seconds ago by this invocation, nothing has been killed, and
      # every session it names is still running with its sidecar intact.
      rm -f "$state" 2>/dev/null
    fi
    $TMUX_CMD set-option -pu -t "$P_ID" @cc-frozen 2>/dev/null
    _cc_log "REFUSE $P_TARGET stale-capture: $(printf '%s' "$survivors" | tr '\n' ' ')"
    _cc_emit REFUSED "$P_TARGET" - "$pane_ct" "$sid_ct" stale-capture
    return 0
  fi

  cc_store_banner_render "$state" | _cc_atomic_write "$banner"

  # ── The pane must survive its own process dying ────────────────────────────
  # `remain-on-exit on` is what makes "the window is never restructured" a
  # structural fact rather than a hope: K3 may SIGKILL the pane's own shell, and
  # a pane whose process exits is DESTROYED by default — which renumbers the
  # remaining panes and rewrites #{window_layout} (measured: a 3-pane window
  # loses a pane and changes layout string). With it on, the pane stays in place
  # as a dead pane and respawn-pane revives it, byte-identical layout.
  roe="$($TMUX_CMD show-option -pqv -t "$P_ID" remain-on-exit 2>/dev/null)"
  $TMUX_CMD set-option -p -t "$P_ID" remain-on-exit on 2>/dev/null

  # ── Kill: only the re-verified captured set, BEFORE the respawn (D2) ───────
  # This ordering is the defect D2 named: `respawn-pane -k` tears down the pty,
  # which SIGHUPs the pane's foreground process — so building the tombstone
  # first meant this pane's Claude was hung up on before the ladder ever ran and
  # never got the measured 7.47 s grace. Every pane now runs its own ladder
  # first and is respawned afterwards, so every pane gets that grace.
  #
  # The pane's own root pid is exempt from the WAIT set only (D3): an
  # interactive shell ignores SIGTERM by design, so waiting for it burned both
  # polls (~8.5 s) on every freeze. It is still signalled, and it is still
  # accounted for — respawn-pane -k reclaims it and the check below proves it.
  survivors="$(cc_proc_kill "$capf" "$P_PID")"

  # ── Tombstone: the SAME pane, respawned in place ───────────────────────────
  # No kill-pane, no split, no select-layout. The `"$0"`/`"$1"` positional form
  # means neither the shell path nor the banner path is ever re-parsed, so
  # spaces in either cannot break it (§H5).
  tsh="$(_cc_tombstone_shell)"
  title="❄ FROZEN $key ${pane_ct}p/${sid_ct}s $(date -r "$now" '+%Y-%m-%d' 2>/dev/null || date -d "@$now" '+%Y-%m-%d' 2>/dev/null)"
  tcmd="sh -c 'cat \"\$1\" 2>/dev/null; exec \"\$0\" -l' $(_cc_shquote "$tsh") $(_cc_shquote "$banner")"
  if ! $TMUX_CMD respawn-pane -k -c "$spawn_cwd" -t "$P_ID" "$tcmd" 2>/dev/null; then
    # The processes are already reclaimed and the record is durable and claimed,
    # so the recoverable answer is to leave both in place: the pane is still
    # thawable, and thaw respawns it anyway. Give the user a live pane back —
    # naming the SHELL explicitly, because a bare `respawn-pane` re-runs the
    # command the pane was created with, which for a Claude pane would relaunch
    # the session this freeze just reclaimed.
    $TMUX_CMD respawn-pane -k -c "$spawn_cwd" -t "$P_ID" \
      "$(_cc_shquote "$tsh") -l" 2>/dev/null
    $TMUX_CMD select-pane -t "$P_ID" -T "$(_cc_lit "$title")" 2>/dev/null
    _cc_restore_remain_on_exit "$P_ID" "$roe"
    _cc_flog "FAILED $P_TARGET key=$key respawn-pane failed AFTER the kill — state retained and still claimed, thaw still works"
    _cc_emit FAILED "$P_TARGET" "$key" "$pane_ct" "$sid_ct" respawn-failed
    return 0
  fi
  if [ -z "$($TMUX_CMD list-panes -a -F '#{pane_id}' 2>/dev/null | grep -x -- "$P_ID")" ]; then
    _cc_emit FAILED "$P_TARGET" "$key" "$pane_ct" "$sid_ct" tombstone-not-alive
    return 0
  fi
  $TMUX_CMD select-pane -t "$P_ID" -T "$(_cc_lit "$title")" 2>/dev/null
  # The identity carrier is protected: the user's prompt cannot overwrite it.
  $TMUX_CMD set-option -p -t "$P_ID" allow-rename off 2>/dev/null
  _cc_restore_remain_on_exit "$P_ID" "$roe"

  # The exempt root pid is not exempt from the ACCOUNTING: respawn-pane -k was
  # its reclamation, and this is where that is proved rather than assumed.
  _cc_wait_gone "$P_PID" 1
  if kill -0 "$P_PID" 2>/dev/null; then
    survivors="${survivors:+$survivors
}$P_PID"
  fi

  [ -n "$pending_dir" ] && rm -f "${pending_dir}/${P_ID#%}" 2>/dev/null
  [ -n "$launch_dir" ] && rm -f "${launch_dir}/${P_ID#%}" 2>/dev/null

  # Drop the by-pid sidecars of the Claudes we just reclaimed. pre_save.sh's GC
  # would remove them at the next save anyway (dead pid), but leaving them for
  # up to 15 minutes lets pid REUSE make a frozen session look live to the
  # thaw's duplicate check, which would then relaunch it without --resume.
  # Their session ids are already durable in the state file and the freeze log.
  for cp in $(cc_proc_claude_pids "$capf"); do
    rm -f "${by_pid_dir}/${cp}.session-id" 2>/dev/null
  done
  # The claim was written before the kill (see above); re-asserting it here is
  # what makes a resumed freeze idempotent in the option as well as on disk.
  $TMUX_CMD set-option -p -t "$P_ID" @cc-frozen "$key" 2>/dev/null

  # The freeze log is the second durable copy of these session ids (§2.6).
  _cc_flog "FROZE $P_TARGET key=$key panes=$pane_ct sids=$(cc_store_sids "$state" | tr '\n' ',' | sed 's/,$//') rss~$rss reason=$REASON cwd=$primary_cwd"

  if [ -n "$survivors" ]; then
    _cc_log "PARTIAL $P_TARGET key=$key survivors=$(printf '%s' "$survivors" | tr '\n' ' ')"
    _cc_emit PARTIAL "$P_TARGET" "$key" "$pane_ct" "$sid_ct" "survivors=$(printf '%s' "$survivors" | tr '\n' ',')"
    return 0
  fi
  _cc_emit FROZE "$P_TARGET" "$key" "$pane_ct" "$sid_ct" "$REASON"
  return 0
}

# `remain-on-exit` is a pane option the user may have set deliberately: restore
# what was there, rather than unsetting whatever we found.
_cc_restore_remain_on_exit() {
  if [ -n "$2" ]; then $TMUX_CMD set-option -p -t "$1" remain-on-exit "$2" 2>/dev/null
  else $TMUX_CMD set-option -pu -t "$1" remain-on-exit 2>/dev/null; fi
}

# ── The container loops: no second kill path, just iteration ─────────────────
# A window freeze is "freeze each of its panes" and a session freeze is "freeze
# each of its windows". Neither may grow a destructive step of its own — every
# irreversible act in this file happens inside __cc_freeze_pane_locked.
#
# Partial outcomes are FIRST CLASS: a window whose third pane refuses reports
# 2 frozen / 1 refused, LEAVES THE TWO SUCCESSES FROZEN, and does not claim
# success. There is no rollback of a successful pane, ever — rolling one back
# would mean thawing a pane the user asked to freeze, which is both destructive
# (it respawns) and a lie about what happened.
_cc_freeze_window() {
  local wid="$1" p ok0 re0 fa0 bu0 n=0 tgt
  ok0=$_CC_N_OK; re0=$_CC_N_REFUSED; fa0=$_CC_N_FAILED; bu0=$_CC_N_BUSY
  tgt="$($TMUX_CMD display-message -p -t "$wid" '#{session_name}:#{window_index}' 2>/dev/null)"
  for p in $(_cc_resolve_panes "$wid"); do
    _cc_freeze_pane "$p"
    n=$((n + 1))
  done
  _CC_LAST_N="$n"
  [ "$_CC_CHECK_ONLY" = "1" ] && return 0
  _cc_container_line WINDOW "${tgt:-$wid}" \
    $((_CC_N_OK - ok0)) $((_CC_N_REFUSED - re0)) $((_CC_N_FAILED - fa0)) $((_CC_N_BUSY - bu0)) "$n"
  return 0
}

_cc_freeze_session() {
  local sess="$1" wid ok0 re0 fa0 bu0 n=0
  ok0=$_CC_N_OK; re0=$_CC_N_REFUSED; fa0=$_CC_N_FAILED; bu0=$_CC_N_BUSY
  for wid in $(_cc_resolve_windows "$sess:"); do
    _CC_LAST_N=0
    _cc_freeze_window "$wid"
    n=$((n + _CC_LAST_N))
  done
  [ "$_CC_CHECK_ONLY" = "1" ] && return 0
  _cc_container_line SESSION "$sess" \
    $((_CC_N_OK - ok0)) $((_CC_N_REFUSED - re0)) $((_CC_N_FAILED - fa0)) $((_CC_N_BUSY - bu0)) "$n"
  return 0
}

# The ATOM verb PARTIAL is counted in BOTH ok and failed by _cc_emit (the pane
# IS frozen, and a pid survived), so `frozen` here is ok and `considered` is the
# pane count — the verdict never over-claims. The verdict this function prints
# is the CONTAINER-level PARTIAL, the same word at the level above: a window all
# of whose panes came back PARTIAL is ALL frozen and PARTIALLY reclaimed, and
# the atom lines are where a caller reads that.
_cc_container_line() {
  local kind="$1" tgt="$2" ok="$3" refused="$4" failed="$5" busy="$6" total="$7" verdict
  if   [ "$total" -eq 0 ];  then verdict=NONE
  elif [ "$ok" -eq 0 ];     then verdict=NONE
  elif [ "$ok" -ge "$total" ]; then verdict=ALL
  else verdict=PARTIAL; fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$kind" "${tgt:--}" "$verdict" "$ok" "$refused" "$failed" "$busy" "$total"
}

# ── The serialised save (§3.1.13, Bug B) ─────────────────────────────────────
# THE PROBLEM. tmux-resurrect names its snapshot `tmux_resurrect_<%Y%m%dT%H%M%S>
# .txt` — ONE SECOND of resolution — and `save_all` then does:
#
#     fetch_and_dump_grouped_sessions >  "$f"      # TRUNCATE
#     dump_panes                      >> "$f"
#     dump_windows                    >> "$f"
#     dump_state                      >> "$f"
#     execute_hook post-save-layout   "$f"         # ← pre_save.sh runs HERE
#     files_differ "$f" last && ln -fs "$f" last
#
# Two saves in the same second share `$f`. B's truncate lands between A's
# `dump_panes` and A's `dump_windows`, or A's and B's appends interleave — which
# is exactly what was measured on the live machine: a snapshot with 146 pane
# rows for 73 live panes (every row doubled) and others with `panes=36
# windows=0` (truncated). tmux runs continuum's check every 5 s while a save on
# that machine took ~20 s, so overlap was the steady state, not the exception:
# 37 continuum_save.sh + 18 save.sh + 3 pre_save.sh alive at once.
#
# WHERE THE MUTEX HAS TO LIVE. Not in pre_save.sh. That hook is
# `post-save-layout`, and by the time it runs all four dumps are already on
# disk — a mutex there can stop a bad snapshot from being PUBLISHED (it runs
# before `files_differ`/`ln -fs last`, which is the last moment at which a save
# can still be made a no-op) but it cannot stop a second DUMP from starting.
# resurrect exposes no pre-save hook at all, so the only place a dump can be
# prevented is UPSTREAM OF save.sh — in whatever invokes it. This function is
# the plugin's invoker, so this is where the gate goes: the lock is taken
# BEFORE save.sh is exec'd and held, in the FOREGROUND, until it has returned.
# A second request while one is in flight finds a confirmed-live owner, exits 0
# and dumps nothing.
#
# CC_SAVE_LOCK_HELD is exported into save.sh's environment so that pre_save.sh —
# which runs as save.sh's child and would otherwise contend with its own
# grandparent — knows the save it belongs to already holds the mutex.
_cc_save_locked() {
  local lock
  if ! _cc_lock_acquire "$(cc_store_lock_root)" save; then
    _cc_log "SAVE skipped: a save is already in progress (BUSY, nothing dumped)"
    return 0
  fi
  lock="$_CC_LOCK_LAST"
  # moox (C5): the save is the engine's, through `moox save --wait 30s`, whose
  # result is logged by _cc_moox_save and never undoes the freeze. The freeze
  # store's own lock above is kept and exported, unchanged: the engine's
  # save.lock is a different lock.
  _cc_log "SAVE-BEGIN reason=freeze: ${REASON:-manual}"
  CC_SAVE_LOCK_HELD="$lock" _cc_moox_save "freeze: ${REASON:-manual}"
  _cc_lock_release "$lock"
  return 0
}

# ── One save request per invocation (§3.1.13) ────────────────────────────────
# v1 issued one per target; with nothing locking resurrect's save, concurrent
# save.sh runs race `ln -fs … last` and the winner can be a pre-freeze dump.
# The request is now routed through `cc_freeze.sh save`, which holds the mutex
# for the whole dump. Detached, because a freeze must not block on a 20 s save.
#
# A PLAIN CHILD PROCESS, NEVER `$TMUX_CMD run-shell -b` (G3-H1): this script is
# run from a pane, the popup or an agent — a tool client — and `tmux run-shell`
# from a tool client is the 3.7b cmd_run_shell_print crash (2026-08-16: a
# finished job's output delivered to a client that had gone away; 16 sessions
# and the save in flight lost; requirements.md, never-bare-tmux-run-shell). A
# child also keeps this invocation's environment (CC_SAVE_CMD, CC_FREEZE_DIR,
# MOOX_BIN, TMUX_CMD, the participation env) and its --reason, which a job the
# server starts loses. It ignores SIGHUP so a popup that closes does not take
# the save with it (bash keeps a signal ignored on entry ignored), reads
# nothing and prints nothing — cc_thaw.sh's _cc_request_save does the same.
_cc_request_save() {
  [ "$NO_SAVE" = "1" ] && return 0
  [ "${CC_NO_SAVE:-0}" = "1" ] && return 0
  ( trap '' HUP; exec bash "$CURRENT_DIR/cc_freeze.sh" save --reason "${REASON:-manual}" ) \
    </dev/null >/dev/null 2>&1 &
  _cc_log "SAVE-REQUESTED (serialised) reason=${REASON:-manual}"
  return 0
}

# Every skip is PRINTED and LOGGED with its reason (§3.3.3, FR3.4, AC7, AC8).
# One helper, so no rail can be added that prints without logging — the log is
# how an unattended sweep is explained after a reboot.
_cc_sweep_skip() {
  printf 'SKIP\t%s\t%s\t%s\n' "$1" "$2" "$3"
  _cc_log "SWEEP skip $1 ($2): $3"
}

# ── sweep (FR3) ──────────────────────────────────────────────────────────────
# The sweep is UNANIMOUS by design: it freezes a window only when EVERY one of
# its panes passes every rail. Manual freezes accept partial outcomes because a
# human is reading the result; an unattended 15-minute job that leaves windows
# half-frozen is a worse answer than one that waits for the next pass, and it
# also keeps `--dry-run` and the real run in exact agreement (FR3.5).
_cc_sweep() {
  local enabled idle_opt idle_secs now done_ct
  local wid sess idx name active attached act nwin live_idle led_idle
  local cands sweep_lock npanes

  # Checked FIRST, before any candidate work: off means nothing is frozen,
  # whatever the idle ages (FR3.6).
  enabled="$(_cc_opt @claude-continuity-autofreeze off)"
  case "$enabled" in
    on|yes|1|true) ;;
    *) _cc_log "SWEEP skipped: autofreeze=$enabled"; return 0 ;;
  esac
  # A second sweep exits 0 immediately and never queues.
  if ! _cc_lock_acquire "$(cc_store_lock_root)" sweep; then
    _cc_log "SWEEP skipped: another sweep holds the lock"
    return 0
  fi
  sweep_lock="$_CC_LOCK_LAST"

  idle_opt="$(_cc_opt @claude-continuity-autofreeze-idle 2d)"
  idle_secs="$(_cc_duration_secs "$idle_opt")" || {
    _cc_log "SWEEP aborted: unparseable idle threshold '$idle_opt'"
    _cc_lock_release "$sweep_lock"
    return 0
  }
  now="$(_cc_now)"
  done_ct=0
  cands="$_CC_WORK/sweep"

  # Read from a FILE, never a pipe: a `while read` on the right-hand side of a
  # pipe runs in a subshell, and the cap counter mutated there would be
  # discarded — a bug that would let one sweep freeze every idle window.
  # session_name is last so a space or tab in it can only truncate itself.
  if ! $TMUX_CMD list-windows -a -F \
    '#{window_id}	#{window_index}	#{window_active}	#{session_attached}	#{window_activity}	#{session_windows}	#{window_name}	#{session_name}' \
    2>/dev/null > "$cands"; then
    # The sweep could not RUN — that, and only that, is a failure status.
    _cc_log "SWEEP aborted: tmux would not list windows"
    _cc_lock_release "$sweep_lock"
    _CC_HARD_RC=2
    return 0
  fi

  while IFS='	' read -r wid idx active attached act nwin name sess; do
    [ -n "$wid" ] || continue

    if [ -n "$($TMUX_CMD show-option -wqv -t "$wid" @cc-frozen 2>/dev/null)" ]; then
      _cc_sweep_skip "$sess:$idx" "$name" already-frozen; continue
    fi
    # Every pane already carrying a claim: the window is frozen in the new
    # model even though no window option says so. An UNEXPANDED `#{@cc-frozen}`
    # (a tmux too old for pane-scoped user options) is not a claim — it is the
    # format string itself, and counting it would skip every window forever.
    npanes="$(_cc_resolve_panes "$wid" | awk 'END { print NR + 0 }')"
    if [ "$npanes" -gt 0 ] && [ "$($TMUX_CMD list-panes -t "$wid" -F '#{@cc-frozen}' 2>/dev/null \
         | awk 'length($0) > 0 && substr($0, 1, 2) != "#{" { n++ } END { print n + 0 }')" = "$npanes" ]; then
      _cc_sweep_skip "$sess:$idx" "$name" already-frozen; continue
    fi
    if cc_pin_is "$wid"; then
      _cc_sweep_skip "$sess:$idx" "$name" pinned; continue
    fi
    if [ "$active" = "1" ] && [ "${attached:-0}" != "0" ]; then
      _cc_sweep_skip "$sess:$idx" "$name" active-window-of-attached-session; continue
    fi
    if [ "${nwin:-1}" = "1" ] && [ "${attached:-0}" != "0" ]; then
      _cc_sweep_skip "$sess:$idx" "$name" sole-window-of-attached-session; continue
    fi

    # The AND-rail: idle by the LEDGER *and* idle by the LIVE window activity.
    # A mis-keyed ledger row can therefore only produce a wrong number in the
    # popup — never a freeze (internal C8). It also subsumes the boot-grace
    # option: right after a restore every window is live-fresh.
    led_idle="$(cc_ledger_last "$wid")"
    case "${led_idle:-}" in ''|*[!0-9]*) led_idle="$now" ;; esac
    case "${act:-}" in ''|*[!0-9]*) act="$now" ;; esac
    live_idle=$((now - act))
    led_idle=$((now - led_idle))
    if [ "$led_idle" -lt "$idle_secs" ] || [ "$live_idle" -lt "$idle_secs" ]; then
      _cc_sweep_skip "$sess:$idx" "$name" not-idle; continue
    fi

    if [ "$done_ct" -ge "$_CC_SWEEP_CAP" ]; then
      _cc_sweep_skip "$sess:$idx" "$name" "cap-reached"; continue
    fi

    # Rail 6 — everything the pane atom itself refuses, reported verbatim, and
    # evaluated for EVERY pane before any of them is touched. The check runs the
    # real capture, the real sid gate and the real audit and stops before
    # persisting, so the preview cannot disagree with the run.
    #
    # Unanimity is over the AWAKE panes. A pane that is already frozen is not
    # part of the operation at all — it is skipped, not refused — so a window
    # holding one frozen pane and two awake ones is still a candidate window
    # with two candidate panes. `already-frozen` is only a whole-window verdict
    # when EVERY pane is frozen, which the two rails above decide; reaching this
    # rail with no awake pane left can only mean the window became fully frozen
    # between those rails and this one, and it is reported the same way.
    _CC_C_OK=0; _CC_C_BAD=0; _CC_C_REASON=""; _CC_C_SIDS=0; _CC_C_RSS=0; _CC_C_ALREADY=0
    _CC_CHECK_ONLY=1
    _cc_freeze_window "$wid" > "$_CC_WORK/sweep.out"
    _CC_CHECK_ONLY=0
    if [ "$_CC_C_BAD" != "0" ]; then
      _cc_sweep_skip "$sess:$idx" "$name" "${_CC_C_REASON:-refused}"
      continue
    fi
    if [ "$_CC_C_OK" = "0" ]; then
      if [ "$_CC_C_ALREADY" != "0" ]; then
        _cc_sweep_skip "$sess:$idx" "$name" already-frozen
      else
        _cc_sweep_skip "$sess:$idx" "$name" nothing-to-freeze
      fi
      continue
    fi

    if [ "$DRY_RUN" = "1" ]; then
      printf 'WOULD-FREEZE\t%s:%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$sess" "$idx" "$name" "$led_idle" "$_CC_C_OK" "$_CC_C_SIDS" "$_CC_C_RSS"
      done_ct=$((done_ct + 1))
      continue
    fi

    _cc_freeze_window "$wid"
    _cc_flog "SWEEP froze $sess:$idx"
    done_ct=$((done_ct + 1))
  done < "$cands"

  _cc_lock_release "$sweep_lock"
  # A sweep that RAN reports success, however many windows it declined to
  # touch: skipping is normal operation, and this runs unattended from
  # `run-shell -b`, where a non-zero exit reads as a failed hook. Per-window
  # refusals are on stdout and in the log; they are not this command's status.
  # The sweep does not request a save: it runs from the tail of pre_save.sh, and
  # the next 15-minute save records the freeze naturally (P7 resolution B).
  return 0
}

# ── reclaim / ledger-seed: the store half of a moox restore (§6.4 steps 9-10) ─
# The engine creates every pane itself and knows which new pane carries which
# tombstone, so there is no resolve-by-content here: it passes the key, the
# new pane id and the new window id. This is post_restore.sh:287-477's store
# and option half, unchanged in what it writes, minus the banner (the engine
# delivers <key>.banner as the pane's MOOX_RESTORE_CONTENT) and minus the
# pending-file nudge. EVERY branch is non-destructive: nothing is killed,
# respawned, deleted or re-frozen.
#
# stdout is a contract the engine's BOOT verdict reads, one line per call:
#   FROZEN-CLAIMED       the claim is set and read back
#   FROZEN-ORPHAN        no readable state file / no live window: a REAL MISS
#   FROZEN-CLAIM-FAILED  set-option did not take: a warning
#   FROZEN-FOREIGN       the entry belongs to another live server: a warning
#   FROZEN-UNREADABLE    the key is not a safe token: a warning
# Exit 0 always (a verdict is a line, not a status).

# The entry belongs to a LIVE pid that is another tmux server (cc_store's
# rule, corroborated against the pid's argv: after a reboot the recorded pid
# is stale and may have been recycled by an unrelated process).
_cc_reclaim_is_foreign() {
  local sp
  cc_store_is_foreign "$1" || return 1
  sp="$(cc_store_scalar "$1" server_pid)"
  case "$(ps -o command= -p "$sp" 2>/dev/null)" in
    *tmux*) return 0 ;;
  esac
  return 1
}

# Re-point server_pid, window_id and (for a pane entry) pane_id at THIS
# server, through the same atomic write the freeze used.
_cc_reclaim_repoint() {
  local state="$1" srv="$2" wid="$3" pid="${4:-}" tmpf
  tmpf="$(dirname "$state")/tmp/reclaim.$$"
  mkdir -p "$(dirname "$tmpf")" 2>/dev/null
  awk -F'\t' -v sp="$srv" -v wd="$wid" -v pd="$pid" 'BEGIN { OFS = "\t" }
    $1 == "server_pid" { print $1, sp; next }
    $1 == "window_id"  { print $1, wd; next }
    $1 == "pane_id" && pd != "" { print $1, pd; next }
    { print }
  ' "$state" > "$tmpf" 2>/dev/null
  if [ ! -s "$tmpf" ] || ! _cc_atomic_write "$state" < "$tmpf"; then
    rm -f "$tmpf"
    _cc_log "FROZEN-REPOINT-FAILED $state: entry left exactly as it was"
    return 1
  fi
  rm -f "$tmpf"
  cc_store_verify "$state" || _cc_log "FROZEN-REPOINT-SUSPECT $state: no longer verifies after re-pointing"
  return 0
}

_cc_reclaim_say() { _cc_log "$1"; printf '%s\n' "$1"; }

_cc_reclaim() {
  local key="${1:-}" pane_id="${2:-}" wid="${3:-}" ns state unit scope got
  if [ -z "$key" ] || ! _cc_is_safe_token "$key"; then
    _cc_reclaim_say "FROZEN-UNREADABLE ${pane_id:--} key=${key:--}: the tombstone carries no usable key"
    return 0
  fi
  case "$pane_id" in
    %[0-9]*) ;;
    *) _cc_reclaim_say "FROZEN-ORPHAN key=$key: no pane id given, REAL MISS"; return 0 ;;
  esac
  # The namespace dir is NOT created here (cc_store_ns_dir would): a restore
  # on a machine that never froze anything must leave no store behind.
  ns="$(_cc_store_root)/$(_cc_socket_ns)"
  state="$ns/$key.state"
  if [ ! -d "$ns" ] || ! cc_store_verify "$state"; then
    _cc_reclaim_say "FROZEN-ORPHAN $pane_id key=$key: no readable state file, REAL MISS"
    return 0
  fi
  if _cc_reclaim_is_foreign "$state"; then
    _cc_reclaim_say "FROZEN-FOREIGN $pane_id key=$key: held by live server pid $(cc_store_scalar "$state" server_pid), not claimed"
    return 0
  fi
  [ -n "$wid" ] || wid="$($TMUX_CMD display-message -p -t "$pane_id" '#{window_id}' 2>/dev/null)"
  if [ -z "$wid" ]; then
    _cc_reclaim_say "FROZEN-ORPHAN $pane_id key=$key: the pane belongs to no live window, REAL MISS"
    return 0
  fi
  # Re-claim at the level the entry is written in: a pane entry's claim is a
  # PANE option, a legacy (a97bff0) entry's a WINDOW option.
  unit="$(cc_store_unit "$state")"
  if [ "$unit" = "pane" ]; then
    scope="pane $pane_id"
    $TMUX_CMD set-option -p -t "$pane_id" @cc-frozen "$key" 2>/dev/null
    got="$($TMUX_CMD show-option -pqv -t "$pane_id" @cc-frozen 2>/dev/null)"
    _cc_reclaim_repoint "$state" "$(_cc_server_pid)" "$wid" "$pane_id"
  else
    scope="window $wid"
    $TMUX_CMD set-option -w -t "$wid" @cc-frozen "$key" 2>/dev/null
    got="$($TMUX_CMD show-option -wqv -t "$wid" @cc-frozen 2>/dev/null)"
    _cc_reclaim_repoint "$state" "$(_cc_server_pid)" "$wid"
  fi
  $TMUX_CMD set-option -p -t "$pane_id" allow-rename off 2>/dev/null
  if [ "$got" != "$key" ]; then
    _cc_reclaim_say "FROZEN-CLAIM-FAILED $pane_id key=$key unit=$unit: set-option on $scope did not take (read back '${got:-}'), nothing undone, entry still thawable by its title"
    return 0
  fi
  _cc_reclaim_say "FROZEN-CLAIMED $pane_id key=$key unit=$unit claim=$scope window=$wid (state re-pointed at this server)"
  return 0
}

# post_restore.sh:964-978: baseline the activity ledger at server start.
# Skipped when this server has no store: seeding would CREATE one.
_cc_ledger_seed_cmd() {
  local ns
  ns="$(_cc_store_root)/$(_cc_socket_ns)"
  [ -d "$ns" ] || return 0
  type cc_ledger_seed >/dev/null 2>&1 || return 0
  cc_ledger_seed
  return 0
}

# ── heartbeat: the freeze store's per-save upkeep (moox §5.7, B23) ───────────
# pre_save.sh ran this block on every resurrect save; under moox the daemon
# (`tt serve`) starts `cc_freeze.sh heartbeat` after each PUBLISHED save, in
# the background with a 60 s deadline, output discarded. It is pre_save.sh
# :201-216 (the namespace initialiser) plus :660-764 (_cc_sid_is_live,
# _cc_confirm_thaws, _cc_kick_sweep and the guarded body), with two edits
# (PR-H16): the ps scratch file lives in this run's $_CC_WORK (pre_save.sh
# used ${SNAPSHOT_FILE}, unbound under `set -u` outside a resurrect save),
# and the sweep is started directly in the background, never through
# `run-shell` — the heartbeat's parent is a tool client, and `tmux run-shell`
# from a tool client crashes tmux 3.7b.

# The namespace directory of the freeze store for THIS tmux socket, resolved
# WITHOUT creating it (cc_store_ns_dir() mkdirs; this must not). Empty means
# this server has never frozen a window — the heartbeat may not materialise
# a feature directory on a machine that does not use the feature.
_cc_ns_dir_if_exists() {
  local root ns
  type _cc_store_root >/dev/null 2>&1 || return 1
  root="$(_cc_store_root 2>/dev/null)"
  [ -n "$root" ] && [ -d "$root" ] || return 1
  ns="$(_cc_socket_ns 2>/dev/null)"
  [ -n "$ns" ] || return 1
  [ -d "$root/$ns" ] || return 1
  printf '%s' "$root/$ns"
}

# A sid is LIVE when some process's argv carries it as a resume/session-id
# argument AND that process is a Claude launcher (the same classifier the
# restore uses). Fail-closed: anything unrecognised is "not live", and the
# entry simply survives to the next heartbeat.
_cc_sid_is_live() {
  local sid="$1" psf="$2" line cmd
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    set -f
    # shellcheck disable=SC2086
    set -- $line
    set +f
    [ "$#" -ge 3 ] || continue
    shift 2                      # drop the pid and ppid columns
    cmd="$*"
    case " $cmd " in
      *" --resume $sid "*|*" --resume=$sid "*|*" -r $sid "*|\
      *" --session-id $sid "*|*" --session-id=$sid "*) ;;
      *) continue ;;
    esac
    _cc_is_claude_launcher "$cmd" && return 0
  done <<EOF
$(grep -F -- "$sid" "$psf" 2>/dev/null)
EOF
  return 1
}

_cc_confirm_thaws() {
  local ns="$1" state ps_file key sid all_live dst
  ps_file=""
  for state in "$ns"/*.state; do
    [ -f "$state" ] || continue
    [ -n "$(cc_store_scalar "$state" thawed_at)" ] || continue
    # Fail closed: an unreadable entry is never archived, never deleted.
    cc_store_verify "$state" || continue
    if [ -z "$ps_file" ]; then
      ps_file="$_CC_WORK/thawps.$$"
      cc_proc_ps_snapshot "$ps_file" || { rm -f "$ps_file"; return 0; }
    fi
    all_live=1
    for sid in $(cc_store_sids "$state" primary); do
      _cc_sid_is_live "$sid" "$ps_file" || { all_live=0; break; }
    done
    [ "$all_live" = "1" ] || continue
    key="$(cc_store_scalar "$state" key)"
    [ -n "$key" ] || continue
    dst="$(cc_store_archive "$key")" || continue
    rm -f "$(cc_store_banner_path "$key")" 2>/dev/null
    _cc_log "THAW-CONFIRMED key=$key: every session is live again — archived to $dst"
  done
  [ -n "$ps_file" ] && rm -f "$ps_file"
  return 0
}

# The sweep is the only thing reachable from a save that can freeze anything,
# so it is launched DETACHED and only when the user has turned autofreeze on.
# Started directly, NOT through `$TMUX_CMD run-shell -b` (pre_save.sh:721): the
# heartbeat runs under `tt serve`, a tool client (PR-H16).
_cc_kick_sweep() {
  [ "$(_cc_opt @claude-continuity-autofreeze off)" = "on" ] || return 0
  [ -x "$CURRENT_DIR/cc_freeze.sh" ] || return 0
  "$CURRENT_DIR/cc_freeze.sh" sweep >/dev/null 2>&1 </dev/null &
  return 0
}

# The guarded body. The three calls MUTATE the freeze store, so they take the
# store's `heartbeat` lock NON-BLOCKING: a held lock costs this heartbeat its
# work and nothing else (the next published save runs it again). The lock is
# NOT `sweep`: this block kicks the sweep. Always exits 0.
_cc_heartbeat() {
  local ns lock=""
  ns="$(_cc_ns_dir_if_exists)"
  [ -n "$ns" ] || return 0
  type cc_ledger_tick >/dev/null 2>&1 || return 0
  if type _cc_lock_acquire >/dev/null 2>&1; then
    if _cc_lock_acquire "$(cc_store_lock_root)" heartbeat; then
      lock="$_CC_LOCK_LAST"
    else
      _cc_log "STORE-BUSY: the freeze store 'heartbeat' lock is held by a live worker — SKIPPING this heartbeat (ledger tick, thaw confirmation, sweep kick)."
      return 0
    fi
  fi
  cc_ledger_tick
  _cc_confirm_thaws "$ns"
  _cc_kick_sweep
  [ -n "$lock" ] && _cc_lock_release "$lock"
  return 0
}

# ── Entry ────────────────────────────────────────────────────────────────────
CMD="${1:-}"
[ "$#" -gt 0 ] && shift
REASON="manual"
FORCE=0
NO_SAVE=0
DRY_RUN=0

_CC_WORK="$(mktemp -d "${TMPDIR:-/tmp}/cc-freeze.XXXXXX" 2>/dev/null)" || { printf 'cc_freeze: cannot create work dir\n' >&2; exit 2; }
TARGET_FILE="$_CC_WORK/targets"
: > "$TARGET_FILE"

# Targets go to a FILE, one per line — a session name may contain a space, and a
# space-joined string would silently split it into two unresolvable targets.
while [ "$#" -gt 0 ]; do
  case "$1" in
    --reason) REASON="${2:-manual}"; shift 2 ;;
    --force)  FORCE=1; shift ;;
    --no-save) NO_SAVE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --) shift; while [ "$#" -gt 0 ]; do printf '%s\n' "$1" >> "$TARGET_FILE"; shift; done ;;
    -*) _cc_usage; exit 1 ;;
    *) printf '%s\n' "$1" >> "$TARGET_FILE"; shift ;;
  esac
done

case "$CMD" in
  freeze)
    [ -s "$TARGET_FILE" ] || { _cc_usage; exit 1; }
    _cc_acted=0
    while IFS= read -r t; do
      [ -n "$t" ] || continue
      case "$(_cc_target_level "$t")" in
        pane)
          ids="$(_cc_resolve_panes "$t")"
          if [ -z "$ids" ]; then
            _cc_emit FAILED "$t" - 0 0 target-unresolvable
            continue
          fi
          for id in $ids; do _cc_freeze_pane "$id"; _cc_acted=1; done ;;
        window)
          ids="$(_cc_resolve_windows "$t")"
          if [ -z "$ids" ]; then
            _cc_emit FAILED "$t" - 0 0 target-unresolvable
            continue
          fi
          for id in $ids; do _cc_freeze_window "$id"; _cc_acted=1; done ;;
        session)
          ids="$(_cc_resolve_windows "$t")"
          if [ -z "$ids" ]; then
            _cc_emit FAILED "$t" - 0 0 target-unresolvable
            continue
          fi
          _cc_freeze_session "${t%:}"; _cc_acted=1 ;;
        *)
          _cc_emit FAILED "$t" - 0 0 target-unresolvable ;;
      esac
    done < "$TARGET_FILE"
    [ "$_cc_acted" = "1" ] && _cc_request_save
    exit "$(_cc_exit_code)" ;;
  sweep)
    _cc_sweep
    exit "$_CC_HARD_RC" ;;
  reclaim)
    # reclaim <key> <pane_id> <window_id>: the targets file holds the three
    # arguments, one per line, in order.
    _cc_r1="$(sed -n 1p "$TARGET_FILE")"; _cc_r2="$(sed -n 2p "$TARGET_FILE")"; _cc_r3="$(sed -n 3p "$TARGET_FILE")"
    _cc_reclaim "$_cc_r1" "$_cc_r2" "$_cc_r3"
    exit 0 ;;
  ledger-seed)
    _cc_ledger_seed_cmd
    exit 0 ;;
  heartbeat)
    # moox §5.7: started by the daemon after each published save.
    _cc_heartbeat
    exit 0 ;;
  save)
    # The serialised save entry point. Always exits 0: "a save is already in
    # flight" is normal operation, not a failure, and this runs from
    # `run-shell -b` where a non-zero exit reads as a failed hook.
    _cc_save_locked
    exit 0 ;;
  ''|-h|--help|help)
    _cc_usage
    [ -z "$CMD" ] && exit 1
    exit 0 ;;
  *)
    _cc_usage
    exit 1 ;;
esac
