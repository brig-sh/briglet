#!/usr/bin/env bash
# Runs hack/fetch-prereqs.sh against a local hull release and a local hull
# repository. A tampered checksums.txt must stop it before anything lands in
# BRIGLET_BIN_DIR, a good release must install hvi, vz-runner and the hull
# CLI, and a hull without --service-cidr must be refused.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
t="$(mktemp -d)"
trap 'rm -rf "$t"' EXIT

# The release: stub hvi and vz-runner in the archive the script asks for.
mkdir -p "$t/rel/src"
printf '#!/bin/sh\necho hvi-fixture\n' >"$t/rel/src/hvi"
printf '#!/bin/sh\necho vz-runner-fixture\n' >"$t/rel/src/vz-runner"
chmod 0755 "$t/rel/src/hvi" "$t/rel/src/vz-runner"
tar -czf "$t/rel/hull-0.1.0-arm64.tar.gz" -C "$t/rel/src" hvi vz-runner
sum="$(shasum -a 256 "$t/rel/hull-0.1.0-arm64.tar.gz" | awk '{ print $1 }')"

# The repository: a hull CLI whose help lists the flags the script checks,
# on branch "good", and one without --service-cidr on branch "old".
src="$t/hullsrc"
mkdir -p "$src/cmd/hull"
printf 'module github.com/brig-sh/hull\n\ngo 1.21\n' >"$src/go.mod"
cat >"$src/cmd/hull/main.go" <<'EOF'
package main

import "fmt"

func main() { fmt.Println("--service-cidr --image-disk") }
EOF
g() { git -C "$src" -c user.name=test -c user.email=test@example.invalid "$@"; }
g init -q -b good
g add -A
g commit -q -m good
g checkout -q -b old
sed -i '' 's/--service-cidr //' "$src/cmd/hull/main.go"
g commit -q -am old

# run <name> <ref> runs the script into its own bin and cache directories.
run() {
	env SKIP_K8S=1 HULL_RELEASE_URL="file://$t/rel" HULL_REPO="file://$src" HULL_REF="$2" \
		BRIGLET_BIN_DIR="$t/$1/bin" BRIGLET_CACHE_DIR="$t/$1/cache" \
		bash "$here/fetch-prereqs.sh" >"$t/$1.log" 2>&1
}
fail() { echo "FAIL: $*" >&2; cat "$t/$name.log" >&2; exit 1; }

name=tampered
printf '%064d  hull-0.1.0-arm64.tar.gz\n' 0 >"$t/rel/checksums.txt"
if run "$name" good; then fail "a tampered checksums.txt was accepted"; fi
grep -q 'failed verification' "$t/$name.log" || fail "no verification error"
for b in hvi vz-runner hull; do
	[ ! -e "$t/$name/bin/$b" ] || fail "$b was installed despite the bad checksum"
done

name=good
printf '%s  hull-0.1.0-arm64.tar.gz\n' "$sum" >"$t/rel/checksums.txt"
run "$name" good || fail "a good release was refused"
cmp -s "$t/rel/src/hvi" "$t/$name/bin/hvi" || fail "hvi differs from the release"
cmp -s "$t/rel/src/vz-runner" "$t/$name/bin/vz-runner" || fail "vz-runner differs from the release"
"$t/$name/bin/hull" | grep -q -- --image-disk || fail "the hull CLI was not built from HULL_REF"

name=old
if run "$name" old; then fail "a hull without --service-cidr was accepted"; fi
grep -q 'no --service-cidr flag' "$t/$name.log" || fail "no --service-cidr error"

echo "fetch-prereqs: ok"
