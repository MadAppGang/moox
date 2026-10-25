#!/bin/bash
# theme-per-screen.test.sh — per-screen light/dark (architecture.md §8), tested
# on PRIVATE tmux servers only.
#
#   bash tests/theme-per-screen.test.sh             every registered case
#   bash tests/theme-per-screen.test.sh H0 T0       only those
#   SUT=dash bash tests/theme-per-screen.test.sh    cases that name no shell run
#                                                   theme.sh under dash
#   THEME_TEST_KEEP=1 bash tests/…                  keep $D for inspection (the
#                                                   servers are still killed)
#
# Exit: 0 every case passed; 1 a case failed; 2 the harness could not run (no
# Catppuccin, no dash, a missing region marker, a screen that failed
# calibration). 2 is never a skip: it stops the run and says why.
#
# Adding a case: write case_<ID>() below the existing ones and append <ID> to
# CASES at the bottom. A case that must run under both shells is registered
# twice, as <ID>@sh and <ID>@dash; inside it, $CSUT says which. Every case gets
# a fresh $C (its dir), $IS/$OS (inner/outer sockets) and HOME; start what it
# needs with inner_start / outer_start / screen_add, assert with ok/no/check/
# expect_eq, and let the runner tear it down.
#
# ############################################################################
# #  SAFETY. Every rule below exists because breaking it has cost this        #
# #  machine its tmux workspace or damaged the state that restores it.        #
# ############################################################################
#
#  1. The harness never runs a bare `tmux`. Its own calls go through ti() (the
#     inner server, -S $C/i -f $C/test.conf) and to() (the outer server,
#     -S $C/o -f /dev/null), which run the real tmux by absolute path and check
#     the socket first: under $D, basename not `default`, shorter than 104
#     bytes. A scratch server that read ~/.tmux.conf once cloned the whole
#     workspace (2026-08-17), and `-L ""` resolves to the DEFAULT server.
#  2. Everything ELSE that calls tmux — hook jobs, Catppuccin, theme.sh, the
#     screens — finds $C/bin/tmux first on PATH: the logging wrapper
#     (tests/theme-harness-tmux.sh), which refuses any call not aimed at a
#     socket under $D. A REFUSED line in any case's log fails the run.
#  3. No pane of a private server runs the user's zsh. The user's zshrc sources
#     tmux-claude-continuity, whose precmd deletes
#     ~/.config/tmux-claude/launch/<N> keyed only by the pane NUMBER, and
#     private panes are numbered %0, %1, … like the real ones (eight live
#     recordings were lost that way on 2026-09-19). So both servers start with
#     SHELL=/bin/sh, default-shell /bin/sh, an explicit /bin/sh first pane, a
#     throwaway HOME and no ENV/BASH_ENV, and the launch dir is diffed before
#     and after the run.
#  4. The harness never issues `run-shell` (it segfaulted tmux 3.7b and killed
#     16 sessions, 2026-08-16). The only run-shell lines on a private server
#     are the silenced ones in test.conf, and lint_test_conf proves they are.
#     send-keys goes only to the OUTER private server.
#  5. Every server is started through `env -i` with an allowlisted
#     environment, so nothing of the calling session leaks in: not $TMUX (the
#     live hub), not SSH_CONNECTION (a later case would read the screens as
#     remote), not kcenv secrets.
#  6. TMUX_TMPDIR=$D, so a stray plain `tmux` resolves to a private default
#     socket, never /private/tmp/tmux-501/default.

set -u

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
REPO=$(cd "$HERE/.." && pwd -P)
# moox: the theme regions live in two conf files now — pre-plugin in
# options.conf (before the plugins), formats and apply in theme.conf — and the
# status line in status.conf. CONF_SRC names them for messages only.
OPTS_SRC="$REPO/share/moox/conf/options.conf"
THEME_SRC="$REPO/share/moox/conf/theme.conf"
STATUS_SRC="$REPO/share/moox/conf/status.conf"
CONF_SRC="$OPTS_SRC + $THEME_SRC"
SCRIPTS_SRC="$REPO/share/moox/scripts"
# Fixtures of the theme.sh/.tmux.conf a server ran before the per-screen design
# (dotfiles commit 6e925af), vendored because that history is not in moox.
FIX_6E925AF="$HERE/fixtures/theme-6e925af"
WRAPPER_SRC="$HERE/theme-harness-tmux.sh"

say()  { printf '%s\n' "$*"; }
die2() { printf 'theme-per-screen: %s\n' "$*" >&2; exit 2; }

now_ms() {
  local t
  t=$(date +%s%N)
  case $t in
    *N) perl -MTime::HiRes=time -e 'printf "%d", time() * 1000' ;;
    *) printf '%s' "${t%??????}" ;;
  esac
}

# ---------------------------------------------------------------------------
# 1. safety preamble (§8.1). Everything the real HOME provides is resolved
#    here, once, before any case changes HOME for its servers.
# ---------------------------------------------------------------------------

unset TMUX TMUX_PANE ENV BASH_ENV
REAL_HOME=$HOME
REAL_TMUX=$(command -v tmux 2>/dev/null) || die2 "tmux not found"
case $REAL_TMUX in /*) ;; *) die2 "tmux resolved to '$REAL_TMUX', not an absolute path" ;; esac
REAL_CATPPUCCIN="$REPO/share/moox/plugins/catppuccin-tmux"   # the vendored copy moox ships
[ -f "$REAL_CATPPUCCIN/catppuccin.tmux" ] || die2 "Catppuccin plugin not found at $REAL_CATPPUCCIN"
DASH=$(command -v dash 2>/dev/null || true)
BASE_PATH=$PATH
[ -f "$WRAPPER_SRC" ] || die2 "the logging wrapper $WRAPPER_SRC is missing"
for f in "$OPTS_SRC" "$THEME_SRC" "$STATUS_SRC"; do [ -f "$f" ] || die2 "$f is missing"; done
[ -d "$SCRIPTS_SRC" ] || die2 "$SCRIPTS_SRC is missing"

LAUNCH_DIR="$REAL_HOME/.config/tmux-claude/launch"
launch_snapshot() { ls -la "$LAUNCH_DIR" 2>&1; }
LAUNCH_BEFORE=$(launch_snapshot)

# /tmp, not $TMPDIR: macOS caps a unix socket path at 104 bytes and $TMPDIR is
# a long /var/folders path. Resolved physically ONCE, because tmux keeps an -S
# path exactly as given (measured, 3.7c) and the wrapper compares strings.
D=$(mktemp -d /tmp/tps.XXXXXX) || die2 "mktemp failed"
D=$(cd "$D" && pwd -P)
export TMUX_TMPDIR="$D"

# sock_ok PATH: the only sockets any tmux call of this harness may name.
sock_ok() {
  local s=$1
  [ -n "$s" ] || return 1
  case $s in "$D"/*) ;; *) return 1 ;; esac
  case $s in */../*|*/..) return 1 ;; esac
  [ "${s##*/}" != default ] || return 1
  [ "${#s}" -lt 104 ] || return 1
}

# kill_private SOCKET: end a private server, only on a socket that passes
# sock_ok, and never with kill-server: every session is killed and tmux exits
# on its own (exit-empty).
kill_private() {
  local s=$1 sid
  if ! sock_ok "$s"; then
    printf 'theme-per-screen: NOT killing %s: not a private socket\n' "$s" >&2
    return 1
  fi
  [ -S "$s" ] || return 0
  for sid in $(env -i PATH=/usr/bin:/bin TMUX_TMPDIR="$D" "$REAL_TMUX" -S "$s" -f /dev/null \
      list-sessions -F '#{session_id}' 2>/dev/null); do
    env -i PATH=/usr/bin:/bin TMUX_TMPDIR="$D" "$REAL_TMUX" -S "$s" -f /dev/null \
      kill-session -t "$sid" >/dev/null 2>&1
  done
  return 0
}

cleanup() {
  local s
  for s in "$D"/*/i "$D"/*/o; do
    [ -S "$s" ] && kill_private "$s"
  done
  if [ -n "${THEME_TEST_KEEP:-}" ]; then
    say "kept: $D"
  else
    rm -rf "$D"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---------------------------------------------------------------------------
# 2. assertions. A case runs in a subshell: `no` records a failure and goes
#    on, fail_now ends the case, die2 ends the run.
# ---------------------------------------------------------------------------

CASE_FAILS=0
ok()       { printf '  ok   %s\n' "$1"; }
no()       { CASE_FAILS=$((CASE_FAILS + 1)); printf '  FAIL %s\n' "$1"; [ -z "${2:-}" ] || printf '%s\n' "$2" | sed 's/^/       /'; }
fail_now() { no "$@"; exit 1; }

# check NAME CMD...: CMD must succeed.
check() {
  local name=$1
  shift
  if "$@"; then ok "$name"; else no "$name" "command failed: $*"; fi
}

# not CMD...: succeeds when CMD fails (for check).
not() { ! "$@"; }

# count_files GLOB-RESULTS...: how many of the arguments exist (an unmatched
# glob stays literal and does not count).
count_files() {
  local n=0 f
  for f in "$@"; do [ -e "$f" ] && n=$((n + 1)); done
  printf '%s\n' "$n"
}

# expect_eq NAME GOT WANT
expect_eq() {
  if [ "$2" = "$3" ]; then ok "$1"; else no "$1" "got:  [$2]
want: [$3]"; fi
}

# poll SECONDS CMD...: run CMD every 50 ms until it succeeds (0) or SECONDS of
# wall time pass (1).
poll() {
  local end
  end=$(( $(now_ms) + $1 * 1000 ))
  shift
  while :; do
    "$@" && return 0
    [ "$(now_ms)" -lt "$end" ] || return 1
    sleep 0.05
  done
}

# hold SECONDS CMD...: CMD must succeed on every 50 ms poll for SECONDS.
hold() {
  local end
  end=$(( $(now_ms) + $1 * 1000 ))
  shift
  while [ "$(now_ms)" -lt "$end" ]; do
    "$@" || return 1
    sleep 0.05
  done
}

# ---------------------------------------------------------------------------
# 3. the case's private world (§8.1). $C/{home,bin,stubs,tmp}, the wrapper, the
#    scripts dir for the shell under test, the Catppuccin copy and test.conf.
# ---------------------------------------------------------------------------

CASE_ID='' CSUT='' C='' IS='' OS='' INNER_PID=''
CASE_ENV=()

# case_dir ENTRY: T0 -> $D/T0, T10@dash -> $D/T10-dash.
case_dir() {
  case $1 in
    *@*) printf '%s/%s-%s' "$D" "${1%@*}" "${1#*@}" ;;
    *) printf '%s/%s' "$D" "$1" ;;
  esac
}

case_setup() {
  local entry=$1
  CASE_ID=${entry%@*}
  if [ "$entry" != "$CASE_ID" ]; then CSUT=${entry#*@}; else CSUT=${SUT:-sh}; fi
  C=$(case_dir "$entry")
  IS="$C/i"
  OS="$C/o"
  if ! sock_ok "$IS" || ! sock_ok "$OS"; then
    die2 "case $entry: its sockets $IS / $OS are not private"
  fi
  mkdir -p "$C/hm/.config/tmux" "$C/hm/.tmux/plugins" "$C/bin" "$C/stubs" "$C/tmp" \
    || die2 "case $entry: cannot create $C"
  : > "$C/tmux.log"
  : > "$C/calls.tsv"
  CASE_ENV=(env -i
    "HOME=$C/hm"
    "PATH=$C/bin:$C/stubs:$BASE_PATH"
    "TMUX_TMPDIR=$D"
    "TMPDIR=$C/tmp"
    "SHELL=/bin/sh"
    "TERM=xterm-256color"
    "LANG=en_US.UTF-8"
    "LC_CTYPE=en_US.UTF-8"
    "USER=${USER:-}"
    "LOGNAME=${LOGNAME:-}")
  install_wrapper "$C/bin/tmux" "$D" "$C" "$REAL_TMUX"
  install_stubs "$C/stubs"
  install_scripts "$C/hm/.tmux/scripts" "$CSUT"
  install_plugins "$C/hm/.tmux/plugins"
  build_test_conf > "$C/test.conf" || die2 "case $entry: cannot assemble test.conf from $CONF_SRC"
  local lint
  lint=$(lint_test_conf "$C/test.conf") || die2 "case $entry: test.conf breaks the §8.2 rules:
$lint"
  # No pin and no screens file at the start of a case, unless it writes them.
  [ -z "$(ls -A "$C/hm/.config/tmux")" ] || die2 "case $entry: $C/hm/.config/tmux is not empty"
  CASE_FAILS=0
}

# install_wrapper DEST ROOT CASE REAL: a copy of tests/theme-harness-tmux.sh
# with its three configuration lines filled in, and nothing else changed.
install_wrapper() {
  local dest=$1 v n
  for v in "$2" "$3" "$4"; do
    case $v in
      /*) ;;
      *) die2 "wrapper configuration '$v' is not an absolute path" ;;
    esac
    case $v in
      *[!A-Za-z0-9/._-]*) die2 "wrapper configuration '$v' has characters it cannot carry" ;;
    esac
  done
  sed -e "s|^H_ROOT=''\$|H_ROOT='$2'|" \
      -e "s|^H_CASE=''\$|H_CASE='$3'|" \
      -e "s|^H_REAL_TMUX=''\$|H_REAL_TMUX='$4'|" "$WRAPPER_SRC" > "$dest" || die2 "cannot write $dest"
  chmod 755 "$dest"
  n=$(diff "$WRAPPER_SRC" "$dest" | grep -c '^>')
  [ "$n" = 3 ] || die2 "the wrapper copy $dest differs from $WRAPPER_SRC in $n lines, want its 3 configuration lines"
}

# install_stubs DIR: `tailscale status` prints fixed peers (§8.1); every call is
# appended to $C/stubs.log and its pid to $C/tailscale.pids. While
# $C/block.tailscale exists the stub hangs before answering, which is how T10
# catches a sync pass inside its host lookup (theme.sh's 2 s watchdog still
# applies). While $C/stubfail.tailscale exists it prints nothing and exits 1
# (T2: a failed lookup). The real tailscale is never reached: this dir
# precedes BASE_PATH, so `command -v tailscale` finds the stub first.
install_stubs() {
  cat > "$1/tailscale" <<EOF
#!/bin/sh
# tailscale stub (theme-per-screen harness): fixed peers, every call logged.
printf '%s\n' "\$\$" >> '$C/tailscale.pids'
printf 'tailscale %s\n' "\$*" >> '$C/stubs.log'
while [ -e '$C/block.tailscale' ]; do sleep 0.05; done
[ ! -e '$C/stubfail.tailscale' ] || exit 1
case "\${1:-}" in
  status) cat <<'PEERS'
100.101.1.1     host-m    user@        macOS   -
100.64.0.10   omarchy   user@        linux   active; direct 192.0.2.20:41641, tx 1 rx 1
100.64.0.9      host-b      user@        iOS     active; relay "fra", tx 1 rx 1
PEERS
  ;;
esac
exit 0
EOF
  chmod 755 "$1/tailscale"
}

# install_scripts DEST SUT: what ~/.tmux/scripts is for the shell under test.
#   sh    a symlink to moox's share/moox/scripts; theme.sh runs under
#         its own #!/bin/sh (bash in POSIX mode on macOS)
#   dash  symlinks to each worktree script, except theme.sh: a copy whose first
#         line is #!<dash> and whose every other line is byte-identical
# Hooks, theme.sh's own relaunches and ts() all exec the file, so its shebang
# is the shell under test everywhere.
install_scripts() {
  local dest=$1 sut=$2 f b
  case $sut in
    sh)
      ln -s "$SCRIPTS_SRC" "$dest" || die2 "cannot link $dest"
      ;;
    dash)
      [ -n "$DASH" ] || die2 "dash not found"
      mkdir "$dest" || die2 "cannot create $dest"
      for f in "$SCRIPTS_SRC"/*; do
        b=${f##*/}
        if [ "$b" = theme.sh ]; then
          { printf '#!%s\n' "$DASH"; tail -n +2 "$f"; } > "$dest/$b" || die2 "cannot write $dest/$b"
          chmod 755 "$dest/$b"
          cmp -s <(tail -n +2 "$f") <(tail -n +2 "$dest/$b") \
            || die2 "$dest/$b is not byte-identical to $f below its first line"
        else
          ln -s "$f" "$dest/$b" || die2 "cannot link $dest/$b"
        fi
      done
      ;;
    *) die2 "unknown shell under test '$sut' (sh or dash)" ;;
  esac
}

# install_plugins DIR: a COPY of the real Catppuccin (never the real tree),
# tmux-continuum's save script as an `exit 0` stub, and no other plugin.
install_plugins() {
  cp -R "$REAL_CATPPUCCIN" "$1/tmux" || die2 "cannot copy Catppuccin into $1"
  mkdir -p "$1/tmux-continuum/scripts"
  printf '#!/bin/sh\nexit 0\n' > "$1/tmux-continuum/scripts/continuum_save.sh"
  chmod 755 "$1/tmux-continuum/scripts/continuum_save.sh"
}

# ---------------------------------------------------------------------------
# 4. test.conf (§8.2): three marked regions of moox's conf files, byte for byte,
#    with a header and the Catppuccin run-shell where TPM sits in the real file.
# ---------------------------------------------------------------------------

REGIONS="pre-plugin formats apply"

# region NAME: the lines strictly between `# >>> theme: NAME >>>` and
# `# <<< theme: NAME <<<`. Each marker must be a whole line and appear exactly
# once, begin before end; anything else is exit 3.
region() {
  awk -v b="# >>> theme: $1 >>>" -v e="# <<< theme: $1 <<<" '
    $0 == b { nb++; on = 1; next }
    $0 == e { ne++; if (!on) bad = 1; on = 0; next }
    on { print }
    END { if (nb != 1 || ne != 1 || on || bad) exit 3 }' "$OPTS_SRC" "$THEME_SRC"
}

build_test_conf() {
  printf '%s\n' \
    '# theme-per-screen test config: this header, then regions of moox conf/options.conf and conf/theme.conf' \
    'set -g status on' \
    'set -g status-interval 1' \
    'set -g default-shell /bin/sh' \
    'set -g prefix C-b' \
    "set -g @moox-root '$C/hm/.tmux'"
  region pre-plugin || return 1
  printf '%s\n' "run-shell '~/.tmux/plugins/tmux/catppuccin.tmux >/dev/null 2>&1 || true'"
  region formats || return 1
  grep -v '^[[:space:]]*#' "$STATUS_SRC" | sed '/^[[:space:]]*$/d'
  region apply || return 1
}

# lint_test_conf FILE: prints each violation, exit 1 if any. Comment lines are
# skipped (they cannot load or run anything); the rest must not mention tpm,
# resurrect or continuum-safe-restore in any case, and every `run-shell` must
# end with `>/dev/null 2>&1 || true` — followed by its closing quote (possibly
# backslash-escaped) or the end of the line, before any further run-shell.
lint_test_conf() {
  awk -v q="'" '
    /^[ \t]*#/ { next }
    {
      low = tolower($0)
      if (low ~ /tpm/)                    { print NR ": mentions tpm: " $0; n++ }
      if (low ~ /resurrect/)              { print NR ": mentions resurrect: " $0; n++ }
      if (low ~ /continuum-safe-restore/) { print NR ": mentions continuum-safe-restore: " $0; n++ }
      sfx = ">/dev/null 2>&1 || true"
      s = $0
      while ((i = index(s, "run-shell")) > 0) {
        s = substr(s, i + 9)
        j = index(s, "run-shell")
        seg = (j > 0) ? substr(s, 1, j - 1) : s
        k = index(seg, sfx)
        good = 0
        if (k > 0) {
          rest = substr(seg, k + length(sfx))
          c = substr(rest, 1, 1)
          if (c == "\\") c = substr(rest, 2, 1)
          if (c == "\"" || c == q || rest ~ /^[ \t]*$/) good = 1
        }
        if (!good) { print NR ": run-shell not silenced: " $0; n++ }
      }
    }
    END { exit (n > 0) }' "$1"
}

# ---------------------------------------------------------------------------
# 5. the two private servers
# ---------------------------------------------------------------------------

# TI_CONF is the -f of every ti call: the case's test.conf unless a case (T5:
# a hub started bare, then sourced) sets it to /dev/null before inner_start.
ti() {
  sock_ok "$IS" || die2 "ti: refusing socket '$IS'"
  "${CASE_ENV[@]}" "$REAL_TMUX" -S "$IS" -f "${TI_CONF:-$C/test.conf}" "$@"
}

to() {
  sock_ok "$OS" || die2 "to: refusing socket '$OS'"
  "${CASE_ENV[@]}" "$REAL_TMUX" -S "$OS" -f /dev/null "$@"
}

# ts ARGS: theme.sh under test, run the way a hook job runs it: the case's
# HOME and PATH, and TMUX naming the inner server. Extra variables for one case
# (T10: THEME_SYNC_TESTHOOK=…) go in TS_EXTRA_ENV; hook-started runs see only
# what the server's global environment holds. `ts … &` does NOT leave
# theme.sh's pid in $! (measured: bash forks a subshell for the function, and
# that forks env, so theme.sh is two pids on); use ts_bg for that.
TS_EXTRA_ENV=()
ts() {
  "${CASE_ENV[@]}" ${TS_EXTRA_ENV[@]+"${TS_EXTRA_ENV[@]}"} \
    "TMUX=$IS,${INNER_PID:?inner server not started},0" "$C/hm/.tmux/scripts/theme.sh" "$@"
}

# ts_bg ARGS: ts in the background, exec'ed all the way down, so TS_PID (and
# $!) is theme.sh's own pid — under `set -m`, also its process group.
ts_bg() {
  ( exec "${CASE_ENV[@]}" ${TS_EXTRA_ENV[@]+"${TS_EXTRA_ENV[@]}"} \
      "TMUX=$IS,${INNER_PID:?inner server not started},0" "$C/hm/.tmux/scripts/theme.sh" "$@" ) &
  TS_PID=$!
}

# inner_start [SESSION]: the hub under test, from test.conf. Its first pane is
# an explicit /bin/sh (rule 3).
inner_start() {
  ti new-session -d -s "${1:-s}" -x 160 -y 40 /bin/sh || return 1
  INNER_PID=$(ti display -p '#{pid}')
  [ -n "$INNER_PID" ]
}

# ---------------------------------------------------------------------------
# 6. screens (§8.3). A screen is a window of the OUTER server whose pane runs
#    `tmux attach` to the inner one: that attach is a real client, the outer
#    window's window-style is its terminal's theme (the outer tmux answers the
#    inner client's OSC 11 from it and pushes 997 when it changes), and
#    capture-pane of the outer pane is what the screen shows, status line last.
#    No client ever attaches to the outer server.
# ---------------------------------------------------------------------------

SCREEN_W=160
SCREEN_H=40
SCREEN_LIGHT_BG='#fbf1c7'
SCREEN_DARK_BG='#1e1e2e'

outer_start() {
  to new-session -d -s scr -x "$SCREEN_W" -y "$SCREEN_H" /bin/sh || return 1
  to set -g default-shell /bin/sh
  to set -g default-size "${SCREEN_W}x${SCREEN_H}"
  # No outer status line: the outer pane is the whole screen.
  to set -g status off
}

screen_tty()     { to display -p -t "scr:$1" '#{pane_tty}'; }
screen_capture() { to capture-pane -p -e -t "scr:$1"; }
screen_status()  { screen_capture "$1" | tail -n 1; }
# screen_keys NAME KEY...: typed on that screen (send-keys to the OUTER pane,
# which the inner client reads as its own input).
screen_keys()    { local n=$1; shift; to send-keys -t "scr:$n" "$@"; }

