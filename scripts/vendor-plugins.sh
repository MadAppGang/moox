#!/usr/bin/env bash
# vendor-plugins.sh — (re)vendor the tmux plugins pinned in share/moox/plugins/PINS.
#
#   scripts/vendor-plugins.sh [--from DIR] [--only NAME]...
#
# Source of each plugin, at exactly its pinned commit:
#   default     `git clone` of the PINS url into a temp dir, checked out at the sha
#   --from DIR  a directory of existing checkouts named like TPM names them (the
#               url's basename, e.g. DIR/tmux for catppuccin/tmux). The checkout's
#               HEAD is read from its .git files, never by running git there, and
#               must equal the pin. Untracked build output listed in the plugin's
#               own top-level .gitignore is not copied.
#
# What lands in moox:
#   share/moox/plugins/<name>/     the tree, minus .git, CI config, tests, media and
#                                  dev-only files (see copy_tree)
#   share/moox/LICENSES/<name>.LICENSE   the license file named in PINS (tmux-fzf also
#                                  ships Anti-996-LICENSE; tmux-fzf-url's LICENSE.txt is a
#                                  pointer, so the MIT text it points at is shipped too)
#   third_party/<name>/            mode `split` only: the full source (its own go.mod)
#   THIRD_PARTY.md                 regenerated from PINS
#
# A `carried` plugin (tmux-claude-continuity) carries moox's own edits and is
# ALWAYS skipped: copying upstream over it would drop those edits (G4-M9). An
# upstream change is merged by hand; THIRD_PARTY.md lists what moox changed.
# Nothing is pushed and nothing outside moox/ (and a temp dir) is written.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SHARE="$ROOT/share/moox"
PINS="$SHARE/plugins/PINS"
FROM=""
ONLY=" "
while [ "$#" -gt 0 ]; do
  case "$1" in
    --from) FROM="$(cd "$2" && pwd)"; shift 2 ;;
    --only) ONLY="$ONLY$2 "; shift 2 ;;
    --carried)
      echo "vendor-plugins: --carried is gone: a carried plugin holds moox's edits and is never re-vendored over;" >&2
      echo "  merge upstream changes into it by hand (THIRD_PARTY.md lists moox's changes)" >&2
      exit 2 ;;
    -h|--help) sed -n '2,26p' "$0"; exit 0 ;;
    *) echo "vendor-plugins: unknown argument $1" >&2; exit 2 ;;
  esac
done

TMP="$(mktemp -d "${TMPDIR:-/tmp}/moox-vendor.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

# head_of DIR -> the commit DIR's checkout is at, read from its .git files.
head_of() {
  local g="$1/.git" h r
  [ -d "$g" ] || return 1
  h="$(cat "$g/HEAD")"
  case "$h" in
    "ref: "*)
      r="${h#ref: }"
      if [ -f "$g/$r" ]; then cat "$g/$r"
      else awk -v r="$r" '$2 == r { print $1 }' "$g/packed-refs" 2>/dev/null; fi ;;
    *) printf '%s\n' "$h" ;;
  esac
}

# fetch NAME URL SHA -> prints the directory holding the tree at SHA.
fetch() {
  local name="$1" url="$2" sha="$3" src head
  if [ -n "$FROM" ]; then
    src="$FROM/$(basename "$url" .git)"
    [ -d "$src" ] || { echo "vendor-plugins: $name: $src does not exist" >&2; return 1; }
    head="$(head_of "$src" || true)"
    [ "$head" = "$sha" ] || { echo "vendor-plugins: $name: $src is at ${head:-?}, PINS says $sha" >&2; return 1; }
    printf '%s\n' "$src"
  else
    git clone -q "$url" "$TMP/$name" >&2
    git -C "$TMP/$name" checkout -q "$sha" >&2
    printf '%s\n' "$TMP/$name"
  fi
}

# copy_tree SRC DST  — the tree minus VCS, CI, tests, media and ignored build output.
copy_tree() {
  local src="$1" dst="$2" ex=()
  # Plus upstream dev-only files that nothing at runtime reads (CI, Vagrant,
  # release config, a lint fixture full of the author's paths).
  ex=(--exclude=/.git --exclude=/.github --exclude=/test --exclude=/tests --exclude=/spec
      --exclude=/assets --exclude=/video --exclude=/screenshots --exclude=/.claude
      --exclude=/.travis.yml --exclude=/citest --exclude=/Vagrantfile --exclude=/vagrant_provisioning.sh
      --exclude=/_config.yml --exclude=/pattern-check --exclude=/.goreleaser.yaml)
  if [ -n "$FROM" ] && [ -f "$src/.gitignore" ]; then
    while IFS= read -r pat; do
      case "$pat" in ''|'#'*|'!'*) continue ;; esac
      ex+=("--exclude=$pat")
    done < "$src/.gitignore"
  fi
  rm -rf "$dst"
  mkdir -p "$dst"
  rsync -a "${ex[@]}" "$src/" "$dst/"
}

mkdir -p "$SHARE/LICENSES"
while read -r name url sha lic spdx mode; do
  case "$name" in ''|'#'*) continue ;; esac
  [ "$ONLY" = " " ] || case "$ONLY" in *" $name "*) ;; *) continue ;; esac
  if [ "$mode" = carried ]; then
    echo "vendor-plugins: $name is carried (moox edits); skipped — merge upstream by hand, see THIRD_PARTY.md"
    continue
  fi
  src="$(fetch "$name" "$url" "$sha")"
  case "$mode" in
    split)
      copy_tree "$src" "$ROOT/third_party/$name"
      rm -rf "${SHARE:?}/plugins/$name"
      mkdir -p "$SHARE/plugins/$name"
      cp -p "$src"/*.tmux "$SHARE/plugins/$name/"
      rsync -a "$src/plugin/" "$SHARE/plugins/$name/plugin/"
      ;;
    *) copy_tree "$src" "$SHARE/plugins/$name" ;;
  esac
  cp "$src/$lic" "$SHARE/LICENSES/$name.LICENSE"
  [ "$name" = tmux-fzf ] && cp "$src/Anti-996-LICENSE" "$SHARE/LICENSES/tmux-fzf.Anti-996-LICENSE"
  if [ "$name" = tmux-fzf-url ]; then
    # LICENSE.txt only names "MIT © wfxr" via wfxr.mit-license.org; ship the text it points at.
    cat > "$SHARE/LICENSES/tmux-fzf-url.MIT-text" <<'MIT'
The MIT License (MIT)

Copyright © 2018 wfxr

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
MIT
  fi
  echo "vendor-plugins: $name @ ${sha:0:12} ($spdx, $mode)"
done < "$PINS"

"$ROOT/scripts/third-party-md.sh" > "$ROOT/THIRD_PARTY.md"
echo "vendor-plugins: THIRD_PARTY.md regenerated"
