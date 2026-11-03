#!/usr/bin/env bash
# verify-sha256.sh ARCHIVE CHECKSUMS checks ARCHIVE against the line of
# CHECKSUMS (sha256sum format) that names its basename. It exits non-zero
# when the archive is unlisted or the digest differs.
set -euo pipefail

[ "$#" -eq 2 ] || { echo "usage: verify-sha256.sh ARCHIVE CHECKSUMS" >&2; exit 2; }
archive="$1"
sums="$2"
name="$(basename "$archive")"

# sha256sum prints "*name" for binary mode, so strip a leading star.
want="$(awk -v f="$name" '{ n = $2; sub(/^\*/, "", n); if (n == f) { print $1; exit } }' "$sums")"
[ -n "$want" ] || { echo "verify-sha256: $name is not listed in $sums" >&2; exit 1; }
got="$(shasum -a 256 "$archive" | awk '{ print $1 }')"
[ "$got" = "$want" ] || { echo "verify-sha256: $name has sha256 $got, checksums say $want" >&2; exit 1; }
