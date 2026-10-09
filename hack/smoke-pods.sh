#!/usr/bin/env bash
# pods smoke test: from a wiped state dir, run nginx as a pod in a hull VM,
# read its logs, exec into it, reach it from the Mac through a gateway
# forward, run a one-shot pod to Succeeded, reject an unsupported pod, check
# that the pod endpoints admit only the apiserver, check that `briglet down`
# removes the VMs, and delete the pods.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BRIGLET="$ROOT/bin/briglet"
KUBECTL=${KUBECTL:-kubectl}
HULL_BIN=${HULL_BIN:-/opt/homebrew/bin/hull}
STATE=${BRIGLET_STATE_DIR:-$HOME/.briglet}
PKI="$STATE/pki"
GW_API="$STATE/gw.sock.api"
export KUBECONFIG="$STATE/admin.kubeconfig"
FWD_PORT=${FWD_PORT:-18087}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { printf '\n## %s\n' "$*"; }
now() { perl -MTime::HiRes=time -e 'printf "%.2f", time'; }
hull_ps() { "$HULL_BIN" --unattended --dnt --store-dir "$STATE/hull-store" ps; }

# wait_phase POD PHASE TIMEOUT: poll every 0.5 s until the pod reaches PHASE
# (and, for Running, has a podIP and a running container).
wait_phase() {
	local pod=$1 want=$2 timeout=$3 t0 got ip running
	t0=$(now)
	while :; do
		got=$("$KUBECTL" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		ip=$("$KUBECTL" get pod "$pod" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
		running=$("$KUBECTL" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.running.startedAt}' 2>/dev/null || true)
		if [ "$got" = "$want" ] && { [ "$want" != Running ] || { [ -n "$ip" ] && [ -n "$running" ]; }; }; then
			return 0
		fi
		if perl -e "exit(!($(now) - $t0 > $timeout))"; then
			"$KUBECTL" get pod "$pod" -o yaml >&2 || true
			fail "pod $pod is $got, not $want, after ${timeout}s"
		fi
		sleep 0.5
	done
}

step "build"
(cd "$ROOT" && go build -o bin/briglet ./cmd/briglet)
echo "built $BRIGLET"

step "briglet down --wipe"
"$BRIGLET" down --wipe

step "briglet up"
t0=$(now)
"$BRIGLET" up
echo "measured briglet up wall time: $(perl -e "printf '%.2f', $(now) - $t0")s"

step "kubectl run web --image=nginx:alpine --port=80"
t0=$(now)
"$KUBECTL" run web --image=nginx:alpine --port=80
wait_phase web Running 180
t1=$(now)
echo "measured pod create -> Running with podIP: $(perl -e "printf '%.2f', $t1 - $t0")s (includes the image pull)"
"$KUBECTL" get pod web -o wide
IP=$("$KUBECTL" get pod web -o jsonpath='{.status.podIP}')
"$KUBECTL" get pod web -o jsonpath='hostIP: {.status.hostIP}{"\n"}podIP: {.status.podIP}{"\n"}startTime: {.status.startTime}{"\n"}containerID: {.status.containerStatuses[0].containerID}{"\n"}imageID: {.status.containerStatuses[0].imageID}{"\n"}conditions: {range .status.conditions[*]}{.type}={.status} {end}{"\n"}'
[ "$("$KUBECTL" get pod web -o jsonpath='{.status.hostIP}')" = 10.87.0.1 ] || fail "hostIP is not 10.87.0.1"
case "$("$KUBECTL" get pod web -o jsonpath='{.status.containerStatuses[0].containerID}')" in
hull://*) ;;
*) fail "containerID is not hull://" ;;
esac
notready=$("$KUBECTL" get pod web -o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}' | grep -v '=True$' || true)
[ -z "$notready" ] || fail "conditions not True: $notready"
echo "VM:"
hull_ps

step "kubectl logs web"
"$KUBECTL" logs web | tee "$TMP/web.log"
grep -q '/docker-entrypoint.sh: Configuration complete; ready for start up' "$TMP/web.log" ||
	fail "nginx entrypoint lines missing from kubectl logs"
echo "--- kubectl logs web --tail=1"
"$KUBECTL" logs web --tail=1

step "kubectl exec web -- cat /etc/hostname"
h=$("$KUBECTL" exec web -- cat /etc/hostname)
echo "$h"
[ "$h" = web ] || fail "hostname is $h, not web"

step "kubectl exec web -- sh -c 'exit 3'"
rc=0
"$KUBECTL" exec web -- sh -c 'exit 3' || rc=$?
echo "exit code: $rc"
[ "$rc" = 3 ] || fail "exec exit code $rc, not 3"

step "kubectl exec -i: 10000 bytes of stdin, container env"
n=$(head -c 10000 /dev/zero | tr '\0' a | "$KUBECTL" exec -i web -- wc -c | tr -d ' ')
echo "guest read $n bytes"
[ "$n" = 10000 ] || fail "guest read $n bytes, not 10000"
"$KUBECTL" exec web -- sh -c 'echo HOSTNAME=$HOSTNAME NGINX_VERSION=$NGINX_VERSION'

step "reach the pod from the Mac through the gateway API"
code=$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "$GW_API" "http://gateway/probe/tcp?addr=$IP:80")
echo "GET /probe/tcp?addr=$IP:80 -> $code"
[ "$code" = 200 ] || fail "probe returned $code"
code=$(curl -s -o "$TMP/fwd.json" -w '%{http_code}' --unix-socket "$GW_API" -X POST \
	-d "{\"protocol\":\"tcp\",\"local\":\"127.0.0.1:$FWD_PORT\",\"remote\":\"$IP:80\"}" http://gateway/forwards)
echo "POST /forwards 127.0.0.1:$FWD_PORT -> $IP:80 -> $code $(cat "$TMP/fwd.json")"
[ "$code" = 201 ] || fail "forward returned $code"
title=$(curl -s -m 5 "http://127.0.0.1:$FWD_PORT/" | grep -o '<title>[^<]*</title>' || true)
echo "curl http://127.0.0.1:$FWD_PORT/ -> $title"
[ "$title" = '<title>Welcome to nginx!</title>' ] || fail "nginx page not served through the forward"
code=$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "$GW_API" -X DELETE \
	"http://gateway/forwards?protocol=tcp&local=127.0.0.1:$FWD_PORT")
