#!/bin/bash
# theme-blackbox.test.sh — per-screen light/dark, black box: spec.md FR1-FR5,
# NFR2, NFR6, AC1-AC5 through the public contract (architecture-contracts.md
# §4) only: theme.sh's verbs, exit codes and streams, the tmux options it
# publishes, the ~/.config/tmux files, and the Omarchy hook's calls under PATH
# stubs. Written without reading theme.sh, .tmux.conf or the hook; the plan
# with its coverage map is qa/test-plan.md.
#
#   bash tests/theme-blackbox.test.sh            every case
#   bash tests/theme-blackbox.test.sh B2 B9      only those
#   THEME_TEST_KEEP=1 bash tests/…               keep the scratch dir
#
# Exit: 0 all passed; 1 a case failed; 2 the harness could not run.
#
# SAFETY. This file does not re-implement the safety preamble: it LOADS the
# existing harness, tests/theme-per-screen.test.sh, unmodified on disk, with
# exactly two lines swapped in memory (HERE, which would resolve against the
# eval, and the trailing `main "$@"`), each checked to match exactly once.
# Everything the harness promises therefore holds here, from the same code:
#   - TMUX, TMUX_PANE, ENV, BASH_ENV unset before anything runs;
#     TMUX_TMPDIR=$D (a /tmp/tps.XXXXXX dir, so sockets stay < 104 bytes);
#   - every server call goes through ti()/to(): -S <private socket> checked by
#     sock_ok, -f test.conf or -f /dev/null, env -i with an allowlisted env;
#   - every OTHER tmux call (theme.sh, hook jobs, screens) meets the guarding,
#     logging wrapper first on PATH; a REFUSED line fails the case;
#   - /bin/sh panes, a throwaway HOME per case, the launch-dir diff in main;
#   - no run-shell and no kill-server outside kill_private.
# This file adds no tmux entry point of its own: it calls ti, to, ts and the
# screen_* helpers, and runs theme.sh only through CASE_ENV with TMUX naming a
# socket under the case dir. send-keys goes only to the OUTER server
# (screen_keys). The Omarchy case (B8) starts no server at all and reaches no
# real pkill, foot or tmux (PATH = stubs + an allowlist of system tools).

set -u
unset TMUX TMUX_PANE ENV BASH_ENV

BB_HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
BB_HARNESS="$BB_HERE/theme-per-screen.test.sh"
bb_die() { printf 'theme-blackbox: %s\n' "$*" >&2; exit 2; }
[ -f "$BB_HARNESS" ] || bb_die "the harness $BB_HARNESS is missing"

# bb_prelude: the harness text with HERE pinned to this directory and its
# `main "$@"` dropped. awk compares whole lines; exit 3 unless each of the two
# lines occurs exactly once.
bb_prelude() {
  awk -v here="$BB_HERE" -v q="'" '
    $0 == "HERE=$(cd \"$(dirname \"${BASH_SOURCE[0]}\")\" && pwd -P)" { print "HERE=" q here q; h++; next }
    $0 == "main \"$@\"" { m++; next }
    { print }
    END { if (h != 1 || m != 1) exit 3 }' "$BB_HARNESS"
}
BB_PRELUDE=$(bb_prelude) || bb_die "the harness no longer has exactly one HERE= line and one main \"\$@\" line; update bb_prelude"
case $BB_HERE in *"'"*) bb_die "this directory's path contains a quote" ;; esac
eval "$BB_PRELUDE"
unset BB_PRELUDE
[ "$HERE" = "$BB_HERE" ] || bb_die "HERE is [$HERE], want [$BB_HERE]"
for f in main case_setup case_teardown ti to ts ts_bg inner_start outer_start screen_add screen_set_mode \
         screen_detach screen_tty screen_id screen_status screen_capture screen_keys hub_ready settle \
         screens_now screens_are screens_want screens_from_status status_line_for status_line_has \
         painted_panes window_painted_as window_unpainted show_window_paint lead_of lead_is win_panes \
         sess_env sess_env_is typed theme_file screens_body log_lines since count_word log_refused \
         prompt_shown strip_sgr has_rgb poll hold check expect_eq ok no fail_now not sock_ok kill_private; do
  declare -F "$f" >/dev/null || bb_die "the harness no longer defines $f()"
done
unset f

# ---------------------------------------------------------------------------
# shared helpers (bb_ prefix: nothing of the harness is redefined)
# ---------------------------------------------------------------------------

# bb_call CMD...: run CMD with stdout and stderr kept apart.
BB_RC=0 BB_OUT='' BB_ERR=''
bb_call() {
  "$@" >"$C/bb.out" 2>"$C/bb.err"
  BB_RC=$?
  BB_OUT=$(cat "$C/bb.out")
  BB_ERR=$(cat "$C/bb.err")
}

# bb_silent NAME: the last bb_call exited 0 and printed nothing (§4.1: these
# verbs print nothing; theme.sh never prints tmux's own errors).
bb_silent() { expect_eq "$1: exit 0, nothing on stdout or stderr" "$BB_RC|$BB_OUT|$BB_ERR" "0||"; }

# bb_rejected NAME: exit 1, stdout empty, a message on stderr (§4.1).
bb_rejected() {
  if [ "$BB_RC" = 1 ] && [ -z "$BB_OUT" ] && [ -n "$BB_ERR" ]; then ok "$1: exit 1, stdout empty, a message on stderr"
  else no "$1: exit 1, stdout empty, a message on stderr" "rc [$BB_RC] stdout [$BB_OUT] stderr [$BB_ERR]"; fi
}

# bb_nosrv ARGS: theme.sh under test, as ts runs it (case HOME and PATH), but
# with TMUX naming a socket under the case dir that nothing listens on: "no
# reachable tmux server" (§4.1). The wrapper lets the attempt through (the
# path is under $D) and the real tmux finds no server.
bb_nosrv() { "${CASE_ENV[@]}" "TMUX=$C/nosrv,1,0" "$C/hm/.tmux/scripts/theme.sh" "$@"; }

BB_CFG() { printf '%s' "$C/hm/.config/tmux"; }

# bb_cfg_extra: entries of ~/.config/tmux other than the three files §4.1
# names (a temp file, a lock, anything else left behind).
# shellcheck disable=SC2010 # the names are ours (theme, theme.pinned, screens) or leftovers to report
bb_cfg_extra() { ls -A "$(BB_CFG)" 2>/dev/null | grep -vxE 'theme|theme\.pinned|screens' || true; }

