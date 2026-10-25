#!/usr/bin/env bash
# packaging.test.sh — V8: the release archives through Homebrew and through setup (§9.1, §9.2, §12.3).
#
#   bash tests/packaging.test.sh                 scratch Homebrew (default; writes nothing outside a temp dir)
#   MOOX_PKG_REAL_BREW=1 bash tests/packaging.test.sh --real-brew
#                                                §12.3's literal procedure against the REAL Homebrew
#                                                (brew tap-new local/mooxtest, brew install, brew test,
#                                                brew uninstall, brew untap) — it writes into the real
#                                                prefix, so it only runs with that variable set
#
# Default (scratch) mode, all inside one temp dir:
#   1. dist/ snapshot archives (built with goreleaser when absent);
#   2. packaging/render-formula.sh with file:// URLs to dist/, and its refusals;
#   3. a scratch Homebrew = a COPY of the installed Homebrew library with its own
#      prefix, Cellar, cache, logs and taps (the real API cache is copied in, read
#      only), a local tap local/mooxtest holding the rendered formula:
#      `brew style`, `brew audit --strict`, `brew install` (dependencies poured
#      as bottles into the scratch prefix), `brew test`, the keg layout,
#      `moox root` = the stable opt/ share root, `brew uninstall`;
#   4. nothing written to the real HOME (~/.config/moox and friends unchanged);
#   5. fake HOME: `moox setup --dry-run` writes nothing; setup with no
#      ~/.tmux.conf, with one (separate, and --mode replace); `moox uninstall`
#      puts every file back byte-identical;
#   6. a Linux container (docker) runs install.sh against the linux archive, or
#      the case is SKIPPED with its reason.
# No tmux server is started: TMUX_TMPDIR points at an empty temp dir and TMUX is unset.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"
dist="${MOOX_DIST:-$root/dist}"
mode=scratch
[ "${1:-}" != "--real-brew" ] || mode=real

pass=0
fail=0
skip=0
ok() { pass=$((pass + 1)); printf '  ok   %s\n' "$1"; }
no() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/       /' | tail -25; }
skipped() { skip=$((skip + 1)); printf '  SKIP %s\n' "$1"; }

if [ "$mode" = real ] && [ "${MOOX_PKG_REAL_BREW:-}" != 1 ]; then
	echo "ABORT: --real-brew installs into the real Homebrew prefix; set MOOX_PKG_REAL_BREW=1 to confirm"
	exit 2
fi

if ! ls "$dist"/moox_*.tar.gz >/dev/null 2>&1; then
	if command -v goreleaser >/dev/null 2>&1 && [ -z "${MOOX_DIST:-}" ]; then
		echo "building snapshot archives (goreleaser release --snapshot --clean)"
		(cd "$root" && goreleaser release --snapshot --clean >/dev/null 2>&1) || { echo "ABORT: snapshot build failed"; exit 1; }
	else
		echo "RESULT: SKIP (no archives in $dist and no goreleaser)"
		exit 0
	fi
fi
dist=$(cd "$dist" && pwd -P)
ver=$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' "$dist/metadata.json" | head -1)
[ -n "$ver" ] || { echo "ABORT: no version in $dist/metadata.json"; exit 1; }
echo "=== V8 packaging, moox $ver, mode $mode ==="

T=$(mktemp -d "${TMPDIR:-/tmp}/moox-pkg.XXXXXX")
T=$(cd "$T" && pwd -P)
if [ -n "${MOOX_PKG_KEEP:-}" ]; then
	echo "work dir kept: $T"
else
	trap 'chmod -R u+w "$T" 2>/dev/null; rm -rf "$T"' EXIT
fi

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{ print $1 }'; }

# The mode format is picked once by a probe whose output is discarded (G4-2-H3):
# GNU `stat -f %Lp FILE` is filesystem mode and prints filesystem statistics to
# stdout before failing, so a `stat -f … || stat -c …` fallback carried them in.
if stat -c %a / >/dev/null 2>&1; then fmode() { stat -c %a "$1"; }; else fmode() { stat -f %Lp "$1"; }; fi