# screen_theme NAME: the inner client's #{client_theme} (possibly empty); 1 if
# the screen is not attached.
screen_theme() {
  local t
  t=$(screen_tty "$1") || return 1
  [ -n "$t" ] || return 1
  ti list-clients -F '#{client_tty} #{client_theme}' | awk -v t="$t" '$1 == t { print $2; f = 1 } END { exit !f }'
}

# screen_id NAME: the inner client as NAME:PID (the form @theme_lead records).
screen_id() {
  local t
  t=$(screen_tty "$1") || return 1
  ti list-clients -F '#{client_tty} #{client_name}:#{client_pid}' | awk -v t="$t" '$1 == t { print $2; f = 1 } END { exit !f }'
}

screen_attached() { screen_theme "$1" >/dev/null; }

# screen_reports NAME light|dark|silent
screen_reports() {
  local got
  got=$(screen_theme "$1") || return 1
  case $2 in
    silent) [ -z "$got" ] ;;
    *) [ "$got" = "$2" ] ;;
  esac
}

# A screen that does not calibrate stops the run (§16 item 8): every case
# depends on screens that report what they are told to, and there is no
# silent fallback to a different kind of screen.
calibration_failed() {
  {
    say '##########################################################################'
    say "#  SCREEN CALIBRATION FAILED: $*"
    say '#  Nested-tmux screens are a prerequisite of every case (§8.3, §16 item 8).'
    say '#  There is no fallback: the run stops here.'
    say '##########################################################################'
    say 'inner clients:'
    ti list-clients -F '  #{client_tty} theme=[#{client_theme}] type=#{client_termtype}' 2>&1
  } >&2
  exit 2
}

# screen_add NAME light|dark|silent [SSH_CONNECTION] [SESSION]: attach a screen
# to SESSION (default s) and wait until it reports its mode (silent: nothing).
# The window-style is set BEFORE the attach, so the inner client's first
# OSC 11 / 996 exchange already sees it. SCREEN_ENV, when set, is one more
# VAR=VALUE in the client's environment (T2's secret canary).
SCREEN_ENV=''
screen_add() {
  local name=$1 mode=$2 ssh=${3:-} target=${4:-s} cmd
  case $name in ''|*[!A-Za-z0-9_]*) die2 "screen name '$name'" ;; esac
  case $ssh in *"'"*) die2 "SSH_CONNECTION '$ssh' contains a quote" ;; esac
  case $target in ''|*[!A-Za-z0-9_.-]*) die2 "session '$target'" ;; esac
  case $SCREEN_ENV in *[!A-Za-z0-9_=.-]*) die2 "SCREEN_ENV '$SCREEN_ENV'" ;; esac
  to new-window -d -t scr: -n "$name" /bin/sh || die2 "cannot open screen window $name"
  case $mode in
    light) to set -w -t "scr:$name" window-style "bg=$SCREEN_LIGHT_BG" ;;
    dark) to set -w -t "scr:$name" window-style "bg=$SCREEN_DARK_BG" ;;
    silent) ;;
    *) die2 "screen mode '$mode' (light, dark or silent)" ;;
  esac
  cmd="exec env -u TMUX"
  [ -z "$ssh" ] || cmd="$cmd SSH_CONNECTION='$ssh'"
  [ -z "$SCREEN_ENV" ] || cmd="$cmd $SCREEN_ENV"
  cmd="$cmd tmux -S $IS attach -t $target"
  to respawn-pane -k -t "scr:$name" "$cmd" || die2 "cannot start screen $name"
  poll 8 screen_attached "$name" || calibration_failed "screen $name did not attach to $target within 8 s"
  poll 8 screen_reports "$name" "$mode" \
    || calibration_failed "screen $name ($mode) reports [$(screen_theme "$name")] after 8 s"
}

# screen_set_mode NAME light|dark: change the screen's terminal theme and wait
# (8 s) for its client to report it. Returns 1 on timeout; the caller decides.
screen_set_mode() {
  local bg
  case $2 in
    light) bg=$SCREEN_LIGHT_BG ;;
    dark) bg=$SCREEN_DARK_BG ;;
    *) die2 "screen_set_mode: mode '$2'" ;;
  esac
  to set -w -t "scr:$1" window-style "bg=$bg"
  poll 8 screen_reports "$1" "$2"
}

screen_detach() { ti detach-client -t "$(screen_tty "$1")"; }

# ---------------------------------------------------------------------------
# 7. the wrapper's logs. $C/tmux.log is the §8.1 line format
# (`epoch-ms pid status argv`); $C/calls.tsv is the same calls with the parsed
# command words: ms, pid, ppid, status, canonical words (comma-separated),
# control note, argv — tab-separated. Count from calls.tsv.
# ---------------------------------------------------------------------------

# log_calls WORD [STATUS-ERE]: calls whose command chain includes WORD (the
# canonical name: set-option, not set) and whose status matches (default: any
# completed call, i.e. a number or exec).
log_calls() {
  awk -F '\t' -v w="$1" -v st="${2:-^([0-9]+|exec)\$}" '
    $4 ~ st { n = split($5, a, ","); for (i = 1; i <= n; i++) if (a[i] == w) { c++; break } }
    END { print c + 0 }' "$C/calls.tsv"
}

log_refused() { grep -c ' REFUSED ' "$1/tmux.log" 2>/dev/null; }

# log_lines: how many calls the wrapper has logged so far (a mark for since).
log_lines() { wc -l < "$C/calls.tsv" | tr -d ' '; }

# since MARK: the calls.tsv lines logged after MARK.
since() { tail -n +"$(( $1 + 1 ))" "$C/calls.tsv"; }

# count_word WORD [STATUS-ERE]: log_calls over stdin (a since slice).
count_word() {
  awk -F '\t' -v w="$1" -v st="${2:-^([0-9]+|exec)\$}" '
    $4 ~ st { n = split($5, a, ","); for (i = 1; i <= n; i++) if (a[i] == w) { c++; break } }
    END { print c + 0 }'
}

# ---------------------------------------------------------------------------
# 7a. theme state (P3+). theme.sh keeps its sync request flag and lock beside
#     the socket it talks to ($TMUX's first field), so for the hub under test
#     they are $IS.theme.dirty and $IS.theme.lock.
# ---------------------------------------------------------------------------

LOCKD() { printf '%s.theme.lock' "$IS"; }
DIRTYF() { printf '%s.theme.dirty' "$IS"; }
sync_idle() { [ ! -e "$(LOCKD)" ] && [ ! -e "$(DIRTYF)" ]; }

# settle [SECONDS]: no sync runner and no owed request, and the wrapper log
# unchanged for half a second (hook-started syncs are over). 1 on timeout.
settle() {
  local end n1
  end=$(( $(now_ms) + ${1:-8} * 1000 ))
  while :; do
    if sync_idle; then
      n1=$(log_lines)
      sleep 0.5
      if sync_idle && [ "$(log_lines)" = "$n1" ]; then return 0; fi
    else
      sleep 0.05
    fi
    [ "$(now_ms)" -lt "$end" ] || return 1
  done
}

screens_now() { ti show -gv @theme_screens 2>/dev/null; }
screens_are() { [ "$(screens_now)" = "$1" ]; }
chrome_now() { ti show -gqv @theme_chrome 2>/dev/null; }
chrome_installed() { [ "$(chrome_now)" = per-client-1 ]; }

# screens_want NAME=MODE...: the @theme_screens string theme.sh must publish
# for these screens: " tty=mode tty=mode ", sorted by tty (bytewise).
screens_want() {
  local a lines=''
  for a in "$@"; do lines+="$(screen_tty "${a%%=*}")=${a#*=}"$'\n'; done
  printf ' %s' "$(printf '%s' "$lines" | LC_ALL=C sort | tr '\n' ' ')"
}

# screens_from_status: the same string, built from a fresh `theme.sh status`.
screens_from_status() { printf ' %s' "$(ts status | awk '{ printf "%s=%s ", $1, $3 }')"; }

# painted_panes: how many panes of the hub carry @theme_painted.
painted_panes() { ti list-panes -a -F '#{pane_id} #{@theme_painted}' | awk 'NF > 1' | wc -l | tr -d ' '; }

ESC=$'\033'
strip_sgr() { sed "s/${ESC}\\[[0-9;:]*m//g"; }

# has_rgb TEXT KIND R;G;B: TEXT carries that SGR colour (KIND 38 = fg, 48 = bg).
has_rgb() { case $1 in *"$2;2;$3"[m\;]*) return 0 ;; esac; return 1; }

# status_line_has NAME KIND R;G;B / status_line_lacks …
status_line_has() { has_rgb "$(screen_status "$1")" "$2" "$3"; }
status_line_lacks() { ! has_rgb "$(screen_status "$1")" "$2" "$3"; }

# theme_file NAME: the content of ~/.config/tmux/NAME in the case HOME, or ''.
theme_file() { cat "$C/hm/.config/tmux/$1" 2>/dev/null || true; }

# status_line_for NAME: this screen's line of `theme.sh status`.
status_line_for() { ts status | awk -v t="$(screen_tty "$1")" '$1 == t'; }

# ago_stamp MINUTES: a `touch -t` stamp that many minutes in the past.
ago_stamp() { date -v-"$1"M +%Y%m%d%H%M.%S 2>/dev/null || date -d "$1 minutes ago" +%Y%m%d%H%M.%S; }

# dead_pid: the pid of a process that has exited and been reaped.
dead_pid() { local p; sh -c 'exit 0' & p=$!; wait "$p"; printf '%s\n' "$p"; }

# lock_owner: the pid in the sync lock's owner file, or ''.
lock_owner() {
  local f
  for f in "$(LOCKD)"/owner.*; do [ -e "$f" ] && { printf '%s\n' "${f##*/owner.}"; return; }; done
}

# artifact NAME CMD...: CMD's output saved as $C/NAME and, when
# THEME_TEST_ARTIFACTS names a directory, copied there too (evidence for a
# reviewer: the menu renders of T2 and T9).
artifact() {
  local name=$1
  shift
  "$@" > "$C/$name" 2>&1
  if [ -n "${THEME_TEST_ARTIFACTS:-}" ] && [ -d "$THEME_TEST_ARTIFACTS" ]; then
    cp "$C/$name" "$THEME_TEST_ARTIFACTS/$name"
  fi
}

# screens_body: the screens file without its header comment ('' if absent).
screens_body() { grep -v '^#' "$C/hm/.config/tmux/screens" 2>/dev/null || true; }

# hosts_now / hosts_has ENTRY: @theme_hosts, and whether it lists ENTRY.
hosts_now() { ti show -gqv @theme_hosts 2>/dev/null; }
hosts_has() { case $(hosts_now) in *" $1 "*) return 0 ;; esac; return 1; }

# remote_now / remote_has TTY: @theme_remote, and whether it lists TTY.
remote_now() { ti show -gqv @theme_remote 2>/dev/null; }
remote_has() { case $(remote_now) in *" $1 "*) return 0 ;; esac; return 1; }

# screen_pid NAME: the inner client's pid.
screen_pid() { local id; id=$(screen_id "$1") || return 1; printf '%s\n' "${id##*:}"; }

# Poll predicates (named, so poll never needs eval).
paused_since()     { since "$1" | grep -q "PAUSED.*$2"; }        # MARK WORD
missing()          { [ ! -e "$1" ]; }
screens_settled()  { screens_are "$1" && sync_idle; }
pane_bg_is()       { [ "$(ti display -p -t "$1" '#{pane_bg}')" = "$2" ]; }
status_text_has()  { screen_status "$1" | strip_sgr | grep -qF "$2"; }

# hub_ready NAME=MODE...: the screens attached, synced, chrome installed and
# @theme_screens as wanted; fail_now otherwise.
hub_ready() {
  local want
  want=$(screens_want "$@")
  poll 5 chrome_installed || fail_now "the chrome is installed (@theme_chrome = per-client-1)" "got [$(chrome_now)]"
  poll 3 screens_are "$want" || fail_now "@theme_screens = [$want]" "got [$(screens_now)]"
  settle 8 || fail_now "the hub settles (no runner, no owed request)" "lock: $(ls -A "$(LOCKD)" 2>&1) dirty: $(ls "$(DIRTYF)" 2>&1)"
}

# ---------------------------------------------------------------------------
# 8. cases
# ---------------------------------------------------------------------------

# H0: the harness's own guards, each watched failing. No server is started:
# the wrapper runs against a stub "real tmux" that only records its argv, so a
# guard that wrongly accepted could reach nothing.
case_H0() {
  local W="$C/w" wr stub out rc n pids pid ok_n fail_n f b

  # --- sock_ok ------------------------------------------------------------
  check "sock_ok accepts this case's inner socket" sock_ok "$IS"
  check "sock_ok rejects an empty path" not sock_ok ""
  check "sock_ok rejects a path outside \$D" not sock_ok "/tmp/elsewhere/i"
  check "sock_ok rejects a socket named default" not sock_ok "$D/x/default"
  check "sock_ok rejects .. out of \$D" not sock_ok "$D/../tmux-501/x"
  check "sock_ok rejects a path of 104+ bytes" not sock_ok "$D/$(printf 'x%.0s' $(seq 1 110))"

  # --- the wrapper, against a stub ----------------------------------------
  mkdir -p "$W/root/c" "$W/bin"
  stub="$W/stub-tmux"
  cat > "$stub" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> '$W/stub.log'
printf 'STUB-PID %s\n' "\$\$"
printf 'STUB-OUT %s\n' "\$*"
exit "\${STUB_RC:-0}"
EOF
  chmod 755 "$stub"
  : > "$W/stub.log"
  install_wrapper "$W/bin/tmux" "$W/root" "$W" "$stub"
  wr="$W/bin/tmux"
  # w [VAR=VAL...] -- ARGS: run the configured wrapper with a clean env.
  w() {
    local e=(env -i PATH=/usr/bin:/bin)
    while [ "$1" != -- ]; do e+=("$1"); shift; done
    shift
    "${e[@]}" "$wr" "$@"
  }
  stub_calls() { wc -l < "$W/stub.log" | tr -d ' '; }

  out=$(env -i PATH=/usr/bin:/bin sh "$WRAPPER_SRC" -S "$W/root/c/i" list-clients 2>/dev/null); rc=$?
  expect_eq "an unconfigured wrapper refuses everything (exit 97)" "$rc" 97

  out=$(w -- -S "$W/root/c/i" list-clients); rc=$?
  expect_eq "-S under the root: runs, exit 0" "$rc" 0
  case $out in *"STUB-OUT -S $W/root/c/i list-clients"*) ok "-S under the root: stdout passes through" ;; *) no "-S under the root: stdout passes through" "$out" ;; esac
  expect_eq "-S under the root: one line in tmux.log, rc 0" \
    "$(awk '{ $1 = ""; $2 = ""; print }' "$W/tmux.log" | sed 's/^  //')" "0 -S $W/root/c/i list-clients"
  expect_eq "calls.tsv records the canonical command word" "$(cut -f4,5 "$W/calls.tsv")" "0"$'\t'"list-clients"

  n=$(stub_calls)
  w -- -S /tmp/tps-elsewhere/i list-clients >/dev/null 2>&1; rc=$?
  expect_eq "-S outside the root: exit 97" "$rc" 97
  w -- -S "$W/root/../escape" list-clients >/dev/null 2>&1; rc=$?
  expect_eq "-S with .. out of the root: exit 97" "$rc" 97
  w -- -uS/tmp/tps-elsewhere/i list-clients >/dev/null 2>&1; rc=$?
  expect_eq "-S inside a flag cluster (-uS/path): exit 97" "$rc" 97
  w -- -S >/dev/null 2>&1; rc=$?
  expect_eq "-S with no value: exit 97" "$rc" 97
  w -- list-clients >/dev/null 2>&1; rc=$?
  expect_eq "no -S and no TMUX: exit 97" "$rc" 97
  w TMUX=/nonexistent/sock,1,0 -- list-clients >/dev/null 2>&1; rc=$?
  expect_eq "no -S, TMUX outside the root: exit 97" "$rc" 97
  w "TMUX=$W/root/c/i,1,0" -- -L default list-clients >/dev/null 2>&1; rc=$?
  expect_eq "-L is refused even with a private TMUX: exit 97" "$rc" 97
  expect_eq "no refused call reached the real tmux" "$(stub_calls)" "$n"
  expect_eq "each refusal is logged REFUSED" "$(grep -c ' REFUSED ' "$W/tmux.log")" 7

  out=$(w "TMUX=$W/root/c/i,1,0" -- show -gv @x); rc=$?
  expect_eq "no -S, TMUX under the root (a hook job): runs, exit 0" "$rc" 0
  expect_eq "an alias is logged by its canonical word" "$(tail -n 1 "$W/calls.tsv" | cut -f5)" "show-options"

  w STUB_RC=3 -- -S "$W/root/c/i" list-panes >/dev/null; rc=$?
  expect_eq "the real exit status passes through" "$rc" 3
  expect_eq "and is logged" "$(tail -n 1 "$W/tmux.log" | awk '{ print $3 }')" 3

  # fail.<subcommand>: any command of the ; chain matches.
  touch "$W/fail.list-clients"
  n=$(stub_calls)
  w -- -S "$W/root/c/i" set -g @x 1 ';' list-clients >/dev/null 2>&1; rc=$?
  expect_eq "fail.list-clients fails a chain containing list-clients, exit 1" "$rc" 1
  expect_eq "without running it" "$(stub_calls)" "$n"
  expect_eq "calls.tsv: both words, rc 1, the control" "$(tail -n 1 "$W/calls.tsv" | cut -f4-6)" "1"$'\t'"set-option,list-clients"$'\t'"fail.list-clients"
  w -- -S "$W/root/c/i" list-panes >/dev/null; rc=$?
  expect_eq "fail.list-clients leaves other subcommands alone" "$rc" 0
  rm -f "$W/fail.list-clients"

  # failonce.<subcommand>: the next call only, claimed atomically; aliases match.
  touch "$W/failonce.source-file"
  w -- -S "$W/root/c/i" source "$W/x.conf" >/dev/null 2>&1; rc=$?
  expect_eq "failonce.source-file fails the next \`source\`, exit 1" "$rc" 1
  w -- -S "$W/root/c/i" source "$W/x.conf" >/dev/null 2>&1; rc=$?
  expect_eq "and only that one" "$rc" 0
  n=$(count_files "$W"/failonce.source-file.claimed.*)
  expect_eq "the claim is left as failonce.source-file.claimed.<pid>" "$n" 1
  touch "$W/failonce.list-windows"
  pids=''
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    w -- -S "$W/root/c/i" list-windows >/dev/null 2>&1 &
    pids="$pids $!"
  done
  fail_n=0 ok_n=0
  for pid in $pids; do
    if wait "$pid"; then ok_n=$((ok_n + 1)); else fail_n=$((fail_n + 1)); fi
  done
  expect_eq "10 concurrent calls against one failonce: exactly 1 fails" "$fail_n/$ok_n" "1/9"

  # pause.<subcommand>: blocks, logs PAUSED, and runs once the file is gone.
  touch "$W/pause.list-sessions"
  n=$(stub_calls)
  w -- -S "$W/root/c/i" list-sessions >/dev/null 2>&1 &
  pid=$!
  check "pause.list-sessions: the call logs PAUSED" poll 3 grep -q ' PAUSED .*list-sessions' "$W/tmux.log"
  sleep 0.3
  expect_eq "and has not run while the file exists" "$(stub_calls)" "$n"
  rm -f "$W/pause.list-sessions"
  wait "$pid"; rc=$?
  expect_eq "it runs once the file is removed, exit 0" "$rc/$(stub_calls)" "0/$((n + 1))"

  # pause.match: only calls whose argv contains the first line.
  printf '::Q %%%%\n' > "$W/pause.match"
  expect_eq "pause.match holds the T13 pattern" "$(cat "$W/pause.match")" "::Q %%"
  w -- -S "$W/root/c/i" display -p 'x' >/dev/null; rc=$?
  expect_eq "pause.match lets a non-matching call through" "$rc" 0
  n=$(stub_calls)
  w -- -S "$W/root/c/i" list-panes -F '::Q %%1' >/dev/null 2>&1 &
  pid=$!
  check "pause.match holds a call whose argv contains it" poll 3 grep -q ' PAUSED .*::Q %%1' "$W/tmux.log"
  expect_eq "without running it" "$(stub_calls)" "$n"
  rm -f "$W/pause.match"
  wait "$pid"; rc=$?
  expect_eq "which runs once pause.match is gone" "$rc" 0

  # record.match: stdout saved to out.*, still printed, rc intact.
  printf 'REC-ME\n' > "$W/record.match"
  out=$(w STUB_RC=4 -- -S "$W/root/c/i" list-panes -F REC-ME); rc=$?
  expect_eq "record.match keeps the exit status" "$rc" 4
  case $out in *"STUB-OUT -S $W/root/c/i list-panes -F REC-ME"*) ok "record.match still prints stdout" ;; *) no "record.match still prints stdout" "$out" ;; esac
  n=$(count_files "$W"/out.*)
  expect_eq "record.match saved exactly one out.* file" "$n" 1
  expect_eq "holding exactly what was printed" "$(cat "$W"/out.* 2>/dev/null)" "$out"
  w -- -S "$W/root/c/i" list-panes -F other >/dev/null
  n=$(count_files "$W"/out.*)
  expect_eq "a call that does not match records nothing" "$n" 1
  rm -f "$W/record.match"

  # attach is exec'ed: the wrapper's pid IS the real client's.
  out=$(w -- -S "$W/root/c/i" attach -t s); rc=$?
  expect_eq "attach: runs, exit status of the client" "$rc" 0
  expect_eq "attach is exec'ed (the logged pid is the client's pid)" \
    "$(tail -n 1 "$W/tmux.log" | awk '{ print $3 " " $2 }')" "exec $(printf '%s\n' "$out" | awk '/^STUB-PID/ { print $2 }')"

  n=$(( $(wc -l < "$W/tmux.log") ))
  w -- -S "$W/root/c/i" display -p 'a
b' >/dev/null
  expect_eq "an argv with a newline is still one log line" "$(( $(wc -l < "$W/tmux.log") ))" "$((n + 1))"

  # --- the scripts dir for each shell under test ---------------------------
  install_scripts "$C/sut-sh" sh
  expect_eq "SUT=sh: ~/.tmux/scripts is the worktree's scripts dir" "$(cd "$C/sut-sh" && pwd -P)" "$SCRIPTS_SRC"
  install_scripts "$C/sut-dash" dash
  expect_eq "SUT=dash: theme.sh's first line is #!<dash>" "$(head -n 1 "$C/sut-dash/theme.sh")" "#!$DASH"
  check "SUT=dash: every other line of theme.sh is byte-identical" \
    cmp -s <(tail -n +2 "$SCRIPTS_SRC/theme.sh") <(tail -n +2 "$C/sut-dash/theme.sh")
  check "SUT=dash: theme.sh is executable" test -x "$C/sut-dash/theme.sh"
  n=0
  for f in "$SCRIPTS_SRC"/*; do
    b=${f##*/}
    [ "$b" = theme.sh ] && continue
    [ -L "$C/sut-dash/$b" ] && [ "$(readlink "$C/sut-dash/$b")" = "$f" ] || { n=$((n + 1)); no "SUT=dash: $b is a symlink to the worktree file"; }
  done
  [ "$n" = 0 ] && ok "SUT=dash: every other script is a symlink to the worktree file"

  # --- test.conf -----------------------------------------------------------
  local r bl el rf
  for r in $REGIONS; do
    # moox: pre-plugin is in options.conf (before the plugins), the rest in theme.conf.
    if [ "$r" = pre-plugin ]; then rf=$OPTS_SRC; else rf=$THEME_SRC; fi
    bl=$(grep -n -x -F "# >>> theme: $r >>>" "$rf" | cut -d: -f1)
    el=$(grep -n -x -F "# <<< theme: $r <<<" "$rf" | cut -d: -f1)
    if [ -z "$bl" ] || [ -z "$el" ]; then no "region $r is marked in ${rf##*/}"; continue; fi
    check "region $r (${rf##*/} lines $((bl + 1))-$((el - 1))) is extracted byte for byte" \
      cmp -s <(region "$r") <(sed -n "$((bl + 1)),$((el - 1))p" "$rf")
  done
  expect_eq "test.conf starts with the §8.2 header" "$(sed -n '2,5p' "$C/test.conf")" "set -g status on
