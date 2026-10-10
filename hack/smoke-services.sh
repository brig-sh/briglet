#!/usr/bin/env bash
# services smoke test: from a wiped state dir, bring the cluster up with a hull
# whose gateway routes Service addresses, and check that CoreDNS runs and is
# Ready, the `kubernetes` Service maps to the apiserver on the host, a pod
# resolves Service names and reaches a ClusterIP Service whose two replicas
# both answer, a pod reaches the apiserver with its service account token,
# a NodePort answers on the Mac, the table follows a scale-down, and a
# deleted Service leaves the table and the forwards.
#
# Telling the replicas apart: each nginx replica writes $HOSTNAME (the pod
# name) to /usr/share/nginx/html/host.txt before it starts nginx, so
# `wget http://web/` still returns the stock nginx page and
# `wget http://web/host.txt` names the replica that answered.
#
# Three client pods: `client` runs the current busybox for wget, HTTPS
# included. `dflt` has dnsPolicy Default, as CoreDNS does. `dns` runs busybox:1.28 for nslookup: newer busybox nslookup
# applies the search list only to names without a dot, so
# `nslookup kubernetes.default` fails there on any cluster, which is why
# the Kubernetes DNS debugging docs pin 1.28. 1.28's wget cannot do TLS
# with the apiserver's ECDSA certificate.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BRIGLET="$ROOT/bin/briglet"
KUBECTL=${KUBECTL:-kubectl}
export HULL_BIN=${HULL_BIN:-hull}
STATE=${BRIGLET_STATE_DIR:-$HOME/.briglet}
GW_API="$STATE/gw.sock.api"
export KUBECONFIG="$STATE/admin.kubeconfig"
NODE_PORT=30080
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}
step() { printf '\n## %s\n' "$*"; }
now() { perl -MTime::HiRes=time -e 'printf "%.2f", time'; }
since() { perl -e "printf '%.2f', $(now) - $1"; }
elapsed_over() { perl -e "exit(!($(now) - $1 > $2))"; }
hull_ps() { "$HULL_BIN" --unattended --dnt --store-dir "$STATE/hull-store" ps; }
gw() { curl -sS --unix-socket "$GW_API" "$@"; }
k() { "$KUBECTL" "$@"; }
kx() { "$KUBECTL" exec "$@"; }

# until_true TIMEOUT WHAT CMD...: run CMD every 0.5 s until it succeeds.
until_true() {
	local timeout=$1 what=$2 t0
	shift 2
	t0=$(now)
	until "$@" >/dev/null 2>&1; do
		if elapsed_over "$t0" "$timeout"; then
			fail "$what: not true after ${timeout}s"
		fi
		sleep 0.5
	done
}

# entry VIP PORT PROTO: the gateway's table entry, as JSON, or "absent".
entry() {
	gw http://gateway/services | python3 -c '
import json, sys
vip, port, proto = sys.argv[1], int(sys.argv[2]), sys.argv[3]
for s in json.load(sys.stdin):
    if s["vip"] == vip and s["port"] == port and s["protocol"] == proto:
        print(json.dumps(s, separators=(",", ":")))
        break
else:
    print("absent")
' "$@"
}
endpoints_of() { entry "$@" | python3 -c 'import json, sys; l = sys.stdin.read().strip(); print("absent" if l == "absent" else len(json.loads(l)["endpoints"]))'; }
has_endpoints() { [ "$(endpoints_of "$1" "$2" "$3")" = "$4" ]; }
entry_absent() { [ "$(entry "$1" "$2" "$3")" = absent ]; }
coredns_ready() { [ "$(k -n kube-system get deploy coredns -o jsonpath='{.status.readyReplicas}')" = 1 ]; }
node_port_forward() { gw http://gateway/forwards | python3 -c '
import json, sys
for f in json.load(sys.stdin) or []:
    if f["local"].endswith(":" + sys.argv[1]):
        print(json.dumps(f, separators=(",", ":")))
' "$1"; }
no_node_port_forward() { [ -z "$(node_port_forward "$1")" ]; }
web_pods() { k get pods -l app=web -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'; }
one_web_pod() { [ "$(web_pods | wc -l | tr -d ' ')" = 1 ]; }

step "build"
(cd "$ROOT" && go build -o bin/briglet ./cmd/briglet)
echo "built $BRIGLET"

step "briglet down --wipe"
"$BRIGLET" down --wipe

step "HULL_BIN=$HULL_BIN briglet up"
"$HULL_BIN" --version
t_up=$(now)
"$BRIGLET" up
echo "measured briglet up: $(since "$t_up")s"
gwpid=$(head -1 "$STATE/run/gateway.pid")
args=$(ps -p "$gwpid" -o args=)
echo "gateway: ${args#*network-gateway }"
case "$args" in *"--service-cidr 10.96.0.0/12"*) ;; *) fail "gateway runs without --service-cidr" ;; esac

