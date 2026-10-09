#!/usr/bin/env bash
# control-plane smoke test: from a wiped state dir, bring the cluster up and check that
# the node is Ready, the apiserver is healthy, the Lease renews, the node
# agent answers on its InternalIP with the apiserver's client cert, and the
# node identity is scoped by the Node authorizer and NodeRestriction.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BRIGLET="$ROOT/bin/briglet"
KUBECTL=${KUBECTL:-kubectl}
STATE=${BRIGLET_STATE_DIR:-$HOME/.briglet}
PKI="$STATE/pki"
NODE_KC="$STATE/kubeconfig/node.kubeconfig"
export KUBECONFIG="$STATE/admin.kubeconfig"
NODE=$(hostname -s | tr '[:upper:]' '[:lower:]')
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { printf '\n## %s\n' "$*"; }
now() { perl -MTime::HiRes=time -e 'printf "%.2f", time'; }

# expect_forbidden DESC CMD...: CMD must fail with an authorization error.
expect_forbidden() {
	local what=$1 out
	shift
	if out=$("$@" 2>&1); then
		fail "$what was allowed: $out"
	fi
	echo "$out" | grep -q 'Forbidden' || fail "$what failed without Forbidden: $out"
	echo "denied as expected: $what"
	echo "  $out"
}

step "build"
(cd "$ROOT" && go build -o bin/briglet ./cmd/briglet)
echo "built $BRIGLET"

step "briglet down --wipe"
"$BRIGLET" down --wipe

step "briglet up"
t0=$(now)
"$BRIGLET" up
t1=$(now)
echo "measured briglet up wall time: $(perl -e "printf '%.2f', $t1 - $t0")s"

step "kubectl get nodes -o wide"
"$KUBECTL" get nodes -o wide
ready=$("$KUBECTL" get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
[ "$ready" = "True" ] || fail "node $NODE Ready=$ready"
echo "node $NODE Ready=$ready"
"$KUBECTL" get node "$NODE" -o jsonpath='capacity: {.status.capacity}{"\n"}labels: {.metadata.labels}{"\n"}'
taints=$("$KUBECTL" get node "$NODE" -o jsonpath='{.spec.taints}')
echo "taints: ${taints:-none}"
[ -z "$taints" ] || fail "node $NODE has taints: $taints"

step "kubectl get --raw /healthz and /readyz"
h=$("$KUBECTL" get --raw /healthz)
echo "/healthz: $h"
[ "$h" = "ok" ] || fail "/healthz returned $h"
r=$("$KUBECTL" get --raw /readyz)
echo "/readyz: $r"
[ "$r" = "ok" ] || fail "/readyz returned $r"

step "kubectl get componentstatuses"
"$KUBECTL" get componentstatuses
bad=$("$KUBECTL" get componentstatuses -o jsonpath='{range .items[*]}{.metadata.name}={.conditions[0].status}{"\n"}{end}' 2>&1 | grep -v '=True$' | grep -v 'deprecated' || true)
[ -z "$bad" ] || fail "unhealthy components: $bad"

step "Lease in kube-node-lease renews"
r1=$("$KUBECTL" -n kube-node-lease get lease "$NODE" -o jsonpath='{.spec.renewTime}')
h1=$("$KUBECTL" get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].lastHeartbeatTime}')
sleep 11
r2=$("$KUBECTL" -n kube-node-lease get lease "$NODE" -o jsonpath='{.spec.renewTime}')
h2=$("$KUBECTL" get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].lastHeartbeatTime}')
echo "lease renewTime:        $r1 -> $r2"
echo "Ready lastHeartbeatTime: $h1 -> $h2"
[ "$r1" != "$r2" ] || fail "lease was not renewed in 11s"
[ "$h1" != "$h2" ] || fail "node status was not updated in 11s"

step "apiserver reaches the node agent with its kubelet client cert"
# The node's InternalIP is 127.0.0.1 and the apiserver dials InternalIP, so
# logs and exec work offline (hack/m1.sh exercises both). The node proxy
# subresource refuses addresses that are not global unicast, so it cannot
# reach a loopback node; check the address and the client cert directly.
addr=$("$KUBECTL" get node "$NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
echo "node InternalIP: $addr"
[ "$addr" = "127.0.0.1" ] || fail "node InternalIP is $addr, not 127.0.0.1"
p=$(curl -sS --cacert "$PKI/ca.crt" --cert "$PKI/apiserver-kubelet-client.crt" --key "$PKI/apiserver-kubelet-client.key" \
	"https://$addr:10250/healthz")
echo "https://$addr:10250/healthz with the apiserver kubelet client cert: $p"
[ "$p" = "ok" ] || fail "kubelet healthz returned $p"
if out=$("$KUBECTL" get --raw "/api/v1/nodes/$NODE/proxy/healthz" 2>&1); then
	echo "/api/v1/nodes/$NODE/proxy/healthz: $out"
else
	echo "/api/v1/nodes/$NODE/proxy/healthz refused for a loopback node, as expected: $out"
	echo "$out" | grep -q 'address not allowed' || fail "node proxy failed for another reason: $out"
fi

step "node agent port rejects clients without a cluster CA cert"
if out=$(curl -sS --cacert "$PKI/ca.crt" "https://127.0.0.1:10250/healthz" 2>&1); then
	fail "kubelet port answered without a client cert: $out"
fi
echo "rejected without client cert: $out"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj /CN=intruder \
	-days 1 -keyout "$TMP/intruder.key" -out "$TMP/intruder.crt" 2>"$TMP/openssl.err" ||
	fail "openssl: $(cat "$TMP/openssl.err")"
if out=$(curl -sS --cacert "$PKI/ca.crt" --cert "$TMP/intruder.crt" --key "$TMP/intruder.key" "https://127.0.0.1:10250/healthz" 2>&1); then
	fail "kubelet port accepted a cert from a foreign CA: $out"
fi
echo "rejected foreign-CA client cert: $out"

step "node identity is scoped by the Node authorizer and NodeRestriction"
"$KUBECTL" --kubeconfig "$NODE_KC" auth whoami
cat >"$TMP/lease.yaml" <<YAML
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata: {name: not-$NODE, namespace: kube-node-lease}
spec: {holderIdentity: not-$NODE, leaseDurationSeconds: 40}
YAML
cat >"$TMP/node.yaml" <<YAML
apiVersion: v1
kind: Node
metadata: {name: not-$NODE}
YAML
expect_forbidden "node lists secrets cluster-wide" \
	"$KUBECTL" --kubeconfig "$NODE_KC" get secrets -A
expect_forbidden "node creates another node's Lease" \
	"$KUBECTL" --kubeconfig "$NODE_KC" create -f "$TMP/lease.yaml"
expect_forbidden "node creates another Node" \
	"$KUBECTL" --kubeconfig "$NODE_KC" create -f "$TMP/node.yaml"
expect_forbidden "node sets a restricted label on itself" \
	"$KUBECTL" --kubeconfig "$NODE_KC" label node "$NODE" node-role.kubernetes.io/control-plane=

step "node agent log has no authorization errors"
if grep -iE 'forbidden|unauthorized' "$STATE/logs/node.log"; then
	fail "node agent hit authorization errors"
fi
echo "none in $STATE/logs/node.log"

step "briglet status"
"$BRIGLET" status

printf '\ncontrol-plane PASS\n'