set -g status-interval 1
set -g default-shell /bin/sh
set -g prefix C-b"
  check "the assembled test.conf passes its own lint" lint_test_conf "$C/test.conf"
  cat > "$C/bad.conf" <<'EOF'
# comments may say tpm, resurrect and run-shell 'x' freely
set -g @plugin 'tmux-plugins/tpm'
run-shell '~/.tmux/scripts/continuum-safe-restore.sh >/dev/null 2>&1 || true'
run-shell '~/x.sh'
run-shell 'a >/dev/null 2>&1 || true; b'
set-hook -ga client-attached 'run-shell -b "~/y.sh >/dev/null 2>&1 || true"'
set-hook -ga client-active { if -F '#{x}' { set -wq @y 1 } ; run-shell -b "~/z.sh >/dev/null 2>&1 || true" }
bind T run-shell '~/t.sh >/dev/null 2>&1 || true'
EOF
  out=$(lint_test_conf "$C/bad.conf"); rc=$?
  expect_eq "the lint rejects a bad config" "$rc" 1
  expect_eq "naming exactly the four bad lines" "$(printf '%s\n' "$out" | cut -d: -f1 | sort -u | tr '\n' ' ')" "2 3 4 5 "
}

# T0 (§8.4): the private hub from test.conf, three calibrated screens — light,
# dark, and one that never reports — and the proof that everything went
# through the wrapper and nothing was refused.
case_T0() {
  local s name want other n line

  inner_start || fail_now "the inner server did not start from test.conf"
  check "test.conf parses (source-file -n)" ti source-file -n "$C/test.conf"
  expect_eq "exactly 1 inner session before any screen attaches" "$(ti list-sessions -F '#{session_name}')" "s"
  expect_eq "prefix is C-b (the live C-a sits outside the regions)" "$(ti show -gv prefix)" "C-b"
  expect_eq "default-shell is /bin/sh" "$(ti show -gv default-shell)" "/bin/sh"
  expect_eq "the hub's HOME is the case's own" "$(ti show-environment -g HOME)" "HOME=$C/hm"
  check "the Catppuccin copy loaded (@catppuccin_status_session is set)" test -n "$(ti show -gqv @catppuccin_status_session)"
  line=$(ts detect 2>&1)
  case $line in light|dark) ok "ts runs the theme.sh under test against the hub (detect: $line)" ;;
    *) no "ts runs the theme.sh under test against the hub" "$line" ;; esac

  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  screen_add C silent
  ok "screens A (light), B (dark) and C (silent) attached and report as configured"

  # Calibration proper: each reporting screen follows its terminal to the
  # other theme and back, which proves the live 997 push, not only the answer
  # the client got at attach.
  for s in A:light:dark B:dark:light; do
    name=${s%%:*}
    want=${s#*:}
    want=${want%%:*}
    other=${s##*:}
    screen_set_mode "$name" "$other" || calibration_failed "screen $name did not follow its terminal to $other"
    screen_set_mode "$name" "$want" || calibration_failed "screen $name did not follow its terminal back to $want"
    ok "screen $name follows its terminal to $other and back to $want"
  done
  hold 1 screen_reports C silent || calibration_failed "screen C (silent) reported [$(screen_theme C)]"
  ok "screen C stays silent (no client_theme) through the other screens' pushes"

  expect_eq "the inner server sees exactly the three screens, as configured" \
    "$(ti list-clients -F '#{client_tty} #{session_name} [#{client_theme}]' | sort)" \
    "$(printf '%s s [light]\n%s s [dark]\n%s s []\n' "$(screen_tty A)" "$(screen_tty B)" "$(screen_tty C)" | sort)"

  line=$(screen_status A)
  case $line in *'48;2;'*) ok "a screen capture carries 24-bit colour in its status line" ;;
    *) no "a screen capture carries 24-bit colour in its status line" "$line" ;; esac

  expect_eq "the three screens attached through the wrapper (exec)" "$(log_calls attach-session '^exec$')" 3
  n=$(log_calls source-file)
  if [ "$n" -ge 2 ]; then
    ok "hook and plugin jobs went through the wrapper ($n source-file calls)"
  else
    no "hook and plugin jobs went through the wrapper" "source-file calls: $n"
  fi
  expect_eq "no REFUSED call in the wrapper log" "$(log_refused "$C")" 0
}

# ---------------------------------------------------------------------------
# 8a. P3: per-client chrome and sync (architecture.md §8.4). Captures are the
#     outer pane of a screen; its last line is that client's status line.
#     Colours are the §4.9 capture table.
# ---------------------------------------------------------------------------

# Light / dark SGR triplets (§4.9).
L_MANTLE='242;229;188'  D_MANTLE='24;24;37'
L_CUR='215;153;33'      D_CUR='249;226;175'     # current window number chip
L_OTHER='124;111;100'   D_OTHER='147;153;178'   # other window number chip
L_OVER0='189;174;147'   D_OVER0='108;112;134'   # pane border
L_BLUE='7;102;120'      D_BLUE='137;180;250'    # active pane border
L_MAUVE='220;198;236'   D_MAUVE='203;166;247'   # local host chip
L_RED='242;194;194'     D_RED='243;139;168'     # remote host chip
AMBER='215;153;33'      INK='29;32;33'          # message line, both screens

# border_colours NAME: every SGR fg colour on the screen above its status line.
screen_body() { screen_capture "$1" | sed '$d'; }

# t1_assert ROUND: AC1 on screens A (light) and B (dark), captured together.
t1_assert() {
  local r=$1 sa sb ba bb
  sa=$(screen_status A); sb=$(screen_status B)
  if has_rgb "$sa" 48 "$L_MANTLE" && ! has_rgb "$sa" 48 "$D_MANTLE"; then ok "[$r] A's status bar is light mantle, not dark"
  else no "[$r] A's status bar is light mantle, not dark" "$(printf '%s' "$sa" | cat -v)"; fi
  if has_rgb "$sb" 48 "$D_MANTLE" && ! has_rgb "$sb" 48 "$L_MANTLE"; then ok "[$r] B's status bar is dark mantle, not light"
  else no "[$r] B's status bar is dark mantle, not light" "$(printf '%s' "$sb" | cat -v)"; fi
  if has_rgb "$sa" 48 "$L_CUR" && has_rgb "$sa" 48 "$L_OTHER" && ! has_rgb "$sa" 48 "$D_CUR" && ! has_rgb "$sa" 48 "$D_OTHER"; then
    ok "[$r] A's window list: current chip $L_CUR, other chip $L_OTHER"
  else no "[$r] A's window list: current chip $L_CUR, other chip $L_OTHER" "$(printf '%s' "$sa" | cat -v)"; fi
  if has_rgb "$sb" 48 "$D_CUR" && has_rgb "$sb" 48 "$D_OTHER" && ! has_rgb "$sb" 48 "$L_CUR" && ! has_rgb "$sb" 48 "$L_OTHER"; then
    ok "[$r] B's window list: current chip $D_CUR, other chip $D_OTHER"
  else no "[$r] B's window list: current chip $D_CUR, other chip $D_OTHER" "$(printf '%s' "$sb" | cat -v)"; fi
  ba=$(screen_body A); bb=$(screen_body B)
  if { has_rgb "$ba" 38 "$L_OVER0" || has_rgb "$ba" 38 "$L_BLUE"; } && ! has_rgb "$ba" 38 "$D_OVER0" && ! has_rgb "$ba" 38 "$D_BLUE"; then
    ok "[$r] A's pane border is drawn light ($L_OVER0 or $L_BLUE)"
  else no "[$r] A's pane border is drawn light ($L_OVER0 or $L_BLUE)" "$(printf '%s' "$ba" | grep -o "38;2;[0-9;]*" | sort | uniq -c)"; fi
  if { has_rgb "$bb" 38 "$D_OVER0" || has_rgb "$bb" 38 "$D_BLUE"; } && ! has_rgb "$bb" 38 "$L_OVER0" && ! has_rgb "$bb" 38 "$L_BLUE"; then
    ok "[$r] B's pane border is drawn dark ($D_OVER0 or $D_BLUE)"
  else no "[$r] B's pane border is drawn dark ($D_OVER0 or $D_BLUE)" "$(printf '%s' "$bb" | grep -o "38;2;[0-9;]*" | sort | uniq -c)"; fi
  expect_eq "[$r] fast check: #{E:@thm_bg} for A" "$(ti display -p -c "$(screen_tty A)" '#{E:@thm_bg}')" '#fbf1c7'
  expect_eq "[$r] fast check: #{E:@thm_bg} for B" "$(ti display -p -c "$(screen_tty B)" '#{E:@thm_bg}')" '#1e1e2e'
}

# T1 (AC1, FR2, validation check 1): two clients of one session, one light and
# one dark, draw different bars, window lists and borders at the same moment,
# and still do after a second source of the config (Catppuccin's -F re-bake,
# auto's token reset).
case_T1() {
  inner_start || fail_now "the inner server did not start from test.conf"
  # W0 (not current) is the session's first window; W1 is current, two panes.
  ti new-window -t s: /bin/sh
  ti split-window -h -t s: /bin/sh
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  ok "within 3 s @theme_screens = [$(screens_want A=light B=dark)]"
  expect_eq "both screens view W1 (index 1)" "$(ti list-clients -F '#{window_index}' | sort -u)" 1
  poll 5 status_line_has A 48 "$L_OTHER" >/dev/null
  t1_assert first

  check "test.conf sources a second time" ti source-file "$C/test.conf"
  hub_ready A=light B=dark
  ok "after the second source: chrome reinstalled, @theme_screens unchanged"
  poll 5 status_line_has A 48 "$L_OTHER" >/dev/null
  t1_assert second
  # A light and a dark screen on one session are mixed (NFR6), so FR3 paints
  # the window they view: W1's two panes, and not W0's (nobody displays it).
  expect_eq "the mixed window's two panes are painted, and nothing else" "$(painted_panes)" 2
}

# T2 (AC2, FR1.2-1.4, FR4): a screen that cannot report — mosh, Blink — is
# declared per HOST. C is silent and remote: its tmux client carries
# SSH_CONNECTION 100.64.0.9, which the tailscale stub names host-b. The setting
# lives in the screens file, so it outlives the client: detached and attached
# again on a new pty, C is still dark.
case_T2() {
  local ta tc td pc pc2 out rc n0 t0 t1 sp canary pd now upto te x
  inner_start || fail_now "the inner server did not start"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add C silent '100.64.0.9 5 10.0.0.1 22'
  # Detaching must leave C's outer pane in place, so that the same attach can
  # run again on a new pty (respawn-pane re-runs the pane's last command).
  to set -w -t scr:C remain-on-exit on
  hub_ready A=light C=light
  ta=$(screen_tty A); tc=$(screen_tty C); pc=$(screen_pid C)

  expect_eq "before: status shows C as host-b light default" "$(status_line_for C)" "$tc host-b light default"
  expect_eq "before: A is local light report" "$(status_line_for A)" "$ta local light report"
  check "the name came from the tailscale stub" grep -q '^tailscale status' "$C/stubs.log"
  check "@theme_hosts caches C as host-b (a resolved name)" hosts_has "$tc:$pc=host-b"
  check "@theme_remote lists C" remote_has "$tc"
  check "... and not A" not remote_has "$ta"

  out=$(ts screen host-b dark 2>&1); rc=$?
  expect_eq "screen host-b dark exits 0 silently" "$rc:$out" "0:"
  expect_eq "the screens file holds host-b dark" "$(screens_body)" "host-b dark"
  expect_eq "... below a one-line header comment" "$(head -n 1 "$C/hm/.config/tmux/screens" | cut -c1):$(grep -c . "$C/hm/.config/tmux/screens")" "#:2"
  if poll 3 status_line_has C 48 "$D_MANTLE"; then ok "C's bar is dark mantle ($D_MANTLE)"
  else no "C's bar is dark mantle ($D_MANTLE)" "$(screen_status C | cat -v)"; fi
  check "... while A's stays light" status_line_has A 48 "$L_MANTLE"
  expect_eq "status: C is host-b dark host" "$(status_line_for C)" "$tc host-b dark host"
  if poll 3 status_line_has C 48 "$D_RED"; then ok "C's host chip is red ($D_RED): a remote screen"
  else no "C's host chip is red ($D_RED): a remote screen" "$(screen_status C | cat -v)"; fi
  if has_rgb "$(screen_status A)" 48 "$L_MAUVE"; then ok "A's host chip is mauve ($L_MAUVE): a local screen"
  else no "A's host chip is mauve ($L_MAUVE): a local screen" "$(screen_status A | cat -v)"; fi

  # The prefix+T menu on C: this screen's host, mode and rule, ● on Dark; then
  # Auto and Dark again, picked from the menu (the commands bake HOST in).
  screen_keys C C-b T
  if poll 3 screen_shows C 'tmux theme'; then ok "prefix+T on C opens the menu on C"
  else no "prefix+T on C opens the menu on C" "$(screen_text C | grep -v '^ *$' | tail -15)"; fi
  check "... This screen: host-b · dark (host)" screen_shows C 'This screen: host-b · dark (host)'
  check "... with Dark marked" screen_shows C '● Dark'
  check "... and Follow marked under All screens (no pin)" screen_shows C '● Follow each screen'
  artifact menu-C-host-b-dark.txt screen_text C
  artifact menu-C-host-b-dark.ansi screen_capture C
  screen_keys C a
  if poll 3 screens_body_is ''; then ok "the menu's Auto removes host-b's line"; else no "the menu's Auto removes host-b's line" "[$(screens_body)]"; fi
  if poll 3 status_line_is C "$tc host-b light default"; then ok "... and C follows the default again"; else no "... and C follows the default again" "$(status_line_for C)"; fi
  screen_keys C C-b T
  poll 3 screen_shows C '● Auto' || no "the menu reopens with Auto marked"
  screen_keys C d
  if poll 3 screens_body_is 'host-b dark'; then ok "the menu's Dark writes host-b dark"; else no "the menu's Dark writes host-b dark" "[$(screens_body)]"; fi
  check "... and C's bar is dark" poll 3 status_line_has C 48 "$D_MANTLE"

  # Detach, then attach again on a new pty: still dark (AC2).
  screen_detach C
  if poll 3 not screen_attached C; then ok "C detached"; else no "C detached"; fi
  to respawn-pane -k -t scr:C || fail_now "respawn C's outer pane"
  poll 8 screen_attached C || fail_now "C attaches again"
  tc=$(screen_tty C); pc2=$(screen_pid C)
  hub_ready A=light C=dark
  expect_eq "after re-attaching: C is host-b dark host" "$(status_line_for C)" "$tc host-b dark host"
  check "... C's bar is dark" poll 3 status_line_has C 48 "$D_MANTLE"
  check "... @theme_hosts caches the new client" hosts_has "$tc:$pc2=host-b"
  case $(hosts_now) in *":$pc="*) no "... and has dropped the old one" "[$(hosts_now)]" ;; *) ok "... and has dropped the old one" ;; esac

  out=$(ts screen host-b auto 2>&1); rc=$?
  expect_eq "screen host-b auto exits 0 silently" "$rc:$out" "0:"
  expect_eq "... C is back to the default" "$(status_line_for C)" "$tc host-b light default"
  check "... and its bar is light" poll 3 status_line_has C 48 "$L_MANTLE"
  # Still remote, now light: the red chip is the light palette's red, so the
  # chip follows the drawing screen's palette as well as its host (D5).
  if poll 3 status_line_has C 48 "$L_RED"; then ok "... and its host chip is the light red ($L_RED)"
  else no "... and its host chip is the light red ($L_RED)" "$(screen_status C | cat -v)"; fi
  check "... not the dark one" status_line_lacks C 48 "$D_RED"

  ts screen host-b dark
  cp "$C/hm/.config/tmux/screens" "$C/screens.before"
  out=$(ts screen 'bad#h' dark 2>&1); rc=$?
  expect_eq "screen 'bad#h' dark exits 1" "$rc" 1
  check "... and leaves the file untouched" cmp -s "$C/screens.before" "$C/hm/.config/tmux/screens"

  # --- §16 item 2: only a RESOLVED name is cached ---------------------------
  # A lookup that fails, overruns its two seconds, or finds no name shows the
  # screen under its IP for that pass and leaves it out of @theme_hosts, so the
  # next pass asks again. Caching the IP would pin C to a host that its
  # `host-b dark` line can never match.
  pc=$pc2
  : > "$C/stubfail.tailscale"
  ti set -g @theme_hosts ' '          # C's host must be looked up again
  n0=$(stub_calls); ts sync
  expect_eq "(failed lookup) the pass asked tailscale once" "$(( $(stub_calls) - n0 ))" 1
  expect_eq "(failed lookup) C is light by the default: under its IP, host-b's line does not match" \
    "$(screens_now)" "$(screens_want A=light C=light)"
  check "(failed lookup) ... the IP is not cached in @theme_hosts" not hosts_has_tty "$tc"
  check "(failed lookup) ... @theme_remote still lists C (an IP is remote)" remote_has "$tc"
  n0=$(stub_calls); ts sync
  expect_eq "(failed lookup) the next pass asks again" "$(( $(stub_calls) - n0 ))" 1
  expect_eq "(failed lookup) status shows C under its IP" "$(status_line_for C)" "$tc 100.64.0.9 light default"
  rm -f "$C/stubfail.tailscale"
  ts sync
  check "(failed lookup) once tailscale answers, the name is cached" hosts_has "$tc:$pc=host-b"
  expect_eq "(failed lookup) ... and C is host-b dark (host) again" "$(status_line_for C)" "$tc host-b dark host"

  # A lookup that hangs: the watchdog ends it at 2 s, and the pass goes on.
  ti set -g @theme_hosts ' '
  : > "$C/block.tailscale"
  t0=$(now_ms); ts sync; t1=$(now_ms)
  sp=$(tail -n 1 "$C/tailscale.pids")
  rm -f "$C/block.tailscale"
  if [ $((t1 - t0)) -ge 1800 ] && [ $((t1 - t0)) -lt 4000 ]; then ok "(timed out) the pass is bounded by the 2 s watchdog ($((t1 - t0)) ms)"
  else no "(timed out) the pass is bounded by the 2 s watchdog" "$((t1 - t0)) ms"; fi
  check "(timed out) the hung lookup was killed" not kill -0 "$sp"
  check "(timed out) the IP is not cached" not hosts_has_tty "$tc"
  expect_eq "(timed out) C is light by the default meanwhile" "$(screens_now)" "$(screens_want A=light C=light)"
  ts sync
  check "(timed out) the next pass resolves and caches host-b" hosts_has "$tc:$pc=host-b"
  expect_eq "(timed out) ... and C is dark again" "$(screens_now)" "$(screens_want A=light C=dark)"

  # An IP the tailnet does not list: no name. Never cached as a NAME, but the
  # miss is remembered for 60 s (@theme_nohost), so the passes in between do
  # not run tailscale again; when it runs out, the next pass asks again. D's
  # client also carries a secret canary (see the end of the case).
  canary="tps-canary-$$-$RANDOM$RANDOM"
  SCREEN_ENV="THEME_CANARY=$canary"
  screen_add D silent '100.99.0.7 5 10.0.0.1 22'
  SCREEN_ENV=''
  td=$(screen_tty D); pd=$(screen_pid D)
  hub_ready A=light C=dark D=light
  expect_eq "(no name) status shows D under its IP" "$(status_line_for D)" "$td 100.99.0.7 light default"
  check "(no name) ... not cached as a name" not hosts_has_tty "$td"
  check "(no name) ... and remote" remote_has "$td"
  now=$(date +%s); upto=$(nohost_until "$td:$pd")
  if [ -n "$upto" ] && [ "$upto" -gt $((now + 45)) ] && [ "$upto" -le $((now + 60)) ]; then
    ok "(no name) the miss is remembered in @theme_nohost until now + at most 60 s ($((upto - now)) s left)"
  else no "(no name) the miss is remembered until now + at most 60 s" "now [$now] @theme_nohost [$(ti show -gqv @theme_nohost)]"; fi
  n0=$(stub_calls); ts sync; ts sync
  expect_eq "(no name) two more passes inside the bound do not run tailscale" "$(( $(stub_calls) - n0 ))" 0
  expect_eq "(no name) ... and still show D under its IP" "$(status_line_for D)" "$td 100.99.0.7 light default"
  # The bound, both ways: a miss that has run out is asked again, once, and
  # remembered afresh; one that claims to last longer than 60 s from now (a
  # hand-made entry, a clock step) is not believed either.
  ti set -g @theme_nohost " $td:$pd=100.99.0.7@$((now - 1)) "
  n0=$(stub_calls); ts sync
  expect_eq "(bound) an expired miss is asked again" "$(( $(stub_calls) - n0 ))" 1
  now=$(date +%s); upto=$(nohost_until "$td:$pd")
  if [ -n "$upto" ] && [ "$upto" -gt "$now" ] && [ "$upto" -le $((now + 60)) ]; then ok "(bound) ... and remembered afresh ($((upto - now)) s)"
  else no "(bound) ... and remembered afresh" "now [$now] @theme_nohost [$(ti show -gqv @theme_nohost)]"; fi
  ti set -g @theme_nohost " $td:$pd=100.99.0.7@$((now + 3600)) "
  n0=$(stub_calls); ts sync
  expect_eq "(bound) a miss claiming an hour is not believed: asked again" "$(( $(stub_calls) - n0 ))" 1
  upto=$(nohost_until "$td:$pd")
  if [ -n "$upto" ] && [ "$upto" -le $(( $(date +%s) + 60 )) ]; then ok "(bound) ... and replaced by one within the bound"
  else no "(bound) ... and replaced by one within the bound" "[$(ti show -gqv @theme_nohost)]"; fi
  check "(bound) D's IP is still not cached as a name" not hosts_has_tty "$td"

  # Outside the tailnet's 100.64.0.0/10, `tailscale status` can name nothing:
  # a LAN screen is never looked up at all.
  n0=$(stub_calls)
  screen_add E silent '192.168.1.50 5 10.0.0.1 22'
  te=$(screen_tty E)
  hub_ready A=light C=dark D=light E=light
  expect_eq "(LAN) attaching a screen from 192.168.1.50 runs no tailscale" "$(( $(stub_calls) - n0 ))" 0
  expect_eq "(LAN) status shows E under its IP" "$(status_line_for E)" "$te 192.168.1.50 light default"
  check "(LAN) ... remote" remote_has "$te"
  n0=$(stub_calls); ts sync
  expect_eq "(LAN) a later pass runs none either" "$(( $(stub_calls) - n0 ))" 0

  # env_conn's reader on a HIDDEN environment: Apple's /bin/sleep shows its
  # argv only, which must read as nothing (`none`), never as `local`.
  eval "$(sed -n "/^ENV_CONN_AWK='\$/,/^  }'\$/p" "$SCRIPTS_SRC/theme.sh")"
  sleep 5 & sp=$!
  expect_eq "(env) a ps line with no environment on it reads as none" \
    "$(ps eww -o command= -p "$sp" | LC_ALL=C awk -v mode=ps "$ENV_CONN_AWK")" none
  kill "$sp" 2>/dev/null; wait "$sp" 2>/dev/null
  expect_eq "(env) the hub's own tmux (Homebrew: environment visible, no SSH_CONNECTION) reads as local" \
    "$(ps eww -o command= -p "$INNER_PID" | LC_ALL=C awk -v mode=ps "$ENV_CONN_AWK")" local

  # The secret canary: D's client environment carries it (kcenv secrets sit
  # beside SSH_CONNECTION the same way), theme.sh has read that environment
  # several times above, and it must have left no trace: no option at any
  # scope, no environment, no message, no file of the case (logs, the
  # wrapper's records, the stubs' logs, HOME).
  if ps eww -o command= -p "$pd" | grep -qF "$canary"; then ok "(canary) D's client environment does carry it"
  else no "(canary) D's client environment does carry it"; fi
  {
    ti show -g; ti show -gw; ti show -s; ti show-environment -g; ti show-messages
    for x in $(ti list-sessions -F '#{session_id}'); do ti show -t "$x"; ti show-environment -t "$x"; done
    for x in $(ti list-windows -a -F '#{window_id}'); do ti show -w -t "$x"; done
    for x in $(ti list-panes -a -F '#{pane_id}'); do ti show -p -t "$x"; done
  } > "$C/canary.dump" 2>&1
  expect_eq "(canary) appears in no option, environment or message of the hub" "$(grep -cF "$canary" "$C/canary.dump")" 0
  expect_eq "(canary) appears in no file of the case" "$(grep -rlF "$canary" "$C" 2>/dev/null | grep -v '/canary.dump$')" ''
  expect_eq "no REFUSED call" "$(log_refused "$C")" 0
}