# tree_digest DIR — one line per entry: type, mode, path, link target or content hash.
tree_digest() {
	(cd "$1" && find . -print | LC_ALL=C sort | while IFS= read -r p; do
		if [ -L "$p" ]; then
			printf 'l %s -> %s\n' "$p" "$(readlink "$p")"
		elif [ -d "$p" ]; then
			printf 'd %s %s\n' "$(fmode "$p")" "$p"
		else
			printf 'f %s %s %s\n' "$(fmode "$p")" "$p" "$(sha "$p")"
		fi
	done)
}

# Real-HOME sentinels: none of these may change (V8 "no writes to \$HOME").
sentinels="$HOME/.config/moox $HOME/.local/share/moox $HOME/.local/state/moox $HOME/.tmux.conf $HOME/.config/tmux/tmux.conf $HOME/.zshrc $HOME/.bashrc $HOME/.homebrew/trust.json $HOME/.config/homebrew $HOME/.bundle $HOME/.gem"
# Content, not just shape (G4-M5): a directory is every entry's name, type,
# mode, size and mtime, recursively; a symlink is its target text AND the state
# of what it points at (followed), so an edit behind an unchanged link counts.
if stat -f '%N' / >/dev/null 2>&1; then statfmt=(-f '%N %HT %Lp %z %m'); else statfmt=(-c '%n %F %a %s %Y'); fi
sentinel_digest() { # sentinel_digest DIR [-L]: one stat digest of the whole tree
	find "${2:--P}" "$1" -exec stat "${statfmt[@]}" {} + 2>/dev/null | LC_ALL=C sort | sha /dev/stdin
}
# Live-owned sentinels (G4-2-M1): after the cutover the live moox daemon writes
# ~/.local/state/moox (logs, saver.state, atomic temp files) and brew rewrites
# ~/.homebrew/trust.json while this test runs. Their contents are not ours to
# freeze, so only their shape is recorded: the entry names outside log/, minus
# temp files, and whether the file exists. A path this test creates there still
# shows up as a new name.
live_owned=" $HOME/.local/state/moox $HOME/.homebrew/trust.json "
live_shape() { # live_shape PATH: the names under PATH that a live writer does not churn
	find -P "$1" ! -path '*/log' ! -path '*/log/*' ! -name '*.tmp*' ! -name '.*.tmp' 2>/dev/null | LC_ALL=C sort | sha /dev/stdin
}
sentinel_state() {
	for s in $sentinels; do
		case $live_owned in
		*" $s "*)
			if [ -d "$s" ] && [ ! -L "$s" ]; then printf '%s live-dir %s\n' "$s" "$(live_shape "$s")"
			elif [ -e "$s" ] || [ -L "$s" ]; then printf '%s live-present\n' "$s"
			else printf '%s absent\n' "$s"; fi
			continue ;;
		esac
		if [ -L "$s" ]; then
			if [ -d "$s" ]; then
				printf '%s link %s -> dir %s\n' "$s" "$(readlink "$s")" "$(sentinel_digest "$s" -L)"
			elif [ -f "$s" ]; then
				printf '%s link %s -> file %s\n' "$s" "$(readlink "$s")" "$(sha "$s")"
			else
				printf '%s link %s -> dangling\n' "$s" "$(readlink "$s")"
			fi
		elif [ -d "$s" ]; then
			printf '%s dir %s\n' "$s" "$(sentinel_digest "$s")"
		elif [ -f "$s" ]; then
			printf '%s file %s\n' "$s" "$(sha "$s")"
		else
			printf '%s absent\n' "$s"
		fi
	done
}
sentinel_state >"$T/home-before"