# bb_screens_model FILE: the host -> mode map a §4.3 reader sees in FILE, one
# "host mode" per line, sorted: comments and blank lines skipped, the host
# lowercased, a MODE other than light|dark ignored, the last line wins.
# Fixtures in this file never use lines of three or more fields (AMBIGUOUS 3).
bb_screens_model() {
  [ -f "$1" ] || return 0
  awk '
    /^[ \t]*#/ || NF == 0 { next }
    NF == 2 && ($2 == "light" || $2 == "dark") { m[tolower($1)] = $2 }
    END { for (h in m) print h, m[h] }' "$1" | LC_ALL=C sort
}

# bb_screens_shape_why FILE: '' when FILE has the written shape of §4.3 (a
# one-line header comment, then lowercase `host mode` lines sorted by host, one
# per host, nothing else); else what is wrong.
bb_screens_shape_why() {
  local f=$1 hosts
  [ -f "$f" ] || { printf 'no file\n'; return; }
  [ "$(head -n 1 "$f" | cut -c1)" = '#' ] || printf 'line 1 is not a comment: [%s]\n' "$(head -n 1 "$f")"
  tail -n +2 "$f" | grep -vE '^[a-z0-9][a-z0-9._:-]{0,62} (light|dark)$' | sed 's/^/not a host-mode line: /'
  hosts=$(tail -n +2 "$f" | awk '{ print $1 }')
  [ "$hosts" = "$(printf '%s\n' "$hosts" | LC_ALL=C sort -u)" ] || printf 'hosts not sorted, or repeated: %s\n' "$(printf '%s' "$hosts" | tr '\n' ' ')"
}

# bb_hand TEXT: replace the screens file by hand, the way an editor or a sync
# tool would (write elsewhere, rename over it). TEXT goes through printf %b.
# A copy is kept as $C/hand.copy.
bb_hand() {
  printf '%b' "$1" > "$C/tmp/hand" && mv "$C/tmp/hand" "$(BB_CFG)/screens" && cp "$(BB_CFG)/screens" "$C/hand.copy"
}

# bb_no_forbidden NAME MARK: since MARK the wrapper logged no call that the
# contract forbids theme.sh (§4.1: never run-shell, never kills anything;
# NFR1: never send-keys). Used only over slices in which nothing but theme.sh
# (by hand or from a hook) calls tmux through the wrapper.
bb_no_forbidden() {
  local w n bad=''
  for w in run-shell kill-server kill-session kill-window kill-pane send-keys respawn-pane; do
    n=$(since "$2" | count_word "$w" '.')
    [ "$n" = 0 ] || bad="$bad $w=$n"
  done
  expect_eq "$1: no run-shell, kill-*, send-keys or respawn-pane call through tmux" "${bad:-none}" none
}

# bb_scope_paint W...: the four paint options at GLOBAL scope and at each W's
# WINDOW scope, one line each (§4.2: the feature never writes them there).
bb_scope_paint() {
  local o w
  for o in window-style window-active-style cursor-colour pane-colours; do
    printf 'global %s [%s]\n' "$o" "$(ti show -gwqv "$o" 2>/dev/null | tr '\n' ' ')"
    for w in "$@"; do printf '%s %s [%s]\n' "$w" "$o" "$(ti show -wqv -t "$w" "$o" 2>/dev/null | tr '\n' ' ')"; done
  done
}

# bb_views W: every non-control client of the hub displays window W.
bb_views() {
  [ "$(ti list-clients -F '#{client_control_mode} #{window_id}' | awk '$1 != 1 { print $2 }' | LC_ALL=C sort -u)" = "$1" ]
}

# bb_status_are STEP NAME=REST...: each screen's `theme.sh status` line is
# "<its tty> REST", and @theme_screens equals what status reports (§4.4: one
# resolver).
bb_status_are() {
  local step=$1 a n
  shift
  settle 5 || no "($step) the hub settles"
  for a in "$@"; do
    n=${a%%=*}
    expect_eq "($step) status: $n is ${a#*=}" "$(status_line_for "$n")" "$(screen_tty "$n") ${a#*=}"
  done
  expect_eq "($step) @theme_screens is what status reports" "$(screens_now)" "$(screens_from_status)"
}

# bb_default_and_mode_agree: @theme_mode = @theme_default, light or dark (§4.2).
bb_default_and_mode_agree() {
  local d m
  d=$(ti show -gqv @theme_default); m=$(ti show -gqv @theme_mode)
  case "$d" in light|dark) [ "$d" = "$m" ] ;; *) return 1 ;; esac
}

REMOTE_B='100.64.0.9 5 10.0.0.1 22'   # the tailscale stub names it host-b

