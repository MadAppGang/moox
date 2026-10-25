#!/usr/bin/env bash
# hook-bash-trap-join.test.sh — G3-H3 / G3-H4: claude-continuity.bash chains
# itself in front of the user's own DEBUG trap and PROMPT_COMMAND whatever
# their text looks like:
#   * a DEBUG trap body with spaces, `;`, quotes and a quoted expansion keeps
#     running EXACTLY as written (one word, never split or re-evaluated), and
#     a path with a space in it stays one path;
#   * a trap body ending in `;` or a comment stays valid;
#   * a PROMPT_COMMAND ending in `;`, a newline or a comment, an empty one, and
#     an array one, give no syntax error, still run, and the hook still records
#     the typed line and clears it at the prompt.
# Run on every bash found (/bin/bash 3.2 on macOS; a Homebrew/Linux bash 5.x
# when installed). No tmux server: $TMUX is a fake string the hook compares.
. "$(dirname "$0")/lib/hooklib.sh"
hk_setup
SHARE="${SHARE_UNDER_TEST:-$SHARE}"   # override: negative control against another share tree

hk_moox_layout k
ENVW="$(hk_gate_env "$M_SOCK" "$M_STATE" "$M_LAUNCH" "$M_PENDING" "$M_PANES")"
L="$M_LAUNCH/5"

BASHES=""
for b in /bin/bash /opt/homebrew/bin/bash /usr/local/bin/bash /usr/bin/bash; do
  [ -x "$b" ] || continue
  r="$(cd "$(dirname "$b")" && pwd -P)/$(basename "$b")"
  case " $BASHES " in *" $r "*) continue ;; esac
  BASHES="$BASHES $r"
done

