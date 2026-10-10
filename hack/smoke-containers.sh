#!/usr/bin/env bash
# containers smoke test: from a wiped state dir, run pods under the guest
# supervisor. Single-container pods reach Running (timed against pods) with
# the pod hostname, resolv.conf and /dev links in place before the first
# instruction. A pod's init container writes a page into an emptyDir that
# its main container serves. nginx with a busybox sidecar: kubectl logs -c
# and exec -c reach each container, tty exec works, and killing the
# sidecar restarts only the sidecar. A failing init container leaves the
# pod in Init:Error. A sidecar-style init container (restartPolicy Always)
# keeps running next to the main container. runAsUser 1000 runs as uid
# 1000. `exit 3` ends the pod Failed with exit code 3. Deleting the pods
# leaves no VM behind.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BRIGLET="$ROOT/bin/briglet"
KUBECTL=${KUBECTL:-kubectl}
export HULL_BIN=${HULL_BIN:-hull}
STATE=${BRIGLET_STATE_DIR:-$HOME/.briglet}
GW_API="$STATE/gw.sock.api"
export KUBECONFIG="$STATE/admin.kubeconfig"
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
hull() { "$HULL_BIN" --unattended --dnt --store-dir "$STATE/hull-store" "$@"; }
gw() { curl -sS --unix-socket "$GW_API" "$@"; }
k() { "$KUBECTL" "$@"; }
kx() { "$KUBECTL" exec "$@"; }
default_vms() { hull ps | awk 'NR>1 && $1 !~ /^kube-system_/ {print $1}'; }
instance_of() { hull ps | awk -v p="default_$1_" 'index($1, p) == 1 {print $1}'; }
# guest_ctl POD JSON: send a request to the pod's supervisor.
guest_ctl() { hull exec "$(instance_of "$1")" -- /run/briglet-bin/briglet-guest ctl "$2"; }
jp() { k get pod "$1" -o jsonpath="$2"; }
# cstat POD KIND NAME FIELD: a field of one container status, KIND being
# containerStatuses or initContainerStatuses.
cstat() { jp "$1" "{.status.$2[?(@.name==\"$3\")].$4}"; }

# until_true TIMEOUT WHAT CMD...: run CMD every 0.5 s until it succeeds.
until_true() {
	local timeout=$1 what=$2 t0
	shift 2
	t0=$(now)
	until "$@" >/dev/null 2>&1; do
		if elapsed_over "$t0" "$timeout"; then
			k get pods -o wide >&2 || true
			fail "$what: not true after ${timeout}s"
		fi
		sleep 0.5
	done
}
running() { [ -n "$(jp "$1" '{.status.podIP}')" ] && [ "$(jp "$1" '{.status.phase}')" = Running ] && [ -n "$(jp "$1" '{.status.containerStatuses[0].state.running.startedAt}')" ]; }
phase_is() { [ "$(jp "$1" '{.status.phase}')" = "$2" ]; }
ready() { [ "$(jp "$1" '{.status.conditions[?(@.type=="Ready")].status}')" = True ]; }
status_col() { k get pod "$1" --no-headers | awk '{print $3}'; }
forward_of() {
	gw http://gateway/forwards | python3 -c '
import json, sys
for f in json.load(sys.stdin):
    if f["remote"] == sys.argv[1]:
        print(f["local"])
        break
' "$1:$2"
}

step "build"
(cd "$ROOT" && go build -o bin/briglet ./cmd/briglet && make -s guest)
ls -l "$ROOT/bin/briglet" "$ROOT/bin/briglet-guest" | awk '{print $5, $9}'
file "$ROOT/bin/briglet-guest" | cut -d, -f1-2

step "briglet down --wipe"
"$BRIGLET" down --wipe

step "briglet up"
t0=$(now)
"$BRIGLET" up
echo "measured briglet up wall time: $(since "$t0")s"
ls -l "$STATE/guest-bin"