echo "--- archive contract (§12.4)"
for a in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
	f="$dist/moox_$a.tar.gz"
	tar -tzf "$f" | LC_ALL=C sort >"$T/list.$a"
	top=$(grep -v / "$T/list.$a" | tr '\n' ' ')
	if [ "$top" = "LICENSE README.md THIRD_PARTY.md moox rpaster tt " ]; then ok "$a: root = moox tt rpaster LICENSE THIRD_PARTY.md README.md"; else no "$a root" "$top"; fi
	if (cd "$root" && git ls-files share/moox) >"$T/tracked" 2>/dev/null && [ -s "$T/tracked" ]; then
		{ cat "$T/tracked"; echo share/moox/VERSION; } | LC_ALL=C sort >"$T/want.$a"
		grep '^share/moox/' "$T/list.$a" >"$T/got.$a"
		if cmp -s "$T/want.$a" "$T/got.$a"; then ok "$a: share/moox = every tracked file + VERSION, nothing else"; else no "$a share/moox" "$(diff "$T/want.$a" "$T/got.$a")"; fi
	fi
	if ! grep -q -v -e '^share/moox/' -e '^[^/]*$' "$T/list.$a"; then ok "$a: nothing outside share/moox and the root files"; else no "$a stray entries"; fi
	if tar -tvzf "$f" | grep -v -q -E '[[:space:]]root[[:space:]/]+root[[:space:]]'; then no "$a: entries not owned by root:root"; else ok "$a: every entry owned root:root (no build-user name)"; fi
done

echo "--- formula rendering"
formula="$T/moox.rb"
if "$root/packaging/render-formula.sh" --url-base "file://$dist" "v$ver" "$dist/checksums.txt" "$formula" 2>"$T/render.err"; then
	ok "render-formula.sh fills the template"
else
	no "render-formula.sh" "$(cat "$T/render.err")"
fi
if ! grep -q '{{' "$formula" && [ "$(grep -c "url \"file://$dist/moox_" "$formula")" -eq 4 ] && grep -q "version \"$ver\"" "$formula"; then
	ok "four file:// URLs, the version, no placeholder left"
else
	no "rendered formula" "$(cat "$formula")"
fi
okshas=1
for a in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
	h=$(awk -v f="moox_$a.tar.gz" '$2 == f { print $1 }' "$dist/checksums.txt")
	grep -q "sha256 \"$h\"" "$formula" || okshas=0
done
if [ "$okshas" -eq 1 ]; then ok "each archive's sha256 from checksums.txt is in the formula"; else no "sha256 lines"; fi
grep -v linux_amd64 "$dist/checksums.txt" >"$T/short.txt"
if ! "$root/packaging/render-formula.sh" "v$ver" "$T/short.txt" "$T/short.rb" 2>"$T/short.err" && [ ! -e "$T/short.rb" ] && grep -q 'moox_linux_amd64.tar.gz' "$T/short.err"; then
	ok "a checksums.txt missing one archive is refused, nothing written"
else
	no "missing-line refusal" "$(cat "$T/short.err")"
fi
"$root/packaging/render-formula.sh" "v$ver" "$dist/checksums.txt" "$T/release.rb"
if grep -q "url \"https://github.com/MadAppGang/moox/releases/download/v$ver/moox_darwin_arm64.tar.gz\"" "$T/release.rb"; then ok "default URLs are the versioned release downloads"; else no "default URL" "$(grep url "$T/release.rb")"; fi

