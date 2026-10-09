#!/usr/bin/env bash
# volumes smoke test: from a wiped state dir, run a 2-replica nginx Deployment
# that serves an index.html from a ConfigMap and is gated by httpGet
# readiness and liveness probes, read the page from the Mac through the
# agent's gateway forward, update the ConfigMap and see the page change,
# replace a deleted replica, run a pod that reads a Secret through env and a
# volume, a downwardAPI volume and the service account token, keep the
# Secret value off the host's process list and the agent log, restart a pod
# whose liveness exec probe fails, and clean up.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BRIGLET="$ROOT/bin/briglet"
KUBECTL=${KUBECTL:-kubectl}
export HULL_BIN=${HULL_BIN:-hull}
STATE=${BRIGLET_STATE_DIR:-$HOME/.briglet}
GW_API="$STATE/gw.sock.api"
export KUBECONFIG="$STATE/admin.kubeconfig"
SECRET_VALUE="m2-s3cr3t-$$"
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
# CoreDNS runs in kube-system on every cluster since services. The checks below
# count only the VMs, addresses and forwards of the other pods.
default_vms() { hull_ps | awk 'NR>1 && $1 !~ /^kube-system_/ {print $1}'; }
default_addrs() {
	python3 -c 'import json, sys; print(sum(1 for a in json.load(open(sys.argv[1])) if not a["pod"].startswith("kube-system/")))' "$STATE/ipam.json"
}
gw() { curl -sS --unix-socket "$GW_API" "$@"; }

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