# ---------------------------------------------------------------------------
# B1 (TEST-1..7): the CLI with no reachable server (§4.1's last rows), under
# the shell named by the entry (B1@sh, B1@dash).
# ---------------------------------------------------------------------------
case_B1() {
  local cfg v
  cfg=$(BB_CFG)

  say "  -- light, dark"
  bb_call bb_nosrv light
  bb_silent "light"
  expect_eq "light: theme.pinned and theme both hold light" "$(theme_file theme.pinned):$(theme_file theme)" light:light
  # shellcheck disable=SC2012 # a listing of our own config dir, compared as text
  expect_eq "light: ~/.config/tmux holds exactly theme and theme.pinned (no temp file)" \
    "$(ls -A "$cfg" | LC_ALL=C sort | tr '\n' ' ')" "theme theme.pinned "
  bb_call bb_nosrv dark
  bb_silent "dark"
  expect_eq "dark: theme.pinned and theme both hold dark" "$(theme_file theme.pinned):$(theme_file theme)" dark:dark
  bb_call bb_nosrv detect
  expect_eq "detect with a dark pin prints dark" "$BB_RC|$BB_OUT|$BB_ERR" "0|dark|"

  say "  -- toggle: no server, so the opposite of detect"
  bb_call bb_nosrv toggle
  bb_silent "toggle (pin dark)"
  expect_eq "toggle pins light" "$(theme_file theme.pinned):$(theme_file theme)" light:light
  bb_call bb_nosrv toggle /dev/ttys999
  bb_silent "toggle /dev/ttys999 (a client name, not attached)"
  expect_eq "... pins dark (the client is treated as absent)" "$(theme_file theme.pinned):$(theme_file theme)" dark:dark
  bb_call bb_nosrv toggle 'x;#{y}'
  bb_silent "toggle 'x;#{y}' (not a client name)"
  expect_eq "... pins light (treated as absent)" "$(theme_file theme.pinned):$(theme_file theme)" light:light

  say "  -- forget re-derives the default from nothing"
  bb_nosrv dark >/dev/null 2>&1
  expect_eq "setup: pin dark, last default dark" "$(theme_file theme.pinned):$(theme_file theme)" dark:dark
  bb_call bb_nosrv forget
  bb_silent "forget"
  check "forget: theme.pinned is gone" test ! -e "$cfg/theme.pinned"
  expect_eq "forget: theme = detect with no pin, no last default and no client: light" "$(theme_file theme)" light

  say "  -- detect's order without a server: pin, last default, light"
  printf 'dark\n' > "$cfg/theme"
  bb_call bb_nosrv detect
  expect_eq "detect with only theme=dark prints dark" "$BB_RC|$BB_OUT|$BB_ERR" "0|dark|"
  rm -f "$cfg/theme"
  bb_call bb_nosrv detect
  expect_eq "detect with no file at all prints light" "$BB_RC|$BB_OUT|$BB_ERR" "0|light|"

  say "  -- request and read-only verbs"
  for v in status sync follow refresh 'refresh s' menu 'menu /dev/ttys001' auto ''; do
    # shellcheck disable=SC2086  # the words of each verb line, split on purpose; '' = no verb
    bb_call bb_nosrv $v
    bb_silent "[${v:-no verb}] with no server"
  done

  say "  -- an unknown verb"
  bb_call bb_nosrv frobnicate
  bb_rejected "frobnicate"
  case $BB_ERR in *[Uu]sage*) ok "... stderr is the usage line" ;; *) no "... stderr is the usage line" "[$BB_ERR]" ;; esac

  say "  -- screen persists with no server"
  bb_call bb_nosrv screen host-b dark
  bb_silent "screen host-b dark"
  expect_eq "... a reader sees host-b dark" "$(bb_screens_model "$cfg/screens")" "host-b dark"
  expect_eq "nothing but theme, theme.pinned and screens is left in ~/.config/tmux" "$(bb_cfg_extra)" ''
  bb_no_forbidden "theme.sh with no server" 0
}

# ---------------------------------------------------------------------------
# B2 (TEST-8..10): the screens file as a READER sees it (§4.3), written by
# hand, judged through `theme.sh status` and @theme_screens.
# ---------------------------------------------------------------------------

# bb_reads LABEL TEXT WANT: with the file replaced by TEXT and one sync, C's
# status line is "<tty> WANT"; the readers left the file byte for byte.
bb_reads() {
  bb_hand "$2" || no "($1) the fixture was written"
  ts sync
  expect_eq "($1) C reads as [$3]" "$(status_line_for C)" "$(screen_tty C) $3"
  expect_eq "($1) @theme_screens agrees with status" "$(screens_now)" "$(screens_from_status)"
  check "($1) sync and status left the hand-written file byte for byte" cmp -s "$C/hand.copy" "$(BB_CFG)/screens"
}

case_B2() {
  local cfg mark n
  cfg=$(BB_CFG)
  printf 'light\n' > "$cfg/theme"     # the last default: light, so a declared dark stands out
  inner_start || fail_now "the inner server did not start"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add C silent "$REMOTE_B"
  hub_ready A=light C=light
  expect_eq "setup: C is host-b light default" "$(status_line_for C)" "$(screen_tty C) host-b light default"

  bb_reads plain            'host-b dark\n'                              'host-b dark host'
  bb_reads host-uppercase   'HOST-B dark\n'                              'host-b dark host'
  bb_reads comment-blank    '# host-b dark\n\n   \n'                     'host-b light default'
  bb_reads tab              'host-b\tdark\n'                             'host-b dark host'
  bb_reads spaces           'host-b    dark\n'                           'host-b dark host'
  bb_reads other-mode       'host-b purple\n'                            'host-b light default'
  bb_reads last-wins-light  'host-b dark\nhost-b light\n'                  'host-b light host'
  bb_reads last-wins-dark   'host-b light\nhost-b dark\n'                  'host-b dark host'
  bb_reads last-wins-case   'host-b light\nHOST-B dark\n'                  'host-b dark host'
  bb_reads ignored-not-last 'host-b dark\nhost-b purple\n'                 'host-b dark host'
  bb_reads exact-host       'host dark\nhost-b.lan dark\nhost-b2 dark\n'    'host-b light default'
  bb_reads comment-then     '# header\nhost-b dark\n# trailer\n'         'host-b dark host'

  # A `local` line is A's, not C's.
  bb_hand 'local dark\n'
  ts sync
  expect_eq "(local) A is local dark host" "$(status_line_for A)" "$(screen_tty A) local dark host"
  expect_eq "(local) C is untouched by it: host-b light default" "$(status_line_for C)" "$(screen_tty C) host-b light default"

  # No file at all: every host is auto.
  rm -f "$cfg/screens"
  ts sync
  expect_eq "(no file) C is host-b light default" "$(status_line_for C)" "$(screen_tty C) host-b light default"
  expect_eq "(no file) A is local light report" "$(status_line_for A)" "$(screen_tty A) local light report"

  # status is read-only (§4.4: no lock, no writes).
  bb_hand 'host-b dark\n'
  ts sync
  settle 5 || no "settles before the read-only check"
  mark=$(log_lines)
  ls -la "$cfg" > "$C/cfg.before"
  for n in 1 2 3; do ts status >/dev/null; done
  ls -la "$cfg" > "$C/cfg.after"
  n=$(since "$mark" | awk -F '\t' '$5 ~ /(^|,)(set-option|set-window-option|set-environment|source-file|refresh-client|set-hook)(,|$)/' | grep -c .)
  expect_eq "status x3: no set-option, set-environment, source-file, refresh-client or set-hook call" "$n" 0
  check "status x3: ~/.config/tmux is unchanged" cmp -s "$C/cfg.before" "$C/cfg.after"
  bb_no_forbidden "B2's syncs and statuses" "$mark"
}

