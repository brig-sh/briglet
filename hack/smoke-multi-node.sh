#!/usr/bin/env bash
# multi-node smoke test: a second node agent joins the cluster. From a wiped state
# dir, `briglet join` mints the second node's credentials and a second
# agent runs from its own state dir against the shared gateway. Both nodes
# come up Ready with complementary pod CIDR halves. CoreDNS runs on both
# nodes, so pods on the second node resolve through a local endpoint. A
# busybox pod on the second node reaches an nginx Service whose endpoint
# runs on the first node, and its logs and exec go through the second
# node's kubelet server. Deleting the pods leaves no VM in either hull
# store. Refuses to run while a Sonobuoy run is active.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BRIGLET="$ROOT/bin/briglet"
KUBECTL=${KUBECTL:-kubectl}
export HULL_BIN=${HULL_BIN:-hull}
STATE=${BRIGLET_STATE_DIR:-$HOME/.briglet}
NODE2=node2
S2=$HOME/.briglet-$NODE2
GW_API="$STATE/gw.sock.api"
export KUBECONFIG="$STATE/admin.kubeconfig"
TMP=$(mktemp -d)
N2PID=

fail() {
	echo "FAIL: $*" >&2
	exit 1
}
step() { printf '\n## %s\n' "$*"; }
k() { "$KUBECTL" "$@"; }
hull1() { "$HULL_BIN" --unattended --dnt --store-dir "$STATE/hull-store" "$@"; }
hull2() { "$HULL_BIN" --unattended --dnt --store-dir "$S2/hull-store" "$@"; }
wait_for() { # wait_for DESC SECONDS CMD...
	local desc=$1 timeout=$2
	shift 2
	local start=$(date +%s)
	while :; do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		if [ $(( $(date +%s) - start )) -ge "$timeout" ]; then
			fail "timeout after ${timeout}s: $desc"
		fi
		sleep 2
	done
}
pod_field() { k get pod "$1" -o "jsonpath=$2"; }
cleanup() {
	[ -n "$N2PID" ] && kill "$N2PID" 2>/dev/null || true
	rm -rf "$TMP"
}
trap cleanup EXIT

if k get pods -n sonobuoy --no-headers 2>/dev/null | grep -q .; then
	fail "a Sonobuoy run is active; multi-node changes cluster state, run it after the conformance run"
fi

step "wipe and bring the cluster up"
"$BRIGLET" down --wipe >/dev/null 2>&1 || true
make -C "$ROOT" >/dev/null
rm -rf "$S2"
"$BRIGLET" up
k get nodes -o wide

step "mint join credentials for $NODE2"
"$BRIGLET" join --name "$NODE2" --ip 127.0.0.1 --port 10251 --out "$S2/join"
[ -f "$S2/join/node.kubeconfig" ] || fail "no node.kubeconfig in the join bundle"
mkdir -p "$S2/guest-bin" "$S2/logs"
cp "$STATE/guest-bin/briglet-guest" "$S2/guest-bin/"

step "start the second node agent against the shared gateway"
BRIGLET_STATE_DIR="$S2" "$BRIGLET" node \
	--kubeconfig "$S2/join/node.kubeconfig" \
	--client-ca-file "$S2/join/ca.crt" \
	--tls-cert-file "$S2/join/kubelet-serving-$NODE2.crt" \
	--tls-private-key-file "$S2/join/kubelet-serving-$NODE2.key" \
	--name "$NODE2" --node-ip 127.0.0.1 --port 10251 \
	--gateway-sock "$STATE/gw.sock" --host-ip 10.87.0.1 >"$S2/logs/node.log" 2>&1 &
N2PID=$!
sleep 1
kill -0 "$N2PID" 2>/dev/null || { tail "$S2/logs/node.log"; fail "second agent exited"; }