ready_replicas() { [ "$("$KUBECTL" get deploy web -o jsonpath='{.status.readyReplicas}')" = "$1" ]; }
pod_ready() { [ "$("$KUBECTL" get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]; }
web_pods() { "$KUBECTL" get pods -l app=web -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'; }

# forward_of IP PORT: the 127.0.0.1 address the agent's gateway forward of
# IP:PORT listens on.
forward_of() {
	gw http://gateway/forwards | python3 -c '
import json, sys
remote = sys.argv[1]
for f in json.load(sys.stdin):
    if f["remote"] == remote:
        print(f["local"])
        break
' "$1:$2"
}

step "build"
(cd "$ROOT" && go build -o bin/briglet ./cmd/briglet)
echo "built $BRIGLET"

step "briglet down --wipe"
"$BRIGLET" down --wipe

step "briglet up"
"$BRIGLET" up

step "ConfigMap web-content and a 2-replica nginx Deployment with probes"
cat >"$TMP/web.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: web-content
data:
  index.html: |
    <h1>briglet volumes: served from ConfigMap web-content, version 1</h1>
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 2
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        ports:
        - {name: http, containerPort: 80}
        volumeMounts:
        - {name: content, mountPath: /usr/share/nginx/html}
        readinessProbe:
          httpGet: {path: /, port: http}
          periodSeconds: 5
        livenessProbe:
          httpGet: {path: /, port: 80, httpHeaders: [{name: X-Probe, value: liveness}]}
          initialDelaySeconds: 5
          periodSeconds: 10
      volumes:
      - name: content
        configMap: {name: web-content}
EOF
cat "$TMP/web.yaml"
t0=$(now)
"$KUBECTL" apply -f "$TMP/web.yaml"
until_true 300 "Deployment web 2/2 Ready" ready_replicas 2
echo "measured Deployment apply -> 2/2 Ready: $(since "$t0")s (includes the nginx:alpine pull)"
"$KUBECTL" get deploy,rs -o wide
"$KUBECTL" get pods -o wide
POD1=$(web_pods | head -1)
IP1=$("$KUBECTL" get pod "$POD1" -o jsonpath='{.status.podIP}')
"$KUBECTL" get pod "$POD1" -o jsonpath='ready: {.status.containerStatuses[0].ready}{"\n"}started: {.status.containerStatuses[0].started}{"\n"}conditions: {range .status.conditions[*]}{.type}={.status} {end}{"\n"}'
echo "--- the mounted volume in $POD1"
"$KUBECTL" exec "$POD1" -- ls -la /usr/share/nginx/html
"$KUBECTL" exec "$POD1" -- sh -c 'mount | grep /usr/share/nginx/html'

step "read the mounted page from the Mac through the agent's gateway forward"
echo "GET /forwards:"
gw http://gateway/forwards
echo
LOCAL=$(forward_of "$IP1" 80)
[ -n "$LOCAL" ] || fail "no gateway forward to $IP1:80"
page=$(curl -sS -m 5 "http://$LOCAL/")
echo "curl http://$LOCAL/ ($POD1, $IP1:80) -> $page"
[ "$page" = "<h1>briglet volumes: served from ConfigMap web-content, version 1</h1>" ] || fail "page is not the ConfigMap content"
# nginx logs to /dev/stdout. hull's guest /dev has no stdout link until the
# agent's post-boot exec adds one, so an nginx that opened its log first
# wrote a regular file /dev/stdout instead of the console. Read both.
echo "--- probe requests in the nginx access log of $POD1"
{
	"$KUBECTL" logs "$POD1"
	"$KUBECTL" exec "$POD1" -- sh -c '[ -f /dev/stdout ] && cat /dev/stdout' || true
} | grep 'kube-probe' >"$TMP/probe.log" || true
tail -3 "$TMP/probe.log"
grep -q '"GET / HTTP/1.1" 200 .*"kube-probe/1.37"' "$TMP/probe.log" || fail "no probe request in the nginx access log"

step "a readiness probe on a closed port keeps a pod Running but not Ready"
cat >"$TMP/notready.yaml" <<'EOF2'
apiVersion: v1
kind: Pod
metadata:
  name: notready
spec:
  automountServiceAccountToken: false
  containers:
  - name: nginx
    image: nginx:alpine
    readinessProbe:
      httpGet: {path: /, port: 81}
      periodSeconds: 5
EOF2
"$KUBECTL" apply -f "$TMP/notready.yaml"
unhealthy() { "$KUBECTL" get events --field-selector involvedObject.name=notready,reason=Unhealthy -o name | grep -q .; }
until_true 120 "an Unhealthy event for notready" unhealthy
"$KUBECTL" get pod notready -o wide
"$KUBECTL" get pod notready -o jsonpath='phase: {.status.phase}{"\n"}ready: {.status.containerStatuses[0].ready}{"\n"}conditions: {range .status.conditions[*]}{.type}={.status} {end}{"\n"}'
"$KUBECTL" get events --field-selector involvedObject.name=notready,reason=Unhealthy -o custom-columns=REASON:.reason,COUNT:.count,MESSAGE:.message
[ "$("$KUBECTL" get pod notready -o jsonpath='{.status.phase}/{.status.containerStatuses[0].ready}')" = "Running/false" ] || fail "notready is not Running and unready"
if "$KUBECTL" exec notready -- ls /var/run/secrets/kubernetes.io/serviceaccount 2>/dev/null; then
	fail "automountServiceAccountToken: false still got a token"
fi
echo "automountServiceAccountToken: false -> no /var/run/secrets/kubernetes.io/serviceaccount in notready"
"$KUBECTL" delete pod notready

step "update the ConfigMap; the page follows"
t0=$(now)
"$KUBECTL" create configmap web-content --dry-run=client -o yaml \
	--from-literal=index.html='<h1>briglet volumes: served from ConfigMap web-content, version 2</h1>' | "$KUBECTL" apply -f -
page_is_v2() { curl -sS -m 5 "http://$LOCAL/" | grep -q 'version 2'; }
until_true 150 "the page shows version 2" page_is_v2
echo "measured ConfigMap update -> new page served: $(since "$t0")s"
echo "curl http://$LOCAL/ -> $(curl -sS -m 5 "http://$LOCAL/")"
"$KUBECTL" exec "$POD1" -- ls -la /usr/share/nginx/html

step "kubectl delete pod $POD1; the Deployment replaces it"
t0=$(now)
"$KUBECTL" delete pod "$POD1" --wait=false
replaced() {
	local pods
	pods=$(web_pods)
	! echo "$pods" | grep -qx "$POD1" && [ "$(echo "$pods" | grep -c .)" = 2 ] && ready_replicas 2
}
until_true 180 "a ready replacement for $POD1" replaced
echo "measured delete -> replacement Ready: $(since "$t0")s"
"$KUBECTL" get pods -o wide
hull_ps

step "a Service and a pod that reads a Secret, a downwardAPI volume and its token"
"$KUBECTL" expose deployment web --port 80
"$KUBECTL" create secret generic creds --from-literal=password="$SECRET_VALUE" --from-literal=user=admin
cat >"$TMP/reader.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: reader
  labels: {app: reader, tier: m2}
spec:
  containers:
  - name: reader
    image: busybox
    command: [sh, -c, 'while true; do sleep 5; done']
    resources:
      limits: {memory: 256Mi}
    env:
    - name: FROM_SECRET
      valueFrom: {secretKeyRef: {name: creds, key: password}}
    - name: FROM_CONFIGMAP
      valueFrom: {configMapKeyRef: {name: web-content, key: index.html}}
    - name: MY_NAME
      valueFrom: {fieldRef: {fieldPath: metadata.name}}
    - name: MY_IP
      valueFrom: {fieldRef: {fieldPath: status.podIP}}
    - name: MEM_MI
      valueFrom: {resourceFieldRef: {resource: limits.memory, divisor: 1Mi}}
    envFrom:
    - {prefix: CREDS_, secretRef: {name: creds}}
    volumeMounts:
    - {name: creds, mountPath: /etc/creds, readOnly: true}
    - {name: podinfo, mountPath: /etc/podinfo}
    - {name: scratch, mountPath: /scratch}
    readinessProbe:
      exec: {command: [sh, -c, 'test "$FROM_SECRET" != ""']}
      periodSeconds: 5
  volumes:
  - name: creds
    secret: {secretName: creds, defaultMode: 0400}
  - name: podinfo
    downwardAPI:
      items:
      - {path: name, fieldRef: {fieldPath: metadata.name}}
      - {path: labels, fieldRef: {fieldPath: metadata.labels}}
  - name: scratch
    emptyDir: {}
EOF
"$KUBECTL" apply -f "$TMP/reader.yaml"
until_true 180 "pod reader Ready" pod_ready reader
"$KUBECTL" get pod reader -o wide

echo "--- kubectl exec reader -- sh -c 'echo \$FROM_SECRET'"
got=$("$KUBECTL" exec reader -- sh -c 'echo $FROM_SECRET')
echo "$got"
[ "$got" = "$SECRET_VALUE" ] || fail "FROM_SECRET is $got"
echo "--- env from envFrom, configMapKeyRef, fieldRef, resourceFieldRef"
"$KUBECTL" exec reader -- sh -c 'echo CREDS_user=$CREDS_user; echo FROM_CONFIGMAP=$FROM_CONFIGMAP; echo MY_NAME=$MY_NAME MY_IP=$MY_IP MEM_MI=$MEM_MI'
[ "$("$KUBECTL" exec reader -- sh -c 'echo $CREDS_user $MY_NAME $MEM_MI')" = "admin reader 256" ] || fail "envFrom, fieldRef or resourceFieldRef wrong"
echo "--- Service links"
"$KUBECTL" exec reader -- sh -c 'env | grep -E "^(KUBERNETES|WEB)_" | sort'
[ "$("$KUBECTL" exec reader -- sh -c 'echo $KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT')" = "10.96.0.1:443" ] || fail "KUBERNETES_SERVICE_HOST/PORT wrong"
[ "$("$KUBECTL" exec reader -- sh -c 'echo $WEB_SERVICE_HOST')" = "$("$KUBECTL" get svc web -o jsonpath='{.spec.clusterIP}')" ] || fail "WEB_SERVICE_HOST is not the web ClusterIP"

echo "--- kubectl exec reader -- cat /etc/creds/password"
got=$("$KUBECTL" exec reader -- cat /etc/creds/password)
echo "$got"
[ "$got" = "$SECRET_VALUE" ] || fail "secret volume file is $got"
"$KUBECTL" exec reader -- ls -la /etc/creds /etc/creds/..data/
"$KUBECTL" exec reader -- sh -c 'echo x > /etc/creds/new' && fail "the secret volume is writable" || echo "write to /etc/creds refused (read-only share)"

echo "--- downwardAPI volume"
"$KUBECTL" exec reader -- sh -c 'echo "name: $(cat /etc/podinfo/name)"; echo "labels:"; cat /etc/podinfo/labels; echo'
[ "$("$KUBECTL" exec reader -- cat /etc/podinfo/name)" = reader ] || fail "downwardAPI name is wrong"
"$KUBECTL" label pod reader extra=added
label_seen() { "$KUBECTL" exec reader -- cat /etc/podinfo/labels | grep -q 'extra="added"'; }
t0=$(now)
until_true 120 "the label in /etc/podinfo/labels" label_seen
echo "new label visible in the guest after $(since "$t0")s"

echo "--- emptyDir is writable"
"$KUBECTL" exec reader -- sh -c 'echo hello > /scratch/f && cat /scratch/f'

step "the service account token at the standard path"
SA=/var/run/secrets/kubernetes.io/serviceaccount
"$KUBECTL" exec reader -- ls -la "$SA"
"$KUBECTL" get pod reader -o jsonpath='{range .spec.volumes[*]}{.name}{" "}{end}{"\n"}'
[ "$("$KUBECTL" exec reader -- cat "$SA/namespace")" = default ] || fail "namespace file is wrong"
"$KUBECTL" exec reader -- cat "$SA/ca.crt" >"$TMP/ca.crt"
cmp "$TMP/ca.crt" "$STATE/pki/ca.crt" || fail "ca.crt in the pod is not the cluster CA"
echo "ca.crt in the pod equals $STATE/pki/ca.crt"
"$KUBECTL" exec reader -- cat "$SA/token" >"$TMP/token"
python3 - "$TMP/token" <<'EOF'
import base64, json, sys, time
parts = open(sys.argv[1]).read().strip().split(".")
assert len(parts) == 3, "token is not a JWT"
claims = json.loads(base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4)))
k8s = claims["kubernetes.io"]
print("sub:", claims["sub"])
print("aud:", claims["aud"])
print("bound pod:", k8s["pod"]["name"], k8s["pod"]["uid"])
print("expires in: %d s" % (claims["exp"] - time.time()))
assert claims["sub"] == "system:serviceaccount:default:default"
assert k8s["pod"]["name"] == "reader"
EOF
code=$(curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(cat "$TMP/token")" https://127.0.0.1:6443/api/v1/namespaces/default/pods/reader)
echo "the token authenticates to the apiserver: GET pods/reader -> $code (403 is RBAC denying an authenticated service account)"
[ "$code" = 403 ] || fail "token rejected with $code"
code=$(curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer not-a-token" https://127.0.0.1:6443/api/v1/namespaces/default/pods/reader)
echo "a forged token: GET pods/reader -> $code"
[ "$code" = 401 ] || fail "forged token got $code"

step "the Secret value stays off the host's process list and the agent log"
n=$(ps -axww -o command= | grep -c -- "$SECRET_VALUE" || true)
echo "processes whose command line holds the value: $((n - 1)) (the grep itself excluded)"
[ "$n" -le 1 ] || fail "the Secret value is on a command line"
if grep -q -- "$SECRET_VALUE" "$STATE/logs/node.log"; then
	fail "the Secret value is in node.log"
fi
inst=$("$KUBECTL" get pod reader -o jsonpath='{.status.containerStatuses[0].containerID}')
inst=${inst#hull://}
if "$HULL_BIN" --unattended --dnt --store-dir "$STATE/hull-store" inspect "$inst" | grep -q -- "$SECRET_VALUE"; then
	fail "the Secret value is in hull inspect $inst"
fi
echo "hull inspect $inst does not hold the value"
grep -- "hull run default_reader" "$STATE/logs/node.log" | tail -1 | sed 's/shares .*//'
echo "node.log does not hold the value"

step "a liveness exec probe that starts failing after 10 s restarts the container"
cat >"$TMP/liveness.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: liveness
spec:
  containers:
  - name: liveness
    image: busybox
    args: [/bin/sh, -c, 'touch /tmp/healthy; sleep 10; rm -f /tmp/healthy; sleep 600']
    livenessProbe:
      exec: {command: [cat, /tmp/healthy]}
      initialDelaySeconds: 5
      periodSeconds: 5
EOF
t0=$(now)
"$KUBECTL" apply -f "$TMP/liveness.yaml"
restarted() { [ "$("$KUBECTL" get pod liveness -o jsonpath='{.status.containerStatuses[0].restartCount}')" -ge 1 ]; }
until_true 90 "liveness restartCount >= 1" restarted
echo "measured create -> restartCount 1: $(since "$t0")s"
"$KUBECTL" get pod liveness -o jsonpath='restartCount: {.status.containerStatuses[0].restartCount}{"\n"}lastState: {.status.containerStatuses[0].lastState}{"\n"}'
echo
"$KUBECTL" get events --field-selector involvedObject.name=liveness --sort-by=.lastTimestamp \
	-o custom-columns=TYPE:.type,REASON:.reason,COUNT:.count,MESSAGE:.message

step "cleanup"
"$KUBECTL" delete deployment web
"$KUBECTL" delete pod reader liveness
"$KUBECTL" delete service web
"$KUBECTL" delete secret creds
"$KUBECTL" delete configmap web-content
no_web_pods() { [ -z "$(web_pods)" ]; }
until_true 60 "web pods gone" no_web_pods
"$KUBECTL" get pods
ps_out=$(hull_ps)
echo "hull ps: $ps_out"
[ -z "$(default_vms)" ] || fail "VMs left after delete"
echo "ipam.json: $(tr -d ' \n' <"$STATE/ipam.json")"
[ "$(default_addrs)" = 0 ] || fail "addresses left allocated"
fw=$(gw http://gateway/forwards | tr -d ' \n')
echo "GET /forwards: $fw"
left=$(gw http://gateway/forwards | python3 -c '
import json, sys
keep = {a["addr"] for a in json.load(open(sys.argv[1])) if a["pod"].startswith("kube-system/")}
print(sum(1 for f in (json.load(sys.stdin) or []) if f["remote"].rsplit(":", 1)[0] not in keep))
' "$STATE/ipam.json")
[ "$left" = 0 ] || fail "forwards left behind"
echo "pod dirs: $(ls "$STATE/pods" | wc -l | tr -d ' ')"

step "briglet status"
"$BRIGLET" status

printf '\nvolumes PASS\n'