# ---------------------------------------------------------------------------
# B3 (TEST-11..14): the screens file as the WRITER writes it (§4.1 `screen`,
# §4.3), with no server, under the shell named by the entry (B3@sh, B3@dash).
# ---------------------------------------------------------------------------
case_B3() {
  local cfg scr h63 h64 bad
  cfg=$(BB_CFG); scr="$cfg/screens"
  bb_w() { bb_call bb_nosrv screen "$@"; }

  say "  -- replace, sort, lowercase"
  bb_w host-b dark;     bb_silent "screen host-b dark"
  bb_w host-b light;    bb_silent "screen host-b light"
  expect_eq "a second write for host-b replaces its line" "$(screens_body)" "host-b light"
  bb_w omarchy dark;  bb_silent "screen omarchy dark"
  bb_w host-m light;  bb_silent "screen host-m light"
  expect_eq "three hosts, one line each, sorted by host" "$(screens_body)" "$(printf 'host-b light\nhost-m light\nomarchy dark')"
  bb_w Host-M dark;   bb_silent "screen Host-M dark"
  expect_eq "the host is lowercased: Host-M replaces host-m" "$(screens_body)" "$(printf 'host-b light\nhost-m dark\nomarchy dark')"
  bb_w host-b DARK;     bb_silent "screen host-b DARK"
  expect_eq "mode DARK is stored as dark" "$(screens_body)" "$(printf 'host-b dark\nhost-m dark\nomarchy dark')"
  expect_eq "the file has the written shape" "$(bb_screens_shape_why "$scr")" ''

  say "  -- host grammar: accepted at the edges"
  h63="h$(printf '0%.0s' $(seq 1 62))"
  h64="h$(printf '0%.0s' $(seq 1 63))"
  expect_eq "setup: the hosts are 63 and 64 characters" "${#h63}:${#h64}" 63:64
  bb_w "$h63" dark;              bb_silent "a 63-character host"
  bb_w 100.64.0.9 light;         bb_silent "an IPv4 host (a screen tailscale cannot name)"
  bb_w fd7a:115c:a1e0::9 dark;   bb_silent "an IPv6 host"
  # REQ (contract §4.3 HOST grammar ^[a-z0-9][a-z0-9._:-]{0,62}$): `_` is in
  # the class, so a_b is a valid host.
  bb_w a_b light;                bb_silent "a host with an underscore (a_b)"
  expect_eq "all four are stored, with the others" "$(bb_screens_model "$scr")" \
    "$(printf '%s\n' '100.64.0.9 light' 'a_b light' 'fd7a:115c:a1e0::9 dark' "$h63 dark" 'host-b dark' 'host-m dark' 'omarchy dark' | LC_ALL=C sort)"
  check "... and a_b has its own line in the file" grep -qx 'a_b light' "$scr"
  expect_eq "... and the file still has the written shape" "$(bb_screens_shape_why "$scr")" ''

  say "  -- rejected: exit 1, stderr, the file byte for byte"
  cp "$scr" "$C/before"
  # a+b and a@b: punctuation outside the HOST class [a-z0-9._:-].
  for bad in 'a+b dark' 'a@b dark' '.lead dark' 'x/y dark' "$h64 dark" 'café dark' 'host-b purple' 'host-b'; do
    # shellcheck disable=SC2086  # host and mode as two words, on purpose
    bb_w $bad
    bb_rejected "screen [$bad]"
    check "... leaves the file byte for byte" cmp -s "$C/before" "$scr"
  done
  bb_w 'ip ad' dark;  bb_rejected "screen ['ip ad'] dark (one argument with a space)"
  check "... leaves the file byte for byte" cmp -s "$C/before" "$scr"
  bb_w '' dark;       bb_rejected "screen [''] dark (an empty host)"
  check "... leaves the file byte for byte" cmp -s "$C/before" "$scr"

  say "  -- a write over a hand-edited file"
  bb_hand '# my own note\nHOST-B dark\n\nomarchy light\nhost-b light\nzz-old dark\n'
  bb_w host-m dark;   bb_silent "screen host-m dark over a hand-edited file"
  expect_eq "every other host survives as a reader sees it (HOST-B/host-b: the last line, light)" \
    "$(bb_screens_model "$scr")" "$(printf 'host-b light\nhost-m dark\nomarchy light\nzz-old dark')"
  expect_eq "... and the file has the written shape (one header, sorted lowercase entries)" "$(bb_screens_shape_why "$scr")" ''

  say "  -- auto"
  bb_hand 'HOST-B dark\nhost-b light\nomarchy dark\n'
  bb_w host-b auto;     bb_silent "screen host-b auto, with host-b listed twice in two cases"
  expect_eq "... leaves no host-b entry a reader would see" "$(bb_screens_model "$scr")" "omarchy dark"
  bb_w nosuch auto;   bb_silent "screen nosuch auto (a host that is not listed)"
  expect_eq "... changes no entry" "$(bb_screens_model "$scr")" "omarchy dark"
  rm -f "$scr"
  bb_w host-b auto;     bb_silent "screen host-b auto with no screens file"
  expect_eq "... leaves no entry" "$(bb_screens_model "$scr")" ''
  expect_eq "no temp file or lock is left in ~/.config/tmux" "$(bb_cfg_extra)" ''
  bb_no_forbidden "theme.sh screen with no server" 0
}

