#!/usr/bin/env bash
# fetch-prereqs.sh puts everything `briglet up` needs into one directory on
# an Apple silicon Mac.
#
# Control plane: etcd, kube-apiserver and kubectl come from the kubebuilder
# envtest release of Kubernetes. kube-controller-manager and kube-scheduler
# have no darwin release, so they are built from a shallow clone of the
# kubernetes repository at the same tag.
#
# Runtime: hvi, vz-runner and the boot assets come from the hull Homebrew
# cask. The hull CLI is built from the gateway-services branch, because
# `briglet up` needs `network-gateway --service-cidr` for Services.
#
# The result lands in BRIGLET_BIN_DIR, which briglet reads by default, so no
# further configuration is needed when the default location is kept.
set -euo pipefail

K8S_VERSION="${K8S_VERSION:-1.37.0}"
HULL_REPO="${HULL_REPO:-https://github.com/brig-sh/hull.git}"
HULL_REF="${HULL_REF:-feat/gateway-services}"
BIN_DIR="${BRIGLET_BIN_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/briglet/bin}"
CACHE_DIR="${BRIGLET_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/briglet}"
# SKIP_HULL=1 leaves the runtime alone, for a machine that already has a
# hull with Service support on PATH or in HULL_BIN.
SKIP_HULL="${SKIP_HULL:-}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'fetch-prereqs: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required: $2"; }

[ "$(uname -s)" = Darwin ] && [ "$(uname -m)" = arm64 ] || die "briglet runs on Apple silicon macOS only"
need go "install Go 1.26 or newer"
need git "install the Xcode command line tools"
mkdir -p "$BIN_DIR" "$CACHE_DIR"

# 1. etcd, kube-apiserver and kubectl from the envtest release.
log "etcd, kube-apiserver and kubectl ${K8S_VERSION} via setup-envtest"
ENVTEST_DIR="$(GOWORK=off go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use "$K8S_VERSION" -p path)"
for b in etcd kube-apiserver kubectl; do
	[ -x "$ENVTEST_DIR/$b" ] || die "$b is missing from $ENVTEST_DIR"
	cp -f "$ENVTEST_DIR/$b" "$BIN_DIR/$b"
done

# 2. kube-controller-manager and kube-scheduler from source. The repository
# is a Go workspace, so GOWORK must stay unset here. The version variables
# are the ones hack/lib/version.sh stamps, so --version reads the tag
# instead of v0.0.0-master.
SRC="$CACHE_DIR/kubernetes"
if [ ! -d "$SRC/.git" ]; then
	log "cloning kubernetes v${K8S_VERSION} (shallow)"
	git clone -q --depth 1 --branch "v${K8S_VERSION}" https://github.com/kubernetes/kubernetes.git "$SRC"
fi
MAJOR="${K8S_VERSION%%.*}"
MINOR="${K8S_VERSION#*.}"
MINOR="${MINOR%%.*}"
LDFLAGS=""
for pkg in k8s.io/client-go/pkg/version k8s.io/component-base/version; do
	LDFLAGS="$LDFLAGS -X ${pkg}.gitVersion=v${K8S_VERSION} -X ${pkg}.gitMajor=${MAJOR} -X ${pkg}.gitMinor=${MINOR} -X ${pkg}.gitTreeState=clean"
done
for b in kube-controller-manager kube-scheduler; do
	log "building $b (a few minutes the first time)"
	(cd "$SRC" && go build -trimpath -ldflags "$LDFLAGS" -o "$BIN_DIR/$b" "./cmd/$b")
done

# 3. The hull runtime: signed hvi, vz-runner and boot assets from the cask,
# and a hull CLI with the gateway Service table built from source.
if [ -z "$SKIP_HULL" ]; then
	if ! command -v hvi >/dev/null 2>&1; then
		need brew "install Homebrew, then rerun"
		log "installing the hull cask (hvi, vz-runner, boot assets)"
		brew tap brig-sh/brig >/dev/null
		brew install --cask hull
	fi
	HSRC="$CACHE_DIR/hull"
	if [ ! -d "$HSRC/.git" ]; then
		log "cloning hull ($HULL_REF)"
		git clone -q --depth 1 --branch "$HULL_REF" "$HULL_REPO" "$HSRC"
	else
		git -C "$HSRC" fetch -q --depth 1 origin "$HULL_REF"
		git -C "$HSRC" checkout -q FETCH_HEAD
	fi
	log "building the hull CLI from $HULL_REF"
	(cd "$HSRC" && GOWORK=off go build -trimpath -o "$BIN_DIR/hull" ./cmd/hull)
	# Captured rather than piped: grep -q closes the pipe on the first match
	# and the SIGPIPE'd hull would fail the pipeline under pipefail.
	help="$("$BIN_DIR/hull" network-gateway --help 2>&1 || true)"
	case "$help" in
	*--service-cidr*) ;;
	*) die "the hull built from $HULL_REF has no --service-cidr flag; check HULL_REPO and HULL_REF" ;;
	esac
fi

log "done: $BIN_DIR"
ls -l "$BIN_DIR"
if [ "$BIN_DIR" != "${XDG_DATA_HOME:-$HOME/.local/share}/briglet/bin" ]; then
	printf '\nbriglet looks in the default directory first; export BRIGLET_BIN_DIR=%q to use this one.\n' "$BIN_DIR"
fi
