#!/bin/sh
# moox installer — Linux (also works on macOS). Architecture §9.4.
#
#   curl -fsSL https://raw.githubusercontent.com/MadAppGang/moox/main/install.sh | sh
#   sh install.sh [--yes] [--dry-run] [--version vX.Y.Z] [--prefix DIR] [--allow-root]
#   sh install.sh --uninstall [--purge] [--yes] [--dry-run] [--prefix DIR]
#
# Installs into <prefix> (default ~/.local):
#   <prefix>/share/moox/<version>/        the release archive, binaries moved into bin/
#   <prefix>/share/moox/current           symlink -> <version>
#   <prefix>/bin/{moox,tt,rpaster}        symlinks -> <prefix>/share/moox/current/bin/*
#   <prefix>/share/moox/install.json      what this installer created (uninstall reads it)
# The archive and checksums.txt come from the SAME release directory and the
# archive is verified before anything is unpacked. Two older versions are kept.
# Dependencies are only checked and reported, never installed. Re-running is
# safe: the same version is re-verified and replaced in place.
#
# --yes          do not stop when <prefix>/bin/{moox,tt,rpaster} already exists and is
#                not ours: move it aside to <name>.pre-moox (uninstall puts it back)
# --uninstall    run `moox uninstall --yes` (undo `moox setup`), then remove exactly
#                what install.json lists and restore what it moved aside
# --purge        with --uninstall: also remove moox's state and config (moox uninstall --purge)
#
# Environment: MOOX_REPO (default MadAppGang/moox), MOOX_DOWNLOAD_BASE (default
# https://github.com/$MOOX_REPO/releases; the archive is fetched from
# $MOOX_DOWNLOAD_BASE/download/v<version>/). curl is preferred, wget works too.
#
# Everything runs inside main(), called on the last line: a truncated download
# runs nothing.

set -eu

TAB=$(printf '\t')

say() { printf 'moox-install: %s\n' "$*"; }
warn() { printf 'moox-install: warning: %s\n' "$*" >&2; }
die() {
	printf 'moox-install: error: %s\n' "$*" >&2
	exit 1
}

# json_str VALUE — VALUE as a JSON string literal.
json_str() {
	printf '"%s"' "$(printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"
}

# json_unstr LITERAL — undo json_str for one value read back from install.json.
json_unstr() {
	printf '%s' "$1" | sed -e 's/\\"/"/g' -e 's/\\\\/\\/g'
}

fetch() { # fetch URL OUT
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "need curl or wget to download"
	fi
}

latest_tag() { # latest_tag BASE — the tag the releases/latest redirect names
	if command -v curl >/dev/null 2>&1; then
		# `|| true`: under set -e a failed lookup would exit here, before the
		# message below says what to do (G4-M7).
		u=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$1/latest" 2>/dev/null || true)
	elif command -v wget >/dev/null 2>&1; then
		u=$(wget -q -S --spider "$1/latest" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1 | tr -d '\r')
	else
		die "need curl or wget to download"
	fi
	t=${u##*/}
	case $t in
	v[0-9]*) printf '%s\n' "$t" ;;
	*) die "could not resolve the latest release from $1/latest (got: ${u:-nothing}); pass --version vX.Y.Z" ;;
	esac
}

sha256_of() { # sha256_of FILE
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | awk '{ print $NF }'
	else
		die "need sha256sum, shasum or openssl to verify the download"
	fi
}

# tmux_ok — 0 when tmux is >= 3.6 (next-* and master count as new enough).
tmux_ok() {
	v=$(tmux -V 2>/dev/null || true)
	case $v in
	"tmux master"*) return 0 ;;
	esac
	mm=$(printf '%s\n' "$v" | sed -n 's/^tmux \(next-\)\{0,1\}\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\2 \3/p')
	[ -n "$mm" ] || return 1
	# shellcheck disable=SC2086 # two numbers, split on purpose
	set -- $mm
	[ "$1" -gt 3 ] || { [ "$1" -eq 3 ] && [ "$2" -ge 6 ]; }
}