step "single-container pods: create -> Running, as in pods"
t0=$(now)
k run web --image=nginx:alpine --port=80
until_true 180 "web Running" running web
echo "measured nginx:alpine create -> Running with podIP: $(since "$t0")s (includes the nginx:alpine and busybox base pulls)"
t0=$(now)
k run keep --image=busybox -- sh -c 'while true; do sleep 5; done'
until_true 120 "keep Running" running keep
echo "measured busybox create -> Running with podIP, image cached: $(since "$t0")s"
k get pods web keep -o wide
inst=$(instance_of web)
hull inspect "$inst" | python3 -c 'import json, sys; i = json.load(sys.stdin); print("VM", i["id"], "image", i["cmdLine"][0].split("/")[-1], "status", i["status"])'
grep -h "hull run $inst" "$STATE/logs/node.log" | tail -1 | cut -c28-
h=$(kx web -- cat /etc/hostname)
echo "kubectl exec web -- cat /etc/hostname: $h"
[ "$h" = web ] || fail "hostname is $h"
echo "kubectl logs web --tail=2:"
k logs web --tail=2
k get pod web -o jsonpath='containerID: {.status.containerStatuses[0].containerID}{"\n"}imageID: {.status.containerStatuses[0].imageID}{"\n"}'

step "the first instruction of a container already sees the pod's hostname, resolv.conf and /dev links"
k run early --image=busybox --restart=Never -- sh -c 'hostname; cat /etc/resolv.conf; ls -l /dev/stdout | sed "s/.*\/dev/\/dev/"; echo to-stderr >&2'
until_true 60 "early Succeeded" phase_is early Succeeded
k logs early | tee "$TMP/early"
grep -qx early "$TMP/early" || fail "early hostname"
grep -qx 'nameserver 10.96.0.10' "$TMP/early" || fail "early resolv.conf"
grep -q '^/dev/stdout -> /proc/self/fd/1' "$TMP/early" || fail "early /dev/stdout"
grep -qx to-stderr "$TMP/early" || fail "stderr missing from kubectl logs"
uid=$(jp early '{.metadata.uid}')
echo "the CRI log file in the pod directory:"
cat "$STATE/pods/$uid/guest/logs/early.log"

step "an init container writes a page into an emptyDir; the main container serves it"
cat >"$TMP/initweb.yaml" <<'Y'
apiVersion: v1
kind: Pod
metadata: {name: initweb}
spec:
  initContainers:
  - name: init
    image: busybox
    imagePullPolicy: IfNotPresent
    command: [sh, -c, 'echo "written by the init container of $(hostname)" > /work/index.html; echo wrote /work/index.html']
    volumeMounts: [{name: work, mountPath: /work}]
  containers:
  - name: nginx
    image: nginx:alpine
    ports: [{containerPort: 80}]
    readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 2}
    volumeMounts: [{name: work, mountPath: /usr/share/nginx/html}]
  volumes: [{name: work, emptyDir: {}}]
Y
t0=$(now)
k apply -f "$TMP/initweb.yaml"
until_true 120 "initweb Ready" ready initweb
echo "measured create -> Ready: $(since "$t0")s"
k get pod initweb -o wide
echo "init: $(cstat initweb initContainerStatuses init 'state.terminated.reason') exit $(cstat initweb initContainerStatuses init 'state.terminated.exitCode')"
echo "kubectl logs initweb -c init: $(k logs initweb -c init)"
ip=$(jp initweb '{.status.podIP}')
local_addr=$(forward_of "$ip" 80)
page=$(curl -s -m 5 "http://$local_addr/")
echo "curl http://$local_addr/ (the agent's gateway forward to $ip:80): $page"
[ "$page" = "written by the init container of initweb" ] || fail "page not served"

step "nginx and a busybox sidecar in one pod"
cat >"$TMP/duo.yaml" <<'Y'
apiVersion: v1
kind: Pod
metadata: {name: duo}
spec:
  containers:
  - name: nginx
    image: nginx:alpine
    ports: [{containerPort: 80}]
  - name: sidecar
    image: busybox
    imagePullPolicy: IfNotPresent
    command: [sh, -c, 'while true; do wget -qO- -T 2 http://localhost:80/ | grep -o "<title>.*</title>" | sed "s/^/sidecar fetched /"; sleep 2; done']