# ---------------------------------------------------------------------------
if [ "$mode" = scratch ]; then
	echo "--- scratch Homebrew"
	real_brew=$(command -v brew || true)
	if [ -z "$real_brew" ]; then
		skipped "no Homebrew on this machine (the formula is exercised on macOS/Linuxbrew only)"
	else
		brew_repo=$("$real_brew" --repository)
		brew_cache=$("$real_brew" --cache)
		# MOOX_PKG_BREW_DIR keeps the scratch Homebrew between runs (its first `brew style`
		# installs the style gems into the copy's own vendor dir); default: inside $T.
		hb="${MOOX_PKG_BREW_DIR:-$T/hb}"
		hbc="${MOOX_PKG_BREW_DIR:+$MOOX_PKG_BREW_DIR.cache}"
		hbc="${hbc:-$T/hbcache}"
		mkdir -p "$hb/bin" "$hb/Library" "$hbc" "$T/hblogs" "$T/hbtmp"
		if [ ! -f "$hb/Library/Homebrew/brew.sh" ]; then
			cp -R "$brew_repo/Library/Homebrew" "$hb/Library/Homebrew"
			for f in .rubocop.yml tap_rubocop_style.yml README.md; do
				[ ! -f "$brew_repo/Library/$f" ] || cp "$brew_repo/Library/$f" "$hb/Library/$f"
			done
			[ ! -d "$brew_cache/api" ] || cp -R "$brew_cache/api" "$hbc/api"
		fi
		# A fresh prefix every run: no keg, no links, no local tap left from before.
		rm -rf "${hb:?}/Cellar" "${hb:?}/opt" "${hb:?}/var" "${hb:?}/etc" "${hb:?}/share" "${hb:?}/Library/Taps/local" "${hb:?}/bin"
		mkdir -p "$hb/bin"
		cp "$brew_repo/bin/brew" "$hb/bin/brew"
		# Homebrew's build sandbox refuses to run when it could write to the brew script
		# ("Inherited sandbox permits writes"); a temp-dir copy is writable unless made read-only.
		chmod 0555 "$hb/bin/brew"
		tap="$hb/Library/Taps/local/homebrew-mooxtest"
		mkdir -p "$tap/Formula"
		cp "$formula" "$tap/Formula/moox.rb"
		# Brew and the bundler it runs also write under HOME: the tap/formula trust store
		# (~/.homebrew/trust.json, or $XDG_CONFIG_HOME/homebrew) and bundler's gem index
		# cache (~/.bundle). A scratch HOME keeps both inside the temp dir.
		hbhome="$T/hbhome"
		mkdir -p "$hbhome/.config"
		hbrew() {
			env HOME="$hbhome" XDG_CONFIG_HOME="$hbhome/.config" BUNDLE_USER_HOME="$hbhome/.bundle" \
				HOMEBREW_CACHE="$hbc" HOMEBREW_LOGS="$T/hblogs" HOMEBREW_TEMP="$T/hbtmp" \
				HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 HOMEBREW_NO_INSTALL_CLEANUP=1 \
				HOMEBREW_NO_ENV_HINTS=1 HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1 \
				"$hb/bin/brew" "$@"
		}
		p=$(hbrew --prefix)
		if [ "$p" = "$hb" ]; then ok "scratch brew prefix is the temp dir ($hb)"; else no "scratch prefix" "$p"; fi

		if out=$(hbrew style "$tap/Formula/moox.rb" 2>&1); then ok "brew style"; else no "brew style" "$out"; fi
		# Homebrew 7 refuses to load a formula from an untrusted tap ("Refusing to
		# load formula ... from untrusted tap"); the trust goes into the scratch
		# HOME's store. An older brew has no `trust` and needs none.
		if hbrew help trust >/dev/null 2>&1; then
			out=$(hbrew trust --tap local/mooxtest 2>&1) || no "brew trust --tap local/mooxtest" "$out"
		fi
		if out=$(hbrew audit --strict --formula local/mooxtest/moox 2>&1); then ok "brew audit --strict"; else no "brew audit --strict" "$out"; fi

		# With its dependencies (bottles poured into the scratch prefix, downloads into the
		# scratch cache): `brew test` refuses to run while any dependency is missing.
		if out=$(hbrew install --formula local/mooxtest/moox 2>&1); then
			ok "brew install local/mooxtest/moox (archive from file://, deps tmux tmux-fingers fzf jq)"
		else
			no "brew install" "$out"
		fi
		printf '%s\n' "$out" >"$T/brew-install.txt"
		for d in tmux tmux-fingers fzf jq; do
			if [ -d "$hb/opt/$d" ]; then ok "dependency $d installed with the formula"; else no "dependency $d"; fi
		done
		keg="$hb/Cellar/moox/$ver"
		for b in moox tt rpaster; do
			if [ -x "$keg/bin/$b" ] && [ -L "$hb/bin/$b" ]; then ok "keg bin/$b, linked into the prefix"; else no "keg bin/$b" "$(ls -la "$keg/bin" "$hb/bin" 2>&1)"; fi
		done
		if [ -f "$keg/share/moox/moox.conf" ] && [ -f "$keg/share/moox/VERSION" ] && [ -d "$keg/share/moox/plugins/tmux-copy-image" ] && [ -f "$keg/share/doc/moox/THIRD_PARTY.md" ]; then
			ok "pkgshare holds moox.conf, VERSION, the plugins; doc holds THIRD_PARTY.md"
		else
			no "keg layout" "$(find "$keg" -maxdepth 3 2>&1 | head -30)"
		fi
		if [ -x "$keg/share/moox/plugins/tmux-sensible/sensible.tmux" ]; then ok "plugin entry scripts keep +x in the keg"; else no "plugin +x"; fi
		r=$(env -u TMUX -u MOOX_ROOT "$hb/bin/moox" root 2>&1)
		if [ "$r" = "$hb/opt/moox/share/moox" ]; then ok "moox root = <prefix>/opt/moox/share/moox (stable across upgrades)"; else no "moox root" "$r"; fi
		if out=$(hbrew info --formula local/mooxtest/moox 2>&1) && printf '%s' "$out" | grep -q "Tarantool"; then ok "caveats mention moox's tt vs Tarantool's"; else no "caveats" "$out"; fi

		# brew test runs the formula's test block (versions, pkgshare, setup --dry-run --yes
		# in brew's sandboxed temp HOME, with the tmux dependency from the prefix).
		if out=$(hbrew test local/mooxtest/moox 2>&1); then ok "brew test"; else no "brew test" "$out"; fi
		printf '%s\n' "$out" >"$T/brew-test.txt"

		if out=$(hbrew uninstall --formula moox 2>&1) && [ ! -e "$hb/Cellar/moox" ] && [ ! -L "$hb/bin/moox" ]; then ok "brew uninstall removes the keg and links"; else no "brew uninstall" "$out"; fi
	fi