# ---------------------------------------------------------------------------
# B4 (TEST-15..17): FR1.1's order, pin > host > global bg > report > default,
# step by step on one reporting and one silent screen, with SOURCE.
# ---------------------------------------------------------------------------
case_B4() {
  local cfg m0 gws
  cfg=$(BB_CFG)
  printf 'light\n' > "$cfg/theme"
  inner_start || fail_now "the inner server did not start"
  outer_start || fail_now "the outer server did not start"
  screen_add B dark
  screen_add C silent "$REMOTE_B"
  hub_ready B=dark C=light
  gws=$(ti show -gwqv window-style)
  case $gws in
    *bg=default*|'') ;;
    *bg=*) fail_now "premise: the test config sets no global window-style bg" "[$gws]" ;;
  esac
  m0=$(log_lines)

  bb_status_are baseline            "B=local dark report"  "C=host-b light default"
  ti set -gw window-style 'bg=default';            ts sync
  bb_status_are "global bg=default" "B=local dark report"  "C=host-b light default"
  ti set -gw window-style 'fg=#ffffff';            ts sync
  bb_status_are "global fg only"    "B=local dark report"  "C=host-b light default"
  ti set -gw window-style 'bg=#ffffff';            ts sync
  bb_status_are "global bg #ffffff" "B=local light bg"     "C=host-b light bg"
  ti set -gw window-style 'fg=#eeeeee,bg=#000000'; ts sync
  bb_status_are "global bg #000000" "B=local dark bg"      "C=host-b dark bg"
  ts screen host-b light
  bb_status_are "host over bg"      "B=local dark bg"      "C=host-b light host"
  ts screen local light
  bb_status_are "host over bg (local)" "B=local light host" "C=host-b light host"
  ts dark
  bb_status_are "pin over host"     "B=local dark pin"     "C=host-b dark pin"
  ts forget
  # forget re-derives the last default from the screens (§4.2 detect); put it
  # back to light so the last step's `default` is known.
  printf 'light\n' > "$cfg/theme"
  ts sync
  bb_status_are "forget: host again" "B=local light host"  "C=host-b light host"
  ts screen local auto
  ts screen host-b auto
  bb_status_are "hosts auto: bg again" "B=local dark bg"   "C=host-b dark bg"
  ti set -gwu window-style;                        ts sync
  bb_status_are "global style unset" "B=local dark report" "C=host-b light default"
  bb_no_forbidden "the ladder" "$m0"
}

# ---------------------------------------------------------------------------
# B5 (TEST-18..22): the published options. @theme_mode = @theme_default;
# a terminal flip recomputes on its own; detect's third rule; a control-mode
# client is not a screen; the empty set.
# ---------------------------------------------------------------------------
bb_default_is() { [ "$(ti show -gqv @theme_default)" = "$1" ]; }
bb_painted_none() { [ "$(painted_panes)" = 0 ]; }
# bb_ctl FIELD: FIELD (a format variable name) of the first control-mode client.
bb_ctl() { ti list-clients -F "#{client_control_mode} #{$1}" 2>/dev/null | awk '$1 == 1 { sub(/^1 /, ""); print; exit }'; }
bb_ctl_attached() { [ -n "$(bb_ctl client_name)" ]; }
# bb_no_tty_in OPTION: the global OPTION names no /dev/ tty.
bb_no_tty_in() { case $(ti show -gqv "$1" 2>/dev/null) in *"/dev/"*) return 1 ;; esac; return 0; }

case_B5() {
  local kname ktty
  inner_start || fail_now "the inner server did not start"
  ti split-window -t s: /bin/sh
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  check "@theme_mode = @theme_default, light or dark" bb_default_and_mode_agree

  say "  -- a terminal flip recomputes with no CLI call (client-light-theme / client-dark-theme)"
  screen_set_mode B light || no "B's terminal turns light"
  if poll 3 screens_are "$(screens_want A=light B=light)"; then ok "@theme_screens lists B light within 3 s"
  else no "@theme_screens lists B light within 3 s" "[$(screens_now)]"; fi
  if poll 3 status_line_has B 48 "$L_MANTLE"; then ok "... and B's bar is light mantle"
  else no "... and B's bar is light mantle" "$(screen_status B | cat -v)"; fi
  screen_set_mode B dark || no "B's terminal turns dark again"
  if poll 3 screens_are "$(screens_want A=light B=dark)"; then ok "@theme_screens lists B dark again within 3 s"
  else no "@theme_screens lists B dark again within 3 s" "[$(screens_now)]"; fi
  if poll 3 status_line_has B 48 "$D_MANTLE"; then ok "... and B's bar is dark mantle"
  else no "... and B's bar is dark mantle" "$(screen_status B | cat -v)"; fi
  settle 8 || no "settles"

  say "  -- detect's third rule: forget takes the most recently active screen's report"
  sleep 1.1; typed B; settle 8 || no "settles"
  bb_call ts forget
  bb_silent "forget (B typed last)"
  expect_eq "... theme = dark" "$(theme_file theme)" dark
  bb_call ts detect
  expect_eq "... detect prints dark" "$BB_RC|$BB_OUT" "0|dark"
  if poll 3 bb_default_is dark; then ok "... @theme_default is dark"; else no "... @theme_default is dark" "[$(ti show -gqv @theme_default)]"; fi
  check "... @theme_mode = @theme_default" bb_default_and_mode_agree
  sleep 1.1; typed A; settle 8 || no "settles"
  bb_call ts forget
  bb_silent "forget (A typed last)"
  expect_eq "... theme = light" "$(theme_file theme)" light
  bb_call ts detect
  expect_eq "... detect prints light" "$BB_RC|$BB_OUT" "0|light"
  if poll 3 bb_default_is light; then ok "... @theme_default is light"; else no "... @theme_default is light" "[$(ti show -gqv @theme_default)]"; fi
  check "... @theme_mode = @theme_default" bb_default_and_mode_agree
  settle 8 || no "settles"

  say "  -- a control-mode client is not a screen"
  to new-window -d -t scr: -n K /bin/sh || fail_now "the outer window for the control client"
  to respawn-pane -k -t scr:K "exec env -u TMUX tmux -S $IS -C attach -t s" || fail_now "start the control client"
  poll 8 bb_ctl_attached || fail_now "the control-mode client attached" \
    "$(ti list-clients -F '#{client_name} #{client_tty} ctl=#{client_control_mode}')"
  kname=$(bb_ctl client_name); ktty=$(bb_ctl client_tty)
  ts sync
  settle 8 || no "settles"
  expect_eq "@theme_screens lists exactly A and B" "$(screens_now)" "$(screens_want A=light B=dark)"
  if [ -n "$ktty" ]; then
    case $(screens_now) in *" $ktty="*) no "... the control client's tty $ktty is not in it" ;; *) ok "... the control client's tty $ktty is not in it" ;; esac
  fi
  expect_eq "status prints one line per screen (2)" "$(ts status | grep -c .)" 2

  say "  -- no screens left"
  ti detach-client -t "$kname" || no "the control client detaches"
  screen_detach A
  screen_detach B
  if poll 5 screens_are ' '; then ok "@theme_screens is \" \""; else no "@theme_screens is \" \"" "[$(screens_now)]"; fi
  if poll 3 bb_no_tty_in @theme_hosts; then ok "@theme_hosts names no tty"; else no "@theme_hosts names no tty" "[$(ti show -gqv @theme_hosts)]"; fi
  if poll 3 bb_no_tty_in @theme_remote; then ok "@theme_remote names no tty"; else no "@theme_remote names no tty" "[$(ti show -gqv @theme_remote)]"; fi
  if poll 3 bb_painted_none; then ok "no pane is painted"; else no "no pane is painted" "$(painted_panes) painted"; fi
  check "@theme_default and @theme_mode are still published and equal" bb_default_and_mode_agree
}

