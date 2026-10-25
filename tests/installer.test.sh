#!/usr/bin/env bash
# installer.test.sh — install.sh against the REAL snapshot archives, in a fake HOME (§9.1, §9.4, V8).
#
# Uses dist/ (GoReleaser's snapshot output; MOOX_DIST overrides). When dist/ has
# no archives, or was built from another commit than HEAD, and goreleaser is
# on PATH, it builds them first
# (`goreleaser release --snapshot --clean`); without goreleaser it SKIPs.
# The archives are served from a file:// "release" dir laid out like GitHub's
# (download/v<ver>/{moox_<os>_<arch>.tar.gz,checksums.txt}) through
# MOOX_DOWNLOAD_BASE. Nothing outside the temp dir is written.
# shellcheck disable=SC2012 # ls over the installer's own version dirs only
set -u
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"
dist="${MOOX_DIST:-$root/dist}"

pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/       /' | head -20; }
check() { # check NAME CMD...
	local n=$1
	shift
	if "$@" >/dev/null 2>&1; then ok "$n"; else no "$n"; fi
}

# A dist/ built from an older commit tests old binaries (the custom --prefix
# cases failed against a snapshot from before G1-H7), so the default dist/ is
# rebuilt when its metadata names another commit than HEAD. MOOX_DIST is used
# as given.
stale=""
if [ -z "${MOOX_DIST:-}" ] && ls "$dist"/moox_*.tar.gz >/dev/null 2>&1; then
	built=$(sed -n 's/.*"commit":"\([^"]*\)".*/\1/p' "$dist/metadata.json" 2>/dev/null | head -1)
	head=$(cd "$root" && git rev-parse HEAD 2>/dev/null) || head=""
	if [ -n "$head" ] && [ "$built" != "$head" ]; then
		stale="dist/ was built from ${built:-an unknown commit}, HEAD is $head"
	fi
fi
if [ -n "$stale" ] || ! ls "$dist"/moox_*.tar.gz >/dev/null 2>&1; then
	if command -v goreleaser >/dev/null 2>&1 && [ -z "${MOOX_DIST:-}" ]; then
		[ -z "$stale" ] || echo "$stale"
		echo "building snapshot archives (goreleaser release --snapshot --clean)"
		(cd "$root" && goreleaser release --snapshot --clean >/dev/null 2>&1) || {
			echo "ABORT: goreleaser snapshot build failed"
			exit 1
		}
	else
		echo "RESULT: SKIP (no archives in $dist and no goreleaser)"
		exit 0
	fi
fi

ver=$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' "$dist/metadata.json" | head -1)
[ -n "$ver" ] || { echo "ABORT: no version in $dist/metadata.json"; exit 1; }
case $(uname -s) in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "RESULT: SKIP (os)"; exit 0 ;; esac
case $(uname -m) in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; *) echo "RESULT: SKIP (arch)"; exit 0 ;; esac
asset="moox_${os}_${arch}.tar.gz"
echo "=== install.sh with $asset $ver ==="

T=$(mktemp -d "${TMPDIR:-/tmp}/moox-installer.XXXXXX")
trap 'chmod -R u+w "$T" 2>/dev/null; rm -rf "$T"' EXIT
rel="$T/rel"
mkdir -p "$rel/download/v$ver"
cp "$dist"/moox_*.tar.gz "$dist/checksums.txt" "$rel/download/v$ver/"

