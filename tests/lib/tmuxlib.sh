#!/usr/bin/env bash
# tmuxlib.sh — an isolated scratch tmux server for the shell tests.
#
# Safety contract (the user's live workspace runs on this machine):
#   * the server is `tmux -f /dev/null -S "$T/<name>"`: a socket file under the
#     test's own temp dir, never `-L`, never the default socket, no user config;
#   * panes run `sleep` under SHELL=/bin/sh, never an interactive zsh/bash with
#     the user's rc files;
#   * the server is never killed with kill-server: ts_stop kills its sessions
#     and tmux exits on its own (exit-empty);
#   * TMUX is removed from every tmux call, so nothing steers a call elsewhere.
# Requires hooklib.sh's hk_setup first ($T).
# Variables set here (TS_SOCK, TS_PID) are read by the sourcing test.
# shellcheck disable=SC2034

# ts_start <name>  -> TS_SOCK; one session "s" (window 0, pane %0) running sleep
ts_start() {
  command -v tmux >/dev/null 2>&1 || { echo "SKIP: tmux not installed"; exit 0; }
  TS_SOCK="$T/$1"
  case "$TS_SOCK" in "$T"/*) ;; *) echo "ABORT: socket outside the temp dir"; exit 1 ;; esac
  case "$TS_SOCK" in */tmux-[0-9]*/default) echo "ABORT: refusing a default-socket path"; exit 1 ;; esac
  env -u TMUX SHELL=/bin/sh tmux -f /dev/null -S "$TS_SOCK" new-session -d -s s -x 80 -y 24 'sleep 600' \
    || { echo "ABORT: scratch server did not start"; exit 1; }
  TS_PID="$(ts display-message -p '#{pid}')"
  TS_STARTED="${TS_STARTED:-} $TS_SOCK"
  trap 'ts_stop_all; rm -rf "$T"' EXIT INT TERM HUP
}

# ts <tmux args...>  — a command on the scratch server
ts() { env -u TMUX tmux -S "$TS_SOCK" "$@"; }

ts_stop_all() {
  local s
  for s in ${TS_STARTED:-}; do
    env -u TMUX tmux -S "$s" kill-session -t s >/dev/null 2>&1 || true
  done
}