fi

if [ "$mode" = real ]; then
	echo "--- REAL Homebrew (§12.3 V8 procedure)"
	export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_FROM_API=1
	brew tap-new --no-git local/mooxtest >/dev/null
	rtap="$(brew --repository)/Library/Taps/local/homebrew-mooxtest"
	cp "$formula" "$rtap/Formula/moox.rb"
	if out=$(brew audit --strict --formula local/mooxtest/moox 2>&1); then ok "brew audit --strict"; else no "brew audit --strict" "$out"; fi
	if out=$(brew install local/mooxtest/moox 2>&1); then ok "brew install local/mooxtest/moox"; else no "brew install" "$out"; fi
	if out=$(brew test local/mooxtest/moox 2>&1); then ok "brew test"; else no "brew test" "$out"; fi
	brew uninstall --formula local/mooxtest/moox >/dev/null 2>&1 || true
	brew untap local/mooxtest >/dev/null 2>&1 || true
fi

# ---------------------------------------------------------------------------
echo "--- fake HOME: moox setup / uninstall (the archive's moox)"
case $(uname -s) in Linux) os=linux ;; Darwin) os=darwin ;; esac
case $(uname -m) in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; esac
inst="$T/inst"
mkdir -p "$inst/bin"
tar -xzf "$dist/moox_${os}_${arch}.tar.gz" -C "$inst"
for b in moox tt rpaster; do mv "$inst/$b" "$inst/bin/$b"; done
moox="$inst/bin/moox"
host_tmux=$(command -v tmux || true)
if [ -z "$host_tmux" ]; then
	skipped "setup cases (setup needs tmux >= 3.6 on PATH)"