# nohost_until TTY:PID: the epoch second @theme_nohost remembers TTY:PID's
# miss until, or ''.
nohost_until() {
  ti show -gqv @theme_nohost 2>/dev/null | tr ' ' '\n' | awk -v k="$1" '
    index($0, k "=") == 1 { a = index($0, "@"); if (a) print substr($0, a + 1); exit }'
}

# stub_calls: how many `tailscale status` runs the stub has logged.
stub_calls() { grep -c '^tailscale status' "$C/stubs.log" 2>/dev/null || true; }

# hosts_has_tty TTY: @theme_hosts has an entry for TTY (any pid, any host).
hosts_has_tty() { case $(hosts_now) in *" $1:"*) return 0 ;; esac; return 1; }

# screens_body_is TEXT / status_line_is NAME LINE: poll predicates.
screens_body_is() { [ "$(screens_body)" = "$1" ]; }
status_line_is() { [ "$(status_line_for "$1")" = "$2" ]; }

# messages_have TEXT: the hub's message log (show-messages) holds TEXT.
messages_have() { ti show-messages 2>/dev/null | grep -qF "$1"; }

# prompt_shown NAME: the screen's last line is a command prompt (starts with
# the default prompt, `:`).
prompt_shown() { screen_capture "$1" | tail -n 1 | strip_sgr | grep -q '^:'; }

# T4 (AC4): the command prompt still clears the bar (fill) and is readable, on
# a light and on a dark screen, after two loads of the config.
case_T4() {
  local s line vis bgs prompt_pids
  inner_start || fail_now "the inner server did not start"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  check "test.conf sources again (1)" ti source-file "$C/test.conf"
  check "test.conf sources again (2)" ti source-file "$C/test.conf"
  hub_ready A=light B=dark
  # -b: the issuing client does not wait for the prompt to be answered (without
  # it, `command-prompt -t CLIENT` blocks until someone dismisses it). Still
  # backgrounded, and reaped after Escape, so a tmux that waited anyway could
  # not hang the run.
  prompt_pids=''
  for s in A B; do
    ti command-prompt -b -t "$(screen_tty "$s")" & prompt_pids="$prompt_pids $!"
  done
  for s in A B; do
    poll 3 prompt_shown "$s" || no "screen $s shows the prompt"
    line=$(to capture-pane -p -e -N -t "scr:$s" | tail -n 1)
    vis=$(printf '%s' "$line" | strip_sgr)
    bgs=$(printf '%s' "$line" | grep -o '48;2;[0-9]*;[0-9]*;[0-9]*' | sort -u | tr '\n' ' ')
    expect_eq "screen $s: the prompt line's only background is amber" "$bgs" "48;2;$AMBER "
    if has_rgb "$line" 38 "$INK"; then ok "screen $s: the prompt ink is $INK"; else no "screen $s: the prompt ink is $INK" "$(printf '%s' "$line" | cat -v)"; fi
    case $vis in
      :*) if [ -z "$(printf '%s' "${vis#:}" | tr -d ' ')" ]; then ok "screen $s: no window-list text remains on the prompt line"
          else no "screen $s: no window-list text remains on the prompt line" "[$vis]"; fi ;;
      *) no "screen $s: the prompt line starts with the prompt" "[$vis]" ;;
    esac
    expect_eq "screen $s: the fill runs from column 0 to the last column" "${#vis}" "$SCREEN_W"
    screen_keys "$s" Escape
  done
  for s in $prompt_pids; do wait "$s"; done
  for s in A B; do
    check "screen $s: Escape dismisses the prompt" poll 3 not prompt_shown "$s"
  done

  # moox: the theme hooks sit in the fixed slot [100], so a reload REPLACES
  # them — an edit deploys on `prefix r` and nothing piles up (the old %if
  # guard and its cksum note are gone with the problem). After the two reloads
  # above: one entry per hook, in slot 100. Then -ga entries an older config
  # appended (the 6e925af block) are removed by the next reload's
  # load_cleanup, and only they: a foreign hook on the same name stays.
  expect_eq "(hooks) two reloads: client-attached has one theme.sh entry, in slot 100" \
    "$(ti show-hooks -g | grep 'theme\.sh' | grep '^client-attached\[' | sed 's/ .*//')" 'client-attached[100]'
  expect_eq "(hooks) no theme option guard is set any more" "$(ti show -gqv @theme_hooks_v2)$(ti show -gqv @theme_hooks_text)" ''
  ti set-hook -ga client-attached 'set -g @foreign_hook_ran 1'
  ti source-file "$FIX_6E925AF/old-hooks.conf"
  expect_eq "(hooks) seeded: the 5 legacy -ga entries" "$(ti show-hooks -g | grep -cE 'theme\.sh (follow|refresh)')" 5
  check "test.conf sources again (3), over the legacy entries" ti source-file "$C/test.conf"
  if poll 5 eval '[ "$(ti show-hooks -g | grep -cE "theme\.sh (follow|refresh)")" = 0 ]'; then
    ok "(hooks) ... load_cleanup removed every legacy theme hook"
  else no "(hooks) ... load_cleanup removed every legacy theme hook" "$(ti show-hooks -g | grep theme)"; fi
  expect_eq "(hooks) ... the slot-100 entry stays, one per hook" \
    "$(ti show-hooks -g | grep -c "^client-attached\[[0-9]*\] .*theme\.sh'* sync")" 1
  check "(hooks) ... and the foreign hook on the same name is intact" eval 'ti show-hooks -g | grep -q @foreign_hook_ran'
}

# T5 (§7 migration): legacy state written by the OLD theme.sh and the OLD hook
# block, plus a foreign hook and a foreign window-level style, then the new
# config sourced into a hub that started bare. The legacy cleanup removes
# only values the old apply could have written (light on s, dark on t), once
# per server: values someone else set under the same names — before the
# migration or after it — survive it and every reload.
case_T5() {
  local old="$C/old-theme.sh" sid wid n name hooks sleg wleg ss st su ws0 wt0
  cp "$FIX_6E925AF/theme.sh" "$old" 2>/dev/null || fail_now "the 6e925af theme.sh fixture is missing"
  chmod 755 "$old"
  TI_CONF=/dev/null
  inner_start || fail_now "the bare inner server did not start"
  ti new-window -d -t s: /bin/sh
  ti new-session -d -s t -x 160 -y 40 /bin/sh
  ti new-window -d -t t: /bin/sh
  ti new-session -d -s u -x 160 -y 40 /bin/sh
  ti set -g status on
  ss=$(ti display -p -t s: '#{session_id}'); st=$(ti display -p -t t: '#{session_id}'); su=$(ti display -p -t u: '#{session_id}')
  ws0=$(ti display -p -t s:0 '#{window_id}'); wt0=$(ti display -p -t t:0 '#{window_id}')
  # Legacy: the old script, pinned light, against every session; then t
  # re-applied dark through the old `refresh`, so both palettes are on the hub.
  "${CASE_ENV[@]}" "TMUX=$IS,$INNER_PID,0" "$old" light >/dev/null 2>&1 || no "the old theme.sh ran"
  ti set -t t: @theme_mode dark
  "${CASE_ENV[@]}" "TMUX=$IS,$INNER_PID,0" "$old" refresh t >/dev/null 2>&1 || no "the old theme.sh refresh t ran"
  # The names the old apply wrote, per scope.
  sleg='^(@thm_[a-z0-9_]+|status-style|message-style|message-command-style|@theme_mode) '
  wleg='^(mode-style|popup-style|popup-border-style|menu-selected-style|pane-border-style|pane-active-border-style|window-status-activity-style|window-status-bell-style|clock-mode-colour) '
  expect_eq "seeded: s holds the light legacy palette, t the dark one" \
    "$(ti show -qv -t "$ss:" @thm_bg) $(ti show -qv -t "$st:" @thm_bg) $(ti show -wqv -t "$wt0" clock-mode-colour)" '#fbf1c7 #1e1e2e #89b4fa'
  expect_eq "seeded: 47 legacy session values on each of s, t and u (43 @thm_*, 3 styles, @theme_mode)" \
    "$(ti show -t "$ss" | grep -cE "$sleg") $(ti show -t "$st" | grep -cE "$sleg") $(ti show -t "$su" | grep -cE "$sleg")" '47 47 47'
  # Someone else's values under the legacy names, over the old ones: a
  # production tint on u, a tab colour of their own on s, a border on one of
  # t's windows.
  ti set -t u: status-style 'bg=#aa0000,fg=#ffffff'
  ti set -t u: message-style 'fg=#000000,bg=#ffff00'
  ti set -t s: @thm_red '#abcdef'
  ti set -w -t "$wt0" pane-border-style 'fg=#00ff00'
  # Legacy hooks: the five old entries, from the old config, and their guard.
  cp "$FIX_6E925AF/old-hooks.conf" "$C/old-hooks.conf"   # 6e925af's tmux.conf lines 561-565
  expect_eq "the old hook block is 5 set-hook lines naming theme.sh" "$(grep -cE '^set-hook -ga .*theme\.sh (follow|refresh)' "$C/old-hooks.conf")" 5
  # A foreign hook first, and a foreign window-level style.
  ti set-hook -ga client-attached 'set -g @foreign_hook_ran 1'
  ti source-file "$C/old-hooks.conf"
  ti set -g @theme_hooks_installed 1
  wid=$(ti display -p -t t: '#{window_id}')
  ti set -w -t "$wid" window-style 'bg=#123456'
  # The seed took: session-level palette and window-level styles exist.
  sid=$(ti display -p -t s: '#{session_id}')
  check "seeded: session-level @thm_bg on s" test -n "$(ti show -qv -t "$sid:" @thm_bg)"
  check "seeded: window-level mode-style on s's first window" test -n "$(ti show -wqv -t "$(ti display -p -t s:0 '#{window_id}')" mode-style)"
  expect_eq "seeded: 5 old theme.sh hook entries" "$(ti show-hooks -g | grep -cE 'theme\.sh (follow|refresh)')" 5
  # The test hook, through the server's global environment (the config's
  # run-shell job inherits it), stamps each event in ms for T6(c).
  cat > "$C/hook.sh" <<EOF
#!/bin/sh
printf '%s %s %s\n' "\$1" "\$PPID" "\$(date +%s%N | cut -c1-13)" >> '$C/hook.log'
exit 0
EOF
  chmod 755 "$C/hook.sh"
  ti set-environment -g THEME_SYNC_TESTHOOK "$C/hook.sh"

  check "the new config sources into the migrated hub" ti source-file "$C/test.conf"
  poll 5 chrome_installed || no "@theme_chrome = per-client-1" "got [$(chrome_now)]"
  settle 8 || no "the hub settles after the migration"
  ok "@theme_chrome = per-client-1"

  expect_eq "the cleanup ran, and says so (@theme_legacy_done)" "$(ti show -gqv @theme_legacy_done)" 1
  expect_eq "s: every light legacy value is gone, and only its own @thm_red stays" \
    "$(ti show -t "$ss" | grep -E "$sleg")" '@thm_red "#abcdef"'
  expect_eq "t: every dark legacy value is gone" "$(ti show -t "$st" | grep -cE "$sleg")" 0
  expect_eq "u: its own status-style and message-style are untouched" \
    "$(ti show -t "$su" | grep -E "$sleg" | LC_ALL=C sort)" \
    "$(printf '%s\n' 'message-style "fg=#000000,bg=#ffff00"' 'status-style "bg=#aa0000,fg=#ffffff"')"
  for wid in $(ti list-windows -a -F '#{window_id}'); do
    if [ "$wid" = "$wt0" ]; then
      expect_eq "window $wid (t:0): of the nine legacy styles only its own pane-border-style stays" \
        "$(ti show -w -t "$wid" | grep -E "$wleg")" 'pane-border-style "fg=#00ff00"'
    else
      expect_eq "window $wid: none of the nine window-level legacy styles" "$(ti show -w -t "$wid" | grep -cE "$wleg")" 0
    fi
  done
  # After the migration, someone sets values that are byte for byte what the
  # old apply wrote: the cleanup has run on this server, so they are theirs.
  ti set -t u: status-style 'bg=#181825,fg=#cdd6f4'
  ti set -w -t "$ws0" mode-style 'bg=#d79921,fg=#3c3836'
  hooks=$(ti show-hooks -g)
  expect_eq "no hook runs theme.sh follow or refresh" "$(printf '%s\n' "$hooks" | grep -cE 'theme\.sh (follow|refresh)')" 0
  check "the foreign client-attached hook is intact" grep -q '@foreign_hook_ran' <<<"$hooks"
  expect_eq "the foreign window-level window-style is intact" "$(ti show -wqv -t "$(ti display -p -t t: '#{window_id}')" window-style)" 'bg=#123456'
  expect_eq "@theme_hooks_installed is unset" "$(ti show -gqv @theme_hooks_installed)" ''
  for n in 1 2; do
    [ "$n" = 2 ] && { check "test.conf sources a second time" ti source-file "$C/test.conf"; settle 8 || no "settles"; }
    hooks=$(ti show-hooks -g)
    for name in client-attached client-detached client-light-theme client-dark-theme session-window-changed after-split-window \
                client-active client-session-changed after-select-window after-new-window; do
      expect_eq "[source $n] $name has exactly one theme.sh sync entry" \
        "$(printf '%s\n' "$hooks" | grep -c "^$name\\[[0-9]*\\] .*theme\\.sh'* sync")" 1
    done
    expect_eq "[source $n] the four lead hooks each record @theme_lead" \
      "$(printf '%s\n' "$hooks" | grep -cE '^(client-active|client-session-changed|after-select-window|after-new-window)\[[0-9]+\] .*@theme_lead')" 4
  done
  expect_eq "@theme_chrome is per-client-1 after the second source" "$(chrome_now)" per-client-1
  expect_eq "after two reloads: u's own status-style (an old dark literal, set after the migration) and message-style stay" \
    "$(ti show -t "$su" | grep -E "$sleg" | LC_ALL=C sort)" \
    "$(printf '%s\n' 'message-style "fg=#000000,bg=#ffff00"' 'status-style "bg=#181825,fg=#cdd6f4"')"
  expect_eq "... s:0's own mode-style (an old light literal) stays" "$(ti show -w -t "$ws0" | grep -E "$wleg")" 'mode-style "bg=#d79921,fg=#3c3836"'
  expect_eq "... s's own @thm_red stays" "$(ti show -t "$ss" | grep -E "$sleg")" '@thm_red "#abcdef"'
  expect_eq "... and t:0's own pane-border-style" "$(ti show -w -t "$wt0" | grep -E "$wleg")" 'pane-border-style "fg=#00ff00"'
  expect_eq "... the cleanup ran once: no legacy call after the first pass (show-options -w per window)" \
    "$(awk -F '\t' '$7 ~ /::W @/' "$C/calls.tsv" | grep -c .)" 1
  # T6(c): the migration batch (chrome, legacy cleanup) from the test hook's
  # timestamps: its source-file started at SF, and the pass reached
  # before_release at BR, so the batch took at most BR - SF.
  local sf br
  sf=$(awk -F '\t' '$5 ~ /(^|,)source-file(,|$)/ && $7 ~ /theme\.batch\./ { print $1; exit }' "$C/calls.tsv")
  br=$(awk '$1 == "before_release" { print $3; exit }' "$C/hook.log" 2>/dev/null)
  if [ -n "$sf" ] && [ -n "$br" ] && [ $((br - sf)) -lt 1000 ]; then ok "(T6c) the migration batch took < 1 s ($((br - sf)) ms)"
  else no "(T6c) the migration batch took < 1 s" "source-file at [$sf], before_release at [$br]"; fi
  say "  info (T6c) migration batch: $((${br:-0} - ${sf:-0})) ms"
}

# T6 (NFR3, D8), part (a): a sync that finds nothing to change makes at most 2
# tmux invocations, writes nothing, and takes < 150 ms.
case_T6() {
  local mark t0 t1 calls n w
  inner_start || fail_now "the inner server did not start"
  ti new-window -t s: /bin/sh
  ti split-window -h -t s: /bin/sh
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  ts sync
  settle 8 || no "settles after a warm-up sync"
  mark=$(log_lines)
  t0=$(now_ms); ts sync; t1=$(now_ms)
  calls=$(since "$mark")
  n=$(printf '%s' "$calls" | grep -c .)
  if [ "$n" -le 2 ]; then ok "(a) an unchanged sync makes $n tmux invocations (<= 2)"
  else no "(a) an unchanged sync makes <= 2 tmux invocations" "$(printf '%s\n' "$calls" | cut -f5,7)"; fi
  for w in source-file set-option set-environment refresh-client; do
    expect_eq "(a) ... none of them $w" "$(printf '%s\n' "$calls" | count_word "$w" '.')" 0
  done
  if [ $((t1 - t0)) -lt 150 ]; then ok "(a) wall time $((t1 - t0)) ms (< 150)"
  else no "(a) wall time < 150 ms" "$((t1 - t0)) ms"; fi
  say "  info (a) unchanged sync: $n invocations, $((t1 - t0)) ms"

  # --- (b)/(c): a server with 40 background windows of 3 panes, and the
  # current window W1 with 4 panes that A (light) and B (dark) both view.
  local w p i bf lines pl el rl ps br sf
  say "  -- (b) a lead flip on a server of 40 windows x 3 panes"
  w=$(ti display -p -t s: '#{window_id}')
  ti split-window -d -t "$w" /bin/sh
  ti split-window -d -t "$w" /bin/sh
  ti select-layout -t "$w" tiled >/dev/null
  for i in $(seq 1 40); do
    # {end} quoted: unquoted, `{` opens a command block in a config file.
    printf "new-window -d -t s: /bin/sh\nsplit-window -d -t 's:{end}' /bin/sh\nsplit-window -d -t 's:{end}' /bin/sh\n"
  done > "$C/big.conf"
  ti source-file "$C/big.conf" || no "(b) the 40 windows were created"
  expect_eq "(b) the server has 42 windows and 125 panes" \
    "$(ti list-windows -a | grep -c .) $(ti list-panes -a | grep -c .)" "42 125"
  settle 10 || no "(b) settles after the build"
  if poll 3 window_painted_as "$w" dark; then ok "(b) W1's 4 panes are painted dark (B leads)"; else no "(b) W1's 4 panes are painted dark" "$(show_window_paint "$w")"; fi
  expect_eq "(b) nothing outside W1 is painted" "$(painted_panes)" 4

  # The repaint batch, held at its source-file and copied while it exists.
  touch "$C/pause.source-file"
  mark=$(log_lines)
  typed A
  if poll 5 paused_since "$mark" source-file; then
    for bf in "$IS".theme.batch.*; do cp "$bf" "$C/flip.batch"; done
  fi
  rm -f "$C/pause.source-file"
  settle 8 || no "(b) settles after the held flip"
  check "(b) the flip repainted W1 light" window_painted_as "$w" light
  touch "$C/flip.batch"
  lines=$(grep -c . "$C/flip.batch")
  pl=$(grep -cE "^set-option -pu? -t '%[0-9]+' |^set -pu? -t '%[0-9]+' " "$C/flip.batch")
  el=$(grep -c '^set-environment ' "$C/flip.batch")
  rl=$(grep -c '^refresh-client ' "$C/flip.batch")
  expect_eq "(b) the repaint batch has exactly 24 pane lines (4 panes x marker+4 options+marker)" "$pl" 24
  if [ "$el" -le 2 ] && [ "$rl" -le 2 ] && [ "$lines" = $((pl + el + rl)) ]; then
    ok "(b) ... plus $el environment and $rl refresh-client lines, and nothing else ($lines lines)"
  else no "(b) ... plus <= 2 environment lines and <= 1 refresh-client line per client" "$(cat "$C/flip.batch" 2>&1 | cut -c1-100)"; fi
  for p in $(win_panes "$w"); do
    expect_eq "(b) $p: 6 lines, transitional marker first, final marker last" \
      "$(grep -F "'$p' " "$C/flip.batch" | awk '{ print ($5 == "@theme_painted") ? (index($0, "+") ? "T" : "M") : "o" }' | tr -d '\n')" "TooooM"
  done

  # The timed flip: the test hook, through the server's global environment,
  # stamps the pass. One pass; its tmux calls are the wrapper's lines between
  # `pass` and `before_release`.
  cat > "$C/hook.sh" <<EOF
#!/bin/sh
printf '%s %s %s\n' "\$1" "\$PPID" "\$(date +%s%N | cut -c1-13)" >> '$C/hook.log'
exit 0
EOF
  chmod 755 "$C/hook.sh"
  : > "$C/hook.log"
  ti set-environment -g THEME_SYNC_TESTHOOK "$C/hook.sh"
  typed B
  poll 3 window_painted_as "$w" dark || no "(b) B types: W1 repainted dark"
  settle 8 || no "(b) settles after the timed flip"
  ti set-environment -gu THEME_SYNC_TESTHOOK
  ps=$(awk '$1 == "pass" { print $3; exit }' "$C/hook.log")
  br=$(awk '$1 == "before_release" { print $3; exit }' "$C/hook.log")
  expect_eq "(b) the flip took one pass" "$(grep -c '^pass ' "$C/hook.log")" 1
  n=$(awk -F '\t' -v a="${ps:-0}" -v b="${br:-0}" '$1 >= a && $1 <= b' "$C/calls.tsv" | grep -c .)
  if [ "$n" -le 4 ]; then ok "(b) the repaint pass made $n tmux invocations (<= 4)"; else no "(b) the repaint pass made <= 4 tmux invocations" "$n"; fi
  sf=$(awk -F '\t' -v a="${ps:-0}" '$1 >= a && $5 ~ /(^|,)source-file(,|$)/ { print $1; exit }' "$C/calls.tsv")
  if [ -n "$ps" ] && [ -n "$br" ] && [ $((br - ps)) -lt 300 ]; then ok "(b) the whole pass took $((br - ps)) ms (< 300)"
  else no "(b) the whole pass took < 300 ms" "pass [$ps] before_release [$br]"; fi
  if [ -n "$sf" ] && [ -n "$br" ] && [ $((br - sf)) -lt 100 ]; then ok "(b) its source-file took $((br - sf)) ms (< 100)"
  else no "(b) its source-file took < 100 ms" "source-file [$sf] before_release [$br]"; fi
  say "  info (b) repaint pass: $n invocations, $((${br:-0} - ${ps:-0})) ms; source-file $((${br:-0} - ${sf:-0})) ms"

  # --- (c) the first-paint pass: W1's paint stripped by hand (no hook fires),
  # so the next pass finds 4 CANDIDATE panes: call 3, then a paint batch.
  say "  -- (c) the first-paint pass"
  set --
  for p in $(win_panes "$w"); do
    set -- "$@" set -pu -t "$p" @theme_painted ';' set -pu -t "$p" window-style ';' set -pu -t "$p" window-active-style ';' \
      set -pu -t "$p" cursor-colour ';' set -pu -t "$p" pane-colours ';'
  done
  ti "$@" display -p ''
  check "(c) W1's paint is gone" window_unpainted "$w"
  mark=$(log_lines)
  t0=$(now_ms); ts sync; t1=$(now_ms)
  n=$(since "$mark" | grep -c .)
  check "(c) the pass painted W1 again" window_painted_as "$w" dark
  if [ "$n" -le 4 ]; then ok "(c) the first-paint pass made $n tmux invocations (<= 4)"; else no "(c) the first-paint pass made <= 4 tmux invocations" "$n"; fi
  say "  info (c) first-paint pass (4 panes, call 3 and the batch): $n invocations, $((t1 - t0)) ms"
}

