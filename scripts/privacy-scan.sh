#!/bin/sh
# privacy-scan.sh — refuse a tree that carries personal or secret data.
#
# Usage:
#   scripts/privacy-scan.sh [--denylist FILE] [--allow-email ADDR]... PATH...
#   scripts/privacy-scan.sh [options] --tree DIR      the tree a commit of DIR would hold
#   scripts/privacy-scan.sh [options] --binary FILE   the printable strings of a binary
#
# PATH mode scans every file under each PATH (contents AND names; .git is
# skipped). --tree scans only what version control would publish from DIR
# (tracked files plus untracked files that are not ignored; without a checkout:
# everything except .git, dist and bin), so dev binaries and build output never
# count. --binary scans `strings -a` of one file (GoReleaser's per-build
# hooks.post, §9.1), which keeps random bytes from looking like addresses.
# Every mode checks for:
#   home       a macOS home path (/Users/<name>), or a Linux one other than the
#              placeholder /home/user
#   email      any e-mail address, except @example.com and the exact addresses
#              given with --allow-email (no domain is ever allowed wholesale)
#   secret     API-token and private-key shapes
#   tailnet    a Tailscale MagicDNS name (anything under ts dot net)
#   key-hex    a 64-hex run inside a .key file (real key material)
#   denylist   every ERE in --denylist FILE (one per line; blank lines and
#              lines starting with # are ignored; matched case-sensitively)
# The exact addresses in scripts/privacy-allow-emails (vendored third-party
# code) are always allowed.
#
# Every hit is printed as "<rule>  <file>:<line>: <text>" (names as
# "<rule>  <path> (file name)"). Exit 0: clean. Exit 1: hits. Exit 2: usage,
# or a scan that could not run (an unreadable file, a denylist line this grep
# cannot compile, a denylist with no patterns): the scan fails CLOSED.
#
# The patterns below are written so this file does not match itself
# (a bracketed character breaks each literal), so the script can live in the
# tree it scans.

set -eu

die() { printf 'privacy-scan: %s\n' "$*" >&2; exit 2; }

denylist=""
allow=""
mode=paths
while [ $# -gt 0 ]; do
	case $1 in
	--denylist)
		[ $# -ge 2 ] || die "--denylist needs a file"
		denylist=$2
		shift 2
		;;
	--allow-email)
		[ $# -ge 2 ] || die "--allow-email needs an address"
		allow="$allow
$2"
		shift 2
		;;
	--tree)
		mode=tree
		shift
		;;
	--binary)
		mode=binary
		shift
		;;
	-h | --help)
		sed -n '2,35p' "$0"
		exit 0
		;;
	--)
		shift
		break
		;;
	-*) die "unknown option: $1" ;;
	*) break ;;
	esac
done
[ $# -gt 0 ] || die "no path to scan (see --help)"
case $mode in
tree) [ $# -eq 1 ] && [ -d "$1" ] || die "--tree needs exactly one directory" ;;
binary) [ $# -eq 1 ] && [ -f "$1" ] || die "--binary needs exactly one file" ;;
esac
for p in "$@"; do
	[ -e "$p" ] || die "no such path: $p"
done
if [ -n "$denylist" ] && [ ! -r "$denylist" ]; then
	die "cannot read denylist: $denylist"
fi

LC_ALL=C
export LC_ALL

work=$(mktemp -d "${TMPDIR:-/tmp}/privacy-scan.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
hits="$work/hits"
: >"$hits"

# Every regular file and every path name under the inputs, .git excluded.
# Names are taken RELATIVE to the input (a directory's own location, e.g. a
# temp dir under the user's home, is not part of the tree being published);
# a file given directly is judged by its base name.
: >"$work/files"
: >"$work/names"
case $mode in
paths)
	find "$@" -name .git -prune -o -type f -print >"$work/files"
	for p in "$@"; do
		if [ -d "$p" ]; then
			find "$p" -name .git -prune -o -print |
				awk -v p="$p" 'length($0) > length(p) { print substr($0, length(p) + 1) }' >>"$work/names"
		else
			basename "$p" >>"$work/names"
		fi
	done
	;;
tree)
	dir=${1%/}
	[ -n "$dir" ] || dir=/
	if (cd "$dir" && git rev-parse --is-inside-work-tree) >/dev/null 2>&1; then
		# Tracked + untracked-not-ignored, relative to DIR.
		(cd "$dir" && git ls-files -z --cached --others --exclude-standard) |
			tr '\000' '\n' | sort -u >"$work/rel"
	else
		(cd "$dir" && find . \( -name .git -o -path ./dist -o -path ./bin \) -prune -o -type f -print) |
			sed 's|^\./||' | sort -u >"$work/rel"
	fi
	while IFS= read -r r; do
		# A tracked file deleted in the work tree is not in the next commit.
		[ -f "$dir/$r" ] || continue
		printf '%s/%s\n' "$dir" "$r" >>"$work/files"
		printf '/%s\n' "$r" >>"$work/names"
	done <"$work/rel"
	;;
