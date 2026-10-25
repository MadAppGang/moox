#!/bin/sh
# embed-copy-image.sh — refresh rpaster's go:embed copy of the copy-image plugin.
#
# third_party/tmux-copy-image/internal/embedded/embed.go embeds `plugin`, a
# GENERATED copy of the module's plugin/ dir (ignored upstream; upstream's
# `make embed-sync` does the same). Without it `go build ./cmd/rpaster` fails
# ("pattern plugin: no matching files found"). GoReleaser runs this in `before`
# (pending fix F2), so every rpaster build embeds the vendored plugin at the pin.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
mod="$root/third_party/tmux-copy-image"
[ -d "$mod/plugin" ] || {
	echo "embed-copy-image: no $mod/plugin" >&2
	exit 1
}
rm -rf "$mod/internal/embedded/plugin"
cp -R "$mod/plugin" "$mod/internal/embedded/plugin"