step "CoreDNS Running and Ready"
until_true 240 "CoreDNS Ready" coredns_ready
echo "measured briglet up start -> CoreDNS Ready: $(since "$t_up")s (image pulled in the run)"
k -n kube-system get deploy,pods,svc -o wide
k -n kube-system get pods -l k8s-app=kube-dns -o jsonpath='{range .items[*]}{.metadata.name} phase={.status.phase} ready={.status.conditions[?(@.type=="Ready")].status} image={.status.containerStatuses[0].image}{"\n"}{end}'
k -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}'
echo
dns_svc=$(k -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP} {range .spec.ports[*]}{.port}/{.protocol} {end}')
echo "kube-dns: $dns_svc"
[ "$dns_svc" = "10.96.0.10 53/UDP 53/TCP 9153/TCP " ] || fail "kube-dns Service: $dns_svc"
# The EndpointSlice update reaches the table up to a debounce window after
# the Deployment turns Ready.
until_true 10 "10.96.0.10:53/udp endpoint" has_endpoints 10.96.0.10 53 udp 1
echo "table: $(entry 10.96.0.10 53 udp)"

step "the kubernetes Service maps to the apiserver on the host"
k get endpointslice -l kubernetes.io/service-name=kubernetes -o jsonpath='{range .items[*]}EndpointSlice {.metadata.name}: {.endpoints[*].addresses[*]} port {.ports[*].name}={.ports[*].port}{"\n"}{end}'
api=$(entry 10.96.0.1 443 tcp)
echo "table: $api"
[ "$api" = '{"vip":"10.96.0.1","port":443,"protocol":"tcp","endpoints":[{"ip":"127.0.0.1","port":6443,"host":true}]}' ] ||
	fail "kubernetes entry: $api"

step "Deployment web (2 nginx:alpine), ClusterIP Service web, NodePort Service web-np, clients"
cat >"$TMP/web.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        command: ["/bin/sh", "-c", "echo \"\$HOSTNAME\" > /usr/share/nginx/html/host.txt && exec nginx -g 'daemon off;'"]
        ports: [{containerPort: 80}]
        readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 2}
---
apiVersion: v1
kind: Service
metadata: {name: web}
spec:
  selector: {app: web}
  ports: [{port: 80, targetPort: 80}]
---
apiVersion: v1
kind: Service
metadata: {name: web-np}
spec:
  type: NodePort
  selector: {app: web}
  ports: [{port: 8080, targetPort: 80, nodePort: $NODE_PORT}]
---
apiVersion: v1
kind: Pod
metadata: {name: client}
spec:
  containers: [{name: client, image: busybox, command: [sleep, "3600"]}]
---
apiVersion: v1
kind: Pod
metadata: {name: dns}
spec:
  containers: [{name: dns, image: "busybox:1.28", command: [sleep, "3600"]}]
---
apiVersion: v1
kind: Pod
metadata: {name: dflt}
spec:
  dnsPolicy: Default
  containers: [{name: dflt, image: busybox, command: [sleep, "3600"]}]
