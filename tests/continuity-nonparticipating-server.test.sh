#!/usr/bin/env bash
# continuity-nonparticipating-server.test.sh — G3-H1 / G3-M1 regression: the
# vendored continuity entry scripts do NOTHING on a tmux server that does not
# take part in moox continuity, and in particular never fall back to the login
# default server's legacy pane-number slots ~/.config/tmux-claude/{launch,
# pending,panes}/<N>.
#
# The case (2026-10-02 class): an isolated `-f /dev/null` server has no
# @claude-continuity-*-dir options, so before the fix `cc_freeze.sh freeze %0`
# defaulted to the legacy dirs, read the REAL default server's launch/0 as the
# frozen pane's ;TYPED= and deleted the real %0's launch recording and pending
# resume; `cc_thaw.sh` wrote an eval'd pending/<N> there.
#
# Asserted, for every entry point (freeze, sweep, save, reclaim, ledger-seed,
# heartbeat; thaw, discard; popup --list) under three environments — no MOOX_*
# at all (the scratch server is not the literal login default), the REAL
# server's MOOX_* inherited into a scratch pane, and an agent's explicit
# TMUX_CMD from outside tmux:
#   1. the scratch HOME's legacy dirs are byte-identical (no write, no delete);
#   2. nothing read from them lands anywhere (their unique marker text appears
#      in no file the run produced), no freeze store and no log are created;
#   3. the panes are untouched (same pid, same title, no @cc-frozen claim).
# Control: the SAME server with its own participating env and its own dirs
# does freeze, and the frozen state carries its OWN launch recording — so the
# harness can see a read when one happens, and the guard is what stops it.
#
# Isolation: `tmux -f /dev/null -S $T/<name>` (never -L, never the default
# socket), /bin/sh panes, scratch HOME/XDG, `env -i` for every script.
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/lib/hooklib.sh"
. "$here/lib/tmuxlib.sh"
hk_setup
SCRIPTS="${CC_SCRIPTS_UNDER_TEST:-$SHARE/plugins/tmux-claude-continuity/scripts}"   # override: negative control
TOOLS="$(dirname "$(command -v tmux)"):$HK_PATH"

ts_start scratch                                   # %0 runs sleep (session s)
ts set-environment -g ENV /dev/null
# Two bare interactive shells: freezable panes (a `sleep` pane is refused).
ts new-window -d -t s:1 -c /tmp 'sh -i'
ts split-window -d -t s:1 -c /tmp 'sh -i'
PANES="$(ts list-panes -a -F '#{pane_id}' | tr '\n' ' ')"

# ── The REAL default server's legacy slots, in the scratch HOME ───────────────
LEG="$H/.config/tmux-claude"
mkdir -p "$LEG/launch" "$LEG/pending" "$LEG/panes/by-pid"
for p in $PANES; do
  n="${p#%}"
  printf 'echo LEGACY-LAUNCH-MARKER-%s\n' "$n" > "$LEG/launch/$n"
  printf 'echo LEGACY-PENDING-MARKER-%s\n' "$n" > "$LEG/pending/$n"
done
printf 'legacy-sid\nLegacy Title\n' > "$LEG/panes/s-1-0.session-id"
# The mtime format is picked once, by a probe whose output is thrown away
# (G4-2-H3): GNU `stat -f %m FILE` is filesystem mode, so it prints the
# filesystem's free-block and free-inode counts to stdout before it fails, and
# a `stat -f … || stat -c …` fallback carried them into every line.
if stat -c %Y / >/dev/null 2>&1; then mtime() { stat -c %Y "$1"; }; else mtime() { stat -f %m "$1"; }; fi
snap() { # every file under the legacy dir: path, checksum, size, mtime
  find "$LEG" -type f -print 2>/dev/null | LC_ALL=C sort | while IFS= read -r f; do
    printf '%s %s %s\n' "${f#"$LEG"/}" "$(cksum < "$f" | tr -s ' ' ' ')" "$(mtime "$f")"
  done
}
BEFORE="$(snap)"
# The snapshot is one 4-field line per file, and a write elsewhere on the same
# filesystem leaves it unchanged (it would not with filesystem statistics in it).
hk_eq "snapshot: one 'path crc size mtime' line per legacy file" \
  "$(find "$LEG" -type f | wc -l | tr -d ' ')" "$(printf '%s\n' "$BEFORE" | awk 'NF == 4' | wc -l | tr -d ' ')"