# ---------------------------------------------------------------------------
# B6 (TEST-23..27): the session environment (FR3.6, §4.2) for agreeing and
# declared screens, an unattached session, the global env, and new panes.
# ---------------------------------------------------------------------------
case_B6() {
  local cfg
  cfg=$(BB_CFG)
  printf 'light\n' > "$cfg/theme"
  inner_start s || fail_now "the inner server did not start"
  ti new-session -d -s s2 -x 160 -y 40 /bin/sh
  ti new-session -d -s u -x 160 -y 40 /bin/sh
  outer_start || fail_now "the outer server did not start"
  screen_add A dark "" s
  screen_add B dark "" s
  screen_add C silent "$REMOTE_B" s2
  ts screen host-b dark
  hub_ready A=dark B=dark C=dark

  say "  -- agreeing dark screens on s"
  if poll 3 sess_env_is s 'dark:15;0'; then ok "s: TERM_THEME=dark, COLORFGBG=15;0"
  else no "s: TERM_THEME=dark, COLORFGBG=15;0" "[$(sess_env s TERM_THEME):$(sess_env s COLORFGBG)]"; fi
  if hold 1 bb_painted_none; then ok "nothing is painted (NFR6)"; else no "nothing is painted (NFR6)" "$(painted_panes) painted"; fi

  say "  -- a declared-only screen on s2"
  if poll 3 sess_env_is s2 'dark:15;0'; then ok "s2 (C, silent, host-b dark): TERM_THEME=dark, COLORFGBG=15;0"
  else no "s2: TERM_THEME=dark, COLORFGBG=15;0" "[$(sess_env s2 TERM_THEME):$(sess_env s2 COLORFGBG)]"; fi

  say "  -- an unattached session, and the global environment"
  expect_eq "u (no client) carries no TERM_THEME or COLORFGBG of its own" "$(sess_env u TERM_THEME):$(sess_env u COLORFGBG)" ":"
  case $(ti show-environment -g TERM_THEME 2>/dev/null) in
    TERM_THEME=light|TERM_THEME=dark) ok "the global env has TERM_THEME light or dark" ;;
    *) no "the global env has TERM_THEME light or dark" "[$(ti show-environment -g TERM_THEME 2>&1)]" ;;
  esac
  ti set-environment -gu COLORFGBG 2>/dev/null
  ts sync
  expect_eq "sync writes no global COLORFGBG (global: TERM_THEME only)" "$(ti show-environment -g COLORFGBG 2>/dev/null)" ''

  say "  -- both screens of s turn light"
  screen_set_mode A light || no "A's terminal turns light"
  screen_set_mode B light || no "B's terminal turns light"
  if poll 5 sess_env_is s 'light:0;15'; then ok "s: TERM_THEME=light, COLORFGBG=0;15"
  else no "s: TERM_THEME=light, COLORFGBG=0;15" "[$(sess_env s TERM_THEME):$(sess_env s COLORFGBG)]"; fi
  settle 8 || no "settles"
  if poll 3 bb_painted_none; then ok "they agree again: nothing is painted"; else no "they agree again: nothing is painted" "$(painted_panes) painted"; fi
  check "s2 is still dark" sess_env_is s2 'dark:15;0'
  ti set-environment -t s TERM_THEME dark
  ts sync
  if poll 3 sess_env_is s 'light:0;15'; then ok "a foreign TERM_THEME=dark on s is put back to light"
  else no "a foreign TERM_THEME=dark on s is put back to light" "[$(sess_env s TERM_THEME):$(sess_env s COLORFGBG)]"; fi
  settle 8 || no "settles"

  say "  -- new panes inherit their session's values"
  ti split-window -d -t s: "printf '%s:%s' \"\$TERM_THEME\" \"\$COLORFGBG\" > $C/np-s.env; exec sleep 60"
  ti split-window -d -t s2: "printf '%s:%s' \"\$TERM_THEME\" \"\$COLORFGBG\" > $C/np-s2.env; exec sleep 60"
  poll 3 test -s "$C/np-s.env" >/dev/null || true
  poll 3 test -s "$C/np-s2.env" >/dev/null || true
  expect_eq "a new pane in s sees light:0;15" "$(cat "$C/np-s.env" 2>/dev/null)" 'light:0;15'
  expect_eq "a new pane in s2 sees dark:15;0" "$(cat "$C/np-s2.env" 2>/dev/null)" 'dark:15;0'
}

# ---------------------------------------------------------------------------
# B7 (TEST-28..33): paint (FR3) driven by a DECLARED screen and by CLI pins,
# where the existing harness drives it by reports and keys. The paint records
# are the harness's own copy of §5.8 (window_painted_as / paint_want).
# ---------------------------------------------------------------------------
bb_current_is_not() { [ "$(ti display -p -t s: '#{window_id}')" != "$1" ]; }