EOF
t0=$(now)
k apply -f "$TMP/web.yaml"
k rollout status deploy/web --timeout=180s
k wait --for=condition=Ready pod/client pod/dns pod/dflt --timeout=180s
echo "measured apply -> web 2/2 Ready and the three clients Ready: $(since "$t0")s"
k get pods,svc -o wide
WEB_IP=$(k get svc web -o jsonpath='{.spec.clusterIP}')
NP_IP=$(k get svc web-np -o jsonpath='{.spec.clusterIP}')
until_true 10 "web entry with 2 endpoints" has_endpoints "$WEB_IP" 80 tcp 2
echo "GET /services:"
gw http://gateway/services | python3 -c 'import json, sys; [print(" ", json.dumps(s, separators=(",", ":"))) for s in json.load(sys.stdin)]'

step "pod resolv.conf"
rc=$(kx client -- cat /etc/resolv.conf)
echo "$rc"
[ "$rc" = "nameserver 10.96.0.10
search default.svc.cluster.local svc.cluster.local cluster.local
options ndots:5" ] || fail "client resolv.conf"
echo "dflt (dnsPolicy Default, as CoreDNS) keeps the gateway resolver:"
rc=$(kx dflt -- cat /etc/resolv.conf)
echo "$rc"
[ "$rc" = "nameserver 10.87.0.1" ] || fail "dflt resolv.conf"
echo "CoreDNS has no shell to exec; the supervisor bound these files from its pod directory:"
dns_pod=$(k -n kube-system get pod -l k8s-app=kube-dns -o jsonpath='{.items[0].metadata.name}')
dns_uid=$(k -n kube-system get pod "$dns_pod" -o jsonpath='{.metadata.uid}')
for f in hostname resolv.conf; do
	printf '%s: %s\n' "$f" "$(tr '\n' ' ' <"$STATE/pods/$dns_uid/guest/etc/$f")"
done
[ "$(cat "$STATE/pods/$dns_uid/guest/etc/hostname")" = "$dns_pod" ] || fail "CoreDNS hostname file"

step "nslookup web (dns pod, busybox:1.28)"
out=$(kx dns -- nslookup web)
echo "$out"
echo "$out" | grep -q "Address 1: $WEB_IP web.default.svc.cluster.local" || fail "nslookup web"
case "$WEB_IP" in 10.9[6-9].* | 10.10[0-9].* | 10.11[01].*) ;; *) fail "web cluster IP $WEB_IP is outside 10.96.0.0/12" ;; esac

step "nslookup kubernetes.default (dns pod, busybox:1.28)"
out=$(kx dns -- nslookup kubernetes.default)
echo "$out"
echo "$out" | grep -q "Address 1: 10.96.0.1 kubernetes.default.svc.cluster.local" || fail "nslookup kubernetes.default"

step "nslookup of an outside name goes upstream through the gateway"
out=$(kx dns -- nslookup registry.k8s.io)
echo "$out"
echo "$out" | grep -q "^Name: *registry.k8s.io" || fail "nslookup registry.k8s.io"

step "wget -qO- http://web (client pod)"
page=$(kx client -- wget -qO- http://web)
echo "$page" | head -4
echo "$page" | grep -q "<title>Welcome to nginx!</title>" || fail "no nginx page through the Service"

step "10 requests to http://web/host.txt reach both replicas"
for i in 1 2 3 4 5 6 7 8 9 10; do kx client -- wget -qO- http://web/host.txt; done >"$TMP/hits"
sort "$TMP/hits" | uniq -c
[ "$(sort -u "$TMP/hits" | wc -l | tr -d ' ')" = 2 ] || fail "requests did not reach both replicas"
sort -u "$TMP/hits" >"$TMP/hit-names"
web_pods | sort | diff - "$TMP/hit-names" || fail "the answering pods are not the web pods"

step "the replicas see the gateway as the client address"
for p in $(web_pods); do
	line=$(k logs "$p" | grep 'GET /host.txt' | tail -1)
	echo "$p: $line"
	case "$line" in "10.87.0.1 "*Wget*) ;; *) fail "$p: unexpected access log line" ;; esac
done

step "https://kubernetes.default.svc/version with the service account token"
out=$(kx client -- sh -c 'wget -qO- --no-check-certificate --header "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" https://kubernetes.default.svc/version')
echo "$out"
echo "$out" | grep -q '"gitVersion": "v1.37.0"' || fail "no version JSON"

