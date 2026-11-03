#!/usr/bin/env bash
# Exercises hack/verify-sha256.sh: a good archive passes, a tampered one, an
# unlisted one and a wrong digest are all refused.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
v="$here/verify-sha256.sh"
t="$(mktemp -d)"
trap 'rm -rf "$t"' EXIT

printf 'payload' >"$t/a.tar.gz"
sum="$(shasum -a 256 "$t/a.tar.gz" | awk '{ print $1 }')"

expect() { # expect pass|fail description command...
	want="$1"; what="$2"; shift 2
	if "$@" 2>/dev/null; then got=pass; else got=fail; fi
	[ "$got" = "$want" ] || { echo "FAIL: $what (wanted $want)" >&2; exit 1; }
}

printf '%s  a.tar.gz\n' "$sum" >"$t/sums"
expect pass "matching digest" "$v" "$t/a.tar.gz" "$t/sums"
printf '%s *a.tar.gz\n' "$sum" >"$t/sums"
expect pass "binary-mode entry" "$v" "$t/a.tar.gz" "$t/sums"
printf '%s  other.tar.gz\n' "$sum" >"$t/sums"
expect fail "unlisted archive" "$v" "$t/a.tar.gz" "$t/sums"
printf '%064d  a.tar.gz\n' 0 >"$t/sums"
expect fail "wrong digest" "$v" "$t/a.tar.gz" "$t/sums"
printf '%s  a.tar.gz\n' "$sum" >"$t/sums"
printf 'tampered' >"$t/a.tar.gz"
expect fail "tampered archive" "$v" "$t/a.tar.gz" "$t/sums"
echo "verify-sha256: ok"