case_B7() {
  local cfg w0 w1 ida idc before
  cfg=$(BB_CFG)
  printf 'light\n' > "$cfg/theme"
  inner_start || fail_now "the inner server did not start"
  ti split-window -t s: /bin/sh
  w0=$(ti display -p -t s: '#{window_id}')
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add C silent "$REMOTE_B"
  hub_ready A=light C=light
  ida=$(screen_id A); idc=$(screen_id C)
  before=$(bb_scope_paint "$w0")

  say "  -- (1) a reporting screen and a defaulted silent one agree"
  if hold 1 bb_painted_none; then ok "(1) nothing is painted"; else no "(1) nothing is painted" "$(show_window_paint "$w0")"; fi

  say "  -- (2) screen host-b dark mixes them"
  bb_call ts screen host-b dark
  bb_silent "(2) screen host-b dark"
  poll 3 bb_views "$w0" || no "(2) both screens view W0" "$(ti list-clients -F '#{client_tty} #{window_id}')"
  expect_eq "(2) @theme_lead(W0) is C, the screen attached last" "$(lead_of "$w0")" "$idc"
  if poll 3 window_painted_as "$w0" dark; then ok "(2) both W0 panes carry exactly the dark record and the four dark values at pane level"
  else no "(2) both W0 panes are painted dark" "$(show_window_paint "$w0")"; fi
  expect_eq "(2) exactly W0's two panes are painted" "$(painted_panes)" 2
  expect_eq "(2) no paint value at global or W0's window scope" "$(bb_scope_paint "$w0")" "$before"
  settle 8 || no "(2) settles"

  say "  -- (3) A types"
  sleep 1.1
  typed A
  if poll 3 lead_is "$w0" "$ida"; then ok "(3) the lead becomes A"; else no "(3) the lead becomes A" "[$(lead_of "$w0")]"; fi
  if poll 3 window_painted_as "$w0" light; then ok "(3) W0 is repainted light"; else no "(3) W0 is repainted light" "$(show_window_paint "$w0")"; fi
  settle 8 || no "(3) settles"

  say "  -- (4) a pin unmixes"
  bb_call ts dark
  bb_silent "(4) dark"
  check "(4) both screens are dark" screens_are "$(screens_want A=dark C=dark)"
  if poll 3 bb_painted_none; then ok "(4) nothing is painted while pinned"; else no "(4) nothing is painted while pinned" "$(show_window_paint "$w0")"; fi
  settle 8 || no "(4) settles"

  say "  -- (5) forget mixes them again"
  bb_call ts forget
  bb_silent "(5) forget"
  if poll 3 screens_are "$(screens_want A=light C=dark)"; then ok "(5) A light by its report, C dark by its host"
  else no "(5) A light, C dark" "[$(screens_now)]"; fi
  if poll 3 window_painted_as "$w0" light; then ok "(5) W0 is painted light again: A, still a viewer, still leads"
  else no "(5) W0 is painted light again" "$(show_window_paint "$w0")"; fi
  settle 8 || no "(5) settles"

  say "  -- (6) C opens a window"
  screen_keys C C-b c
  poll 3 bb_current_is_not "$w0" || fail_now "(6) C-b c on C made a new current window"
  w1=$(ti display -p -t s: '#{window_id}')
  if poll 3 bb_views "$w1"; then ok "(6) both screens view the new window W1"; else no "(6) both screens view W1" "$(ti list-clients -F '#{client_tty} #{window_id}')"; fi
  if poll 3 lead_is "$w1" "$idc"; then ok "(6) @theme_lead(W1) is C, the screen that opened it"; else no "(6) @theme_lead(W1) is C" "[$(lead_of "$w1")]"; fi
  if poll 3 window_painted_as "$w1" dark; then ok "(6) W1 is painted dark"; else no "(6) W1 is painted dark" "$(show_window_paint "$w1")"; fi
  if poll 3 window_unpainted "$w0"; then ok "(6) W0, which nobody views now, is unpainted"; else no "(6) W0 is unpainted" "$(show_window_paint "$w0")"; fi
  settle 8 || no "(6) settles"

  say "  -- (7) screen host-b auto unmixes"
  bb_call ts screen host-b auto
  bb_silent "(7) screen host-b auto"
  if poll 3 bb_painted_none; then ok "(7) nothing is painted anywhere"; else no "(7) nothing is painted anywhere" "$(show_window_paint "$w1")"; fi
  expect_eq "(7) the global and W0 window-level paint values are as before" "$(bb_scope_paint "$w0")" "$before"
  expect_eq "(7) W1 has no window-level paint value" "$(bb_scope_paint "$w1" | grep "^$w1 " | grep -v '\[\]$')" ''
  expect_eq "no REFUSED call" "$(log_refused "$C")" 0
}

# ---------------------------------------------------------------------------
# B9 (TEST-41..46): chrome per client (FR2.1-FR2.3, AC1, AC4) with a light,
# a dark and a declared-dark silent screen on one session.
# ---------------------------------------------------------------------------
BB_STYLE_OPTS='status-style pane-border-style pane-active-border-style popup-style popup-border-style menu-style menu-selected-style menu-border-style window-status-activity-style window-status-bell-style'
BB_SESSION_LEGACY='^(@thm_[a-z0-9_]+|status-style|message-style|message-command-style|@theme_mode) '
BB_WINDOW_FORBIDDEN='^(mode-style|popup-style|popup-border-style|menu-selected-style|pane-border-style|pane-active-border-style|window-status-activity-style|window-status-bell-style|clock-mode-colour|window-style|window-active-style|cursor-colour|pane-colours)( |\[|$)'

# bb_opt OPTION: the option's global value, whichever global table holds it.
bb_opt() {
  local v
  v=$(ti show -gqv "$1" 2>/dev/null)
  [ -n "$v" ] || v=$(ti show -gwqv "$1" 2>/dev/null)
  printf '%s' "$v"
}