step "both nodes Ready with complementary pod CIDRs"
wait_for "node $NODE2 Ready" 90 k get node "$NODE2" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}'
CIDR1=$(k get node "$(hostname -s | tr '[:upper:]' '[:lower:]')" -o jsonpath='{.spec.podCIDR}')
CIDR2=$(k get node "$NODE2" -o jsonpath='{.spec.podCIDR}')
echo "node1 podCIDR: $CIDR1"
echo "node2 podCIDR: $CIDR2"
[ "$CIDR1" != "$CIDR2" ] || fail "both nodes have podCIDR $CIDR1"
k get nodes -o wide

step "CoreDNS runs on both nodes"
k scale deployment -n kube-system coredns --replicas=2
wait_for "coredns 2/2" 120 sh -c "$KUBECTL get deployment -n kube-system coredns -o 'jsonpath={.status.readyReplicas}' | grep -q 2"
for _ in $(seq 1 10); do
	if k get pods -n kube-system -l k8s-app=kube-dns -o wide | grep -q "$NODE2"; then
		break
	fi
	# Both replicas landed on node1; move one by deleting it.
	POD=$(k get pods -n kube-system -l k8s-app=kube-dns -o jsonpath='{.items[0].metadata.name}')
	k delete pod -n kube-system "$POD" >/dev/null
	wait_for "coredns pod back" 60 sh -c "$KUBECTL get pods -n kube-system -l k8s-app=kube-dns -o 'jsonpath={.status.readyReplicas}' | grep -q 2"
done
k get pods -n kube-system -l k8s-app=kube-dns -o wide

step "pods on both nodes, Service across them"
cat >"$TMP/web.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      nodeSelector: {kubernetes.io/hostname: "$(hostname -s | tr '[:upper:]' '[:lower:]')"}
      containers:
      - name: nginx
        image: nginx:alpine
        ports: [{containerPort: 80}]
EOF
cat >"$TMP/client.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata: {name: client}
spec:
  nodeSelector: {kubernetes.io/hostname: "$NODE2"}
  containers:
  - name: client
    image: busybox
    command: [sh, -c, 'while true; do wget -qO- -T 2 http://web/ >/dev/null; sleep 2; done']
EOF
k apply -f "$TMP/web.yaml" >/dev/null
k expose deployment web --port=80 >/dev/null
k apply -f "$TMP/client.yaml" >/dev/null
wait_for "client Running" 90 sh -c "[ \$($KUBECTL get pod client -o 'jsonpath={.status.phase}') = Running ]"
CLIENT_IP=$(pod_field client '{.status.podIP}')
WEB_IP=$(pod_field "$(k get pod -l app=web -o jsonpath='{.items[0].metadata.name}')" '{.status.podIP}')
echo "web pod IP (node1): $WEB_IP"
echo "client pod IP (node2): $CLIENT_IP"
echo "client node: $(pod_field client '{.spec.nodeName}')"

step "cross-node Service, DNS, logs and exec"
k exec client -- wget -qO- -T 5 http://web/ | grep -q "Welcome to nginx" || fail "client on node2 cannot reach the web Service on node1"
k exec client -- nslookup kubernetes.default.svc.cluster.local | grep -q "10.96.0.1" || fail "client on node2 cannot resolve through CoreDNS"
k logs client | tail -1
k exec client -- sh -c 'echo exec through node2 kubelet ok'

step "delete the pods; no VM is left in either store"
k delete -f "$TMP/client.yaml" --grace-period=1 >/dev/null
k delete deployment web --grace-period=1 >/dev/null
k scale deployment -n kube-system coredns --replicas=1 >/dev/null
sleep 3
LEFT1=$(hull1 ps 2>/dev/null | awk 'NR>1 && $1 !~ /^kube-system_/ {print $1}')
LEFT2=$(hull2 ps 2>/dev/null | awk 'NR>1 {print $1}')
[ -z "$LEFT1" ] || fail "VMs left in node1 store: $LEFT1"
[ -z "$LEFT2" ] || fail "VMs left in node2 store: $LEFT2"
kill "$N2PID"
N2PID=
wait "$N2PID" 2>/dev/null || true
echo "no VMs left"

"$BRIGLET" status
echo "multi-node PASS"
