#!/usr/bin/env bash
# privacy-scan-identity.test.sh — the commit-metadata scan allows EXACTLY the
# publishing identity's address, nothing else (§9.6, R2-H21).
#
# Addresses are assembled at run time ("${at}") so this file itself never holds
# one: the tree scan has no allowance and would (rightly) flag it.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
scan="$here/../scripts/privacy-scan.sh"

pass=0
fail=0
ok() { pass=$((pass + 1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/       /' | head -12; }

T=$(mktemp -d "${TMPDIR:-/tmp}/scan-identity.XXXXXX")
trap 'rm -rf "$T"' EXIT

at='@'
pub="noreply${at}madappgang.com"
same_domain="someone.else${at}madappgang.com"
personal="first.last${at}gmail.com"
prefixed="xnoreply${at}madappgang.com"
suffixed="noreply${at}madappgang.com.example.org"

commit_text() { # commit_text ADDRESS — a commit object as `git cat-file -p` prints it
	printf 'tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n'
	printf 'author MadAppGang <%s> 1761350400 +0000\n' "$1"
	printf 'committer MadAppGang <%s> 1761350400 +0000\n\n' "$1"
	printf 'moox v0.1.0\n'
}

expect() { # expect NAME WANT_RC FILE [ARGS...]
	local n=$1 want=$2 f=$3
	shift 3
	local out rc
	out=$("$scan" "$@" "$f" 2>&1)
	rc=$?
	if [ "$rc" -eq "$want" ]; then ok "$n (exit $rc)"; else no "$n: want exit $want, got $rc" "$out"; fi
}

echo "=== synthetic commit objects ==="
commit_text "$pub" >"$T/pub.txt"
expect "public identity passes with --allow-email <it>" 0 "$T/pub.txt" --allow-email "$pub"
expect "public identity blocks WITHOUT the allowance" 1 "$T/pub.txt"
commit_text "$same_domain" >"$T/same.txt"
expect "another address at the same domain blocks (no domain allowance)" 1 "$T/same.txt" --allow-email "$pub"
commit_text "$personal" >"$T/personal.txt"
expect "a personal address blocks" 1 "$T/personal.txt" --allow-email "$pub"
commit_text "$prefixed" >"$T/prefixed.txt"
expect "a longer local part blocks (exact match only)" 1 "$T/prefixed.txt" --allow-email "$pub"
commit_text "$suffixed" >"$T/suffixed.txt"
expect "a longer domain blocks (exact match only)" 1 "$T/suffixed.txt" --allow-email "$pub"
{
	commit_text "$pub"
	printf 'Co-authored-by: Someone <%s>\n' "$personal"
} >"$T/trailer.txt"
expect "a second address in the message blocks even with the identity allowed" 1 "$T/trailer.txt" --allow-email "$pub"
expect "the identity allowance does not lift --denylist" 1 "$T/pub.txt" --allow-email "$pub" --denylist <(printf 'MadAppGang\n')

echo "=== real commit objects (git commit-tree in a temp repo) ==="
if command -v git >/dev/null 2>&1; then
	r="$T/repo"
	mkdir -p "$r"
	(
		cd "$r" || exit 1
		export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
		git init -q .
		printf 'x\n' >f
		git add f
		t=$(git write-tree)
		mk() { # mk ADDRESS — an orphan commit of t as publish.sh makes it
			GIT_AUTHOR_NAME=MadAppGang GIT_AUTHOR_EMAIL="$1" GIT_AUTHOR_DATE="1761350400 +0000" \
				GIT_COMMITTER_NAME=MadAppGang GIT_COMMITTER_EMAIL="$1" GIT_COMMITTER_DATE="1761350400 +0000" \
				git commit-tree "$t" -m "moox v0.1.0"
		}
		git cat-file -p "$(mk "$pub")" >"$T/real-pub.txt"
		git cat-file -p "$(mk "$same_domain")" >"$T/real-other.txt"
	)
	if [ -s "$T/real-pub.txt" ] && ! grep -q '^parent ' "$T/real-pub.txt"; then ok "commit-tree without -p makes a parentless commit"; else no "parentless commit" "$(cat "$T/real-pub.txt")"; fi
	expect "a real commit by the public identity passes" 0 "$T/real-pub.txt" --allow-email "$pub"
	expect "a real commit by another address blocks" 1 "$T/real-other.txt" --allow-email "$pub"
else
	echo "  skip (no git)"
fi

echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