binary)
	command -v strings >/dev/null 2>&1 || die "--binary needs strings(1)"
	mkdir "$work/bin"
	dump="$work/bin/$(basename "$1").strings"
	strings -a -n 6 "$1" >"$dump"
	printf '%s\n' "$dump" >"$work/files"
	basename "$1" >"$work/names"
	;;
esac

# grep over the file list; prints file:line:text. -a: a binary is scanned as
# text (a key pasted into a binary still blocks); -I would skip it.
#
# Fail CLOSED (G4-H4): grep's exit 1 means "no match", but 2 means a bad pattern
# or an unreadable file, and must never read as clean. xargs folds every child
# status 1-125 into one 123, so each batch's grep status is recorded by a small
# sh wrapper instead; check_errors (before the verdict) turns any record into
# exit 2. scan runs on the left of a pipe (a subshell), so it cannot exit itself.
gerr="$work/grep-errors"
: >"$gerr"
: >"$gerr.stderr"
scan() { # scan <grep flags> <pattern>
	# shellcheck disable=SC2016 # $0 and $@ belong to the inner sh
	tr '\n' '\0' <"$work/files" |
		xargs -0 sh -c 'grep -n -a -H "$@" 2>>"$0.stderr"; r=$?; [ "$r" -le 1 ] || echo "grep exit $r" >>"$0"; exit 0' "$gerr" "$@" ||
		echo "xargs failed" >>"$gerr"
}
check_errors() {
	if [ -s "$gerr" ]; then
		sed 's/^/privacy-scan: grep: /' "$gerr.stderr" | head -5 >&2
		die "scan error ($(head -n 1 "$gerr")): refusing to report clean"
	fi
}

report() { # report <rule>  (reads file:line:text on stdin)
	while IFS= read -r line; do
		printf '%s  %s\n' "$1" "$(printf '%s' "$line" | cut -c1-240)"
	done >>"$hits"
}

# --- home paths -------------------------------------------------------------
scan -E '/Users/[A-Za-z]' | report home
# Only the matched path is compared, so "/home/user" on the same line as a
# real home cannot hide it, and a longer name that merely starts with "user"
# is not the placeholder.
scan -o -E '/home/[A-Za-z0-9_.-]+' | grep -v -E ':/home/user$' | report home || true
# The same homes flattened into a Claude project slug (~/.claude/projects/
# -Users-<name>-…, -home-<name>-…), G4-L1. Only at a word start, so an option
# like "set-home-dir" is not one. The test fixtures' placeholder names (user,
# someone, somebody, example) pass, exactly and only as the whole name.
slug_ok='(user|someone|somebody|example)'
scan -o -E '(^|[^A-Za-z0-9])-User[s]-[A-Za-z][A-Za-z0-9_.]*' | grep -v -E ":[^A-Za-z0-9]?-Users-$slug_ok\$" | report home || true
scan -o -E '(^|[^A-Za-z0-9])-hom[e]-[A-Za-z0-9_.]+-' | grep -v -E ":[^A-Za-z0-9]?-home-$slug_ok-\$" | report home || true

# --- e-mail addresses -------------------------------------------------------
email_re='[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+[.][A-Za-z]{2,}'
if [ "$mode" = binary ]; then
	# A Go binary packs its string constants back to back, so option names
	# like "…-req-@moox-…reflect.Value" read as addresses. In a binary an
	# address must end in a real-world top-level domain, in lower case.
	email_re='[A-Za-z0-9._%+-]*[A-Za-z0-9_%+]@[A-Za-z0-9-]+([.][A-Za-z0-9-]+)*[.](com|net|org|io|dev|me|co|app|ai|biz|info|email|cloud|uk|de|ru|ua|us|eu)'