step "the token is the pod's identity; without it the apiserver refuses"
review='{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}'
out=$(kx client -- sh -c "wget -qO- --no-check-certificate --header \"Authorization: Bearer \$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)\" --header 'Content-Type: application/json' --post-data '$review' https://kubernetes.default.svc/apis/authentication.k8s.io/v1/selfsubjectreviews")
echo "$out" | python3 -c 'import json, sys; u = json.load(sys.stdin)["status"]["userInfo"]; print("with token:", u["username"], "pod", u["extra"]["authentication.kubernetes.io/pod-name"][0])'
echo "$out" | grep -q '"username": *"system:serviceaccount:default:default"' || fail "token identity"
if out=$(kx client -- sh -c "wget -qO- --no-check-certificate --header 'Content-Type: application/json' --post-data '$review' https://kubernetes.default.svc/apis/authentication.k8s.io/v1/selfsubjectreviews" 2>&1); then
	fail "anonymous SelfSubjectReview succeeded: $out"
fi
echo "without token: $out"
echo "$out" | grep -q "403 Forbidden" || fail "anonymous request was not refused with 403"

step "NodePort Service web-np answers on the Mac at 127.0.0.1:$NODE_PORT"
fwd=$(node_port_forward $NODE_PORT)
echo "forward: $fwd"
[ "$fwd" = "{\"protocol\":\"tcp\",\"local\":\"0.0.0.0:$NODE_PORT\",\"remote\":\"$NP_IP:8080\"}" ] || fail "node port forward: $fwd"
curl -sS -m 5 "http://127.0.0.1:$NODE_PORT/" | grep "<title>" || fail "NodePort page"
for i in 1 2 3 4 5 6; do curl -sS -m 5 "http://127.0.0.1:$NODE_PORT/host.txt"; done | sort | uniq -c

step "scale web to 1; the table shrinks to one endpoint"
t0=$(now)
k scale deploy/web --replicas=1
until_true 60 "web entry with 1 endpoint" has_endpoints "$WEB_IP" 80 tcp 1
echo "measured scale -> table at 1 endpoint: $(since "$t0")s"
echo "table: $(entry "$WEB_IP" 80 tcp)"
until_true 60 "one web pod" one_web_pod
k get pods -l app=web -o wide
left=$(web_pods)
for i in 1 2 3 4; do kx client -- wget -qO- http://web/host.txt; done >"$TMP/hits"
sort "$TMP/hits" | uniq -c
[ "$(sort -u "$TMP/hits")" = "$left" ] || fail "requests after scale-down did not all reach $left"
curl -sS -m 5 "http://127.0.0.1:$NODE_PORT/host.txt" | grep -qx "$left" || fail "NodePort after scale-down"

step "delete Services web and web-np; the entries and the node port go"
t0=$(now)
k delete svc web web-np
until_true 30 "web entry gone" entry_absent "$WEB_IP" 80 tcp
until_true 30 "web-np entry gone" entry_absent "$NP_IP" 8080 tcp
until_true 30 "node port forward gone" no_node_port_forward $NODE_PORT
echo "measured delete -> entries and forward gone: $(since "$t0")s"
gw http://gateway/services | python3 -c 'import json, sys; [print(" ", s["vip"], s["port"], s["protocol"], len(s["endpoints"]), "endpoint(s)") for s in json.load(sys.stdin)]'
if curl -sS -m 3 "http://127.0.0.1:$NODE_PORT/" >/dev/null 2>&1; then fail "node port still answers"; fi
echo "127.0.0.1:$NODE_PORT: refused"
if out=$(kx client -- wget -qO- -T 3 "http://$WEB_IP/" 2>&1); then fail "old cluster IP still answers"; fi
echo "wget http://$WEB_IP/ after delete: $out"

step "cleanup"
k delete deploy web
k delete pod client dns dflt --grace-period=1
until_true 60 "web pods gone" sh -c "[ -z \"\$($KUBECTL get pods -l app=web -o name)\" ]"
hull_ps
[ -z "$(hull_ps | awk 'NR>1 && $1 !~ /^kube-system_/')" ] || fail "VMs left after delete"

step "briglet status"
"$BRIGLET" status

printf '\nservices PASS\n'
