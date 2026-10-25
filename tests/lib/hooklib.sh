#!/usr/bin/env bash
# hooklib.sh — shared harness for the continuity hook tests (tests/hook-*.test.sh).
#
# The hooks only COMPARE $TMUX with $MOOX_SOCKET (and with the literal default
# socket path); they never contact tmux. So these tests run an interactive zsh
# or bash with a scratch HOME/ZDOTDIR, a FAKE $TMUX string and a clean
# environment (`env -i`), feeding command lines on stdin. No tmux server is
# started, the user's rc files are never read, and every path written is under
# a fresh temp dir. The legacy branch's literal `/private/tmp/tmux-$UID/default`
# is only ever a string here.
#
# Usage (sourced):
#   . "$(dirname "$0")/lib/hooklib.sh"
#   hk_setup                  # T (temp root), H (scratch HOME), SHARE (share root)
#   hk_zsh  VAR=val ... <<'EOF'   # stdin = the lines typed
#   hk_bash VAR=val ... <<'EOF'
#   ok "name" / no "name" "detail" / hk_eq name want got / hk_finish

# Variables set here are read by the sourcing test.
# shellcheck disable=SC2034
set -u

HK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SHARE="$(cd "$HK_DIR/../../share/moox" && pwd)"
PASS=0
FAIL=0

ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
no() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n       %s\n' "$1" "${2:-}"; }
hk_eq() { # name want got
  if [ "$2" = "$3" ]; then ok "$1"; else no "$1" "want <$2> got <$3>"; fi
}
hk_ne() { # name unwanted got
  if [ "$2" != "$3" ]; then ok "$1"; else no "$1" "got the unwanted <$3>"; fi
}
hk_gone() { # name path  (the file must not exist at all: R2-C6, nothing to replay)
  if [ ! -e "$2" ]; then ok "$1"; else no "$1" "$2 exists, holding <$(cat "$2")>"; fi
}
hk_absent() { # name path  (absent or empty both count as "no recording", B15)
  if [ ! -s "$2" ]; then ok "$1"; else no "$1" "$2 holds <$(cat "$2")>"; fi
}

hk_setup() {
  T="$(mktemp -d "${TMPDIR:-/tmp}/moox-hooktest.XXXXXX")" || exit 1
  case "$T" in "$HOME"|"$HOME/"*) echo "ABORT: temp root under HOME"; exit 1 ;; esac
  H="$T/home"
  mkdir -p "$H"
  trap 'rm -rf "$T"' EXIT INT TERM HUP
  HK_UID="$(id -u)"
  # The legacy default socket, as a STRING only. Never contacted.
  HK_LEGACY_SOCK="/private/tmp/tmux-$HK_UID/default"
  [ -d /private/tmp ] || HK_LEGACY_SOCK="/tmp/tmux-$HK_UID/default"
}

# A per-key moox state layout under $T, as the gate would export it.
#   hk_moox_env <name>  -> sets M_SOCK M_STATE M_LAUNCH M_PENDING M_PANES; marker created
hk_moox_layout() {
  M_SOCK="$T/sock-$1"
  M_STATE="$T/state/$1"
  M_LAUNCH="$M_STATE/launch"
  M_PENDING="$M_STATE/pending"
  M_PANES="$M_STATE/panes"
  mkdir -p "$M_LAUNCH" "$M_PENDING" "$M_PANES"
  : > "$M_STATE/active"
}

# The env a pane of server <sock> gets from its gate (C1). Prints VAR=val words.
hk_gate_env() { # sock state launch pending panes
  printf '%s\n' "MOOX_SOCKET=$1" "MOOX_ACTIVE=1" "MOOX_STATE_KEY=k" "MOOX_STATE_DIR=$2" \
    "MOOX_LAUNCH_DIR=$3" "MOOX_PENDING_DIR=$4" "MOOX_PANES_DIR=$5"
}

HK_PATH="/usr/bin:/bin:/usr/sbin:/sbin"

# hk_zsh [VAR=val ...]  — an interactive zsh reading stdin; rc = $H/.zshrc,
# which sources share/moox/shell/moox.zsh unless HK_ZSHRC is set.
hk_zsh() {
  command -v zsh >/dev/null 2>&1 || { echo "SKIP: zsh not installed"; return 0; }
  if [ -n "${HK_ZSHRC:-}" ]; then
    printf '%s\n' "$HK_ZSHRC" > "$H/.zshrc"
  else
    printf 'PS1=""\nsource %q\n' "$SHARE/shell/moox.zsh" > "$H/.zshrc"
  fi
  env -i HOME="$H" ZDOTDIR="$H" PATH="$HK_PATH" TERM=dumb SHELL=/bin/zsh "$@" zsh -i 2>"$T/zsh.err"
}

# hk_bash [VAR=val ...]  — an interactive bash reading stdin; rc = $H/.bashrc,
# which sources share/moox/shell/moox.bash unless HK_BASHRC is set.
HK_BASH="${HK_BASH:-/bin/bash}"
hk_bash() {
  if [ -n "${HK_BASHRC:-}" ]; then
    printf '%s\n' "$HK_BASHRC" > "$H/.bashrc"
  else
    printf 'PS1=""\n. %q\n' "$SHARE/shell/moox.bash" > "$H/.bashrc"
  fi
  env -i HOME="$H" PATH="$HK_PATH" TERM=dumb SHELL="$HK_BASH" "$@" \
    "$HK_BASH" --noprofile --rcfile "$H/.bashrc" -i 2>"$T/bash.err"
}

hk_finish() {
  echo "RESULT: $PASS passed, $FAIL failed"
  [ "$FAIL" -eq 0 ]
}