Y
t0=$(now)
k apply -f "$TMP/duo.yaml"
until_true 120 "duo Ready" ready duo
echo "measured create -> Ready: $(since "$t0")s"
k get pod duo -o wide
until_true 20 "sidecar output" sh -c "\"$KUBECTL\" logs duo -c sidecar | grep -q 'sidecar fetched'"
echo "--- kubectl logs duo -c nginx --tail=2"
k logs duo -c nginx --tail=2 | tee "$TMP/nginx.log"
grep -q '"GET / HTTP/1.1" 200 .*"Wget"' "$TMP/nginx.log" || fail "no sidecar request in the nginx log"
echo "--- kubectl logs duo -c sidecar --tail=2 --timestamps"
k logs duo -c sidecar --tail=2 --timestamps | tee "$TMP/side.log"
grep -q 'sidecar fetched <title>Welcome to nginx!</title>' "$TMP/side.log" || fail "no sidecar output"
echo "--- kubectl exec duo -c nginx"
kx duo -c nginx -- sh -c 'head -1 /etc/os-release; ls /docker-entrypoint.sh; printf "pid 1: "; tr "\0" " " </proc/1/cmdline; echo' | tee "$TMP/x-nginx"
grep -q 'Alpine Linux' "$TMP/x-nginx" && grep -q 'pid 1: nginx: master process' "$TMP/x-nginx" || fail "exec -c nginx is not in the nginx rootfs"
echo "--- kubectl exec duo -c sidecar"
kx duo -c sidecar -- sh -c 'ls /etc/os-release /docker-entrypoint.sh 2>&1; busybox | head -1; printf "pid 1: "; tr "\0" " " </proc/1/cmdline; echo' | tee "$TMP/x-side"
grep -q 'pid 1: sh -c while true' "$TMP/x-side" && grep -q '/docker-entrypoint.sh: No such file or directory' "$TMP/x-side" || fail "exec -c sidecar is not in the busybox rootfs"
echo "--- kubectl exec -it duo -c sidecar under a 30x100 terminal"
rc=0
script -q /dev/null bash -c "stty rows 30 cols 100; \"$KUBECTL\" exec -it duo -c sidecar -- sh -c 'tty; stty size; echo TERM=\$TERM; exit 4'" </dev/null >"$TMP/tty" || rc=$?
# script(1) echoes the ^D of its closed stdin before the output.
tr -d '\r\b' <"$TMP/tty" | sed 's/^\^D//' | tee "$TMP/tty.txt"
grep -q '^/dev/pts/' "$TMP/tty.txt" && grep -qx '30 100' "$TMP/tty.txt" && grep -q 'exit code 4' "$TMP/tty.txt" || fail "tty exec"
echo "--- kubectl exec duo -c sidecar -- sh -c 'exit 3'"
rc=0
kx duo -c sidecar -- sh -c 'exit 3' || rc=$?
echo "exit code: $rc"
[ "$rc" = 3 ] || fail "exec exit code $rc"
echo "--- kill the sidecar's process (SIGKILL through the supervisor's ctl.sock)"
nginx_started=$(cstat duo containerStatuses nginx 'state.running.startedAt')
t0=$(now)
guest_ctl duo '{"op":"kill","container":"sidecar","signal":9}'
side_restarted() { [ "$(cstat duo containerStatuses sidecar restartCount)" = 1 ] && [ -n "$(cstat duo containerStatuses sidecar 'state.running.startedAt')" ]; }
until_true 30 "sidecar restartCount 1" side_restarted
echo "measured kill -> sidecar running again: $(since "$t0")s"
k get pod duo -o jsonpath='{range .status.containerStatuses[*]}{.name}: restartCount={.restartCount} ready={.ready} lastState={.lastState.terminated.reason}/{.lastState.terminated.exitCode} containerID={.containerID}{"\n"}{end}'
[ "$(cstat duo containerStatuses nginx restartCount)" = 0 ] || fail "nginx restarted"
[ "$(cstat duo containerStatuses nginx 'state.running.startedAt')" = "$nginx_started" ] || fail "nginx started again"
[ "$(cstat duo containerStatuses sidecar 'lastState.terminated.exitCode')" = 137 ] || fail "sidecar lastState"
echo "--- kubectl logs duo -c sidecar --previous --tail=1"
k logs duo -c sidecar --previous --tail=1
k get events --field-selector involvedObject.name=duo -o custom-columns=REASON:.reason,MESSAGE:.message --no-headers | grep -E 'Created|Started' | sort | uniq -c

step "an init container that fails: Init:Error"
cat >"$TMP/initfail.yaml" <<'Y'
apiVersion: v1
kind: Pod
metadata: {name: initfail}
spec:
  restartPolicy: Never
  initContainers:
  - {name: check, image: busybox, imagePullPolicy: IfNotPresent, command: [sh, -c, 'echo the check fails; exit 7']}
  containers:
  - {name: app, image: busybox, imagePullPolicy: IfNotPresent, command: [sleep, '3600']}
