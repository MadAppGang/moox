#!/usr/bin/env bash
# copy-image-paste.test.sh — G3-H5: the vendored tmux-copy-image `paste.sh`
# (prefix V) under the shape a run-shell job really has:
#   * no TMUX_PANE (tmux sets it only for a pane's own processes): the pane
#     comes from $2 ('#{pane_id}' in the binding), else the session's active
#     pane; before the fix `set -u` aborted on "${TMUX_PANE}" before send-keys;
#   * the daemon's /image/meta reply is DATA: a line that would run code under
#     `eval` runs nothing, and an error body pastes nothing;
#   * the path is sent with `send-keys -l … --` (literal, never a key name);
#   * the TCP fallback (no socket: an EMPTY curl-args array) works on
#     /bin/bash 3.2 under `set -u`.
# `tmux` and `curl` are stubs on PATH that log their argv: no tmux server and no
# network; every path is under the test's temp dir.
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/lib/hooklib.sh"
hk_setup
PASTE="${PASTE_UNDER_TEST:-$SHARE/plugins/tmux-copy-image/plugin/scripts/paste.sh}"   # override: negative control
BIND="$SHARE/plugins/tmux-copy-image/plugin/tmux-clip-image.tmux"
mkdir -p "$T/bin" "$T/tmp"

cat > "$T/bin/tmux" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "$T/tmux.log"
case "\$1" in
  display-message) case "\$*" in *"#{pane_id}"*) echo '%9' ;; esac ;;
esac
exit 0
EOF
cat > "$T/bin/curl" <<EOF
#!/bin/sh
out="" url=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    --output) out=\$2; shift 2 ;;
    --max-time|--max-redirs|--config|--write-out|--unix-socket) shift 2 ;;
    -*) shift ;;
    *) url=\$1; shift ;;
  esac
done
printf '%s\n' "\$url" >> "$T/curl.log"
case "\$url" in
  */health) exit 0 ;;
  */image/meta*) cat "$T/meta-body"; exit 0 ;;
  */image) printf 'PNGDATA' > "\$out"; printf '200'; exit 0 ;;
esac
exit 7
EOF
chmod +x "$T/bin/tmux" "$T/bin/curl"

paste() { # [args...] — paste.sh as a run-shell job sees it: no TMUX_PANE
  : > "$T/tmux.log"; : > "$T/curl.log"
  env -i HOME="$H" PATH="$T/bin:$HK_PATH" TMPDIR="$T/tmp" TMUX="$T/sock,1,0" \
    /bin/bash "$PASTE" "$@" > "$T/paste.out" 2>&1
}
sent() { grep '^send-keys' "$T/tmux.log"; }

# 1. The binding's own shape: session id and pane id, no TMUX_PANE.
printf 'AVAILABLE=true\nFORMAT=png\nWIDTH=10\nHEIGHT=20\nSIZE_BYTES=7\n' > "$T/meta-body"
paste '$1' '%7'; rc=$?
hk_eq "pane from \$2: exit 0" "0" "$rc"
s="$(sent)"
case "$s" in
  "send-keys -l -t %7 -- $T/tmp/clip-"*.png) ok "pane from \$2: the saved .png path is sent literally to %7" ;;
  *) no "pane from \$2: the saved .png path is sent literally to %7" "<$s> $(cat "$T/paste.out")" ;;
esac
f="${s##* -- }"
[ -s "$f" ] && ok "pane from \$2: the image file exists" || no "pane from \$2: the image file exists" "<$f>"
grep -q '^display-message -d 2000 clip-image: image saved to' "$T/tmux.log" \
  && ok "pane from \$2: success message shown" || no "pane from \$2: success message shown" "$(cat "$T/tmux.log")"

# 2. Only the session (an older binding): its active pane, asked of tmux.
paste '$1'; rc=$?
hk_eq "session only: exit 0" "0" "$rc"
case "$(sent)" in "send-keys -l -t %9 -- "*) ok "session only: sent to the session's active pane %9" ;;
  *) no "session only: sent to the session's active pane %9" "<$(sent)> $(cat "$T/paste.out")" ;; esac

# 3. A hostile meta reply: nothing in it runs.
{ printf 'AVAILABLE=true\n'
  printf 'FORMAT=png;touch %s/PWNED1\n' "$T"
  printf '$(touch %s/PWNED2)\n' "$T"
  printf '`touch %s/PWNED3`\n' "$T"
  printf 'X=1; touch %s/PWNED4\n' "$T"
  printf 'AVAILABLE=true; touch %s/PWNED5\n' "$T"; } > "$T/meta-body"
paste '$1' '%7'
pw="$(find "$T" -maxdepth 1 -name "PWNED*" 2>/dev/null)"
hk_eq "hostile meta: nothing in the reply was executed" "" "$pw"
case "$(sent)" in "send-keys -l -t %7 -- $T/tmp/clip-"*.bin) ok "hostile meta: a malformed FORMAT is ignored (.bin)" ;;
  *) no "hostile meta: a malformed FORMAT is ignored (.bin)" "<$(sent)>" ;; esac

# 4. An error body (no image: the daemon answers 404 with JSON) pastes nothing.
printf '{"error":"no image in clipboard","code":"NO_IMAGE"}' > "$T/meta-body"
paste '$1' '%7'; rc=$?
hk_eq "error body: exit 1" "1" "$rc"
hk_eq "error body: nothing sent" "" "$(sent)"
grep -q 'clip-image: no image found in clipboard' "$T/tmux.log" \
  && ok "error body: 'no image found' is shown (the script did not abort first)" \
  || no "error body: 'no image found' is shown (the script did not abort first)" "$(cat "$T/tmux.log") $(cat "$T/paste.out")"

# 5. No pane at all: an error, never a send to an empty target.
paste; rc=$?
hk_eq "no session, no pane: exit 1" "1" "$rc"
hk_eq "no session, no pane: nothing sent" "" "$(sent)"

# 6. The binding passes the pane id, silent and || true.
grep -q "paste.sh' '#{session_id}' '#{pane_id}' >/dev/null 2>&1 || true\"" "$BIND" \
  && ok "binding passes '#{pane_id}' and stays silent" || no "binding passes '#{pane_id}' and stays silent" "$(grep bind-key "$BIND")"

# 7. No eval of daemon data anywhere in the plugin scripts.
if grep -n 'eval' "$SHARE/plugins/tmux-copy-image/plugin/scripts/"*.sh | grep -v ':[0-9]*: *#'; then
  no "no eval in the copy-image scripts" "see above"
else ok "no eval in the copy-image scripts"; fi

hk_finish