check_deps() {
	if ! command -v tmux >/dev/null 2>&1; then
		warn "tmux is not installed; moox needs tmux >= 3.6"
	elif ! tmux_ok; then
		warn "$(tmux -V 2>/dev/null) is too old; moox needs tmux >= 3.6"
	fi
	command -v fzf >/dev/null 2>&1 || warn "fzf is not installed (tt popups and tmux-fzf use it)"
	command -v jq >/dev/null 2>&1 || warn "jq is not installed; the Claude session hooks need it (moox setup --claude-hooks refuses without it)"
	if ! command -v zsh >/dev/null 2>&1 && ! command -v bash >/dev/null 2>&1; then
		warn "neither zsh nor bash found; continuity resume needs one of them"
	fi
	if ! command -v tmux-fingers >/dev/null 2>&1; then
		say "optional: tmux-fingers is not installed (prefix F hints). Arch: the AUR package tmux-fingers; x86_64: the release binary from github.com/Morantron/tmux-fingers"
	fi
	command -v ruby >/dev/null 2>&1 || say "optional: ruby is not installed (the tmux-jump plugin needs it)"
}

usage() {
	sed -n '2,28p' "$0" 2>/dev/null || true
	exit 2
}

# bin_conflicts — print each <bindir>/<b> that exists and is not our link.
bin_conflicts() {
	for b in moox tt rpaster; do
		t="$bindir/$b"
		if [ -L "$t" ]; then
			[ "$(readlink "$t")" = "$share/current/bin/$b" ] && continue
		elif [ ! -e "$t" ]; then
			continue
		fi
		printf '%s\n' "$t"
	done
}

# old_backups — the backups a previous install.json recorded, as
# "<json path literal><TAB><json backup literal>" lines.
old_backups() {
	[ -f "$manifest" ] || return 0
	sed -n "s/^ *{\"path\": \\(\".*\"\\), \"backup\": \\(\".*\"\\)},\\{0,1\\}\$/\\1$TAB\\2/p" "$manifest"
}

write_manifest() { # write_manifest VERSION BACKUPS_FILE
	tmpm="$manifest.tmp.$$"
	{
		printf '{\n'
		printf '  "schema": 1,\n'
		printf '  "version": %s,\n' "$(json_str "$1")"
		printf '  "prefix": %s,\n' "$(json_str "$prefix")"
		printf '  "installed_at": %s,\n' "$(json_str "$(date -u +%Y-%m-%dT%H:%M:%SZ)")"
		printf '  "paths": [\n'
		printf '    %s,\n' "$(json_str "$bindir/moox")"
		printf '    %s,\n' "$(json_str "$bindir/tt")"
		printf '    %s,\n' "$(json_str "$bindir/rpaster")"
		printf '    %s\n' "$(json_str "$share")"
		printf '  ],\n'
		printf '  "backups": [\n'
		n=$(wc -l <"$2" | tr -d ' ')
		i=0
		while IFS="$TAB" read -r p bk; do
			i=$((i + 1))
			sep=,
			[ "$i" -lt "$n" ] || sep=
			printf '    {"path": %s, "backup": %s}%s\n' "$p" "$bk" "$sep"
		done <"$2"
		printf '  ]\n'
		printf '}\n'
	} >"$tmpm"
	mv -f "$tmpm" "$manifest"
}

prune_old() { # keep `current` and the two newest other versions
	cur=$(readlink "$share/current" 2>/dev/null || true)
	# shellcheck disable=SC2010,SC2012 # names are checked against a version pattern
	ls -1t "$share" | grep -E '^[0-9][0-9A-Za-z.+-]*$' | while IFS= read -r d; do
		[ "$d" = "$cur" ] && continue
		printf '%s\n' "$d"
	done | sed '1,2d' | while IFS= read -r d; do
		say "removing old version $d"
		rm -rf "${share:?}/$d"
	done
}