fi
scan -o -E "$email_re" >"$work/emails"
printf '%s\n' "$allow" | sed '/^$/d' >"$work/allowed"
# Exact third-party addresses that ship inside vendored upstream code (author
# headers, git URLs), one per line with a comment saying where each one lives.
# Public and exact: no domain is allowed wholesale, the user's own never.
if [ -r "$(dirname "$0")/privacy-allow-emails" ]; then
	grep -v -E '^[[:space:]]*(#|$)' "$(dirname "$0")/privacy-allow-emails" | sed 's/[[:space:]].*//' >>"$work/allowed" || true
fi
while IFS= read -r line; do
	addr=${line#*:}
	addr=${addr#*:}
	case $addr in
	*@example.com) continue ;;
	esac
	if grep -q -x -F -e "$addr" "$work/allowed"; then
		continue
	fi
	printf 'email  %s\n' "$line" >>"$hits"
done <"$work/emails"

# --- secrets and tailnet names ---------------------------------------------
scan -E 'sk[-]ant[-]|gh[p]_|github[_]pat_|AKI[A]|xox[bap][-]|-----BEGI[N] .*PRIVATE KE[Y]' | report secret
scan -E '[.]ts[.]ne[t]' | report tailnet

# --- real key material in .key files ---------------------------------------
grep -E '[.]key$' "$work/files" | while IFS= read -r f; do
	r=0
	grep -n -a -H -E '[0-9A-Fa-f]{64}' "$f" 2>>"$gerr.stderr" || r=$?
	[ "$r" -le 1 ] || echo "grep exit $r on $f" >>"$gerr"
done | report key-hex

# --- the private denylist ---------------------------------------------------
# Fail CLOSED (G4-H4): every line is first compiled on its own by THIS grep
# (BSD and GNU accept different EREs), because all lines go to one `grep -f`
# and a single bad one would otherwise switch the whole list off. A denylist
# with no pattern at all is refused too: asking for one and getting none is a
# broken file, not a clean tree.
if [ -n "$denylist" ]; then
	grep -v -E '^[[:space:]]*(#|$)' "$denylist" >"$work/deny" || true
	[ -s "$work/deny" ] || die "denylist has no patterns: $denylist"
	ln=0
	while IFS= read -r pat || [ -n "$pat" ]; do
		ln=$((ln + 1))
		r=0
		printf '\n' | grep -E -e "$pat" >/dev/null 2>&1 || r=$?
		[ "$r" -le 1 ] || die "denylist pattern $ln is not a valid ERE for this grep (exit $r); refusing to scan"
	done <"$work/deny"
	scan -E -f "$work/deny" | report denylist
	r=0
	grep -E -f "$work/deny" "$work/names" >"$work/deny-names" 2>>"$gerr.stderr" || r=$?
	[ "$r" -le 1 ] || echo "grep exit $r on file names" >>"$gerr"
	while IFS= read -r n; do
		printf 'denylist  %s (file name)\n' "$n"
	done <"$work/deny-names" >>"$hits"
fi

# --- file names (the generic rules) -----------------------------------------
grep -E '/Users/[A-Za-z]|(^|[^A-Za-z0-9])-User[s]-[A-Za-z]|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+[.][A-Za-z]{2,}|[.]ts[.]ne[t]' "$work/names" |
	while IFS= read -r n; do
		printf 'name  %s (file name)\n' "$n"
	done >>"$hits" || true
# /home/<name> in a file name, the placeholder /home/user excepted (G4-L1).
grep -E '/hom[e]/[A-Za-z0-9_.-]+' "$work/names" | while IFS= read -r n; do
	printf '%s\n' "$n" | grep -o -E '/hom[e]/[A-Za-z0-9_.-]+' | grep -q -v -x '/home/user' &&
		printf 'name  %s (file name)\n' "$n"
done >>"$hits" || true

check_errors
n=$(wc -l <"$hits" | tr -d ' ')
if [ "$n" -gt 0 ]; then
	cat "$hits"
	printf 'privacy-scan: %s hit(s) in %s\n' "$n" "$*"
	exit 1
fi
printf 'privacy-scan: clean (%s files) in %s\n' "$(wc -l <"$work/files" | tr -d ' ')" "$*"