Y
k apply -f "$TMP/initfail.yaml"
until_true 60 "initfail Failed" phase_is initfail Failed
k get pod initfail
k get pod initfail -o jsonpath='phase: {.status.phase}{"\n"}message: {.status.message}{"\n"}init check: {.status.initContainerStatuses[0].state}{"\n"}app: {.status.containerStatuses[0].state}{"\n"}Initialized: {.status.conditions[?(@.type=="Initialized")].status} {.status.conditions[?(@.type=="Initialized")].reason}{"\n"}'
[ "$(status_col initfail)" = Init:Error ] || fail "initfail STATUS is $(status_col initfail)"
[ "$(cstat initfail initContainerStatuses check 'state.terminated.exitCode')" = 7 ] || fail "init exit code"
[ "$(cstat initfail containerStatuses app 'state.waiting.reason')" = PodInitializing ] || fail "app state"
echo "kubectl logs initfail -c check: $(k logs initfail -c check)"

step "a sidecar-style init container (restartPolicy Always) runs next to the main container"
cat >"$TMP/side.yaml" <<'Y'
apiVersion: v1
kind: Pod
metadata: {name: side}
spec:
  initContainers:
  - {name: logger, image: busybox, imagePullPolicy: IfNotPresent, restartPolicy: Always, command: [sh, -c, 'while true; do echo tick; sleep 1; done']}
  containers:
  - {name: app, image: busybox, imagePullPolicy: IfNotPresent, command: [sh, -c, 'echo app up; sleep 3600']}
Y
k apply -f "$TMP/side.yaml"
until_true 60 "side Ready" ready side
sleep 5
k get pod side
k get pod side -o jsonpath='{range .status.initContainerStatuses[*]}init {.name}: started={.started} ready={.ready} restartCount={.restartCount} running since {.state.running.startedAt}{"\n"}{end}{range .status.containerStatuses[*]}{.name}: running since {.state.running.startedAt}{"\n"}{end}'
[ "$(cstat side initContainerStatuses logger restartCount)" = 0 ] || fail "logger restarted"
[ -n "$(cstat side initContainerStatuses logger 'state.running.startedAt')" ] || fail "logger not running"
[ "$(status_col side)" = Running ] || fail "side STATUS"
echo "kubectl logs side -c logger --tail=1: $(k logs side -c logger --tail=1)"
echo "kubectl logs side -c app: $(k logs side -c app)"

step "runAsUser: 1000"
k run asuser --image=busybox --image-pull-policy=IfNotPresent --overrides='{"spec":{"securityContext":{"runAsUser":1000}}}' -- sh -c 'id; sleep 3600'
until_true 60 "asuser Running" running asuser
out=$(kx asuser -- id)
echo "kubectl exec asuser -- id: $out"
echo "kubectl logs asuser: $(k logs asuser)"
case "$out" in uid=1000*) ;; *) fail "id is $out" ;; esac

step "a container that exits 3: exit code 3, pod Failed"
k run exit3 --image=busybox --image-pull-policy=IfNotPresent --restart=Never -- sh -c 'echo bye; exit 3'
until_true 60 "exit3 Failed" phase_is exit3 Failed
k get pod exit3
k get pod exit3 -o jsonpath='phase: {.status.phase}{"\n"}terminated: {.status.containerStatuses[0].state.terminated.reason} exit {.status.containerStatuses[0].state.terminated.exitCode}{"\n"}'
[ "$(cstat exit3 containerStatuses exit3 'state.terminated.exitCode')" = 3 ] || fail "exit code"

step "delete every pod; no VM is left"
t0=$(now)
k delete pod web initweb exit3 early initfail
echo "measured delete of 5 pods (nginx stops on SIGTERM, the rest had ended): $(since "$t0")s"
t0=$(now)
k delete pod keep duo side asuser
echo "measured delete of 4 pods with a shell as PID 1, which ignores SIGTERM: $(since "$t0")s (the 30 s grace period)"
k get pods
hull ps
[ -z "$(default_vms)" ] || fail "VMs left after delete"
python3 -c 'import json, sys; left = [a for a in json.load(open(sys.argv[1])) if not a["pod"].startswith("kube-system/")]; print("addresses left:", left); sys.exit(1 if left else 0)' "$STATE/ipam.json" || fail "addresses left"

step "briglet status"
"$BRIGLET" status

printf '\ncontainers PASS\n'