for HK_BASH in $BASHES; do
  BV="$("$HK_BASH" -c 'echo "${BASH_VERSINFO[0]}.${BASH_VERSINFO[1]}"')"
  echo "=== $HK_BASH (bash $BV) ==="
  rm -rf "$T/case"; mkdir -p "$T/case"
  C="$T/case"

  # run_case <name> <rc-lines-before-moox>: one interactive bash, two typed lines.
  run_case() {
    rm -f "$L"
    HK_BASHRC="PS1=''
$2
. '$SHARE/shell/moox.bash'"
    # shellcheck disable=SC2086
    hk_bash $ENVW TMUX="$M_SOCK,9,0" TMUX_PANE=%5 >/dev/null <<EOF
cat "$L" > "$C/$1.rec"
trap -p DEBUG > "$C/$1.trap"
EOF
    if grep -q 'syntax error\|unexpected' "$T/bash.err" 2>/dev/null; then
      no "bash $BV $1: no syntax error at the prompt" "$(head -n 3 "$T/bash.err")"
    else ok "bash $BV $1: no syntax error at the prompt"; fi
    hk_eq "bash $BV $1: the typed line is recorded" "cat \"$L\" > \"$C/$1.rec\"" "$(cat "$C/$1.rec" 2>/dev/null)"
    hk_absent "bash $BV $1: cleared at the next prompt" "$L"
  }

  # ── DEBUG trap bodies ──────────────────────────────────────────────────────
  run_case trap-words "trap 'echo \"dbg \$HOME;x\" >> \"$C/dbg words\"; : > \"$C/sep marker\" # tail comment' DEBUG"
  grep -qx "dbg $H;x" "$C/dbg words" 2>/dev/null && ok "bash $BV trap-words: the body runs as written (quoted expansion, ';' inside quotes)" \
    || no "bash $BV trap-words: the body runs as written (quoted expansion, ';' inside quotes)" "$(cat "$C/dbg words" 2>/dev/null | head -n 2)"
  [ -e "$C/sep marker" ] && [ ! -e "$C/sep" ] && [ ! -e "marker" ] && ok "bash $BV trap-words: a path with a space stays one path" \
    || no "bash $BV trap-words: a path with a space stays one path" "$(ls "$C")"
  case "$(cat "$C/trap-words.trap")" in
    *_cc_debug*'dbg $HOME;x'*'tail comment'*) ok "bash $BV trap-words: chained after _cc_debug, the user's text intact" ;;
    *) no "bash $BV trap-words: chained after _cc_debug, the user's text intact" "$(cat "$C/trap-words.trap")" ;;
  esac

  run_case trap-quote "trap 'echo \"it'\"'\"'s\" >> \"$C/dbg quote\"' DEBUG"
  grep -qx "it's" "$C/dbg quote" 2>/dev/null && ok "bash $BV trap-quote: a body with an embedded single quote runs as written" \
    || no "bash $BV trap-quote: a body with an embedded single quote runs as written" "$(head -n 2 "$C/dbg quote" 2>/dev/null)"

  case "$(cat "$C/trap-quote.trap")" in
    *_cc_debug*'it'*'s'*'dbg quote'*) ok "bash $BV trap-quote: still chained after _cc_debug at the prompt" ;;
    *) no "bash $BV trap-quote: still chained after _cc_debug at the prompt" "$(cat "$C/trap-quote.trap")" ;;
  esac

  run_case trap-semi "trap 'echo semi >> \"$C/dbg semi\";' DEBUG"
  [ -s "$C/dbg semi" ] && ok "bash $BV trap-semi: a body ending in ';' still runs" || no "bash $BV trap-semi: a body ending in ';' still runs" "no output"
  case "$(cat "$C/trap-semi.trap")" in
    *_cc_debug*'dbg semi'*) ok "bash $BV trap-semi: still chained after _cc_debug at the prompt" ;;
    *) no "bash $BV trap-semi: still chained after _cc_debug at the prompt" "$(cat "$C/trap-semi.trap")" ;;
  esac

  # The user's trap must keep firing for TYPED commands, not only for the rc
  # lines that ran before the hook was sourced: bash 5.x keeps a DEBUG trap a
  # sourced file sets, so a source-time install replaced the user's for good
  # (iter-2 G3, bash 5.2: only `trap -- '_cc_debug' DEBUG` was left).
  run_case trap-typed "trap '[[ \$BASH_COMMAND == \"cat \"* ]] && echo typed >> \"$C/dbg typed\"' DEBUG"
  grep -qx typed "$C/dbg typed" 2>/dev/null && ok "bash $BV trap-typed: the user's trap fires for a typed command" \
    || no "bash $BV trap-typed: the user's trap fires for a typed command" "no output: the user's DEBUG trap was lost"

  # ── PROMPT_COMMAND shapes ──────────────────────────────────────────────────
  run_case pc-semi "userpc() { echo \"pc rc=\$?\" >> '$C/pc-semi'; }
PROMPT_COMMAND='userpc;'"
  [ -s "$C/pc-semi" ] && ok "bash $BV pc-semi: PROMPT_COMMAND ending in ';' still runs" || no "bash $BV pc-semi: PROMPT_COMMAND ending in ';' still runs" "no output"

  run_case pc-newline "userpc() { echo pc >> '$C/pc-nl'; }
PROMPT_COMMAND=\$'userpc\\n'"
  [ -s "$C/pc-nl" ] && ok "bash $BV pc-newline: PROMPT_COMMAND ending in a newline still runs" || no "bash $BV pc-newline: PROMPT_COMMAND ending in a newline still runs" "no output"

  run_case pc-comment "userpc() { echo pc >> '$C/pc-cm'; }
PROMPT_COMMAND='userpc # set by some rc'"
  [ -s "$C/pc-cm" ] && ok "bash $BV pc-comment: PROMPT_COMMAND ending in a comment still runs" || no "bash $BV pc-comment: PROMPT_COMMAND ending in a comment still runs" "no output"

  run_case pc-history "PROMPT_COMMAND='history -a;'"

  run_case pc-empty "PROMPT_COMMAND="

  run_case pc-array "userpc() { echo pc >> '$C/pc-arr'; }
PROMPT_COMMAND=(userpc)"
  [ -s "$C/pc-arr" ] && ok "bash $BV pc-array: an array PROMPT_COMMAND's command still runs" || no "bash $BV pc-array: an array PROMPT_COMMAND's command still runs" "no output"
done

hk_finish