echo "DELETE /forwards 127.0.0.1:$FWD_PORT -> $code"
if curl -s -m 3 -o /dev/null "http://127.0.0.1:$FWD_PORT/"; then
	fail "forward still answers after DELETE"
fi
echo "forward withdrawn: 127.0.0.1:$FWD_PORT refuses"

step "kubectl run bb --image=busybox --restart=Never -- sh -c 'sleep 2'"
"$KUBECTL" run bb --image=busybox --restart=Never -- sh -c 'sleep 2'
wait_phase bb Succeeded 120
"$KUBECTL" get pod bb -o jsonpath='phase: {.status.phase}{"\n"}state: {.status.containerStatuses[0].state}{"\n"}'

step "kubectl run bad with hostNetwork: true"
"$KUBECTL" run bad --image=busybox --restart=Never --overrides='{"spec":{"hostNetwork":true}}' -- sleep 1
wait_phase bad Failed 30
"$KUBECTL" get pod bad -o jsonpath='phase: {.status.phase}{"\n"}reason: {.status.reason}{"\n"}message: {.status.message}{"\n"}'
ev=""
for _ in $(seq 1 20); do
	ev=$("$KUBECTL" get events --field-selector involvedObject.name=bad,reason=Unsupported -o jsonpath='{.items[*].message}')
	[ -n "$ev" ] && break
	sleep 0.5
done
echo "event Unsupported: $ev"
[ -n "$ev" ] || fail "no Unsupported event for pod bad"

step "kubelet pod endpoints admit only the apiserver"
out=$(curl -sS --cacert "$PKI/ca.crt" --cert "$PKI/apiserver-kubelet-client.crt" --key "$PKI/apiserver-kubelet-client.key" \
	https://127.0.0.1:10250/pods)
echo "apiserver-kubelet-client GET /pods: $(echo "$out" | python3 -c 'import sys,json; print(sorted(p["metadata"]["name"] for p in json.load(sys.stdin)["items"]))')"
for who in admin node; do
	code=$(curl -s -o "$TMP/deny.txt" -w '%{http_code}' --cacert "$PKI/ca.crt" --cert "$PKI/$who.crt" --key "$PKI/$who.key" \
		"https://127.0.0.1:10250/exec/default/web/web?command=id&output=1")
	echo "$who cert GET /exec -> $code $(cat "$TMP/deny.txt")"
	[ "$code" = 403 ] || fail "$who cert reached /exec with $code"
	code=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$PKI/ca.crt" --cert "$PKI/$who.crt" --key "$PKI/$who.key" \
		"https://127.0.0.1:10250/containerLogs/default/web/web")
	echo "$who cert GET /containerLogs -> $code"
	[ "$code" = 403 ] || fail "$who cert reached /containerLogs with $code"
done

step "kubectl delete pod web bb bad"
t0=$(now)
"$KUBECTL" delete pod web bb bad
echo "measured delete wall time: $(perl -e "printf '%.2f', $(now) - $t0")s"
"$KUBECTL" get pods
ps_out=$(hull_ps)
echo "hull ps: $ps_out"
[ "$ps_out" = "No instances found" ] || fail "VMs left after delete"
echo "ipam.json: $(tr -d ' \n' <"$STATE/ipam.json")"
[ "$(tr -d ' \n' <"$STATE/ipam.json")" = "[]" ] || fail "addresses left allocated"

step "briglet down removes the VMs; up brings the bound pod back"
t0=$(now)
"$KUBECTL" run keep --image=busybox -- sh -c 'while true; do sleep 5; done'
wait_phase keep Running 120
echo "measured pod create -> Running with podIP, image cached: $(perl -e "printf '%.2f', $(now) - $t0")s"
hull_ps
"$BRIGLET" down
ps_out=$(hull_ps)
echo "hull ps after down: $ps_out"
[ "$ps_out" = "No instances found" ] || fail "VMs left after briglet down"
"$BRIGLET" up
# The status in etcd still says Running from before. Wait for the agent to
# see the VM gone and boot a new one.
for i in $(seq 1 120); do
	rs=$("$KUBECTL" get pod keep -o jsonpath='{.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].state.running.startedAt}')
	case "$rs" in
	"1 "?*) break ;;
	esac
	[ "$i" = 120 ] && fail "keep did not restart after up: $rs"
	sleep 1
done
"$KUBECTL" get pod keep -o wide
hull_ps
"$KUBECTL" delete pod keep
ps_out=$(hull_ps)
echo "hull ps: $ps_out"
[ "$ps_out" = "No instances found" ] || fail "VMs left after delete"

step "briglet status"
"$BRIGLET" status

printf '\npods PASS\n'