# T7 (NFR6): one light screen: nothing painted, a light bar, a mauve host chip
# whose text is the hostname (#() output is format-expanded, D6).
case_T7() {
  local line vis host
  inner_start || fail_now "the inner server did not start"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  hub_ready A=light
  host=$(hostname)
  poll 5 status_text_has A " $host " || true
  line=$(screen_status A)
  vis=$(printf '%s' "$line" | strip_sgr)
  expect_eq "no pane is painted" "$(painted_panes)" 0
  if has_rgb "$line" 48 "$L_MANTLE" && ! has_rgb "$line" 48 "$D_MANTLE"; then ok "the bar is light"; else no "the bar is light" "$(printf '%s' "$line" | cat -v)"; fi
  if has_rgb "$line" 48 "$L_MAUVE"; then ok "the host chip is mauve ($L_MAUVE)"; else no "the host chip is mauve ($L_MAUVE)" "$(printf '%s' "$line" | cat -v)"; fi
  case $vis in *"󰒋 $host "*) ok "the chip text is the hostname ($host)" ;; *) no "the chip text is the hostname ($host)" "[$vis]" ;; esac
  case $vis in *'#H'*) no "the chip text is not the literal #H" "[$vis]" ;; *) ok "the chip text is not the literal #H" ;; esac
}

# T7a (NFR6): two light screens on one session: nothing painted, both light.
case_T7a() {
  local s
  inner_start || fail_now "the inner server did not start"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B light
  hub_ready A=light B=light
  expect_eq "no pane is painted" "$(painted_panes)" 0
  for s in A B; do
    check "screen $s's bar is light" status_line_has "$s" 48 "$L_MANTLE"
    check "screen $s's bar is not dark" status_line_lacks "$s" 48 "$D_MANTLE"
  done
}

# T7b (NFR6): A light on s1, B dark on s2: nothing painted anywhere, and each
# bar is its own screen's, captured together.
case_T7b() {
  local sa sb
  inner_start s1 || fail_now "the inner server did not start"
  ti new-session -d -s s2 -x 160 -y 40 /bin/sh
  outer_start || fail_now "the outer server did not start"
  screen_add A light "" s1
  screen_add B dark "" s2
  hub_ready A=light B=dark
  expect_eq "no pane anywhere is painted" "$(painted_panes)" 0
  sa=$(screen_status A); sb=$(screen_status B)
  if has_rgb "$sa" 48 "$L_MANTLE" && ! has_rgb "$sa" 48 "$D_MANTLE"; then ok "A (s1) is light"; else no "A (s1) is light" "$(printf '%s' "$sa" | cat -v)"; fi
  if has_rgb "$sb" 48 "$D_MANTLE" && ! has_rgb "$sb" 48 "$L_MANTLE"; then ok "B (s2) is dark"; else no "B (s2) is dark" "$(printf '%s' "$sb" | cat -v)"; fi
  # The host chip is per screen too: local on both, in each screen's mauve.
  poll 5 status_line_has B 48 "$D_MAUVE" >/dev/null
  sa=$(screen_status A); sb=$(screen_status B)
  if has_rgb "$sa" 48 "$L_MAUVE" && ! has_rgb "$sa" 48 "$D_MAUVE"; then ok "A's local host chip is light mauve"; else no "A's local host chip is light mauve" "$(printf '%s' "$sa" | cat -v)"; fi
  if has_rgb "$sb" 48 "$D_MAUVE" && ! has_rgb "$sb" 48 "$L_MAUVE"; then ok "B's local host chip is dark mauve"; else no "B's local host chip is dark mauve" "$(printf '%s' "$sb" | cat -v)"; fi
}

# The §4.5 identities, byte for byte.
HOOK_ID_WANT='#{L:#{?#{&&:#{==:#{client_name},#{hook_client}},#{!=:#{client_control_mode},1}},#{client_name}:#{client_pid},}}'
CMD_ID_WANT='#{?client_name,#{?#{!=:#{client_control_mode},1},#{client_name}:#{client_pid},},}'

# heredoc_names FUNC: the palette names a theme.sh heredoc defines, sorted.
heredoc_body() { awk -v f="$1() {" '$0 == f { on = 1; next } on && $0 == "EOF" { exit } on && NR > 0' "$SCRIPTS_SRC/theme.sh" | sed '1d'; }

# T8 (contracts): the published palette, E: everywhere a colour is drawn,
# per-client expansion, silenced menu run-shells, the hook identities, and no
# config that sets the test hook.
case_T8() {
  local nd nl bad vals opt fa fb rs n
  # --- static: the two heredocs --------------------------------------------
  nd=$(heredoc_body palette_dark | awk 'NF == 2 { print $1 }' | sort)
  nl=$(heredoc_body palette_light | awk 'NF == 2 { print $1 }' | sort)
  expect_eq "the dark palette defines 43 names" "$(printf '%s\n' "$nd" | grep -c .)" 43
  expect_eq "the two palettes define the same names" "$nl" "$nd"
  bad=$( { heredoc_body palette_dark; heredoc_body palette_light; } | awk 'NF == 2 && $2 !~ /^#[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]$/')
  expect_eq "every palette value is lowercase #rrggbb" "$bad" ''
  # --- static: menu run-shells --------------------------------------------
  rs=$(grep -v '^[[:space:]]*#' "$SCRIPTS_SRC/theme.sh" | grep -o "run-shell '[^']*'")
  n=$(printf '%s\n' "$rs" | grep -c .)
  if [ "$n" -ge 4 ]; then ok "theme.sh's menu has $n run-shell strings"; else no "theme.sh's menu has run-shell strings" "$rs"; fi
  bad=$(printf '%s\n' "$rs" | grep -v ">/dev/null 2>&1 || true'\$")
  expect_eq "every run-shell in theme.sh's menu is silenced" "$bad" ''
  # --- static: the test hook is never configured --------------------------
  check "no config mentions THEME_SYNC_TESTHOOK" not grep -q THEME_SYNC_TESTHOOK "$OPTS_SRC" "$THEME_SRC" "$STATUS_SRC"
  # --- static: the four lead hooks (§4.5), byte for byte, in slot [100] -----
  expect_eq "the apply region holds the four §4.5 lead hooks byte for byte" \
    "$(region apply | grep -E "^set-hook -g '(client-active|client-session-changed|after-select-window|after-new-window)\\[100\\]' ")" "$LEAD_HOOKS_WANT"
  # --- static: every theme hook is indexed in the theme's slot (moox §5.1) --
  # An indexed set-hook replaces itself on reload, so no %if guard and no
  # fingerprint of a frozen block exist any more.
  expect_eq "the apply region registers the ten theme hooks, each in slot [100]" \
    "$(region apply | grep -c "^set-hook -g '[a-z-]*\\[100\\]' ")" 10
  expect_eq "... and no other set-hook (no -ga, no unindexed one)" \
    "$(region apply | grep '^set-hook' | grep -vc "^set-hook -g '[a-z-]*\\[100\\]' ")" 0
  expect_eq "... no %if guard and no hook fingerprint" \
    "$(region apply | grep -cE '^%if|@theme_hooks_(v2|text)')" 0
  # --- static: the frozen legacy palette is what 6e925af shipped -------------
  local oldsrc
  oldsrc=$(cat "$FIX_6E925AF/theme.sh" 2>/dev/null) || no "the 6e925af theme.sh fixture"
  expect_eq "legacy_palette is byte for byte the 6e925af palettes, name dark light, in their order" \
    "$(heredoc_body legacy_palette | awk 'NF == 3 { print $1, $2, $3 }')" \
    "$(printf '%s\n' "$oldsrc" | awk '
        /^palette_dark\(\) \{$/ { s = "d"; next } /^palette_light\(\) \{$/ { s = "l"; next } /^EOF$/ { s = ""; next }
        $1 == "cat" { next }
        s == "d" && NF == 2 { d[$1] = $2; o[++n] = $1 } s == "l" && NF == 2 { l[$1] = $2 }
        END { for (i = 1; i <= n; i++) print o[i], d[o[i]], l[o[i]] }')"
  expect_eq "... 43 names" "$(heredoc_body legacy_palette | awk 'NF == 3' | grep -c .)" 43
  # --- static: the paint palettes (FR3.4): bg, fg and slots 0-15 each ------
  for f in term_palette_light term_palette_dark; do
    expect_eq "$f defines bg, fg and 0-15" "$(heredoc_body "$f" | awk 'NF == 2 { print $1 }' | LC_ALL=C sort | tr '\n' ' ')" \
      "$(printf '%s\n' bg fg $(seq 0 15) | LC_ALL=C sort | tr '\n' ' ')"
    expect_eq "$f: every value is lowercase #rrggbb" "$(heredoc_body "$f" | awk 'NF == 2 && $2 !~ /^#[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]$/')" ''
  done
  # --- static: the two window-number pins (§5.5, D7) ------------------------
  # chrome_cmds rewrites both at every sync, so a live server cannot show a
  # broken pin in .tmux.conf; only the text can.
  expect_eq "the options.conf window-number pins read @thm_* through E:" \
    "$(region pre-plugin | grep -E '^set -g @catppuccin_window_(current_)?number_color ' | sort)" \
    "$(printf '%s\n' 'set -g @catppuccin_window_current_number_color "#{E:@thm_yellow}"' 'set -g @catppuccin_window_number_color "#{E:@thm_overlay_2}"')"

  # --- live ---------------------------------------------------------------
  inner_start || fail_now "the inner server did not start"
  ti new-window -t s: /bin/sh
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  vals=$(ti show -g | grep '^@thm_')
  expect_eq "show -g lists 43 @thm_* options" "$(printf '%s\n' "$vals" | grep -c .)" 43
  bad=$(printf '%s\n' "$vals" | grep -vE '^@thm_[a-z0-9_]+ "#\{\?#\{E:@theme_dark\},#[0-9a-f]{6},#[0-9a-f]{6}\}"$')
  expect_eq "every @thm_* is #{?#{E:@theme_dark},#dark,#light}" "$bad" ''
  vals=$( { ti show -gv status-left; ti show -gv status-right; ti show -gw | grep '^window-status';
            ti show -g | grep -E '^@catppuccin_(window_|status_(session|date_time))'; } )
  bad=$(printf '%s\n' "$vals" | grep -F '#{@thm_')
  expect_eq "no drawn option reads #{@thm_…} without E:" "$bad" ''
  for opt in status-left status-right; do
    fa=$(ti display -p -c "$(screen_tty A)" "$(ti show -gv "$opt")")
    fb=$(ti display -p -c "$(screen_tty B)" "$(ti show -gv "$opt")")
    case "$fa$fb" in *'#{'*) no "$opt expands fully for A and B" "A: $fa
B: $fb" ;; *) ok "$opt expands fully for A and B" ;; esac
    if [ "$fa" != "$fb" ]; then ok "$opt expands differently for A and B"; else no "$opt expands differently for A and B" "$fa"; fi
  done
  expect_eq "@theme_hook_id is §4.5 byte for byte" "$(ti show -gv @theme_hook_id)" "$HOOK_ID_WANT"
  expect_eq "@theme_cmd_id is §4.5 byte for byte" "$(ti show -gv @theme_cmd_id)" "$CMD_ID_WANT"
  check "the test.conf in use does not mention THEME_SYNC_TESTHOOK" not grep -q THEME_SYNC_TESTHOOK "$C/test.conf"
  check "the hub's global environment has no THEME_SYNC_TESTHOOK" not ti show-environment -g THEME_SYNC_TESTHOOK
}

# T9 (CLI, §8.4): verbs, status, toggle (D18), the prefix+T menu, and `screen`
# (FR4.2), including `screen this … CLIENT`.
case_T9() {
  local out rc ta tb tc re def
  inner_start || fail_now "the inner server did not start"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  ta=$(screen_tty A); tb=$(screen_tty B)

  out=$(ts bogus 2>&1); rc=$?
  expect_eq "an unknown verb exits 1" "$rc" 1
  case $out in usage:*) ok "and prints the usage line" ;; *) no "and prints the usage line" "$out" ;; esac

  out=$(ts detect); rc=$?
  case "$rc:$out" in 0:light|0:dark) ok "detect prints light or dark ($out)" ;; *) no "detect prints light or dark" "rc=$rc [$out]" ;; esac

  out=$(ts status); rc=$?
  expect_eq "status exits 0" "$rc" 0
  expect_eq "status prints one line per screen" "$(printf '%s\n' "$out" | grep -c .)" 2
  re='^/dev/[^ ]+ [a-z0-9._:-]+ (light|dark) (pin|host|bg|report|default)$'
  expect_eq "every status line matches §4.4" "$(printf '%s\n' "$out" | grep -cvE "$re")" 0
  expect_eq "status: A is local light report" "$(status_line_for A)" "$ta local light report"
  expect_eq "status: B is local dark report" "$(status_line_for B)" "$tb local dark report"
  expect_eq "status is sorted by tty" "$(printf '%s\n' "$out" | cut -d' ' -f1)" "$(printf '%s\n%s\n' "$ta" "$tb" | LC_ALL=C sort)"

  ti set -g @theme_screens ' '
  ts follow
  expect_eq "follow behaves as sync (restores @theme_screens)" "$(screens_now)" "$(screens_want A=light B=dark)"
  ti set -g @theme_screens ' '
  ts refresh x
  expect_eq "refresh x behaves as sync (restores @theme_screens)" "$(screens_now)" "$(screens_want A=light B=dark)"
  settle 5 || true

  out=$("${CASE_ENV[@]}" "TMUX=$C/nosuch,1,0" "$C/hm/.tmux/scripts/theme.sh" sync 2>&1); rc=$?
  expect_eq "TMUX naming no socket: sync exits 0 silently" "$rc:$out" "0:"
  "${CASE_ENV[@]}" "$REAL_TMUX" -S "$C/x" -f /dev/null new-session -d /bin/sh
  kill_private "$C/x"
  check "a killed server's socket file stays behind" test -S "$C/x"
  out=$("${CASE_ENV[@]}" "TMUX=$C/x,1,0" "$C/hm/.tmux/scripts/theme.sh" sync 2>&1); rc=$?
  expect_eq "TMUX naming a dead server: sync exits 0 silently" "$rc:$out" "0:"
  rm -rf "$C"/x.theme.* 2>/dev/null

  # toggle (D18): A light, B dark, no pin.
  check "no pin before the toggles" test ! -e "$C/hm/.config/tmux/theme.pinned"
  ts toggle "$tb"
  expect_eq "toggle <B (dark)> pins light" "$(theme_file theme.pinned)" light
  expect_eq "... and every screen is light" "$(screens_now)" "$(screens_want A=light B=light)"
  ts toggle "$ta"
  expect_eq "toggle <A (now light by the pin)> pins dark" "$(theme_file theme.pinned)" dark
  expect_eq "... and every screen is dark" "$(screens_now)" "$(screens_want A=dark B=dark)"
  expect_eq "@theme_default follows the pin" "$(ti show -gv @theme_default)" dark
  ts toggle
  expect_eq "bare toggle pins the opposite of @theme_default (dark)" "$(theme_file theme.pinned)" light
  ts toggle /dev/ttys-not-attached
  expect_eq "toggle <a client not attached> flips @theme_default (light)" "$(theme_file theme.pinned)" dark
  ts toggle 'bad;#{x}'
  expect_eq "toggle <not a client name> is treated as absent" "$(theme_file theme.pinned)" light
  ts forget
  check "forget drops the pin" test ! -e "$C/hm/.config/tmux/theme.pinned"
  expect_eq "forget: screens follow their reports again" "$(screens_now)" "$(screens_want A=light B=dark)"
  ts dark
  expect_eq "dark pins dark" "$(theme_file theme.pinned):$(theme_file theme)" dark:dark
  ts light
  expect_eq "light pins light" "$(theme_file theme.pinned):$(theme_file theme)" light:light
  expect_eq "status after a pin says pin" "$(status_line_for B)" "$tb local light pin"

  # prefix+T, pressed on screen A: the binding passes #{client_name}, so the
  # menu opens on A and its Toggle flips A. Discriminating setup: the default
  # is dark while A is light by its report, so toggling A pins dark, where a
  # toggle that lost the client (bare: the opposite of the default) would pin
  # light.
  ts forget
  printf 'dark\n' > "$C/hm/.config/tmux/theme"
  ts sync
  expect_eq "setup: default dark, A light by its report" "$(ti show -gv @theme_default) $(status_line_for A)" "dark $ta local light report"
  screen_keys A C-b T
  if poll 3 screen_shows A 'tmux theme'; then ok "prefix+T on A opens the menu on A"; else no "prefix+T on A opens the menu on A" "$(screen_capture A | strip_sgr | grep -v '^ *$' | tail -15)"; fi
  check "... and not on B" not screen_shows B 'tmux theme'
  check "... with Follow marked (no pin)" screen_shows A '● Follow each screen'
  check "... This screen names A's host, mode and rule" screen_shows A 'This screen: local · light (report)'
  check "... with Auto marked (the screens file has no line for local)" screen_shows A '● Auto'
  check "... and an All screens section" screen_shows A 'All screens'
  artifact menu-A-local-light.txt screen_text A
  artifact menu-A-local-light.ansi screen_capture A
  screen_keys A t
  if poll 3 pinned_is dark; then ok "the menu's Toggle flips A (light): pins dark, not the default's opposite"; else no "the menu's Toggle flips A (light): pins dark" "pin [$(theme_file theme.pinned)]"; fi
  check "the menu closed" poll 3 not screen_shows A 'tmux theme'
  if poll 3 screens_are "$(screens_want A=dark B=dark)"; then ok "... and every screen is dark"; else no "... and every screen is dark" "[$(screens_now)]"; fi
  expect_eq "no REFUSED call (the menu's jobs went through the wrapper)" "$(log_refused "$C")" 0

  # --- screen HOST|this MODE [CLIENT] (FR4.2) -------------------------------
  ts forget
  settle 5 || no "settles after forget"
  for out in 'local bogus' 'local' '' 'bad#h dark' '-x dark' "$(printf 'h%.0s' $(seq 1 64)) dark" 'this dark /dev/ttys-not-attached'; do
    # shellcheck disable=SC2086  # the words of each case, split on purpose
    rc=$(ts screen $out >/dev/null 2>&1; printf '%s' "$?")
    expect_eq "screen [$out] exits 1" "$rc" 1
  done
  case $(ts screen local bogus 2>&1) in usage:*) ok "... a bad mode prints the usage line" ;; *) no "... a bad mode prints the usage line" "$(ts screen local bogus 2>&1)" ;; esac
  check "... and no rejected call wrote a screens file" test ! -e "$C/hm/.config/tmux/screens"

  screen_add C silent '100.64.0.10 5 10.0.0.1 22'
  tc=$(screen_tty C)
  def=$(ti show -gv @theme_default)
  hub_ready A=light B=dark C="$def"
  expect_eq "C is omarchy (its SSH_CONNECTION through the tailscale stub)" "$(status_line_for C)" "$tc omarchy $def default"
  out=$(ts screen this dark "$tc" 2>&1); rc=$?
  expect_eq "screen this dark <ttyC> exits 0 silently" "$rc:$out" "0:"
  expect_eq "... and writes C's host" "$(screens_body)" "omarchy dark"
  expect_eq "... status: C is omarchy dark host" "$(status_line_for C)" "$tc omarchy dark host"
  out=$(ts screen THIS Light "$ta" 2>&1); rc=$?
  expect_eq "screen THIS Light <ttyA> exits 0 (words compared lowercase)" "$rc:$out" "0:"
  expect_eq "... and writes A's host, local, sorted with the rest" "$(screens_body)" "$(printf 'local light\nomarchy dark')"
  expect_eq "... status: A is local light host" "$(status_line_for A)" "$ta local light host"
  expect_eq "... and so is B, the other local screen" "$(status_line_for B)" "$tb local light host"
  expect_eq "... @theme_screens" "$(screens_now)" "$(screens_want A=light B=light C=dark)"
  ts screen local auto
  ts screen omarchy auto
  expect_eq "auto removes each line: only the header remains" "$(screens_body):$(grep -c . "$C/hm/.config/tmux/screens")" ":1"
  expect_eq "... and every screen is back to its report or the default" "$(screens_now)" "$(screens_want A=light B=dark C="$def")"
}

# screen_text NAME: what the screen shows, without its SGR colours.
# screen_shows NAME TEXT: the screen's visible text contains TEXT.
screen_text() { screen_capture "$1" | strip_sgr; }
screen_shows() { screen_text "$1" | grep -qF "$2"; }
pinned_is() { [ "$(theme_file theme.pinned)" = "$1" ]; }