case_B9() {
  local ta tb tc opt raw fa fb fc v ms sa sb sc line bgs pp x
  inner_start || fail_now "the inner server did not start"
  ti new-window -t s: /bin/sh
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  screen_add C silent "$REMOTE_B"
  ts screen host-b dark
  hub_ready A=light B=dark C=dark
  ta=$(screen_tty A); tb=$(screen_tty B); tc=$(screen_tty C)

  say "  -- @theme_dark per client"
  expect_eq "A (reports light) draws with @theme_dark 0" "$(ti display -p -c "$ta" '#{E:@theme_dark}')" 0
  expect_eq "B (reports dark) draws with @theme_dark 1" "$(ti display -p -c "$tb" '#{E:@theme_dark}')" 1
  expect_eq "C (silent, host-b dark by host) draws with @theme_dark 1" "$(ti display -p -c "$tc" '#{E:@theme_dark}')" 1

  say "  -- three bars at once"
  poll 5 status_line_has C 48 "$D_MANTLE" >/dev/null || true
  sa=$(screen_status A); sb=$(screen_status B); sc=$(screen_status C)
  if has_rgb "$sa" 48 "$L_MANTLE" && ! has_rgb "$sa" 48 "$D_MANTLE"; then ok "A's bar is light mantle only"
  else no "A's bar is light mantle only" "$(printf '%s' "$sa" | cat -v | cut -c1-160)"; fi
  if has_rgb "$sb" 48 "$D_MANTLE" && ! has_rgb "$sb" 48 "$L_MANTLE"; then ok "B's bar is dark mantle only"
  else no "B's bar is dark mantle only" "$(printf '%s' "$sb" | cat -v | cut -c1-160)"; fi
  if has_rgb "$sc" 48 "$D_MANTLE" && ! has_rgb "$sc" 48 "$L_MANTLE"; then ok "C's bar is dark mantle only"
  else no "C's bar is dark mantle only" "$(printf '%s' "$sc" | cat -v | cut -c1-160)"; fi

  say "  -- the ten FR2.2 style options, per client"
  for opt in $BB_STYLE_OPTS; do
    raw=$(bb_opt "$opt")
    if [ -z "$raw" ]; then no "$opt is set globally"; continue; fi
    fa=$(ti display -p -c "$ta" "$raw"); fb=$(ti display -p -c "$tb" "$raw"); fc=$(ti display -p -c "$tc" "$raw")
    case "$fa$fb$fc" in
      *'#{'*) no "$opt expands fully for A, B and C" "A: $fa | B: $fb | C: $fc" ;;
      *) ok "$opt expands fully for A, B and C" ;;
    esac
    if [ "$fa" != "$fb" ]; then ok "$opt: A (light) and B (dark) get different styles"
    else no "$opt: A (light) and B (dark) get different styles" "both [$fa]"; fi
    expect_eq "$opt: C (declared dark) gets B's style" "$fc" "$fb"
  done

  say "  -- FR2.3 theme-neutral literals"
  for opt in message-style message-command-style mode-style; do
    v=$(bb_opt "$opt")
    case $v in
      '') no "$opt is set" ;;
      *'#{'*) no "$opt is a literal, not a format" "[$v]" ;;
      *) ok "$opt is a literal, not a format" ;;
    esac
    case $v in *'#d79921'*) ok "$opt carries amber #d79921" ;; *) no "$opt carries amber #d79921" "[$v]" ;; esac
    case $v in *'#1d2021'*) ok "$opt carries ink #1d2021" ;; *) no "$opt carries ink #1d2021" "[$v]" ;; esac
  done
  for opt in message-style message-command-style; do
    v=$(bb_opt "$opt")
    case $v in *fill=*) ok "$opt keeps fill=" ;; *) no "$opt keeps fill=" "[$v]" ;; esac
    case $v in *align=*) no "$opt has no align=" "[$v]" ;; *) ok "$opt has no align=" ;; esac
  done
  expect_eq "clock-mode-colour is the neutral #458588" "$(bb_opt clock-mode-colour)" '#458588'
  # FR2.3 "copy-mode-*-style if set": a set value must be a literal. tmux's own
  # default for copy-mode-position-style and copy-mode-selection-style is
  # #{E:mode-style}; it is accepted only verbatim, only while mode-style is
  # itself a literal, and only if it resolves to that literal for the light, the
  # dark and the declared-dark client. Any other format fails.
  ms=$(bb_opt mode-style)
  for x in $( { ti show -g; ti show -gw; } 2>/dev/null | awk '$1 ~ /^copy-mode-.*-style$/ { print $1 }' | LC_ALL=C sort -u); do
    v=$(bb_opt "$x")
    case $v in
      *'#{'*) ;;
      *) ok "$x is a literal, not a format"; continue ;;
    esac
    if [ "$v" != '#{E:mode-style}' ]; then
      no "$x is a literal, or exactly tmux's default #{E:mode-style}" "[$v]"; continue
    fi
    case $ms in
      ''|*'#{'*) no "$x is tmux's default #{E:mode-style} over a literal mode-style" "mode-style [$ms]"; continue ;;
    esac
    fa=$(ti display -p -c "$ta" "$v"); fb=$(ti display -p -c "$tb" "$v"); fc=$(ti display -p -c "$tc" "$v")
    if [ "$fa" = "$ms" ] && [ "$fb" = "$ms" ] && [ "$fc" = "$ms" ]; then
      ok "$x is tmux's default #{E:mode-style}: the neutral mode-style literal for A, B and C"
    else
      no "$x is tmux's default #{E:mode-style}: the neutral mode-style literal for A, B and C" "mode-style [$ms] | A: $fa | B: $fb | C: $fc"
    fi
  done

  say "  -- the command prompt on C (silent, declared dark)"
  ti command-prompt -b -t "$tc" & pp=$!
  if poll 3 prompt_shown C; then
    line=$(to capture-pane -p -e -N -t scr:C | tail -n 1)
    bgs=$(printf '%s' "$line" | grep -o '48;2;[0-9]*;[0-9]*;[0-9]*' | sort -u | tr '\n' ' ')
    expect_eq "C's prompt line: its only background is amber" "$bgs" "48;2;$AMBER "
  else
    no "C shows the prompt" "$(screen_capture C | strip_sgr | tail -n 2)"
  fi
  screen_keys C Escape
  wait "$pp" 2>/dev/null
  check "Escape dismisses C's prompt" poll 3 not prompt_shown C

  say "  -- after a lead flip: nothing per session, nothing at window level"
  settle 8 || no "settles"
  typed A
  x=$(ti display -p -t s: '#{window_id}')
  if poll 3 window_painted_as "$x" light; then ok "A types: the current window is repainted light"
  else no "A types: the current window is repainted light" "$(show_window_paint "$x")"; fi
  settle 8 || no "settles"
  for x in $(ti list-sessions -F '#{session_id}'); do
    expect_eq "session $x: no @thm_*, status-style, message-style, message-command-style or @theme_mode of its own" \
      "$(ti show -t "$x" 2>/dev/null | grep -E "$BB_SESSION_LEGACY")" ''
  done
  for x in $(ti list-windows -a -F '#{window_id}'); do
    expect_eq "window $x: no legacy window-level style and no window-level paint value" \
      "$(ti show -w -t "$x" 2>/dev/null | grep -E "$BB_WINDOW_FORBIDDEN")" ''
  done
}

# B8 (the Omarchy hook, TEST-34..40) is not here: it tests omarchy/…, which
# is the dotfiles', not moox's, and stays covered by the dotfiles'
# tests/omarchy-term-theme.test.sh (architecture §10 step 5).

# ---------------------------------------------------------------------------
# runner: the harness's own main, over this file's cases. H0 first: the
# harness's guards, each watched failing, in this same run. B1 and B3 run
# theme.sh under sh and under dash (NFR4).
# ---------------------------------------------------------------------------
# shellcheck disable=SC2034 # read by the harness's main
CASES=(H0 B1@sh B1@dash B2 B3@sh B3@dash B4 B5 B6 B7 B9)

main "$@"
