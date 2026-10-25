#!/bin/sh
# silence-bindings.sh — make every key binding the vendored plugins and
# tmux-fingers installed silent and `|| true` (G3-H2, G3-2-H1, G3-2-M1, K14,
# RR12).
#
#   silence-bindings.sh [<moox-root>]      (plugins.conf, after the last plugin)
#
# A run-shell job that prints, or exits non-zero, makes tmux synthesise a line
# for the client that issued it; when that client has gone away, tmux 3.7b
# dereferences it in cmd_run_shell_print and the server dies with every session
# (options.conf header). Every run-shell moox writes ends `>/dev/null 2>&1 ||
# true`, but plugins bind keys AT LOAD TIME, outside any file the conf-lint
# reads: tmux-fingers' `load-config` binds `prefix F`/`J` and its whole
# `fingers` key table, tmux-yank `prefix y`/`Y`, tmux-fzf `prefix f`, tmux-jump
# `prefix j`, and tmux-sensible's `prefix R` is a two-line run-shell that
# prints a source-file error. Those plugins ship unmodified, so the bindings
# are rewrapped here, on the server, after they exist:
#
#   … run-shell -b CMD        →   … run-shell -b "( CMD ) >/dev/null 2>&1 || true"
#   … run-shell -b "CMD"      →   … run-shell -b "( CMD ) >/dev/null 2>&1 || true"
#   … run-shell -b 'CMD'      →   … run-shell -b '( CMD ) >/dev/null 2>&1 || true'
#
# The input is tmux's own `list-keys` text, tokenised the way tmux prints it
# (args_escape: an argument is bare when it has no space or metacharacter,
# single-quoted when it only has a `"`, double-quoted otherwise; commands are
# joined by ` \; `, and by ` ; ` inside `{ … }` blocks). EVERY run-shell at a
# command position is looked at — first, last, or inside a brace block — not
# only one that ends the line. A run-shell with -C (a tmux command, no job) or
# with no command (`run-shell -d 0.3`, a delay) has no output and is left
# alone. The command token is replaced in place, so the key and every other
# quote stay exactly as tmux printed them, and the lines are re-read through
# `source-file`, never `run-shell`.
#
# Only bindings that belong to moox's vendored plugins or tmux-fingers are
# touched — a command naming <moox-root>/plugins/, tmux-fingers, or
# tmux-sensible's `Sourced …!` reload. A user's own binding keeps whatever
# output it was written to show.
#
# "Already silent" means exactly the shape this script writes — `( CMD ) >/dev/
# null 2>&1 || true` — not merely a command that ENDS that way: in
# `a; b >/dev/null 2>&1 || true` the `a` still prints (G3-2-M1). A command
# with a word-start `#` (a shell comment once tmux has expanded its formats)
# gets a newline before the `)`, so the comment cannot swallow it. A reload is a
# no-op: the plugins bind their originals again and this rewraps them once.
#
# Runs inside a config-time run-shell job, so the bare `tmux` reaches THIS
# server through the job's $TMUX; with no $TMUX it does nothing at all. Always
# exits 0 and prints nothing.
[ -n "${TMUX:-}" ] || exit 0

root="${1:-}"
[ -n "$root" ] || root="$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)"

tmp="$(mktemp "${TMPDIR:-/tmp}/moox-silence.XXXXXX" 2>/dev/null)" || exit 0
trap 'rm -f "$tmp"' EXIT HUP INT TERM

tmux list-keys 2>/dev/null | awk -v root="$root/plugins/" '
  # tok(): split the line into tmux tokens; ts[i]/te[i] are the first and last
  # character of token i, tt[i] its text. A quoted token runs to its closing
  # quote (a backslash escapes one character inside "…", nothing inside '\''…'\''),
  # a bare one to the next blank (a backslash escapes one character).
  function tok(line,    n, i, L, c, s) {
    n = 0; i = 1; L = length(line)
    while (i <= L) {
      c = substr(line, i, 1)
      if (c == " " || c == "\t") { i++; continue }
      s = i
      if (c == "\"") {
        i++
        while (i <= L) { c = substr(line, i, 1); if (c == "\\") { i += 2; continue }; if (c == "\"") break; i++ }
        if (i > L) return -1                     # unterminated: not tmux text
        i++
      } else if (c == "'\''") {
        i++
        while (i <= L && substr(line, i, 1) != "'\''") i++
        if (i > L) return -1
        i++
      } else {
        while (i <= L) { c = substr(line, i, 1); if (c == " " || c == "\t") break; if (c == "\\") { i += 2; continue }; i++ }
        if (i > L + 1) i = L + 1
      }
      n++; ts[n] = s; te[n] = i - 1; tt[n] = substr(line, s, i - s)
    }
    return n
  }
  function sep(t) { return t == "\\;" || t == ";" || t == "{" || t == "}" }
  # silent(): the exact shape this script writes, and nothing looser.
  function silent(c,    k) {
    if (substr(c, 1, 2) != "( ") return 0
    k = ") >/dev/null 2>&1 || true"
    if (length(c) < length(k) + 3 || substr(c, length(c) - length(k) + 1) != k) return 0
    c = substr(c, 1, length(c) - length(k))
    return substr(c, length(c)) == " " || substr(c, length(c) - 1) == "\\n"
  }
  function ours(c) {
    return index(c, root) > 0 || index(c, "tmux-fingers") > 0 || c ~ /display-message .Sourced /
  }
  index($0, "bind-key") != 1 { next }
  {
    line = $0
    n = tok(line)
    if (n < 3) next
    # Header: bind-key [-r] [-N note] -T table key …
    i = 2
    while (i <= n && (tt[i] == "-r" || tt[i] == "-T" || tt[i] == "-N")) i += (tt[i] == "-r") ? 1 : 2
    i++                                          # the key
    out = ""; done = 1; changed = 0; cmdpos = 1
    for (k = i; k <= n; k++) {
      if (sep(tt[k])) { cmdpos = 1; continue }
      if (!cmdpos) continue
      cmdpos = 0
      if (tt[k] != "run-shell" && tt[k] != "run") continue
      # Flags: booleans grouped (-bCE), each value flag on its own (-c -d -s -t).
      j = k + 1; isC = 0; bad = 0
      while (j <= n && substr(tt[j], 1, 1) == "-" && length(tt[j]) > 1) {
        if (tt[j] ~ /^-[bCE]+$/) { if (tt[j] ~ /C/) isC = 1; j++ }
        else if (tt[j] ~ /^-[cdst]$/) j += 2
        else { bad = 1; break }
      }
      if (bad || isC || j > n || sep(tt[j])) continue   # odd flags, -C, or no command (a delay)
      if (j < n && !sep(tt[j + 1])) continue            # more than one argument: not this shape
      a = tt[j]; q = substr(a, 1, 1)
      if (q == "\"" || q == "'\''") cmd = substr(a, 2, length(a) - 2)
      else { q = ""; cmd = a; if (substr(cmd, 1, 2) == "\\~") cmd = substr(cmd, 2) }
      if (silent(cmd) || !ours(cmd)) continue
      close_ = " "
      if (q == "\"" && cmd ~ /(^|[ ;&|(]|\\[tn])#([^{]|$)/) close_ = "\\n"
      if (q == "") q = "\""
      new = q "( " cmd close_ ") >/dev/null 2>&1 || true" q
      out = out substr(line, done, ts[j] - done) new
      done = te[j] + 1; changed = 1
    }
    if (changed) print out substr(line, done)
  }' > "$tmp" 2>/dev/null

[ -s "$tmp" ] && tmux source-file "$tmp" >/dev/null 2>&1
exit 0