# T10 (D9, §6.5 with §16 items 1, 2, 6 and 10): the request/lock protocol and
# request acknowledgement, run under the shell named by the entry (T10@sh,
# T10@dash). `theme.sh screen local …` is the request that changes the
# published state, as §8.4.2 has it. (e2) interrupts a pass inside its host
# lookup.
case_T10() {
  local hook="$C/hook.sh" want pids rc t0 t1 mark r1 pg n dp stamp lk tc pc tsf sp np out def
  inner_start || fail_now "the inner server did not start"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  expect_eq "theme.sh runs under $CSUT" "$(head -n 1 "$C/hm/.tmux/scripts/theme.sh")" \
    "$(if [ "$CSUT" = dash ]; then printf '#!%s' "$DASH"; else printf '#!/bin/sh'; fi)"
  lk=$(LOCKD)
  cat > "$hook" <<EOF
#!/bin/sh
printf '%s %s\n' "\$1" "\$PPID" >> '$C/hook.log'
if [ "\$1" = before_release ] && mv '$C/hook.action' '$C/hook.action.run' 2>/dev/null; then
  sh '$C/hook.action.run' >/dev/null 2>&1
fi
exit 0
EOF
  chmod 755 "$hook"
  TS_EXTRA_ENV=("THEME_SYNC_TESTHOOK=$hook")
  pend() { ti set -g @theme_screens ' '; }   # a pending change with no request
  want=$(screens_want A=light B=dark)

  # --- (a) coalescing -------------------------------------------------------
  say "  -- (a) coalescing"
  pend
  mark=$(log_lines)
  touch "$C/pause.list-clients"
  ts_bg sync; r1=$TS_PID
  poll 3 paused_since "$mark" list-clients || no "(a) R1's list-clients is paused"
  pids=''
  t0=$(now_ms)
  for n in $(seq 1 20); do ts sync & pids="$pids $!"; done
  rc=0; for n in $pids; do wait "$n" || rc=$?; done
  t1=$(now_ms)
  expect_eq "(a) 20 concurrent syncs all exit 0" "$rc" 0
  if [ $((t1 - t0)) -le 1000 ]; then ok "(a) ... within 1 s ($((t1 - t0)) ms)"; else no "(a) ... within 1 s" "$((t1 - t0)) ms"; fi
  check "(a) the dirty flag exists" test -e "$(DIRTYF)"
  expect_eq "(a) exactly one lock, holding owner.<R1>" "$(ls -A "$lk" 2>&1)" "owner.$r1"
  expect_eq "(a) ... whose owner file records the runner's script, which R1's command line names" \
    "$(cat "$lk/owner.$r1" 2>&1)" "$C/hm/.tmux/scripts/theme.sh"
  say "  info (a) R1's command line: $(ps -ww -o command= -p "$r1")"
  ok "(a) 20 requests checked R1's identity against that record and none broke its lock (a live runner is never broken)"
  expect_eq "(a) the log shows exactly one list-clients (R1's, paused)" "$(since "$mark" | count_word list-clients '.')" 1
  out=$(ts screen local dark 2>&1); rc=$?
  expect_eq "(a) screen local dark while paused exits 0 at once" "$rc:$out" "0:"
  expect_eq "(a) ... and wrote the screens file" "$(screens_body)" "local dark"
  rm -f "$C/pause.list-clients"
  wait "$r1"
  expect_eq "(a) R1 ran exactly one more pass (two list-clients in all)" "$(since "$mark" | count_word list-clients)" 2
  expect_eq "(a) the published state reflects local dark" "$(screens_now)" "$(screens_want A=dark B=dark)"
  check "(a) no lock and no dirty afterwards" sync_idle
  ts screen local auto; settle 5 || no "(a) settles after screen local auto"

  # --- (b) release boundary -------------------------------------------------
  say "  -- (b) release boundary"
  : > "$C/hook.log"
  cat > "$C/hook.action" <<'EOF'
exec "$HOME/.tmux/scripts/theme.sh" screen local light
EOF
  ts sync
  expect_eq "(b) exactly 2 passes" "$(grep -c '^pass ' "$C/hook.log")" 2
  expect_eq "(b) re-acquired after the release (2 acquired, one pid)" \
    "$(awk '$1 == "acquired" { print $2 }' "$C/hook.log" | sort -u | wc -l | tr -d ' '):$(grep -c '^acquired ' "$C/hook.log")" 1:2
  expect_eq "(b) the request made at before_release is published (local light)" "$(screens_now)" "$(screens_want A=light B=light)"
  check "(b) no lock and no dirty" sync_idle
  ts screen local auto; settle 5 || no "(b) settles after screen local auto"

  # --- (c) stale rules (§16 item 1: a live owner is never broken) -----------
  say "  -- (c) stale rules"
  dp=$(dead_pid)
  mkdir "$lk"; : > "$lk/owner.$dp"
  pend
  ts sync; rc=$?
  expect_eq "(c1) a dead owner: sync exits 0" "$rc" 0
  expect_eq "(c1) ... breaks the lock and runs" "$(screens_now)" "$want"
  check "(c1) ... and removes the lock" sync_idle
  stamp=$(ago_stamp 3)
  mkdir "$lk"; : > "$lk/owner.$$"; touch -t "$stamp" "$lk/owner.$$" "$lk"
  pend
  ts sync; rc=$?
  expect_eq "(c2) a live owner 3 min old: sync exits 0" "$rc" 0
  expect_eq "(c2) ... without a pass (age alone is not staleness)" "$(screens_now)" ' '
  check "(c2) ... leaving dirty" test -e "$(DIRTYF)"
  expect_eq "(c2) ... and the live owner's lock" "$(ls -A "$lk" 2>&1)" "owner.$$"
  rm -rf "$lk"
  ts sync
  expect_eq "(c2) once the lock is gone, the owed pass runs" "$(screens_now)" "$want"
  check "(c2) ... and dirty is consumed" sync_idle
  mkdir "$lk"
  pend
  ts sync; rc=$?
  expect_eq "(c3) an empty fresh lock: exit 0 without a pass" "$rc:$(screens_now)" "0: "
  check "(c3) ... leaving dirty" test -e "$(DIRTYF)"
  rmdir "$lk"
  ts sync
  expect_eq "(c3) rmdir, sync: the pass runs" "$(screens_now)" "$want"
  check "(c3) ... and dirty is consumed" sync_idle
  mkdir "$lk"; touch -t "$stamp" "$lk"
  pend
  ts sync
  expect_eq "(c4) an empty lock 3 min old is broken and the pass runs" "$(screens_now)" "$want"
  check "(c4) ... no lock, no dirty" sync_idle
  # (c5) A runner killed with SIGKILL leaves its lock, and its pid is soon
  # some other live process: kill -0 alone would keep that lock forever. The
  # owner file records the script, the live pid's command line does not name
  # it, so the lock is broken — and the process itself is left alone.
  sleep 30 & sp=$!
  mkdir "$lk"; printf '%s\n' "$C/hm/.tmux/scripts/theme.sh" > "$lk/owner.$sp"
  pend
  ts sync; rc=$?
  expect_eq "(c5) a reused pid (a live 'sleep 30' under a recorded theme.sh owner): sync exits 0" "$rc" 0
  expect_eq "(c5) ... breaks the lock and runs" "$(screens_now)" "$want"
  check "(c5) ... no lock, no dirty" sync_idle
  check "(c5) ... and the process behind the reused pid is untouched" kill -0 "$sp"
  # (c6) A live owner whose command line DOES name the recorded script is the
  # runner, at any age (§16 item 1): never broken. Two commands, so sh cannot
  # exec the sleep in its place and drop the script's path from its argv.
  sh -c 'sleep 30; :' "$C/hm/.tmux/scripts/theme.sh" & np=$!
  mkdir "$lk"; printf '%s\n' "$C/hm/.tmux/scripts/theme.sh" > "$lk/owner.$np"; touch -t "$stamp" "$lk/owner.$np" "$lk"
  pend
  ts sync; rc=$?
  expect_eq "(c6) a live owner 3 min old whose command names the recorded script: exit 0 without a pass" "$rc:$(screens_now)" "0: "
  expect_eq "(c6) ... and its lock is left alone" "$(ls -A "$lk" 2>&1)" "owner.$np"
  { pkill -P "$np"; kill "$sp" "$np"; wait "$sp" "$np"; } 2>/dev/null
  rm -rf "$lk"
  ts sync
  expect_eq "(c6) once the lock is gone, the owed pass runs" "$(screens_now)" "$want"
  check "(c6) ... no lock, no dirty" sync_idle

  # --- (d) a failed pass, and no later request ------------------------------
  say "  -- (d) a failed pass"
  : > "$C/hook.log"
  pend
  touch "$C/failonce.source-file"
  mark=$(log_lines)
  ts sync
  expect_eq "(d) 2 source-file calls: rc 1 (failonce) then rc 0" \
    "$(since "$mark" | awk -F '\t' '$5 ~ /(^|,)source-file(,|$)/ { printf "%s ", $4 }')" "1 0 "
  expect_eq "(d) one acquired, two passes, one pid" \
    "$(grep -c '^acquired ' "$C/hook.log"):$(grep -c '^pass ' "$C/hook.log"):$(awk '{ print $2 }' "$C/hook.log" | sort -u | wc -l | tr -d ' ')" 1:2:1
  expect_eq "(d) @theme_screens is correct again" "$(screens_now)" "$want"
  check "(d) no lock and no dirty" sync_idle

  # --- (e) an interrupted pass, and no later request ------------------------
  say "  -- (e) an interrupted pass"
  pend
  mark=$(log_lines)
  touch "$C/pause.list-clients"
  set -m
  ts_bg sync; pg=$TS_PID
  set +m
  poll 3 paused_since "$mark" list-clients || no "(e) R1's list-clients is paused"
  r1=$(lock_owner)
  check "(e) R1 holds the lock" test -n "$r1"
  # Grouped under one redirect: bash's own "Terminated" job notice goes to
  # the shell's stderr when it reaps the job, not to `wait`'s.
  { kill -TERM -- -"$pg"; wait "$pg"; } 2>/dev/null
  if poll 1 missing "$lk/owner.$r1"; then ok "(e) within 1 s owner.<R1> is gone"; else no "(e) within 1 s owner.<R1> is gone" "$(ls -A "$lk" 2>&1)"; fi
  n=$(lock_owner)
  if [ -z "$n" ] || { [ "$n" != "$r1" ] && kill -0 "$n" 2>/dev/null; }; then ok "(e) the lock is absent or held by a relaunched runner ($n)"
  else no "(e) the lock is absent or held by a relaunched runner" "owner [$n], R1 [$r1]"; fi
  if [ -e "$(DIRTYF)" ] || [ -n "$n" ]; then ok "(e) the request is still owed (dirty, or taken by the relaunch)"
  else no "(e) the request is still owed (dirty, or taken by the relaunch)"; fi
  rm -f "$C/pause.list-clients"
  if poll 3 screens_settled "$want"; then ok "(e) with no further request: correct, no lock, no dirty"
  else no "(e) with no further request: correct, no lock, no dirty" "screens [$(screens_now)] lock [$(ls -A "$lk" 2>&1)]"; fi

  # --- (f) a persistent failure is bounded, and its request stays owed ------
  say "  -- (f) a persistent failure"
  settle 5 || true
  pend
  touch "$C/fail.source-file"
  mark=$(log_lines)
  t0=$(now_ms); ts sync; rc=$?; t1=$(now_ms)
  expect_eq "(f) sync exits 0" "$rc" 0
  if [ $((t1 - t0)) -lt 2000 ]; then ok "(f) ... within 2 s ($((t1 - t0)) ms)"; else no "(f) ... within 2 s" "$((t1 - t0)) ms"; fi
  expect_eq "(f) exactly 3 source-file attempts, all rc 1" \
    "$(since "$mark" | count_word source-file):$(since "$mark" | count_word source-file '^1$')" 3:3
  check "(f) no lock" test ! -e "$lk"
  check "(f) dirty exists: the request is still owed" test -e "$(DIRTYF)"
  rm -f "$C/fail.source-file"
  mark=$(log_lines)
  sleep 2
  expect_eq "(f) nothing polls: no list-clients in 2 s" "$(since "$mark" | count_word list-clients '.')" 0
  ts sync
  expect_eq "(f) the next sync publishes" "$(screens_now)" "$want"
  check "(f) ... and dirty is gone" sync_idle

  # --- (g) storm ------------------------------------------------------------
  say "  -- (g) storm"
  pend
  pids=''
  for n in $(seq 1 20); do ts sync & pids="$pids $!"; done
  for n in $pids; do wait "$n"; done
  poll 3 sync_idle || no "(g) the storm drains"
  expect_eq "(g) the published state equals a fresh status" "$(screens_now)" "$(screens_from_status)"
  expect_eq "(g) ... which is the screens' own" "$(screens_now)" "$want"
  check "(g) no lock, no dirty" sync_idle
  expect_eq "(g) no REFUSED call" "$(log_refused "$C")" 0

  # --- (e2) an interrupted pass, inside its host lookup (§16 items 2, 6, 10) -
  # A remote screen whose host is not cached sends the pass into `tailscale
  # status`, which the stub holds while block.tailscale exists. The runner
  # alone is signalled (not its group): the shell is waiting in `wait`, which
  # a trapped signal interrupts at once, so on_exit must itself kill the hung
  # lookup, delete its temp file, re-mark the request and hand it on.
  say "  -- (e2) an interrupted host lookup"
  settle 5 || true
  screen_add C silent '100.64.0.10 5 10.0.0.1 22'
  tc=$(screen_tty C); pc=$(screen_pid C)
  def=$(ti show -gv @theme_default)
  hub_ready A=light B=dark C="$def"
  check "(e2) C is cached as omarchy" hosts_has "$tc:$pc=omarchy"
  ti set -g @theme_hosts ' '          # a pending change that needs a lookup
  : > "$C/block.tailscale"
  np=$(grep -c . "$C/tailscale.pids" 2>/dev/null || true)
  ts_bg sync; r1=$TS_PID
  if poll 3 lines_above "$C/tailscale.pids" "$np"; then ok "(e2) R1 is inside its tailscale lookup"
  else no "(e2) R1 is inside its tailscale lookup" "pids: $(cat "$C/tailscale.pids" 2>&1)"; fi
  sp=$(tail -n 1 "$C/tailscale.pids")
  tsf="$IS.theme.tailscale.$r1"
  check "(e2) the lookup writes to $tsf (named before it exists)" test -e "$tsf"
  expect_eq "(e2) R1 holds the lock" "$(lock_owner)" "$r1"
  { kill -TERM "$r1"; wait "$r1"; } 2>/dev/null
  if poll 1 not kill -0 "$sp" 2>/dev/null; then ok "(e2) the hung lookup died with its runner (on_exit killed it)"
  else no "(e2) the hung lookup died with its runner (on_exit killed it)" "stub $sp is alive"; fi
  rm -f "$C/block.tailscale"
  check "(e2) on_exit deleted the lookup's temp file" missing "$tsf"
  if poll 1 missing "$lk/owner.$r1"; then ok "(e2) within 1 s owner.<R1> is gone"; else no "(e2) within 1 s owner.<R1> is gone" "$(ls -A "$lk" 2>&1)"; fi
  n=$(lock_owner)
  if [ -e "$(DIRTYF)" ] || { [ -n "$n" ] && [ "$n" != "$r1" ]; }; then ok "(e2) the request is still owed (dirty, or taken by the relaunch)"
  else no "(e2) the request is still owed (dirty, or taken by the relaunch)" "lock [$n]"; fi
  if poll 3 hosts_has "$tc:$pc=omarchy"; then ok "(e2) with no further request the relaunch re-caches C as omarchy"
  else no "(e2) with no further request the relaunch re-caches C as omarchy" "[$(hosts_now)]"; fi
  if poll 3 screens_settled "$(screens_want A=light B=dark C="$def")"; then ok "(e2) ... correct, no lock, no dirty"
  else no "(e2) ... correct, no lock, no dirty" "screens [$(screens_now)] lock [$(ls -A "$lk" 2>&1)]"; fi
  expect_eq "(e2) no tailscale temp file is left beside the socket" "$(count_files "$IS".theme.tailscale.*)" 0

  # --- (h) the lookup's watchdog ends with the lookup (§16 item 10) ----------
  # The watchdog is a forked copy of theme.sh that sleeps two seconds, then
  # kills an overrunning lookup. Cancelled when the lookup ends, nothing of a
  # runner outlives its pass. Left running, a copy of theme.sh would linger
  # for 2 s after the runner exited, and could then kill a reused pid.
  say "  -- (h) the lookup's watchdog is cancelled"
  settle 5 || true
  ti set -g @theme_hosts ' '          # C's host must be looked up again
  np=$(grep -c . "$C/tailscale.pids" 2>/dev/null || true)
  ts sync
  check "(h) the pass ran a tailscale lookup" lines_above "$C/tailscale.pids" "$np"
  if poll 1 no_theme_procs; then ok "(h) within 1 s of the runner's exit no theme.sh process remains (the watchdog was cancelled)"
  else no "(h) within 1 s of the runner's exit no theme.sh process remains (the watchdog was cancelled)" "$(pgrep -fl "$C/hm/.tmux/scripts/theme" 2>&1)"; fi
  check "(h) the lookup's answer is cached again (omarchy)" hosts_has "$tc:$pc=omarchy"
  expect_eq "(h) no tailscale temp file is left beside the socket" "$(count_files "$IS".theme.tailscale.*)" 0
  TS_EXTRA_ENV=()
}

# lines_above FILE N: FILE has more than N lines.
lines_above() { [ "$(grep -c . "$1" 2>/dev/null || true)" -gt "$2" ]; }

# no_theme_procs: no process runs this case's theme.sh (a runner, a relaunch,
# or a forked copy such as the lookup's watchdog).
no_theme_procs() { ! pgrep -f "$C/hm/.tmux/scripts/theme" >/dev/null 2>&1; }

# T11 (D1, FR1.1 as amended): only the GLOBAL window-style bg outranks a
# report; a window's own bg and a pane's OSC 11 never decide a mode.
case_T11() {
  local ta tb tc w2 pane def
  inner_start || fail_now "the inner server did not start"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  ta=$(screen_tty A); tb=$(screen_tty B)

  # (i) an Omarchy-like global window-style, light.
  ti set -gw window-style 'fg=#3c3836,bg=#fbf1c7'
  ts sync
  expect_eq "(i) B (reports dark) resolves light by the global bg" "$(status_line_for B)" "$tb local light bg"
  expect_eq "(i) A resolves light by the global bg" "$(status_line_for A)" "$ta local light bg"
  expect_eq "(i) @theme_screens: both light" "$(screens_now)" "$(screens_want A=light B=light)"
  expect_eq "(i) nothing is painted" "$(painted_panes)" 0
  ti set -gwu window-style
  ts sync
  expect_eq "(i) unset again: B is dark by its report" "$(status_line_for B)" "$tb local dark report"

  # (ii) a window-level bg on W2, which both screens view.
  ti new-window -t s: /bin/sh
  w2=$(ti display -p -t s: '#{window_id}')
  pane=$(ti display -p -t s: '#{pane_id}')
  ti set -w -t "$w2" window-style 'bg=#123456'
  ts sync
  expect_eq "(ii) W2 is current for both screens" "$(ti list-clients -F '#{window_id}' | sort -u)" "$w2"
  expect_eq "(ii) A is light by its report" "$(status_line_for A)" "$ta local light report"
  check "(ii) A's status line stays mantle-light while W2 is current" poll 3 status_line_has A 48 "$L_MANTLE"
  check "(ii) ... and is not dark" status_line_lacks A 48 "$D_MANTLE"

  # (iii) an app's OSC 11 in A's current pane. Its #{pane_bg} shows the OSC
  # colour only while nothing paints the pane, so the screens agree for this
  # step: B's terminal turns light too (A and B mixed would paint W2, and the
  # paint's bg would hide the OSC colour, dark lead or light).
  screen_set_mode B light || no "(iii) B's terminal turns light"
  if poll 3 unpainted "$pane"; then ok "(iii) A and B agree: W2's pane carries no paint"; else no "(iii) W2's pane carries no paint" "$(paint_listing "$pane")"; fi
  screen_keys A -l "printf '\\033]11;rgb:1e1e/1e1e/2e2e\\033\\\\'"
  screen_keys A Enter
  ti set -wu -t "$w2" window-style
  if poll 3 pane_bg_is "$pane" '#1e1e2e'; then ok "(iii) the pane's OSC 11 landed (pane_bg #1e1e2e)"
  else no "(iii) the pane's OSC 11 landed" "pane_bg [$(ti display -p -t "$pane" '#{pane_bg}')]
$(ti capture-pane -p -t "$pane" | grep -v '^$')
screen A shows: $(screen_capture A | strip_sgr | grep -v '^ *$' | head -5)"; fi
  ts sync
  expect_eq "(iii) A is still light by its report" "$(status_line_for A)" "$ta local light report"
  ti set -w -t "$w2" window-style 'bg=#123456'

  # (iv) an Omarchy-like GLOBAL cursor-colour, and A and B mixed again: the
  # paint's pane-level cursor-colour (the lead's fg) wins while painted, and
  # unmixing leaves the global value exactly as it was, with no pane value.
  ti set -gw cursor-colour '#ff0000'
  screen_set_mode B dark || no "(iv) B's terminal turns dark again"
  settle 8 || no "(iv) settles"
  typed A
  if poll 3 window_painted_as "$w2" light; then ok "(iv) mixed again, A leads: W2's panes are painted light"
  else no "(iv) W2's panes are painted light" "$(show_window_paint "$w2")"; fi
  expect_eq "(iv) a painted pane's pane-level cursor-colour is the lead's fg" "$(ti show -pqv -t "$pane" cursor-colour)" '#3c3836'
  expect_eq "(iv) ... and it is the effective cursor colour over the global #ff0000" "$(ti display -p -t "$pane" '#{cursor-colour}')" '#3c3836'
  settle 8 || no "(iv) settles"
  screen_set_mode B light || no "(iv) B's terminal turns light"
  if poll 3 window_unpainted "$w2"; then ok "(iv) unmixed: no pane-level paint value remains on W2's panes"
  else no "(iv) unmixed: W2's panes are unpainted" "$(show_window_paint "$w2")"; fi
  expect_eq "(iv) show -gwv cursor-colour is still #ff0000" "$(ti show -gwv cursor-colour)" '#ff0000'
  expect_eq "(iv) no pane anywhere holds a pane-level cursor-colour" \
    "$(for p in $(ti list-panes -a -F '#{pane_id}'); do ti show -pqv -t "$p" cursor-colour; done | grep -c .)" 0
  settle 8 || no "(iv) settles"

  # (v) a screen that never reports, viewing W2 with its own bg.
  screen_add C silent
  tc=$(screen_tty C)
  ts sync
  def=$(ti show -gv @theme_default)
  expect_eq "(v) C resolves by the default, not W2's bg" "$(status_line_for C)" "$tc local $def default"
}

# T12 (D16, §6.7, §16 item 10): the screens file's own lock, under the shell
# named by the entry (T12@sh, T12@dash). A writer waits up to 5 s for it, a
# dead owner's lock is broken, a live one never is, and a write that cannot
# complete leaves the file exactly as it was.
case_T12() {
  local cfg="$C/hm/.config/tmux" scr lk pids rc n t0 t1 out dp want home2 r
  scr="$cfg/screens"; lk="$cfg/screens.lock"
  inner_start || fail_now "the inner server did not start"
  expect_eq "theme.sh runs under $CSUT" "$(head -n 1 "$C/hm/.tmux/scripts/theme.sh")" \
    "$(if [ "$CSUT" = dash ]; then printf '#!%s' "$DASH"; else printf '#!/bin/sh'; fi)"

  # (a) 20 concurrent writers of 20 distinct hosts.
  pids=''
  for n in $(seq -w 1 20); do ts screen "host$n" dark & pids="$pids $!"; done
  rc=0; for n in $pids; do wait "$n" || rc=$?; done
  expect_eq "(a) all 20 exit 0" "$rc" 0
  want=$(for n in $(seq -w 1 20); do printf 'host%s dark\n' "$n"; done)
  expect_eq "(a) the file holds exactly the 20 entries, sorted" "$(screens_body)" "$want"
  expect_eq "(a) ... plus the one-line header" "$(grep -c . "$scr")" 21
  check "(a) no screens.lock remains" test ! -e "$lk"
  expect_eq "(a) no .screens.tmp.* remains" "$(count_files "$cfg"/.screens.tmp.*)" 0

  # (b) a live owner (this harness): busy after about 5 s, nothing written.
  cp "$scr" "$C/before"
  mkdir "$lk"; : > "$lk/owner.$$"
  t0=$(now_ms); out=$(ts screen x dark 2>&1); rc=$?; t1=$(now_ms)
  expect_eq "(b) a live owner: exit 1" "$rc" 1
  case $out in *"is busy"*) ok "(b) ... with the busy message" ;; *) no "(b) ... with the busy message" "[$out]" ;; esac
  if [ $((t1 - t0)) -ge 4500 ] && [ $((t1 - t0)) -le 9000 ]; then ok "(b) ... after about 5 s ($((t1 - t0)) ms)"
  else no "(b) ... after about 5 s" "$((t1 - t0)) ms"; fi
  check "(b) the file is unchanged" cmp -s "$C/before" "$scr"
  expect_eq "(b) the live owner's lock is left alone" "$(ls -A "$lk" 2>&1)" "owner.$$"
  rm -rf "$lk"

  # (c) a dead owner: the lock is broken and the write goes through.
  dp=$(dead_pid)
  mkdir "$lk"; : > "$lk/owner.$dp"
  out=$(ts screen x dark 2>&1); rc=$?
  expect_eq "(c) a dead owner: exit 0 silently" "$rc:$out" "0:"
  check "(c) x dark is written" grep -qx 'x dark' "$scr"
  check "(c) the stale lock is gone" test ! -e "$lk"

  # (d) a fresh HOME with no ~/.config/tmux at all (and no server to sync).
  home2="$C/hm2"; mkdir "$home2"
  t0=$(now_ms)
  out=$("${CASE_ENV[@]}" "HOME=$home2" "TMUX=$C/nosuch,1,0" "$C/hm/.tmux/scripts/theme.sh" screen x dark 2>&1); rc=$?
  t1=$(now_ms)
  expect_eq "(d) a fresh HOME: exit 0 silently" "$rc:$out" "0:"
  if [ $((t1 - t0)) -lt 1000 ]; then ok "(d) ... at once ($((t1 - t0)) ms)"; else no "(d) ... at once" "$((t1 - t0)) ms"; fi
  check "(d) ~/.config/tmux was created" test -d "$home2/.config/tmux"
  check "(d) screens holds x dark" grep -qx 'x dark' "$home2/.config/tmux/screens"
  check "(d) no lock remains" test ! -e "$home2/.config/tmux/screens.lock"

  # (e) the temp write fails: a directory squats on the writer's temp name
  # (.screens.tmp.<its pid>) while this harness holds the lock, then lets go.
  cp "$scr" "$C/before"
  mkdir "$lk"; : > "$lk/owner.$$"
  ts_bg screen y dark 2>"$C/e.err"; r=$TS_PID
  mkdir "$cfg/.screens.tmp.$r"
  sleep 0.2
  rm -f "$lk/owner.$$"; rmdir "$lk"
  wait "$r"; rc=$?
  expect_eq "(e) a failed temp write: exit 1" "$rc" 1
  check "(e) ... says it could not write the file" grep -q 'cannot write' "$C/e.err"
  check "(e) ... the file is unchanged" cmp -s "$C/before" "$scr"
  check "(e) ... and the lock is released" test ! -e "$lk"
  rmdir "$cfg/.screens.tmp.$r"

  # (f) an unreadable file is never rewritten from nothing (its other hosts
  # would be lost): exit 1, untouched.
  chmod 000 "$scr"
  out=$(ts screen z dark 2>&1); rc=$?
  chmod 644 "$scr"
  expect_eq "(f) an unreadable screens file: exit 1" "$rc" 1
  check "(f) ... the file is unchanged" cmp -s "$C/before" "$scr"
  check "(f) ... and no lock remains" test ! -e "$lk"
  expect_eq "(f) no .screens.tmp.* remains" "$(count_files "$cfg"/.screens.tmp.*)" 0
}