else
	tpath="$inst/bin:$(dirname "$host_tmux"):/usr/bin:/bin"
	fake() { # fake HOME ARGS... — moox in a fake HOME, no tmux server reachable
		local h=$1
		shift
		mkdir -p "$h/.tmuxtmp"
		env -i HOME="$h" PATH="$tpath" SHELL=/bin/zsh TERM=dumb TMUX_TMPDIR="$h/.tmuxtmp" LANG=C.UTF-8 "$moox" "$@" </dev/null
	}
	digest_home() { tree_digest "$1" | grep -v '^[dfl] [0-7]* *\./\.tmuxtmp' | grep -v ' \./\.tmuxtmp'; }

	H="$T/fh-dry"
	mkdir -p "$H"
	printf 'export A=1\n' >"$H/.zshrc"
	digest_home "$H" >"$T/dry.before"
	out=$(fake "$H" setup --dry-run --yes 2>&1)
	rc=$?
	digest_home "$H" >"$T/dry.after"
	if [ "$rc" -eq 0 ] && cmp -s "$T/dry.before" "$T/dry.after" && [ -n "$out" ]; then ok "setup --dry-run lists actions and writes nothing"; else no "setup --dry-run rc=$rc" "$out
$(diff "$T/dry.before" "$T/dry.after")"; fi
	printf '%s\n' "$out" >"$T/dry-run.txt"

	# No ~/.tmux.conf: replace mode, moox's entry file becomes ~/.tmux.conf.
	H="$T/fh-none"
	mkdir -p "$H"
	printf 'export A=1\n' >"$H/.zshrc"
	digest_home "$H" >"$T/none.before"
	out=$(fake "$H" setup --yes 2>&1)
	rc=$?
	if [ "$rc" -eq 0 ] && [ -f "$H/.tmux.conf" ] && grep -q 'moox' "$H/.tmux.conf" && grep -q 'moox.zsh' "$H/.zshrc" && [ -f "$H/.config/moox/setup.json" ]; then
		ok "setup (no ~/.tmux.conf): writes the entry file, the zsh hook line, the manifest"
	else
		no "setup none rc=$rc" "$out"
	fi
	out=$(fake "$H" uninstall --yes 2>&1)
	rc=$?
	digest_home "$H" >"$T/none.after"
	if [ "$rc" -eq 0 ] && cmp -s "$T/none.before" "$T/none.after"; then ok "uninstall (no ~/.tmux.conf case): HOME byte-identical to before setup"; else no "uninstall none rc=$rc" "$out
$(diff "$T/none.before" "$T/none.after")"; fi

	# An existing ~/.tmux.conf, default mode = separate: the user's file is never touched.
	H="$T/fh-sep"
	mkdir -p "$H"
	printf 'set -g mouse on\n# mine\n' >"$H/.tmux.conf"
	printf 'export A=1' >"$H/.zshrc" # no trailing newline
	digest_home "$H" >"$T/sep.before"
	tsum=$(sha "$H/.tmux.conf")
	out=$(fake "$H" setup --yes 2>&1)
	rc=$?
	if [ "$rc" -eq 0 ] && [ "$(sha "$H/.tmux.conf")" = "$tsum" ] && grep -q '@moox-sockets' "$H/.config/moox/local.conf" 2>/dev/null; then
		ok "setup (existing ~/.tmux.conf, separate): user file untouched, local.conf selects socket moox"
	else
		no "setup separate rc=$rc" "$out
$(ls -la "$H" "$H/.config/moox" 2>&1)"
	fi
	out=$(fake "$H" uninstall --yes 2>&1)
	rc=$?
	digest_home "$H" >"$T/sep.after"
	if [ "$rc" -eq 0 ] && cmp -s "$T/sep.before" "$T/sep.after"; then ok "uninstall (separate): HOME byte-identical (incl. an rc without a final newline)"; else no "uninstall separate rc=$rc" "$out
$(diff "$T/sep.before" "$T/sep.after")"; fi

	# An existing ~/.tmux.conf, --mode replace: backed up, replaced, restored by uninstall.
	H="$T/fh-rep"
	mkdir -p "$H"
	printf 'set -g mouse on\n# mine\n' >"$H/.tmux.conf"
	printf 'export A=1\n' >"$H/.zshrc"
	digest_home "$H" >"$T/rep.before"
	out=$(fake "$H" setup --yes --mode replace 2>&1)
	rc=$?
	if [ "$rc" -eq 0 ] && [ "$(sha "$H/.tmux.conf")" != "$tsum" ] && grep -q moox "$H/.tmux.conf" && find "$H/.config/moox/backups" -type f -name '.tmux.conf' 2>/dev/null | grep -q .; then
		ok "setup --mode replace: ~/.tmux.conf backed up and replaced"
	else
		no "setup replace rc=$rc" "$out