do_install() {
	case $(uname -s) in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported OS: $(uname -s) (moox ships linux and darwin)" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported CPU: $(uname -m) (moox ships amd64 and arm64)" ;;
	esac
	asset="moox_${os}_${arch}.tar.gz"

	if [ -n "$want" ]; then
		tag=v${want#v}
	else
		tag=$(latest_tag "$base")
	fi
	ver=${tag#v}
	case $ver in
	'' | *[!0-9A-Za-z.+-]* | [!0-9]*) die "not a version: $tag" ;;
	esac
	url="$base/download/$tag"

	conflicts=$(bin_conflicts)
	if [ -n "$conflicts" ] && [ "$yes" -ne 1 ]; then
		die "these already exist and are not moox's links:
$conflicts
re-run with --yes to move each aside to <name>.pre-moox (--uninstall puts them back)"
	fi

	if [ "$dry" -eq 1 ]; then
		say "dry run: nothing is downloaded or written"
		say "would download $url/$asset and $url/checksums.txt and verify sha256"
		say "would unpack into $share/$ver (binaries into $share/$ver/bin)"
		say "would link $share/current -> $ver"
		for b in moox tt rpaster; do say "would link $bindir/$b -> $share/current/bin/$b"; done
		printf '%s\n' "$conflicts" | while IFS= read -r c; do
			[ -z "$c" ] || say "would move $c aside to $c.pre-moox"
		done
		say "would keep two older versions and write $manifest"
		check_deps
		return 0
	fi

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/moox-install.XXXXXX")
	trap 'rm -rf "$tmp"' EXIT HUP INT TERM

	say "downloading moox $ver ($os/$arch)"
	fetch "$url/$asset" "$tmp/$asset" || die "download failed: $url/$asset"
	fetch "$url/checksums.txt" "$tmp/checksums.txt" || die "download failed: $url/checksums.txt"
	want_sum=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")
	[ -n "$want_sum" ] || die "checksums.txt of $tag has no line for $asset"
	got_sum=$(sha256_of "$tmp/$asset")
	[ "$want_sum" = "$got_sum" ] || die "checksum mismatch for $asset: expected $want_sum, got $got_sum"
	say "verified sha256 $got_sum"

	mkdir -p "$share" "$bindir"
	stage="$share/.stage.$$"
	rm -rf "$stage"
	mkdir "$stage"
	# The stage dir is inside $share, so the final rename never crosses a filesystem.
	trap 'rm -rf "$tmp" "$stage"' EXIT HUP INT TERM
	tar -xzf "$tmp/$asset" -C "$stage"
	for b in moox tt rpaster; do
		[ -f "$stage/$b" ] || die "$asset has no $b at its root"
	done
	[ -f "$stage/share/moox/moox.conf" ] || die "$asset has no share/moox/moox.conf"
	mkdir "$stage/bin"
	for b in moox tt rpaster; do
		mv "$stage/$b" "$stage/bin/$b"
		chmod 0755 "$stage/bin/$b"
	done

	# A same-version re-install: the active release is renamed aside, not removed,
	# until the replacement is in place (G4-M6). `current` names the version, so
	# it only dangles between two renames on one filesystem; a failed move puts
	# the old release back.
	old=""
	if [ -e "$share/$ver" ]; then
		say "replacing the existing $ver"
		old="$share/.old.$ver.$$"
		rm -rf "$old"
		mv "$share/$ver" "$old"
		# On any exit: no new release in place puts the old one back; a new one in
		# place (a later step such as `ln -sfn` failed) drops the aside copy (G4-2-L1).
		trap 'rm -rf "$tmp" "$stage"; if [ -n "$old" ]; then if [ -e "$share/$ver" ]; then rm -rf "$old"; else mv "$old" "$share/$ver"; fi; fi' EXIT HUP INT TERM
	fi
	if ! mv "$stage" "$share/$ver"; then
		[ -z "$old" ] || mv "$old" "$share/$ver"
		die "could not move the new $ver into place; the previous one is kept"
	fi
	ln -sfn "$ver" "$share/current"
	if [ -n "$old" ]; then
		rm -rf "$old"
		old=""
	fi

	old_backups >"$tmp/backups"
	printf '%s\n' "$conflicts" >"$tmp/conflicts"
	while IFS= read -r c; do
		[ -n "$c" ] || continue
		bk="$c.pre-moox"
		[ ! -e "$bk" ] && [ ! -L "$bk" ] || bk="$c.pre-moox.$(date +%Y%m%d%H%M%S)"
		mv "$c" "$bk"
		say "moved $c aside to $bk"
		printf '%s\t%s\n' "$(json_str "$c")" "$(json_str "$bk")" >>"$tmp/backups"
	done <"$tmp/conflicts"
	for b in moox tt rpaster; do
		ln -sfn "$share/current/bin/$b" "$bindir/$b"
	done
	write_manifest "$ver" "$tmp/backups"
	prune_old

	say "installed moox $ver: $bindir/{moox,tt,rpaster} -> $share/current/bin"
	check_deps
	case ":${PATH}:" in
	*":$bindir:"*) ;;
	*) warn "$bindir is not on PATH; add it to your shell profile" ;;
	esac
	say "done. Now run: moox"
}