# ---------------------------------------------------------------------------
# 8b. P5: paint and lead (architecture.md §5.8, §6.2, §6.3, §8.4.1-§8.4.4).
#     The paint records are the test's OWN copy of §5.8, so a wrong palette in
#     theme.sh cannot agree with itself. Pane options are read the way
#     theme.sh's call 3 reads them (§16 item 5): the `show -p -t %N` listing,
#     where a per-index unset leaves a bare `pane-colours` line that `show
#     -pqv` would hide.
# ---------------------------------------------------------------------------

P_LIGHT_WS='bg=#fbf1c7,fg=#3c3836'
P_DARK_WS='bg=#1e1e2e,fg=#cdd6f4'
P_LIGHT_CC='#3c3836'
P_DARK_CC='#cdd6f4'
P_LIGHT_PC='#fbf1c7 #cc241d #98971a #d79921 #458588 #b16286 #689d6a #7c6f64 #928374 #9d0006 #79740e #b57614 #076678 #8f3f71 #427b58 #3c3836'
P_DARK_PC='#45475a #f38ba8 #a6e3a1 #f9e2af #89b4fa #f5c2e7 #94e2d5 #bac2de #585b70 #f38ba8 #a6e3a1 #f9e2af #89b4fa #f5c2e7 #94e2d5 #a6adc8'
REC_LIGHT="light;$P_LIGHT_WS;$P_LIGHT_WS;$P_LIGHT_CC;$P_LIGHT_PC"
REC_DARK="dark;$P_DARK_WS;$P_DARK_WS;$P_DARK_CC;$P_DARK_PC"

# paint_listing PANE: the pane's own (pane-level) paint options, as `show -p`
# lists them, sorted: the marker, both styles, cursor-colour and pane-colours
# (each index, or a bare entry). Empty: nothing of the paint at pane level.
paint_listing() {
  ti show -p -t "$1" 2>/dev/null |
    grep -E '^(@theme_painted|window-style|window-active-style|cursor-colour|pane-colours)( |\[|$)' |
    LC_ALL=C sort
}

# paint_want light|dark: paint_listing of a pane painted in that mode.
paint_want() {
  local ws cc pc rec c i=0
  case $1 in
    light) ws=$P_LIGHT_WS cc=$P_LIGHT_CC pc=$P_LIGHT_PC rec=$REC_LIGHT ;;
    dark) ws=$P_DARK_WS cc=$P_DARK_CC pc=$P_DARK_PC rec=$REC_DARK ;;
  esac
  {
    printf '@theme_painted "%s"\n' "$rec"
    printf 'cursor-colour %s\n' "$cc"
    for c in $pc; do printf 'pane-colours[%d] %s\n' "$i" "$c"; i=$((i + 1)); done
    printf 'window-active-style "%s"\n' "$ws"
    printf 'window-style "%s"\n' "$ws"
  } | LC_ALL=C sort
}

# painted_as PANE MODE / unpainted PANE / listing_is PANE LINES…
painted_as() { [ "$(paint_listing "$1")" = "$(paint_want "$2")" ]; }
unpainted() { [ -z "$(paint_listing "$1")" ]; }
listing_is() { local p=$1; shift; [ "$(paint_listing "$p")" = "$(printf '%s\n' "$@" | LC_ALL=C sort)" ]; }

# win_panes W: the pane ids of window W.
win_panes() { ti list-panes -t "$1" -F '#{pane_id}'; }

# window_painted_as W MODE [EXCEPT…] / window_unpainted W [EXCEPT…]: every pane
# of W (but the EXCEPT panes) is painted in MODE / carries none of the paint.
window_painted_as() {
  local w=$1 m=$2 p
  shift 2
  for p in $(win_panes "$w"); do
    case " $* " in *" $p "*) continue ;; esac
    painted_as "$p" "$m" || return 1
  done
}
window_unpainted() {
  local w=$1 p
  shift
  for p in $(win_panes "$w"); do
    case " $* " in *" $p "*) continue ;; esac
    unpainted "$p" || return 1
  done
}

# show_window_paint W: every pane's listing, for a failure message.
show_window_paint() {
  local p
  for p in $(win_panes "$1"); do printf '%s:\n%s\n' "$p" "$(paint_listing "$p" | cut -c1-90)"; done
}

# lead_of W: the window's recorded lead (@theme_lead), or ''.
lead_of() { ti show -wqv -t "$1" @theme_lead 2>/dev/null; }
lead_is() { [ "$(lead_of "$1")" = "$2" ]; }

# both_on W: A and B are both clients of session s, both displaying W — the
# premise of every T3 step from (a) to (j) (§8.4.1, round 2 Issue 6).
both_on() {
  local got want
  got=$(ti list-clients -F '#{client_tty} #{session_name} #{window_id}' | LC_ALL=C sort)
  want=$(printf '%s s %s\n%s s %s\n' "$(screen_tty A)" "$1" "$(screen_tty B)" "$1" | LC_ALL=C sort)
  [ "$got" = "$want" ]
}
need_both_on() {
  if poll 3 both_on "$1"; then ok "both_on $1 ($2)"
  else fail_now "both_on $1 ($2): the step's premise is broken" "$(ti list-clients -F '#{client_tty} #{session_name} #{window_id}')"; fi
}

# app_n MODE: how many CSI ? 997 ; MODE n the 2031 app pane has received.
app_n() { { grep -oF "${ESC}[?997;$1n" "$C/app.log" 2>/dev/null || true; } | wc -l | tr -d ' '; }
app_above() { [ "$(app_n "$1")" -gt "$2" ]; }

# sess_env SESSION VAR: the session's own value of VAR ('' when unset).
sess_env() { ti show-environment -t "$1" "$2" 2>/dev/null | sed -n "s/^$2=//p"; }
sess_env_is() { [ "$(sess_env "$1" TERM_THEME):$(sess_env "$1" COLORFGBG)" = "$2" ]; }

# typed NAME: the screen types one key into whatever its pane runs (/bin/sh).
typed() { screen_keys "$1" x; }