# A PATH of exactly the tools install.sh uses — no jq, and a fake tmux that is too old.
tools="$T/tools"
mkdir -p "$tools"
for t in sh bash curl tar gzip awk sed grep mktemp uname id date mv rm ln mkdir chmod readlink ls wc tr cat head tail sort cut dirname basename env sha256sum shasum perl openssl find printf pwd cp touch stat strings; do
	p=$(command -v "$t" 2>/dev/null) || continue
	case $p in /*) ln -s "$p" "$tools/$t" ;; esac
done
printf '#!/bin/sh\necho "tmux 3.4"\n' >"$tools/tmux"
chmod +x "$tools/tmux"

new_home() { # new_home NAME — a fresh fake HOME, printed
	local h="$T/$1"
	mkdir -p "$h"
	printf '%s\n' "$h"
}

run_install() { # run_install HOME ARGS... — output in $out, status in $rc
	local h=$1
	shift
	out=$(env -i HOME="$h" PATH="$tools" TMPDIR="$T" MOOX_DOWNLOAD_BASE="file://$rel" \
		"${INSTALL_SH_SHELL:-sh}" "$root/install.sh" "$@" 2>&1)
	rc=$?
}

shells="sh"
command -v dash >/dev/null 2>&1 && shells="$shells dash"

for shell in $shells; do
	INSTALL_SH_SHELL=$(command -v "$shell")
	echo "--- shell: $INSTALL_SH_SHELL"
	H=$(new_home "home-$shell")
	share="$H/.local/share/moox"
	bin="$H/.local/bin"

	# --dry-run writes nothing.
	run_install "$H" --version "v$ver" --dry-run
	if [ "$rc" -eq 0 ] && [ -z "$(ls -A "$H")" ]; then ok "[$shell] --dry-run exits 0 and writes nothing"; else no "[$shell] --dry-run" "$out"; fi

	# Fresh install.
	run_install "$H" --version "v$ver"
	if [ "$rc" -eq 0 ]; then ok "[$shell] install exits 0"; else no "[$shell] install" "$out"; continue; fi
	check "[$shell] versioned dir with bin/{moox,tt} and libexec/rpaster" test -x "$share/$ver/bin/moox" -a -x "$share/$ver/bin/tt" -a -x "$share/$ver/libexec/rpaster"
	check "[$shell] no rpaster in the versioned bin/" test ! -e "$share/$ver/bin/rpaster"
	check "[$shell] no ~/.local/bin/rpaster (V10: never shadow a separate rpaster)" test ! -e "$bin/rpaster" -a ! -L "$bin/rpaster"
	check "[$shell] no binary left at the version root" test ! -e "$share/$ver/moox"
	check "[$shell] share/moox/moox.conf unpacked" test -f "$share/$ver/share/moox/moox.conf"
	check "[$shell] plugin entry scripts keep +x" test -x "$share/$ver/share/moox/plugins/tmux-sensible/sensible.tmux"
	if [ "$(readlink "$share/current")" = "$ver" ]; then ok "[$shell] current -> $ver"; else no "[$shell] current link" "$(ls -l "$share")"; fi
	for b in moox tt; do
		if [ "$(readlink "$bin/$b")" = "$share/current/bin/$b" ]; then ok "[$shell] ~/.local/bin/$b -> current/bin/$b"; else no "[$shell] link $b" "$(ls -l "$bin")"; fi
	done
	check "[$shell] install.json written" test -f "$share/install.json"
	if grep -q "\"version\": \"$ver\"" "$share/install.json" && grep -q "\"$share\"" "$share/install.json"; then ok "[$shell] install.json names the version and the share dir"; else no "[$shell] install.json" "$(cat "$share/install.json")"; fi
	if command -v jq >/dev/null 2>&1; then
		check "[$shell] install.json is valid JSON" jq -e '.paths | length == 3' "$share/install.json"
	fi
	if printf '%s' "$out" | grep -q 'jq is not installed'; then ok "[$shell] warns that jq is missing"; else no "[$shell] jq warning" "$out"; fi
	if printf '%s' "$out" | grep -q 'tmux 3.4 is too old'; then ok "[$shell] warns that tmux < 3.6"; else no "[$shell] tmux version warning" "$out"; fi
	if printf '%s' "$out" | grep -q 'verified sha256'; then ok "[$shell] archive verified against checksums.txt"; else no "[$shell] verify" "$out"; fi
	if printf '%s' "$out" | grep -q 'run: moox'; then ok "[$shell] ends with 'run moox'"; else no "[$shell] final hint" "$out"; fi

	# The installed binaries, through the ~/.local/bin links.
	v1=$(env -i HOME="$H" PATH="$bin:/usr/bin:/bin" "$bin/moox" --version 2>&1)
	if [ "$v1" = "moox $ver" ]; then ok "[$shell] ~/.local/bin/moox --version = moox $ver"; else no "[$shell] moox --version" "$v1"; fi
	v2=$(env -i HOME="$H" PATH="$bin:/usr/bin:/bin" "$bin/tt" --version 2>&1)
	if [ "$v2" = "tt $ver (moox)" ]; then ok "[$shell] ~/.local/bin/tt --version = tt $ver (moox)"; else no "[$shell] tt --version" "$v2"; fi
	v3=$(env -i HOME="$H" PATH="$bin:/usr/bin:/bin" "$bin/moox" rpaster version 2>&1)
	# rpaster prints a first-run tip before the version line (its marker goes under the fake HOME).
	if printf '%s\n' "$v3" | grep -q -x -F "rpaster $ver (commit none, built unknown)"; then ok "[$shell] HOME=<fake> moox rpaster version = rpaster $ver"; else no "[$shell] moox rpaster version" "$v3"; fi
	w=$(env -i HOME="$H" PATH="$bin:/usr/bin:/bin" "$bin/moox" rpaster --which 2>&1)
	wp=${w% (bundled)}
	# Physical paths again (macOS /var -> /private/var), and it must go through `current`.
	if [ "$wp" != "$w" ] && [ "${wp%/current/libexec/rpaster}" != "$wp" ] &&
		[ "$(cd -P "$(dirname "$wp")" 2>/dev/null && pwd -P)" = "$(cd -P "$share/current/libexec" && pwd -P)" ]; then ok "[$shell] moox rpaster resolves current/libexec/rpaster"; else no "[$shell] moox rpaster --which" "$w"; fi
	r=$(env -i HOME="$H" PATH="$bin:/usr/bin:/bin" "$bin/moox" root 2>&1)
	# moox reports the resolved path (macOS: /var -> /private/var); compare physical paths.
	if [ "$(cd -P "$r" 2>/dev/null && pwd -P)" = "$(cd -P "$share/current/share/moox" && pwd -P)" ] && [ "${r%/current/share/moox}" != "$r" ]; then ok "[$shell] moox root = the stable current/share/moox"; else no "[$shell] moox root" "$r"; fi

	# Idempotent: the same version again replaces in place.
	before=$(cd "$share" && ls -A | sort | tr '\n' ' ')
	run_install "$H" --version "v$ver"
	after=$(cd "$share" && ls -A | sort | tr '\n' ' ')
	if [ "$rc" -eq 0 ] && [ "$before" = "$after" ] && [ "$(readlink "$share/current")" = "$ver" ]; then ok "[$shell] re-install is idempotent"; else no "[$shell] re-install" "rc=$rc before=[$before] after=[$after] $out"; fi

	# Retention: current + two older versions.
	for o in 0.0.1 0.0.2 0.0.3 0.0.4; do mkdir -p "$share/$o/bin"; touch -t "20200101010${o##*.}" "$share/$o"; done
	run_install "$H" --version "v$ver"
	kept=$(cd "$share" && ls -d 0.0.[1-4] 2>/dev/null | sort | tr '\n' ' ')
	if [ "$rc" -eq 0 ] && [ "$kept" = "0.0.3 0.0.4 " ] && [ -d "$share/$ver" ]; then ok "[$shell] keeps current plus the two newest old versions"; else no "[$shell] retention" "kept=[$kept] $out"; fi
	rm -rf "$share/0.0.3" "$share/0.0.4"

	# Uninstall removes exactly what install.json lists.
	run_install "$H" --uninstall
	if [ "$rc" -eq 0 ] && [ ! -e "$share" ] && [ ! -L "$bin/moox" ] && [ ! -L "$bin/tt" ] && [ ! -L "$bin/rpaster" ]; then ok "[$shell] --uninstall removes the share dir and the links"; else no "[$shell] uninstall" "rc=$rc $out"; fi
	if printf '%s' "$out" | grep -q 'moox uninstall --yes'; then ok "[$shell] --uninstall ran moox uninstall --yes first"; else no "[$shell] uninstall ran moox uninstall" "$out"; fi
	run_install "$H" --uninstall
	if [ "$rc" -ne 0 ]; then ok "[$shell] a second --uninstall refuses (nothing installed)"; else no "[$shell] second uninstall" "$out"; fi
done
INSTALL_SH_SHELL=""

echo "--- refusals"
H=$(new_home home-sum)
bad="$T/rel-bad/download/v$ver"
mkdir -p "$bad"
cp "$rel/download/v$ver/"* "$bad/"
awk '{ $1 = "0000000000000000000000000000000000000000000000000000000000000000"; print $1 "  " $2 }' "$rel/download/v$ver/checksums.txt" >"$bad/checksums.txt"
out=$(env -i HOME="$H" PATH="$tools" TMPDIR="$T" MOOX_DOWNLOAD_BASE="file://$T/rel-bad" sh "$root/install.sh" --version "v$ver" 2>&1)
rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'checksum mismatch' && [ ! -e "$H/.local/share/moox/$ver" ] && [ ! -e "$H/.local/bin/moox" ]; then ok "a checksum mismatch installs nothing"; else no "checksum mismatch" "rc=$rc $out"; fi

grep -v "$asset" "$rel/download/v$ver/checksums.txt" >"$bad/checksums.txt"
cp "$rel/download/v$ver/$asset" "$bad/$asset"
out=$(env -i HOME="$H" PATH="$tools" TMPDIR="$T" MOOX_DOWNLOAD_BASE="file://$T/rel-bad" sh "$root/install.sh" --version "v$ver" 2>&1)
rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'no line for'; then ok "a checksums.txt without the asset's line installs nothing"; else no "missing checksum line" "rc=$rc $out"; fi

out=$(env -i HOME="$H" PATH="$tools" TMPDIR="$T" MOOX_DOWNLOAD_BASE="file://$rel" sh "$root/install.sh" --version "v9.9.9" 2>&1)
rc=$?
if [ "$rc" -ne 0 ] && [ ! -e "$H/.local/bin/moox" ]; then ok "a version that does not exist installs nothing"; else no "missing version" "$out"; fi

echo "--- an existing ~/.local/bin/tt (another tt)"
H=$(new_home home-conflict)
mkdir -p "$H/.local/bin"
printf '#!/bin/sh\necho other tt\n' >"$H/.local/bin/tt"
chmod 0755 "$H/.local/bin/tt"
orig=$(shasum -a 256 "$H/.local/bin/tt" 2>/dev/null || sha256sum "$H/.local/bin/tt")
orig=${orig%% *}
run_install "$H" --version "v$ver"
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'not moox' && [ ! -e "$H/.local/share/moox" ] && [ ! -L "$H/.local/bin/tt" ]; then ok "refuses to replace a foreign ~/.local/bin/tt without --yes, before downloading"; else no "conflict refusal" "rc=$rc $out"; fi
run_install "$H" --version "v$ver" --yes
if [ "$rc" -eq 0 ] && [ -L "$H/.local/bin/tt" ] && [ -f "$H/.local/bin/tt.pre-moox" ]; then ok "--yes moves the foreign tt aside to tt.pre-moox"; else no "conflict --yes" "rc=$rc $out"; fi
run_install "$H" --version "v$ver"
if [ "$rc" -eq 0 ] && grep -q 'tt.pre-moox' "$H/.local/share/moox/install.json"; then ok "a re-install keeps the recorded backup"; else no "backup kept across re-install" "$(cat "$H/.local/share/moox/install.json" 2>/dev/null)"; fi
run_install "$H" --uninstall
now=$(shasum -a 256 "$H/.local/bin/tt" 2>/dev/null || sha256sum "$H/.local/bin/tt" 2>/dev/null)
now=${now%% *}
if [ "$rc" -eq 0 ] && [ ! -L "$H/.local/bin/tt" ] && [ "$now" = "$orig" ] && [ ! -e "$H/.local/bin/tt.pre-moox" ]; then ok "--uninstall puts the foreign tt back byte-identical"; else no "restore foreign tt" "rc=$rc $out"; fi

echo "--- a separately installed ~/.local/bin/rpaster (V10)"
H=$(new_home home-rpaster)
mkdir -p "$H/.local/bin"
printf '#!/bin/sh\necho "rpaster 0.2.1 (separate)"\n' >"$H/.local/bin/rpaster"
chmod 0755 "$H/.local/bin/rpaster"
orig=$(shasum -a 256 "$H/.local/bin/rpaster" 2>/dev/null || sha256sum "$H/.local/bin/rpaster")
orig=${orig%% *}
run_install "$H" --version "v$ver"
now=$(shasum -a 256 "$H/.local/bin/rpaster" 2>/dev/null || sha256sum "$H/.local/bin/rpaster")
now=${now%% *}
if [ "$rc" -eq 0 ] && [ ! -L "$H/.local/bin/rpaster" ] && [ "$now" = "$orig" ] && [ ! -e "$H/.local/bin/rpaster.pre-moox" ] &&
	[ -L "$H/.local/bin/moox" ] && [ -L "$H/.local/bin/tt" ]; then
	ok "a foreign ~/.local/bin/rpaster is no conflict (no --yes needed), left byte-identical, moox and tt linked"
else
	no "foreign rpaster" "rc=$rc $out"
fi
w=$(env -i HOME="$H" PATH="$H/.local/bin:/usr/bin:/bin" "$H/.local/bin/moox" rpaster --which 2>&1)
if [ "${w% (bundled)}" != "$w" ]; then ok "moox rpaster still uses its bundled copy, not the PATH one"; else no "moox rpaster with a foreign PATH rpaster" "$w"; fi
run_install "$H" --uninstall
if [ "$rc" -eq 0 ] && [ -f "$H/.local/bin/rpaster" ] && [ ! -L "$H/.local/bin/rpaster" ]; then ok "--uninstall leaves the foreign rpaster alone"; else no "uninstall with foreign rpaster" "rc=$rc $out"; fi

echo "--- upgrading over a moox <= 0.1.0 layout (~/.local/bin/rpaster was moox's link)"
H=$(new_home home-legacy-rp)
run_install "$H" --version "v$ver"
lshare="$H/.local/share/moox"
ln -s "$lshare/current/bin/rpaster" "$H/.local/bin/rpaster"
run_install "$H" --version "v$ver"
if [ "$rc" -eq 0 ] && [ ! -e "$H/.local/bin/rpaster" ] && [ ! -L "$H/.local/bin/rpaster" ] && printf '%s' "$out" | grep -q "removed $H/.local/bin/rpaster"; then
	ok "the old (now dangling) rpaster link is removed"
else
	no "old rpaster link" "rc=$rc $out $(ls -l "$H/.local/bin")"
fi
# The old install had moved a foreign rpaster aside and recorded it: it comes back.
ln -s "$lshare/current/bin/rpaster" "$H/.local/bin/rpaster"
printf '#!/bin/sh\necho mine\n' >"$H/.local/bin/rpaster.pre-moox"
bk_lit="{\"path\": \"$H/.local/bin/rpaster\", \"backup\": \"$H/.local/bin/rpaster.pre-moox\"}"
awk -v e="$bk_lit" '{ print } /"backups": \[/ { print "    " e }' "$lshare/install.json" >"$T/ij" && mv "$T/ij" "$lshare/install.json"
run_install "$H" --version "v$ver"
if [ "$rc" -eq 0 ] && [ -f "$H/.local/bin/rpaster" ] && [ ! -L "$H/.local/bin/rpaster" ] && [ "$(cat "$H/.local/bin/rpaster")" = "$(printf '#!/bin/sh\necho mine')" ] &&
	[ ! -e "$H/.local/bin/rpaster.pre-moox" ] && ! grep -q 'rpaster' "$lshare/install.json"; then
	ok "the rpaster an old install moved aside is put back, and dropped from install.json"
else
	no "restore pre-moox rpaster" "rc=$rc $out $(cat "$lshare/install.json")"
fi
if command -v jq >/dev/null 2>&1; then
	check "install.json still valid JSON after dropping the backup" jq -e '.backups | length == 0' "$lshare/install.json"
fi
run_install "$H" --uninstall

echo "--- uninstall edge cases (G4-M7)"
H=$(new_home home-unedge)
run_install "$H" --version "v$ver"
tree_of() { (cd "$1" && find . -exec ls -ld {} + 2>/dev/null | awk '{print $1, $5, $NF}' | sort); }
before=$(tree_of "$H")
run_install "$H" --uninstall --dry-run
after=$(tree_of "$H")
if [ "$rc" -eq 0 ] && [ "$before" = "$after" ]; then ok "--uninstall --dry-run writes nothing"; else no "uninstall dry-run wrote" "rc=$rc $(diff <(printf '%s\n' "$before") <(printf '%s\n' "$after"))"; fi
rm -f "$H/.local/bin/moox"
run_install "$H" --uninstall
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q 'running: moox uninstall --yes' && [ ! -e "$H/.local/share/moox" ]; then ok "a deleted ~/.local/bin/moox: moox uninstall still runs (the installed one)"; else no "uninstall without the bin link" "rc=$rc $out"; fi
H=$(new_home home-unedge2)
run_install "$H" --version "v$ver"
rm -f "$H/.local/bin/moox"
printf '#!/bin/sh\necho IMPOSTOR-RAN >>"%s/impostor.log"\n' "$H" >"$H/.local/bin/moox"
chmod +x "$H/.local/bin/moox"
run_install "$H" --uninstall
if [ ! -e "$H/impostor.log" ] && [ -f "$H/.local/bin/moox" ]; then ok "a replaced ~/.local/bin/moox is never run, and is left alone"; else no "impostor moox" "rc=$rc $out"; fi
H=$(new_home home-latest)
run_install "$H"
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'could not resolve the latest release' && [ -z "$(ls -A "$H")" ]; then ok "a failed latest lookup says what to do (pass --version), installs nothing"; else no "latest lookup failure" "rc=$rc $out"; fi

echo "--- a custom --prefix resolves through current, across upgrades and pruning (G4-H3)"
H=$(new_home home-prefix)
P="$H/opt/moox"
for v in "$ver" 0.0.7 0.0.8 0.0.9; do
	mkdir -p "$rel/download/v$v"
	[ "$v" = "$ver" ] || cp "$rel/download/v$ver/"* "$rel/download/v$v/"
done
run_install "$H" --version "v$ver" --prefix "$P"
r=$(env -i HOME="$H" PATH="/usr/bin:/bin" "$P/bin/moox" root 2>&1)
if [ "$rc" -eq 0 ] && [ "${r%/share/moox/current/share/moox}" != "$r" ] && [ "$(cd -P "$r" && pwd -P)" = "$(cd -P "$P/share/moox/current/share/moox" && pwd -P)" ]; then ok "--prefix: moox root = <prefix>/share/moox/current/share/moox"; else no "--prefix root" "rc=$rc root=$r $out"; fi
if printf '%s' "$out" | grep -q 'writes the versioned path'; then no "the stale non-default-prefix warning is still printed" "$out"; else ok "no stale 'versioned path' warning"; fi
touch -t 202001010101 "$P/share/moox/$ver"
for v in 0.0.7 0.0.8 0.0.9; do run_install "$H" --version "v$v" --prefix "$P"; sleep 1; done
r=$(env -i HOME="$H" PATH="/usr/bin:/bin" "$P/bin/moox" root 2>&1)
if [ ! -e "$P/share/moox/$ver" ] && [ "${r%/share/moox/current/share/moox}" != "$r" ] && [ -f "$r/moox.conf" ]; then ok "--prefix: after upgrades prune the first version, the root still resolves (current)"; else no "--prefix after prune" "root=$r; $(ls -A "$P/share/moox")"; fi
rm -rf "$rel/download/v0.0.7" "$rel/download/v0.0.8" "$rel/download/v0.0.9"

echo "--- root (a fake id that answers 0)"
rtools="$T/tools-root"
cp -R "$tools" "$rtools"
rm -f "$rtools/id"
printf '#!/bin/sh\necho 0\n' >"$rtools/id"
chmod +x "$rtools/id"
H=$(new_home home-root)
out=$(env -i HOME="$H" PATH="$rtools" TMPDIR="$T" MOOX_DOWNLOAD_BASE="file://$rel" sh "$root/install.sh" --version "v$ver" 2>&1)
rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'refusing to run as root' && [ -z "$(ls -A "$H")" ]; then ok "refuses to run as root"; else no "root refusal" "$out"; fi
out=$(env -i HOME="$H" PATH="$rtools" TMPDIR="$T" MOOX_DOWNLOAD_BASE="file://$rel" sh "$root/install.sh" --version "v$ver" --allow-root 2>&1)
rc=$?
if [ "$rc" -eq 0 ] && [ -L "$H/.local/bin/moox" ]; then ok "--allow-root installs"; else no "--allow-root" "$out"; fi

echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