$(find "$H/.config/moox" 2>&1 | head)"
	fi
	out=$(fake "$H" uninstall --yes 2>&1)
	rc=$?
	digest_home "$H" >"$T/rep.after"
	if [ "$rc" -eq 0 ] && cmp -s "$T/rep.before" "$T/rep.after"; then ok "uninstall (replace): the original ~/.tmux.conf is back, HOME byte-identical"; else no "uninstall replace rc=$rc" "$out
$(diff "$T/rep.before" "$T/rep.after")"; fi
fi

# ---------------------------------------------------------------------------
echo "--- Linux container: install.sh"
if [ -n "${MOOX_PKG_NO_DOCKER:-}" ]; then
	skipped "Linux container (MOOX_PKG_NO_DOCKER set)"
elif ! command -v docker >/dev/null 2>&1; then
	skipped "Linux container (no docker on PATH)"
elif ! docker info >/dev/null 2>&1; then
	skipped "Linux container (docker daemon not reachable)"
else
	img=${MOOX_PKG_IMAGE:-curlimages/curl:latest}
	if ! docker image inspect "$img" >/dev/null 2>&1 && ! docker pull -q "$img" >/dev/null 2>&1; then
		skipped "Linux container (cannot pull $img)"
	else
		rel="$T/rel/download/v$ver"
		mkdir -p "$rel"
		cp "$dist"/moox_linux_*.tar.gz "$dist/checksums.txt" "$rel/"
		cp "$root/install.sh" "$T/rel/install.sh"
		chmod -R a+rX "$T/rel"
		# The image runs as an unprivileged user; HOME is a fresh dir inside the container.
		out=$(docker run --rm --network none -v "$T/rel:/rel:ro" -e MOOX_DOWNLOAD_BASE=file:///rel "$img" sh -c '
			set -e
			export HOME=$(mktemp -d)
			sh /rel/install.sh --version "v'"$ver"'"
			echo "--- versions"
			$HOME/.local/bin/moox --version
			$HOME/.local/bin/tt --version
			$HOME/.local/bin/rpaster version | tail -n 1
			readlink $HOME/.local/share/moox/current
			sh /rel/install.sh --uninstall
			test ! -e $HOME/.local/share/moox && echo UNINSTALLED
		' 2>&1)
		rc=$?
		printf '%s\n' "$out" >"$T/linux.txt"
		if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q -x "moox $ver" && printf '%s' "$out" | grep -q -x "tt $ver (moox)" &&
			printf '%s' "$out" | grep -q "^rpaster $ver " && printf '%s' "$out" | grep -q -x UNINSTALLED; then
			ok "Linux ($img, $(uname -m)): install.sh, the three binaries report $ver, uninstall"
		else
			no "Linux container rc=$rc" "$out"
		fi
	fi
fi

sentinel_state >"$T/home-after"
if cmp -s "$T/home-before" "$T/home-after"; then ok "real HOME untouched (~/.config/moox, ~/.local/share/moox, ~/.tmux.conf, rc files)"; else no "real HOME changed" "$(diff "$T/home-before" "$T/home-after")"; fi

if [ -n "${MOOX_PKG_EVIDENCE:-}" ]; then
	mkdir -p "$MOOX_PKG_EVIDENCE"
	cp "$formula" "$MOOX_PKG_EVIDENCE/moox.rb.file-url"
	cp "$T/release.rb" "$MOOX_PKG_EVIDENCE/moox.rb"
	[ ! -f "$T/dry-run.txt" ] || cp "$T/dry-run.txt" "$MOOX_PKG_EVIDENCE/setup-dry-run.txt"
	[ ! -f "$T/linux.txt" ] || cp "$T/linux.txt" "$MOOX_PKG_EVIDENCE/linux-container.txt"
fi

echo "RESULT: $pass passed, $fail failed, $skip skipped"
[ "$fail" -eq 0 ]