# The four §4.5 lead hooks, byte for byte.
LEAD_HOOKS_WANT="set-hook -g 'client-active[100]'          { if -F '#{&&:#{E:@theme_hook_id},#{!=:#{@theme_lead},#{E:@theme_hook_id}}}' { set -wqF @theme_lead '#{E:@theme_hook_id}' } ; run-shell -b \"'#{@moox-root}/scripts/theme.sh' sync >/dev/null 2>&1 || true\" }
set-hook -g 'client-session-changed[100]' { if -F '#{&&:#{E:@theme_hook_id},#{!=:#{@theme_lead},#{E:@theme_hook_id}}}' { set -wqF @theme_lead '#{E:@theme_hook_id}' } ; run-shell -b \"'#{@moox-root}/scripts/theme.sh' sync >/dev/null 2>&1 || true\" }
set-hook -g 'after-select-window[100]'    { if -F '#{&&:#{E:@theme_cmd_id},#{!=:#{@theme_lead},#{E:@theme_cmd_id}}}' { set -wqF @theme_lead '#{E:@theme_cmd_id}' } ; run-shell -b \"'#{@moox-root}/scripts/theme.sh' sync >/dev/null 2>&1 || true\" }
set-hook -g 'after-new-window[100]'       { if -F '#{&&:#{E:@theme_cmd_id},#{!=:#{@theme_lead},#{E:@theme_cmd_id}}}' { set -wqF @theme_lead '#{E:@theme_cmd_id}' } ; run-shell -b \"'#{@moox-root}/scripts/theme.sh' sync >/dev/null 2>&1 || true\" }"

# T3 (AC3, FR3, D3, D4, D14, D15, D17; §8.4.1): paint, lead and ownership on
# real key presses. A and B are clients of one session, so a `C-b n` from
# either moves both, and every step (a)-(j) is about a window that the light
# and the dark screen view AT THE SAME TIME (both_on). (k) and (l) are the
# one-viewer transitions.
case_T3() {
  local w1 w2 w3 papp psh pa pb ida idb pida ta tb n1 n2 sa sb new
  inner_start || fail_now "the inner server did not start"
  w1=$(ti display -p -t s:0 '#{window_id}')
  psh=$(ti display -p -t s:0 '#{pane_id}')
  # %app is a 2031 app (mode 2031 on, raw input into app.log); %sh stays W1's
  # active pane (-d), so typed keys go to sh.
  ti split-window -d -t s:0 "/bin/sh -c 'printf \"\\033[?2031h\"; stty raw -echo; exec cat > $C/app.log'"
  papp=$(win_panes "$w1" | grep -vx "$psh")
  ti new-window -d -t s: /bin/sh
  w2=$(ti display -p -t s:1 '#{window_id}')
  pa=$(win_panes "$w2")
  ti set -w -t "$w2" window-style 'bg=#123456'
  ti new-session -d -s s2 -x 160 -y 40 /bin/sh
  w3=$(ti display -p -t s2:0 '#{window_id}')
  expect_eq "setup: W1 is s's current window, %sh active" "$(ti display -p -t s: '#{window_id} #{pane_id}')" "$w1 $psh"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  ida=$(screen_id A); idb=$(screen_id B); pida=${ida##*:}
  ta=$(screen_tty A); tb=$(screen_tty B)

  say "  -- (a) attach makes a lead"
  need_both_on "$w1" a
  expect_eq "(a) @theme_lead(W1) = B, from client-session-changed" "$(lead_of "$w1")" "$idb"
  if poll 3 window_painted_as "$w1" dark; then ok "(a) both W1 panes carry the dark record and the four dark values at pane level"
  else no "(a) both W1 panes carry the dark record and the four dark values at pane level" "$(show_window_paint "$w1")"; fi
  check "(a) ... the 2031 app pane among them" painted_as "$papp" dark
  expect_eq "(a) W1 has no window-level value for any of the four" \
    "$(for o in window-style window-active-style cursor-colour pane-colours; do ti show -wqv -t "$w1" "$o"; done)" ''
  settle 8 || no "(a) settles"

  say "  -- (b) B attached last, then types"
  typed B
  if hold 1 lead_is "$w1" "$idb"; then ok "(b) the lead is still B"; else no "(b) the lead is still B" "[$(lead_of "$w1")]"; fi
  check "(b) the panes are still dark" window_painted_as "$w1" dark
  settle 8 || no "(b) settles"

  say "  -- (c) A types"
  n2=$(app_n 2)
  typed A
  if poll 3 lead_is "$w1" "$ida"; then ok "(c) client-active: the lead becomes A"; else no "(c) client-active: the lead becomes A" "[$(lead_of "$w1")]"; fi
  if poll 3 window_painted_as "$w1" light; then ok "(c) W1's panes are repainted light, each marker exactly the light record"
  else no "(c) W1's panes are repainted light" "$(show_window_paint "$w1")"; fi
  if poll 3 app_above 2 "$n2"; then ok "(c) the 2031 app hears CSI ?997;2n"; else no "(c) the 2031 app hears CSI ?997;2n" "$(od -c "$C/app.log" | tail -3)"; fi
  if poll 3 sess_env_is s 'light:0;15'; then ok "(c) s's TERM_THEME/COLORFGBG follow the lead (light, 0;15)"
  else no "(c) s's env follows the lead" "[$(sess_env s TERM_THEME):$(sess_env s COLORFGBG)]"; fi
  settle 8 || no "(c) settles"

  say "  -- (d) B types"
  n1=$(app_n 1)
  typed B
  if poll 3 window_painted_as "$w1" dark; then ok "(d) the panes are dark again"; else no "(d) the panes are dark again" "$(show_window_paint "$w1")"; fi
  if poll 3 app_above 1 "$n1"; then ok "(d) the app hears CSI ?997;1n"; else no "(d) the app hears CSI ?997;1n" "$(od -c "$C/app.log" | tail -3)"; fi
  if poll 3 sess_env_is s 'dark:15;0'; then ok "(d) s's env follows the lead (dark, 15;0)"
  else no "(d) s's env follows the lead" "[$(sess_env s TERM_THEME):$(sess_env s COLORFGBG)]"; fi
  settle 8 || no "(d) settles"
  # How often client-active fires: on a change of the window's latest client,
  # not per keystroke (server_client_update_latest returns early otherwise).
  # B is W1's latest now, so a burst of typing from B must start no sync.
  n1=$(log_lines)
  for sa in 1 2 3 4 5 6 7 8; do typed B; done
  sleep 1
  expect_eq "(d) a burst of 8 keys from the screen that typed last starts no sync (no list-clients, list-panes or source-file)" \
    "$(since "$n1" | awk -F '\t' '$5 ~ /(^|,)(list-clients|list-panes|source-file)(,|$)/' | grep -c .)" 0
  settle 8 || no "(d) settles after the burst"

  say "  -- (e) window navigation, with both screens on W2"
  screen_keys A C-b n
  need_both_on "$w2" e1
  if poll 3 lead_is "$w2" "$ida"; then ok "(e1) @theme_lead(W2) = A, from after-select-window"; else no "(e1) @theme_lead(W2) = A" "[$(lead_of "$w2")]"; fi
  if poll 3 painted_as "$pa" light; then ok "(e1) %w2a is painted light at pane scope"; else no "(e1) %w2a is painted light at pane scope" "$(paint_listing "$pa")"; fi
  expect_eq "(e1) W2's window-level window-style is still exactly bg=#123456" "$(ti show -wv -t "$w2" window-style)" 'bg=#123456'
  if poll 3 window_unpainted "$w1"; then ok "(e1) W1 has no viewer: no marker and no pane-level value on its panes"
  else no "(e1) W1 has no viewer: its panes are unpainted" "$(show_window_paint "$w1")"; fi
  settle 8 || no "(e1) settles"
  sa=$(screen_status A); sb=$(screen_status B)
  if has_rgb "$sa" 48 "$L_MANTLE" && ! has_rgb "$sa" 48 "$D_MANTLE" && has_rgb "$sb" 48 "$D_MANTLE" && ! has_rgb "$sb" 48 "$L_MANTLE"; then
    ok "(e2) captured together on W2 (bg=#123456): A's bar is light, B's is dark"
  else no "(e2) captured together on W2: A's bar light, B's dark" "A: $(printf '%s' "$sa" | cat -v | cut -c1-120)
B: $(printf '%s' "$sb" | cat -v | cut -c1-120)"; fi
  screen_keys A C-b p
  need_both_on "$w1" e3
  check "(e3) lead(W1) = A" poll 3 lead_is "$w1" "$ida"
  if poll 3 window_painted_as "$w1" light; then ok "(e3) W1 is light"; else no "(e3) W1 is light" "$(show_window_paint "$w1")"; fi
  if poll 3 window_unpainted "$w2"; then ok "(e3) W2 is unpainted"; else no "(e3) W2 is unpainted" "$(show_window_paint "$w2")"; fi
  settle 8 || no "(e3) settles"
  screen_keys B C-b n
  need_both_on "$w2" e4
  if poll 3 lead_is "$w2" "$idb"; then ok "(e4) @theme_lead(W2) = B, from after-select-window"; else no "(e4) @theme_lead(W2) = B" "[$(lead_of "$w2")]"; fi
  if poll 3 painted_as "$pa" dark; then ok "(e4) %w2a is dark"; else no "(e4) %w2a is dark" "$(paint_listing "$pa")"; fi
  settle 8 || no "(e4) settles"
  typed B
  if hold 1 painted_as "$pa" dark; then ok "(e5) B types: still dark"; else no "(e5) B types: still dark" "$(paint_listing "$pa")"; fi
  settle 8 || no "(e5) settles"
  typed A
  if poll 3 painted_as "$pa" light; then ok "(e5) A types: light"; else no "(e5) A types: light" "$(paint_listing "$pa")"; fi
  settle 8 || no "(e5) settles"
  typed B
  if poll 3 painted_as "$pa" dark; then ok "(e5) B types: dark"; else no "(e5) B types: dark" "$(paint_listing "$pa")"; fi
  settle 8 || no "(e5) settles"

  say "  -- (f) split"
  screen_keys B C-b '"'
  need_both_on "$w2" f
  poll 3 test "$(win_panes "$w2" | grep -c .)" = 2 >/dev/null || true
  pb=$(win_panes "$w2" | grep -vx "$pa")
  if [ -n "$pb" ] && poll 3 painted_as "$pb" dark; then ok "(f) the new pane %w2b ($pb) is painted dark (after-split-window)"
  else no "(f) the new pane %w2b is painted dark" "panes: $(win_panes "$w2" | tr '\n' ' ')
$(show_window_paint "$w2")"; fi
  settle 8 || no "(f) settles"

  say "  -- (g) a foreign pane-level style (FR3.2's exception; D14)"
  ti select-pane -t "$pb" -P 'bg=#224466'
  typed A
  need_both_on "$w2" g
  if poll 3 painted_as "$pa" light; then ok "(g) %w2a follows the lead to light"; else no "(g) %w2a is light" "$(paint_listing "$pa")"; fi
  if poll 3 listing_is "$pb" 'window-active-style "bg=#224466"' 'window-style "bg=#224466"'; then
    ok "(g) %w2b stepped aside: only its own bg=#224466 styles remain at pane level (no marker, cursor-colour or pane-colours entry)"
  else no "(g) %w2b stepped aside" "$(paint_listing "$pb")"; fi
  settle 8 || no "(g) settles"
  typed B
  if poll 3 painted_as "$pa" dark; then ok "(g) a further B type: %w2a dark"; else no "(g) B type: %w2a dark" "$(paint_listing "$pa")"; fi
  settle 8 || no "(g) settles"
  typed A
  if poll 3 painted_as "$pa" light; then ok "(g) a further A type: %w2a light"; else no "(g) A type: %w2a light" "$(paint_listing "$pa")"; fi
  check "(g) %w2b is still FOREIGN (unmarked, its own styles only)" listing_is "$pb" 'window-active-style "bg=#224466"' 'window-style "bg=#224466"'
  settle 8 || no "(g) settles"

  say "  -- (h) repair"
  need_both_on "$w2" h
  check "(h) lead A, light" painted_as "$pa" light
  ti set -pu -t "$pa" pane-colours
  ts sync
  if poll 3 painted_as "$pa" light; then ok "(h1) a missing pane-colours is written back, the marker unchanged"
  else no "(h1) a missing pane-colours is written back" "$(paint_listing "$pa")"; fi
  ti set -p -t "$pa" @theme_painted "$REC_LIGHT+$REC_DARK"
  ti set -p -t "$pa" window-style "$P_DARK_WS"
  ts sync
  if poll 3 painted_as "$pa" light; then ok "(h2) an interrupted mode change (light+dark marker, a dark style) is finished: all four light, marker exactly light"
  else no "(h2) an interrupted mode change is finished" "$(paint_listing "$pa")"; fi
  settle 8 || no "(h) settles"

  say "  -- (i) environment"
  typed B
  poll 3 painted_as "$pa" dark || no "(i) B types: W2 dark"
  settle 8 || no "(i) settles"
  ti set-environment -t s COLORFGBG '0;15'
  ts sync
  if poll 3 sess_env_is s 'dark:15;0'; then ok "(i) a foreign COLORFGBG 0;15 is put back to 15;0, TERM_THEME dark"
  else no "(i) the session env is reconciled" "[$(sess_env s TERM_THEME):$(sess_env s COLORFGBG)]"; fi
  settle 8 || no "(i) settles"

  say "  -- (j) lead identity"
  need_both_on "$w2" j
  typed A
  poll 3 painted_as "$pa" light >/dev/null
  settle 8 || no "(j) settles"
  sleep 1.1
  typed B
  poll 3 painted_as "$pa" dark >/dev/null
  settle 8 || no "(j) settles"
  ti set -w -t "$w2" @theme_lead "$ta:1"
  ts sync
  if hold 1 painted_as "$pa" dark; then ok "(j1) a record with the right name and a wrong pid is ignored: the fallback picks B (dark)"
  else no "(j1) the wrong-pid record is ignored" "$(paint_listing "$pa")"; fi
  ti set -w -t "$w2" @theme_lead "$ta:$pida"
  ts sync
  if poll 3 painted_as "$pa" light; then ok "(j2) A's exact NAME:PID: W2 turns light"; else no "(j2) A's exact NAME:PID: W2 turns light" "$(paint_listing "$pa")"; fi
  settle 8 || no "(j) settles"

  say "  -- (k) one-viewer transitions"
  screen_keys A C-b p
  need_both_on "$w1" k
  check "(k) lead(W1) = A" poll 3 lead_is "$w1" "$ida"
  settle 8 || no "(k) settles"
  n1=$(app_n 1)
  typed B
  if poll 3 window_painted_as "$w1" dark; then ok "(k) B types: W1 dark"; else no "(k) B types: W1 dark" "$(show_window_paint "$w1")"; fi
  if poll 3 app_above 1 "$n1"; then ok "(k) the app hears CSI ?997;1n"; else no "(k) the app hears ?997;1n" "$(od -c "$C/app.log" | tail -3)"; fi
  settle 8 || no "(k) settles"
  n2=$(app_n 2)
  screen_keys B C-b ')'
  if poll 3 test "$(ti list-clients -F '#{client_tty} #{session_name} #{window_id}' | LC_ALL=C sort)" = \
      "$(printf '%s s %s\n%s s2 %s\n' "$ta" "$w1" "$tb" "$w3" | LC_ALL=C sort)"; then
    ok "(k1) A on s/W1, B on s2/W3: one viewer each"
  else fail_now "(k1) A on s/W1, B on s2/W3" "$(ti list-clients -F '#{client_tty} #{session_name} #{window_id}')"; fi
  if poll 3 window_unpainted "$w1" && window_unpainted "$w2" "$pb" && window_unpainted "$w3"; then
    ok "(k1) no pane of W1, W2 or W3 carries a marker or a pane-level paint value"
  else no "(k1) W1, W2, W3 unpainted" "$(show_window_paint "$w1")
$(show_window_paint "$w2")"; fi
  check "(k1) ... but %w2b keeps exactly its own bg=#224466" listing_is "$pb" 'window-active-style "bg=#224466"' 'window-style "bg=#224466"'
  if poll 3 app_above 2 "$n2"; then ok "(k1) the app hears CSI ?997;2n: tmux answers from A's terminal now"; else no "(k1) the app hears ?997;2n" "$(od -c "$C/app.log" | tail -3)"; fi
  check "(k1) @theme_lead(W3) = B, from client-session-changed" poll 3 lead_is "$w3" "$idb"
  if poll 3 sess_env_is s 'light:0;15' && sess_env_is s2 'dark:15;0'; then ok "(k1) s's env is light and s2's is dark"
  else no "(k1) s light, s2 dark" "s [$(sess_env s TERM_THEME):$(sess_env s COLORFGBG)] s2 [$(sess_env s2 TERM_THEME):$(sess_env s2 COLORFGBG)]"; fi
  settle 8 || no "(k1) settles"
  sa=$(screen_status A); sb=$(screen_status B)
  if has_rgb "$sa" 48 "$L_MANTLE" && has_rgb "$sb" 48 "$D_MANTLE"; then ok "(k1) A's bar is light, B's is dark"
  else no "(k1) A's bar light, B's dark" "A: $(printf '%s' "$sa" | cat -v | cut -c1-100)"; fi
  n1=$(app_n 1)
  screen_keys B C-b '('
  need_both_on "$w1" k2
  check "(k2) @theme_lead(W1) = B, from client-session-changed" poll 3 lead_is "$w1" "$idb"
  if poll 3 window_painted_as "$w1" dark; then ok "(k2) W1 is painted dark"; else no "(k2) W1 is painted dark" "$(show_window_paint "$w1")"; fi
  if poll 3 app_above 1 "$n1"; then ok "(k2) the app hears CSI ?997;1n"; else no "(k2) the app hears ?997;1n" "$(od -c "$C/app.log" | tail -3)"; fi
  settle 8 || no "(k2) settles"

  say "  -- (l) unmix"
  n2=$(app_n 2)
  screen_detach B
  if poll 3 window_unpainted "$w1" && window_unpainted "$w2" "$pb"; then ok "(l) no pane of W1 or W2 carries a marker or a pane-level paint value"
  else no "(l) W1 and W2 unpainted" "$(show_window_paint "$w1")
$(show_window_paint "$w2")"; fi
  check "(l) %w2b keeps exactly bg=#224466" listing_is "$pb" 'window-active-style "bg=#224466"' 'window-style "bg=#224466"'
  expect_eq "(l) W2's window-level window-style is exactly bg=#123456" "$(ti show -wv -t "$w2" window-style)" 'bg=#123456'
  if poll 3 app_above 2 "$n2"; then ok "(l) the app hears CSI ?997;2n (the unpaint direction)"; else no "(l) the app hears ?997;2n" "$(od -c "$C/app.log" | tail -3)"; fi
  if poll 3 sess_env_is s 'light:0;15'; then ok "(l) the session env is light (light, 0;15)"; else no "(l) the session env is light" "[$(sess_env s TERM_THEME):$(sess_env s COLORFGBG)]"; fi

  say "  -- (m) stability"
  settle 8 || no "(m) settles"
  n1=$(log_lines)
  sleep 3
  new=$(since "$n1" | awk -F '\t' '$5 ~ /(^|,)(list-clients|list-panes|source-file)(,|$)/' | grep -c .)
  expect_eq "(m) no input for 3 s: no sync-shaped call (list-clients, list-panes, source-file)" "$new" 0
  expect_eq "no REFUSED call" "$(log_refused "$C")" 0
}

# T3a (the scripted-switch limitation, .tmux.conf's @theme_cmd_id note): a
# window a SCRIPT switches to is credited to the most recently active screen,
# while tmux leaves the window's `latest` where it was; typing on the screen
# that IS latest there then fires no client-active, so the paint does not
# follow it until the credited screen has typed there. This pins today's
# behaviour, so a tmux that exposes `latest` to hooks can close it on purpose.
case_T3a() {
  local w1 w2 ida idb
  inner_start || fail_now "the inner server did not start"
  w1=$(ti display -p -t s:0 '#{window_id}')
  ti new-window -d -t s: /bin/sh
  w2=$(ti display -p -t s:1 '#{window_id}')
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  ida=$(screen_id A); idb=$(screen_id B)
  need_both_on "$w1" setup

  # B makes itself W2's latest (its own C-b n), and comes back to W1.
  screen_keys B C-b n
  need_both_on "$w2" "B pressed C-b n"
  if poll 3 lead_is "$w2" "$idb"; then ok "setup: B's C-b n records B as W2's lead"; else no "setup: B leads W2" "[$(lead_of "$w2")]"; fi
  settle 8 || no "setup settles"
  screen_keys B C-b p
  need_both_on "$w1" "B pressed C-b p"
  settle 8 || no "setup settles"
  # A types on W1: A is now the most recently active screen.
  typed A
  if poll 3 lead_is "$w1" "$ida"; then ok "setup: A types on W1 and leads it"; else no "setup: A leads W1" "[$(lead_of "$w1")]"; fi
  settle 8 || no "setup settles"

  say "  -- a script runs select-window (a client with no session)"
  ti select-window -t "$w2"
  need_both_on "$w2" scripted
  if poll 3 lead_is "$w2" "$ida"; then ok "the scripted switch credits W2 to A, the most recently active screen (tmux's best guess)"
  else no "the scripted switch credits W2 to A" "[$(lead_of "$w2")]"; fi
  if poll 3 window_painted_as "$w2" light; then ok "... so W2 is painted light"; else no "... so W2 is painted light" "$(show_window_paint "$w2")"; fi
  settle 8 || no "settles"

  say "  -- B, still W2's latest in tmux's eyes, types"
  typed B
  if hold 2 window_painted_as "$w2" light && lead_is "$w2" "$ida"; then
    ok "(the limitation) B types on W2: no client-active fires, W2 stays light, the lead stays A"
  else no "(the limitation) B types on W2: W2 stays light" "lead [$(lead_of "$w2")] $(show_window_paint "$w2")"; fi
  settle 8 || no "settles"

  say "  -- recovery: A types on W2, then B"
  typed A
  settle 8 || no "settles"
  typed B
  if poll 3 lead_is "$w2" "$idb" && poll 3 window_painted_as "$w2" dark; then
    ok "once A has typed on W2, B's typing moves the lead: W2 is dark"
  else no "B's typing moves the lead after A typed" "lead [$(lead_of "$w2")] $(show_window_paint "$w2")"; fi
  expect_eq "no REFUSED call" "$(log_refused "$C")" 0
}

# T7c (NFR6 as clarified): two LOCAL screens of different modes on one session
# are mixed, and FR3 paints the current window.
case_T7c() {
  local w
  inner_start || fail_now "the inner server did not start"
  ti split-window -t s: /bin/sh
  w=$(ti display -p -t s: '#{window_id}')
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  expect_eq "both screens are local" "$(ts status | awk '{ print $2 }' | sort -u)" local
  if poll 3 window_painted_as "$w" dark; then ok "the current window's panes carry @theme_painted (B, attached last, leads: dark)"
  else no "the current window's panes are painted" "$(show_window_paint "$w")"; fi
  expect_eq "exactly its two panes are painted" "$(painted_panes)" 2
}

# T13 (D14, §6.3 call 3; §8.4.3): a pane that dies between call 2 and call 3
# truncates call 3 at its own header. The panes after it then have no answer,
# so they are left alone for that pass, the pass is incomplete, and the runner
# retries it with no further request.
case_T13() {
  local w p1 p2 p3 hook="$C/hook.sh" r1 outs first second
  inner_start || fail_now "the inner server did not start"
  ti split-window -t s: /bin/sh
  ti split-window -t s: /bin/sh
  ti select-layout -t s: even-vertical >/dev/null
  w=$(ti display -p -t s: '#{window_id}')
  read -r p1 p2 p3 <<<"$(win_panes "$w" | tr -d % | sort -n | sed 's/^/%/' | tr '\n' ' ')"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  if poll 3 window_painted_as "$w" dark; then ok "setup: B attached last, W is painted dark"; else no "setup: W is painted dark" "$(show_window_paint "$w")"; fi
  ti select-pane -t "$p3" -P 'bg=#224466'
  typed A
  if poll 3 listing_is "$p3" 'window-active-style "bg=#224466"' 'window-style "bg=#224466"'; then ok "setup: $p3 stepped aside and is FOREIGN"
  else no "setup: $p3 stepped aside" "$(paint_listing "$p3")"; fi
  poll 3 painted_as "$p1" light || no "setup: $p1 is light (A leads)"
  settle 8 || no "setup settles"
  cat > "$hook" <<EOF
#!/bin/sh
printf '%s %s\n' "\$1" "\$PPID" >> '$C/hook.log'
exit 0
EOF
  chmod 755 "$hook"
  TS_EXTRA_ENV=("THEME_SYNC_TESTHOOK=$hook")

  # 1. p2 SUSPECT, so it precedes FOREIGN p3 in call 3.
  ti set -pu -t "$p2" pane-colours
  # 2. Hold and record only call 3: its argv is the only one with `::Q %%`.
  printf '::Q %%%%\n' > "$C/pause.match"
  cp "$C/pause.match" "$C/record.match"
  expect_eq "pause.match holds ::Q %%" "$(cat "$C/pause.match")" '::Q %%'
  # 3. One sync; kill p2 while its call 3 is held.
  : > "$C/hook.log"
  ts_bg sync; r1=$TS_PID
  if poll 5 grep -q ' PAUSED .*::Q %%' "$C/tmux.log"; then ok "R1's call 3 is held"; else no "R1's call 3 is held"; fi
  ti kill-pane -t "$p2"
  rm -f "$C/pause.match"
  # 4. R1 releases, then stop recording.
  wait "$r1"
  rm -f "$C/record.match"

  # out.<epoch-ms>.<pid>: the names sort by time.
  outs=$(for first in "$C"/out.*; do [ -e "$first" ] && printf '%s\n' "$first"; done | LC_ALL=C sort)
  first=$(printf '%s\n' "$outs" | sed -n 1p)
  second=$(printf '%s\n' "$outs" | sed -n 2p)
  expect_eq "two call-3 outputs were recorded" "$(printf '%s\n' "$outs" | grep -c .)" 2
  expect_eq "the first call 3 ends at the dead pane's header ::Q $p2" "$(tail -n 1 "$first" 2>/dev/null)" "::Q $p2"
  check "... with no ::Q $p3" not grep -qx "::Q $p3" "$first"
  check "... and no ::END" not grep -qx '::END' "$first"
  expect_eq "... and its logged rc is non-zero" \
    "$(awk -F '\t' '$6 == "record" && $7 ~ /::Q %%/ { print $4; exit }' "$C/calls.tsv" | sed 's/^0$/zero/')" 1
  check "$p3 is left alone: exactly its own bg=#224466 styles, no marker, cursor-colour or pane-colours" \
    listing_is "$p3" 'window-active-style "bg=#224466"' 'window-style "bg=#224466"'
  check "$p1 is still painted, its marker unchanged (light)" painted_as "$p1" light
  expect_eq "R1 ran a second pass with no further request: one acquired, two passes, all R1's" \
    "$(grep -c '^acquired ' "$C/hook.log"):$(grep -c '^pass ' "$C/hook.log"):$(awk '{ print $2 }' "$C/hook.log" | sort -u | tr '\n' ' ')" "1:2:$r1 "
  expect_eq "the second recorded call 3 ends in ::END" "$(tail -n 1 "$second" 2>/dev/null)" '::END'
  check "no lock and no dirty remain" sync_idle
  TS_EXTRA_ENV=()
}

# T14 (D14 and FR3.2's residual; per-index pane-colours; §8.4.4): value
# ownership at the edges. Every step changes one pane with ti, then syncs.
case_T14() {
  local w c1 c2 c3 c4 c5 c6 c7 i
  inner_start || fail_now "the inner server did not start"
  ti split-window -t s: /bin/sh
  ti split-window -t s: /bin/sh
  ti split-window -t s: /bin/sh
  ti select-layout -t s: tiled >/dev/null
  # Three more for (e)-(g), re-tiled after each split so none runs out of room.
  for i in 1 2 3; do ti split-window -t s: /bin/sh; ti select-layout -t s: tiled >/dev/null; done
  w=$(ti display -p -t s: '#{window_id}')
  read -r c1 c2 c3 c4 c5 c6 c7 <<<"$(win_panes "$w" | tr -d % | sort -n | sed 's/^/%/' | tr '\n' ' ')"
  [ -n "$c7" ] || fail_now "setup: the window has 7 panes" "$(win_panes "$w" | tr '\n' ' ')"
  outer_start || fail_now "the outer server did not start"
  screen_add A light
  screen_add B dark
  hub_ready A=light B=dark
  typed A
  if poll 3 window_painted_as "$w" light; then ok "setup: A typed last, every pane holds the light record"; else no "setup: every pane light" "$(show_window_paint "$w")"; fi
  settle 8 || no "setup settles"

  say "  -- (a) byte-identical to the record: the accepted residual (FR3.2, R20)"
  check "(a) before: $c1 holds the light record" painted_as "$c1" light
  ti set -p -t "$c1" window-style "$P_LIGHT_WS"
  ts sync
  check "(a) $c1 is still INTACT (light record and values)" painted_as "$c1" light

  say "  -- (b) the other palette's string is foreign"
  check "(b) before: $c2 holds the light record" painted_as "$c2" light
  ti set -p -t "$c2" window-style "$P_DARK_WS"
  ts sync
  if poll 3 listing_is "$c2" "window-style \"$P_DARK_WS\""; then ok "(b) $c2 stepped aside: only its window-style $P_DARK_WS remains at pane level"
  else no "(b) $c2 stepped aside" "$(paint_listing "$c2")"; fi

  say "  -- (c) one foreign pane-colours index"
  check "(c) before: $c3 holds the light record" painted_as "$c3" light
  ti set -p -t "$c3" 'pane-colours[3]' '#123456'
  ts sync
  if poll 3 listing_is "$c3" 'pane-colours[3] #123456'; then ok "(c) $c3's pane level holds only pane-colours[3] #123456 (our 15 indices, styles, cursor and marker unset)"
  else no "(c) $c3 holds only pane-colours[3]" "$(paint_listing "$c3")"; fi

  say "  -- (d) an index of 16 or above is never ours"
  check "(d) before: $c4 holds the light record" painted_as "$c4" light
  ti set -p -t "$c4" 'pane-colours[16]' '#654321'
  ts sync
  if poll 3 listing_is "$c4" 'pane-colours[16] #654321'; then ok "(d) $c4 stepped aside: only pane-colours[16] #654321 remains"
  else no "(d) $c4 stepped aside" "$(paint_listing "$c4")"; fi
  settle 8 || no "(d) settles"
  typed B
  poll 3 painted_as "$c1" dark || no "(d) B types: the lead flips to dark ($c1)"
  settle 8 || no "(d) settles"
  typed A
  poll 3 painted_as "$c1" light || no "(d) A types: the lead flips back to light ($c1)"
  settle 8 || no "(d) settles"
  check "(d) after two lead flips $c4 is still unpainted and index 16 survives" listing_is "$c4" 'pane-colours[16] #654321'
  check "(d) ... $c2 is unchanged" listing_is "$c2" "window-style \"$P_DARK_WS\""
  check "(d) ... and so is $c3" listing_is "$c3" 'pane-colours[3] #123456'

  # An EMPTY pane-level pane-colours (every index unset one by one) hides the
  # window's and the global pane-colours from the pane: someone's value.
  say "  -- (e) an unmarked pane with an empty pane-colours array is never painted"
  check "(e) before: $c5 holds the light record" painted_as "$c5" light
  ti set -pu -t "$c5" @theme_painted \; set -pu -t "$c5" window-style \; set -pu -t "$c5" window-active-style \; \
    set -pu -t "$c5" cursor-colour \; set -pu -t "$c5" pane-colours \; \
    set -p -t "$c5" 'pane-colours[0]' '#111111' \; set -pu -t "$c5" 'pane-colours[0]'
  check "(e) setup: $c5's pane level holds only an empty pane-colours, no marker" listing_is "$c5" 'pane-colours'
  ts sync
  if hold 1 listing_is "$c5" 'pane-colours'; then ok "(e) the window is mixed and $c5 is unmarked, yet it stays unpainted: its empty array is FOREIGN"
  else no "(e) $c5 stays unpainted" "$(paint_listing "$c5")"; fi

  say "  -- (f) a painted pane whose array someone emptied steps aside"
  check "(f) before: $c6 holds the light record" painted_as "$c6" light
  set --
  for i in $(seq 0 15); do set -- "$@" set -pu -t "$c6" "pane-colours[$i]" ';'; done
  ti "$@" display -p ''
  check "(f) setup: $c6 holds the light marker, styles and cursor, and an EMPTY array" \
    listing_is "$c6" "@theme_painted \"$REC_LIGHT\"" "cursor-colour $P_LIGHT_CC" 'pane-colours' \
    "window-active-style \"$P_LIGHT_WS\"" "window-style \"$P_LIGHT_WS\""
  ts sync
  if poll 3 listing_is "$c6" 'pane-colours'; then ok "(f) $c6 stepped aside: our styles, cursor and marker are gone, the empty array stays"
  else no "(f) $c6 stepped aside" "$(paint_listing "$c6")"; fi

  # A paint made before an edit of term_palette_light: the record names the
  # palette of its day, and the pane must still be recognised as ours.
  say "  -- (g) a record of an OLDER palette is still ours"
  local old_ws old_pc old_rec old_dws old_drec
  old_ws='bg=#fbf1c8,fg=#3c3836'
  old_pc=${P_LIGHT_PC/\#cc241d/\#cc0000}
  old_rec="light;$old_ws;$old_ws;$P_LIGHT_CC;$old_pc"
  check "(g) before: $c7 holds the light record" painted_as "$c7" light
  ti set -p -t "$c7" @theme_painted "$old_rec" \; set -p -t "$c7" window-style "$old_ws" \; \
    set -p -t "$c7" window-active-style "$old_ws" \; set -p -t "$c7" pane-colours "$old_pc"
  check "(g) setup: $c7 holds a light record whose bg and slot 1 differ from today's, and those values" \
    test "$(ti show -pqv -t "$c7" 'pane-colours[1]') $(ti show -pqv -t "$c7" window-style)" = "#cc0000 $old_ws"
  ts sync
  if poll 3 painted_as "$c7" light; then ok "(g) $c7 is repainted in TODAY's light palette, its marker exactly today's record, nothing of the old one left"
  else no "(g) $c7 is repainted in today's light palette" "$(paint_listing "$c7")"; fi
  # Just before the unmix: an older DARK record and its values, which the
  # unmix's pass must take off whole.
  old_dws='bg=#1e1e2f,fg=#cdd6f4'
  old_drec="dark;$old_dws;$old_dws;$P_DARK_CC;$P_DARK_PC"
  ti set -p -t "$c7" @theme_painted "$old_drec" \; set -p -t "$c7" window-style "$old_dws" \; \
    set -p -t "$c7" window-active-style "$old_dws" \; set -p -t "$c7" cursor-colour "$P_DARK_CC" \; \
    set -p -t "$c7" pane-colours "$P_DARK_PC"

  say "  -- unmix: B detaches"
  screen_detach B
  if poll 3 unpainted "$c1"; then ok "(a) after the unmix $c1 has no pane-level window-style: the byte-identical write was unset with ours (the documented residual)"
  else no "(a) after the unmix $c1 is unpainted" "$(paint_listing "$c1")"; fi
  check "(b) after the unmix $c2's window-style is unchanged" listing_is "$c2" "window-style \"$P_DARK_WS\""
  check "(c) after the unmix $c3 is unchanged" listing_is "$c3" 'pane-colours[3] #123456'
  check "(d) after the unmix $c4 is unchanged" listing_is "$c4" 'pane-colours[16] #654321'
  check "(e) after the unmix $c5 still holds only its empty array" listing_is "$c5" 'pane-colours'
  check "(f) ... and so does $c6" listing_is "$c6" 'pane-colours'
  if poll 3 unpainted "$c7"; then ok "(g) after the unmix $c7, painted in an older dark palette, is unpainted whole"
  else no "(g) after the unmix $c7 is unpainted" "$(paint_listing "$c7")"; fi
}

# ---------------------------------------------------------------------------
# 9. runner
# ---------------------------------------------------------------------------

# Every case, in order. T10 and T12 run under both shells whatever SUT says;
# the rest run under SUT.
CASES=(H0 T0 T1 T2 T3 T3a T4 T5 T6 T7 T7a T7b T7c T8 T9 T10@sh T10@dash T11 T12@sh T12@dash T13 T14)

known_case() {
  local e
  for e in "${CASES[@]}"; do [ "$e" = "$1" ] && return 0; done
  return 1
}

# case_teardown DIR: kill both private servers, then fail the case if its
# wrapper refused anything, then remove DIR.
case_teardown() {
  local dir=$1 rc=0 n
  kill_private "$dir/i"
  kill_private "$dir/o"
  n=$(log_refused "$dir")
  if [ -n "$n" ] && [ "$n" != 0 ]; then
    printf '  FAIL %s REFUSED call(s) in %s/tmux.log\n' "$n" "$dir"
    grep ' REFUSED ' "$dir/tmux.log" | head -n 20 | sed 's/^/       /'
    rc=1
  fi
  [ -n "${THEME_TEST_KEEP:-}" ] || rm -rf "$dir"
  return "$rc"
}

main() {
  local entries e rc passed='' failed='' aborted='' launch_after
  if [ "$#" -gt 0 ]; then entries=("$@"); else entries=("${CASES[@]}"); fi
  for e in "${entries[@]}"; do
    known_case "$e" || die2 "unknown case '$e' (known: ${CASES[*]})"
  done
  say "theme-per-screen: private servers under $D (tmux $("$REAL_TMUX" -V | cut -d' ' -f2), SUT default ${SUT:-sh})"
  for e in "${entries[@]}"; do
    say "=== $e ==="
    (
      case_setup "$e"
      "case_${e%@*}"
      [ "$CASE_FAILS" = 0 ]
    )
    rc=$?
    case_teardown "$(case_dir "$e")" || { [ "$rc" = 0 ] && rc=1; }
    case $rc in
      0) passed="$passed $e" ;;
      2) aborted=$e; break ;;
      *) failed="$failed $e" ;;
    esac
  done

  say "=== run ==="
  launch_after=$(launch_snapshot)
  if [ "$LAUNCH_BEFORE" = "$launch_after" ]; then
    ok "the launch dir $LAUNCH_DIR is unchanged"
  else
    failed="$failed launch-dir"
    no "the launch dir $LAUNCH_DIR is unchanged" \
      "$(diff <(printf '%s\n' "$LAUNCH_BEFORE") <(printf '%s\n' "$launch_after"))"
  fi
  say ""
  say "passed:${passed:- none}"
  say "failed:${failed:- none}"
  if [ -n "$aborted" ]; then
    say "ABORTED in $aborted (exit 2): the harness could not run it; see above"
    exit 2
  fi
  [ -z "$failed" ]
}

main "$@"