do_uninstall() {
	[ -f "$manifest" ] || die "nothing to uninstall: no $manifest (install.sh did not install moox under $prefix)"
	# The moox this installer installed, not whatever <prefix>/bin/moox is now:
	# that link may be gone or replaced by another program (G4-M7).
	mx="$share/current/bin/moox"
	if [ -x "$mx" ]; then
		set -- uninstall --yes
		[ "$purge" -eq 0 ] || set -- "$@" --purge
		[ "$dry" -eq 0 ] || set -- "$@" --dry-run
		say "running: moox $*"
		"$mx" "$@" || die "\`moox $*\` failed; nothing removed. Fix it (moox doctor) and re-run --uninstall"
	else
		warn "$mx is missing; skipping \`moox uninstall\` (undo moox setup by hand)"
	fi

	# paths: the bin links (only while they are still our links) and the share dir.
	# Piped, not through a temp file: --dry-run writes nothing (G4-M7).
	sed -n '/"paths": \[/,/\]/p' "$manifest" | sed -n 's/^ *\(".*"\),\{0,1\}$/\1/p' | while IFS= read -r lit; do
		p=$(json_unstr "${lit#\"}")
		p=${p%\"}
		if [ "$p" = "$share" ]; then
			continue
		fi
		if [ -L "$p" ]; then
			case $(readlink "$p") in
			"$share"/*)
				if [ "$dry" -eq 1 ]; then say "would remove $p"; else rm -f "$p" && say "removed $p"; fi
				;;
			*) warn "$p is no longer moox's link; left alone" ;;
			esac
		elif [ -e "$p" ]; then
			warn "$p is no longer moox's link; left alone"
		fi
	done

	old_backups | while IFS="$TAB" read -r pl bl; do
		p=$(json_unstr "${pl#\"}")
		p=${p%\"}
		bk=$(json_unstr "${bl#\"}")
		bk=${bk%\"}
		if [ ! -e "$bk" ] && [ ! -L "$bk" ]; then
			warn "backup $bk is gone; cannot restore $p"
		elif [ -e "$p" ] || [ -L "$p" ]; then
			warn "$p exists again; left $bk in place"
		elif [ "$dry" -eq 1 ]; then
			say "would restore $p from $bk"
		else
			mv "$bk" "$p" && say "restored $p"
		fi
	done

	if [ "$dry" -eq 1 ]; then
		say "would remove $share"
		return 0
	fi
	rm -rf "$share"
	say "removed $share"
	say "moox is uninstalled"
}

main() {
	yes=0
	dry=0
	want=""
	prefix=""
	uninstall=0
	purge=0
	allow_root=0
	while [ $# -gt 0 ]; do
		case $1 in
		--yes | -y) yes=1 ;;
		--dry-run) dry=1 ;;
		--version)
			[ $# -ge 2 ] || die "--version needs a value (e.g. v0.1.0)"
			want=$2
			shift
			;;
		--version=*) want=${1#--version=} ;;
		--prefix)
			[ $# -ge 2 ] || die "--prefix needs a directory"
			prefix=$2
			shift
			;;
		--prefix=*) prefix=${1#--prefix=} ;;
		--uninstall) uninstall=1 ;;
		--purge) purge=1 ;;
		--allow-root) allow_root=1 ;;
		-h | --help) usage ;;
		*) die "unknown argument: $1 (see --help)" ;;
		esac
		shift
	done
	[ -n "${HOME:-}" ] || die "HOME is not set"
	if [ "$(id -u)" -eq 0 ] && [ "$allow_root" -ne 1 ]; then
		die "refusing to run as root (moox installs per user into ~/.local); pass --allow-root to insist"
	fi
	[ "$purge" -eq 0 ] || [ "$uninstall" -eq 1 ] || die "--purge only goes with --uninstall"

	prefix=${prefix:-$HOME/.local}
	case $prefix in
	/*) ;;
	*) prefix=$(pwd)/$prefix ;;
	esac
	prefix=${prefix%/}
	share="$prefix/share/moox"
	bindir="$prefix/bin"
	manifest="$share/install.json"
	repo=${MOOX_REPO:-MadAppGang/moox}
	base=${MOOX_DOWNLOAD_BASE:-https://github.com/$repo/releases}
	base=${base%/}

	if [ "$uninstall" -eq 1 ]; then
		do_uninstall
	else
		do_install
	fi
}

main "$@"