head -c 65536 /dev/zero > "$T/unrelated-file"
hk_eq "snapshot: an unrelated file leaves it unchanged" "$BEFORE" "$(snap)"
rm -f "$T/unrelated-file"
# What a read would leave behind: the marker text, or its base64 (;TYPED=).
PAT="$T/legacy-patterns"
{ printf '%s\n' LEGACY-LAUNCH-MARKER LEGACY-PENDING-MARKER legacy-sid
  for f in "$LEG"/launch/* "$LEG"/pending/*; do base64 < "$f" | tr -d '\n'; echo; done; } > "$PAT"
panes_state() { ts list-panes -a -F '#{pane_id} #{pane_pid} #{pane_title} [#{@cc-frozen}]' | LC_ALL=C sort; }
PANES_BEFORE="$(panes_state)"

# ── The environments ─────────────────────────────────────────────────────────
hk_moox_layout real                                # the REAL server's exported layout (marker present)
base_env() { printf '%s\n' HOME="$H" XDG_STATE_HOME="$H/.local/state" XDG_CONFIG_HOME="$H/.config" \
  PATH="$TOOLS" SHELL=/bin/sh CC_TAB_RECONCILE_DELAY=0 CC_NO_SAVE=1 TMPDIR="$T/tmp"; }
mkdir -p "$T/tmp"
in_pane() {  # no MOOX_* at all, inside a scratch pane
  # shellcheck disable=SC2046
  env -i $(base_env) TMUX="$TS_SOCK,$TS_PID,1" TMUX_PANE=%1 "$@"
}
inherited() { # the REAL server's MOOX_* inherited into a scratch pane
  # shellcheck disable=SC2046
  env -i $(base_env) MOOX_SOCKET="$M_SOCK" MOOX_ACTIVE=1 MOOX_STATE_KEY=default MOOX_STATE_DIR="$M_STATE" \
    MOOX_PANES_DIR="$M_PANES" MOOX_LAUNCH_DIR="$M_LAUNCH" MOOX_PENDING_DIR="$M_PENDING" \
    TMUX="$TS_SOCK,$TS_PID,1" TMUX_PANE=%1 "$@"
}
agent() {     # an agent outside tmux naming the scratch server explicitly
  # shellcheck disable=SC2046
  env -i $(base_env) TMUX_CMD="tmux -S $TS_SOCK" "$@"
}

run_all() { # <env-fn>: every entry point; prints their combined output to $T/out.<env>
  local e="$1" o="$T/out.$1"
  : > "$o"
  {
    "$e" bash "$SCRIPTS/cc_freeze.sh" freeze %1
    "$e" bash "$SCRIPTS/cc_freeze.sh" freeze --force --no-save s:1
    "$e" bash "$SCRIPTS/cc_freeze.sh" sweep
    "$e" bash "$SCRIPTS/cc_freeze.sh" save
    "$e" bash "$SCRIPTS/cc_freeze.sh" reclaim k1 %1 @1
    "$e" bash "$SCRIPTS/cc_freeze.sh" ledger-seed
    "$e" bash "$SCRIPTS/cc_freeze.sh" heartbeat
    "$e" bash "$SCRIPTS/cc_thaw.sh" thaw %1
    "$e" bash "$SCRIPTS/cc_thaw.sh" thaw s:1
    "$e" bash "$SCRIPTS/cc_thaw.sh" discard --yes %1
    "$e" bash "$SCRIPTS/cc_popup.sh" --list
  } >> "$o" 2>&1 < /dev/null
}

for e in in_pane inherited agent; do
  run_all "$e"
  hk_eq "$e: the legacy launch/pending/panes slots are byte-identical" "$BEFORE" "$(snap)"
  hk_eq "$e: every pane is untouched (pid, title, no @cc-frozen)" "$PANES_BEFORE" "$(panes_state)"
  hk_gone "$e: no freeze store created in the scratch HOME" "$H/.config/tmux-cc"
  hk_gone "$e: no continuity log created under the default key" "$H/.local/state/moox"
  hits="$(grep -rlF -f "$PAT" "$T" 2>/dev/null | grep -v -e "^$LEG/" -e "^$PAT\$" || true)"
  hk_eq "$e: nothing read from the legacy slots landed anywhere" "" "$hits"
  hk_eq "$e: the scripts printed nothing" "" "$(cat "$T/out.$e")"
done
# The real server's own per-socket dirs are not touched by the inherited env either.
hk_eq "inherited: the real server's launch/ stays empty" "" "$(ls -A "$M_LAUNCH")"
hk_eq "inherited: the real server's pending/ stays empty" "" "$(ls -A "$M_PENDING")"

# ── Control: the scratch server's OWN participating env does freeze ──────────
OWN="$T/own"
mkdir -p "$OWN/launch" "$OWN/pending" "$OWN/panes/by-pid" "$OWN/frozen" "$OWN/log"
: > "$OWN/active"
for o in launch pending panes; do ts set -g "@claude-continuity-$o-dir" "$OWN/$o"; done
ts set -g @claude-continuity-by-pid-dir "$OWN/panes/by-pid"
ts set -g @claude-continuity-freeze-dir "$OWN/frozen"
ts set -g @claude-continuity-log-file "$OWN/log/continuity.log"
ts set -g @claude-continuity-claude-cmd echo
printf 'echo OWN-LAUNCH-MARKER\n' > "$OWN/launch/1"
want="$(base64 < "$OWN/launch/1" | tr -d '\n')"   # as the freeze encodes the file
own() {
  # shellcheck disable=SC2046
  env -i $(base_env) MOOX_SOCKET="$TS_SOCK" MOOX_ACTIVE=1 MOOX_STATE_KEY="scratch@00000000" \
    MOOX_STATE_DIR="$OWN" MOOX_PANES_DIR="$OWN/panes" MOOX_LAUNCH_DIR="$OWN/launch" \
    MOOX_PENDING_DIR="$OWN/pending" TMUX="$TS_SOCK,$TS_PID,1" TMUX_PANE=%1 "$@"
}
out="$(own bash "$SCRIPTS/cc_freeze.sh" freeze --no-save %1 2>&1)"
case "$out" in FROZE*) ok "control: the own env freezes %1" ;; *) no "control: the own env freezes %1" "$out" ;; esac
st="$(find "$OWN/frozen" -name '*.state' -type f 2>/dev/null | head -n 1)"
if [ -n "$st" ] && grep -q ";TYPED=$want" "$st"; then ok "control: the frozen state carries the OWN launch recording (a read is visible)"
else no "control: the frozen state carries the OWN launch recording (a read is visible)" "state <$st>: $(cat "$st" 2>/dev/null | head -c 400)"; fi
hk_gone "control: the own launch/1 recording is consumed" "$OWN/launch/1"
hk_eq "control: the legacy slots are still byte-identical" "$BEFORE" "$(snap)"

# ── The freeze's save request: a plain child, its --reason and env kept ──────
# Before, it was `$TMUX_CMD run-shell -b "… cc_freeze.sh save"` — a run-shell
# from a tool client (the 3.7b crash), which also dropped --reason and the
# caller's environment (MOOX_BIN here). Now the stub moox must see THIS
# server's -S and the freeze's own reason.
mkdir -p "$T/bin"
cat > "$T/bin/moox" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "$T/moox-argv"
exit 0
EOF
chmod +x "$T/bin/moox"
out="$(own env CC_NO_SAVE=0 MOOX_BIN="$T/bin" bash "$SCRIPTS/cc_freeze.sh" freeze --reason auto %2 2>&1)"
case "$out" in FROZE*) ok "save request: %2 froze" ;; *) no "save request: %2 froze" "$out" ;; esac
i=0; while [ $i -lt 100 ] && [ ! -s "$T/moox-argv" ]; do sleep 0.1; i=$((i + 1)); done
hk_eq "save request: moox save -S <this server> with the freeze's reason, from the caller's env" \
  "save -S $TS_SOCK --wait 30s --reason freeze: auto" "$(cat "$T/moox-argv" 2>/dev/null)"
# ── G3-M2: namespace and trust follow the server TMUX_CMD addresses ──────────
# A scratch server whose socket is NAMED default is not the login default: its
# freeze namespace is Go's sockets.Key `default@<hex8(sha256(dir))>`, never
# the real store's `default`; and a key or participation env is trusted only
# for the server the commands really go to.
DEF="$T/default"
env -u TMUX SHELL=/bin/sh tmux -f /dev/null -S "$DEF" new-session -d -s s 'sleep 600' || no "scratch default server" "did not start"
TS_STARTED="${TS_STARTED:-} $DEF"
lib() { # <env…> -- <shell snippet>: run a snippet with cc_common.sh sourced, in a clean env
  local a=()
  while [ "$1" != -- ]; do a+=("$1"); shift; done; shift
  env -i HOME="$H" PATH="$TOOLS" "${a[@]}" bash -c ". '$SCRIPTS/lib/cc_common.sh'; $1"
}
hash="$(printf '%s' "$(cd "$T" && pwd -P)" | shasum -a 256 | cut -c1-8)"
hk_eq "M2: -S …/default (not the login default) → namespace default@<hash>, as Go's sockets.Key" \
  "default@$hash" "$(lib TMUX_CMD="tmux -S $DEF" -- _cc_socket_ns)"
hk_eq "M2: a key exported for ANOTHER server is not trusted when TMUX_CMD addresses this one" \
  "default@$hash" "$(lib MOOX_SOCKET="$TS_SOCK" MOOX_STATE_KEY=other-key TMUX="$TS_SOCK,1,0" TMUX_CMD="tmux -S $DEF" -- _cc_socket_ns)"
hk_eq "M2: the own key is trusted when TMUX_CMD addresses its server" \
  "own-key" "$(lib MOOX_SOCKET="$DEF" MOOX_STATE_KEY=own-key TMUX="$DEF,1,0" -- _cc_socket_ns)"
hk_eq "M2: participation env for server A, commands to server B → not participating" "no" \
  "$(lib MOOX_SOCKET="$TS_SOCK" MOOX_ACTIVE=1 MOOX_STATE_DIR="$OWN" TMUX="$TS_SOCK,1,0" TMUX_CMD="tmux -S $DEF" -- '_cc_participating && echo yes || echo no')"
hk_eq "M1: no MOOX_* on a server that is not the login default → not participating" "no" \
  "$(lib TMUX="$DEF,1,0" -- '_cc_participating && echo yes || echo no')"
hk_eq "M1: no MOOX_* on the literal login default socket (a string only) → participating" "yes" \
  "$(lib TMUX="$HK_LEGACY_SOCK,1,0" -- '_cc_participating && echo yes || echo no')"

if grep -n 'run-shell' "$SCRIPTS"/*.sh "$SCRIPTS"/lib/*.sh | grep -v ':[0-9]*:[[:space:]]*#' | grep -v '# .*run-shell' | grep -q .; then
  no "no tmux run-shell anywhere in the continuity scripts" "$(grep -n 'run-shell' "$SCRIPTS"/*.sh "$SCRIPTS"/lib/*.sh | grep -v ':[0-9]*:[[:space:]]*#')"
else ok "no tmux run-shell anywhere in the continuity scripts"; fi

hk_finish
