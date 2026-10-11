# Setting up briglet

briglet runs on Apple silicon Macs. It starts the Kubernetes control plane
as native processes and boots one Linux microVM per pod through
[hull](https://github.com/brig-sh/hull), so it needs two sets of
prerequisites: the control plane binaries built for darwin/arm64, and a
hull runtime whose gateway knows about Services.

## What you need

| Component | Where it comes from |
|---|---|
| Go 1.26 or newer | `brew install go` or [go.dev](https://go.dev/dl/) |
| etcd 3.7, kube-apiserver, kubectl (1.37) | the kubebuilder envtest release |
| kube-controller-manager, kube-scheduler (1.37) | built from the kubernetes source at the same tag; there is no darwin release |
| hvi, vz-runner, boot assets | the hull Homebrew cask |
| hull CLI with `network-gateway --service-cidr` | built from hull's `feat/gateway-services` branch until that lands in a release |

## The short way

```sh
hack/fetch-prereqs.sh
```

The script fetches the envtest binaries, builds the two missing control
plane binaries from a shallow clone of kubernetes, installs the hull cask
when `hvi` is not on `PATH`, and builds the hull CLI from the Service-aware
branch. Everything lands in `~/.local/share/briglet/bin`, which briglet
looks in by default. The first run takes a few minutes, most of it the
kubernetes build; later runs reuse the clones under `~/.cache/briglet`.

Variables the script reads:

| Variable | Default | Meaning |
|---|---|---|
| `BRIGLET_BIN_DIR` | `$XDG_DATA_HOME/briglet/bin` or `~/.local/share/briglet/bin` | where the binaries go |
| `BRIGLET_CACHE_DIR` | `$XDG_CACHE_HOME/briglet` or `~/.cache/briglet` | source clones |
| `K8S_VERSION` | `1.37.0` | Kubernetes tag to fetch and build |
| `HULL_REPO`, `HULL_REF` | `brig-sh/hull`, `feat/gateway-services` | where the hull CLI is built from |
| `SKIP_HULL` | unset | set to `1` to leave the runtime alone |

## Where briglet looks for binaries

For each of etcd, kube-apiserver, kube-controller-manager and
kube-scheduler, briglet takes the first that exists:

1. `BRIGLET_ETCD_BIN` for etcd, `BRIGLET_K8S_BIN_DIR/<name>` for the rest;
2. `BRIGLET_BIN_DIR/<name>`, with the same default directory as the script;
3. `<name>` on `PATH`.

hull is `HULL_BIN` when set, then `BRIGLET_BIN_DIR/hull`, then `hull` on
`PATH`, then the Homebrew cask's `/opt/homebrew/bin/hull`. `briglet up`
refuses a hull whose `network-gateway` has no `--service-cidr` flag, and a
gateway that is already running without it.

kubectl is only used by the smoke checks; set `KUBECTL` to point them at a
different one.

## Doing it by hand

If you would rather not run the script:

```sh
# envtest: etcd, kube-apiserver, kubectl
go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use 1.37.0 -p path

# the two missing binaries, from a kubernetes checkout at v1.37.0
git clone --depth 1 --branch v1.37.0 https://github.com/kubernetes/kubernetes.git
(cd kubernetes && go build -o /path/to/bin/kube-controller-manager ./cmd/kube-controller-manager \
              && go build -o /path/to/bin/kube-scheduler ./cmd/kube-scheduler)

# the runtime
brew tap brig-sh/brig && brew install --cask hull
git clone --depth 1 --branch feat/gateway-services https://github.com/brig-sh/hull.git
(cd hull && go build -o /path/to/bin/hull ./cmd/hull)
```

Then either copy everything into `~/.local/share/briglet/bin`, or export
`BRIGLET_K8S_BIN_DIR`, `BRIGLET_ETCD_BIN` and `HULL_BIN`.

## First cluster

```sh
make
bin/briglet up
export KUBECONFIG="$HOME/.briglet/admin.kubeconfig"
kubectl get nodes -o wide
kubectl -n kube-system rollout status deployment/coredns --timeout=180s
```

Docker Hub's anonymous pull limit is easy to hit while trying things out.
`export BRIGLET_REGISTRY_MIRROR=mirror.gcr.io` before `briglet up` routes
Docker Hub pulls through Google's mirror.

State lives under `~/.briglet`, or `BRIGLET_STATE_DIR`. `briglet down`
stops everything and keeps it; `briglet down --wipe` removes it.

## A second Mac

A second Mac joins as a node. It runs the node agent and a gateway of
its own; the control plane stays on the first Mac. Pod addresses are not
routed between the two, so what crosses the wire is node addresses only:
the apiserver port, the kubelet port and node ports. See
[architecture.md](architecture.md#more-than-one-node) for what that
means for Services.

Both Macs need the same prerequisites, built the same way; `make` on
each, or copy `bin/` and `~/.local/share/briglet/bin` across. Then:

1. **Sign the binaries on both Macs.** macOS Local Network privacy
   makes an unsigned binary's dials to another machine on the LAN fail
   silently (`EHOSTUNREACH`; loopback is exempt). An ad-hoc signature is
   enough:

   ```sh
   codesign -f -s - bin/briglet ~/.local/share/briglet/bin/hull
   ```

   The first LAN dial after that makes macOS ask, per binary, for
   Local Network access; allow it. When nothing asks, for example in a
   session started over SSH, grant it under System Settings > Privacy &
   Security > Local Network. Rebuilding a binary drops the signature
   and the grant. A node whose dials to the other
   Mac fail shows up as `services: no endpoint for ...` in its gateway
   log while `curl` from the same Mac succeeds.

2. **Mint the node's credentials on Mac 1.** `--ip` is the address Mac 1
   reaches Mac 2 at (the apiserver dials it for logs and exec);
   `--server-ip` is the address Mac 2 reaches Mac 1 at, which the
   apiserver certificate already covers for the LAN:

   ```sh
   bin/briglet join --name mac2 --ip <mac2-ip> --server-ip <mac1-ip> \
     --out ~/.briglet/join/mac2
   scp -r ~/.briglet/join/mac2 mac2:~/.briglet-mac2/join
   ```

   If Mac 2 cannot dial Mac 1 directly, a reverse SSH tunnel from Mac 1
   makes the apiserver a loopback address on Mac 2, which Local Network
   privacy leaves alone. Keep the port 6443, so the `kubernetes` Service
   entry still matches:

   ```sh
   ssh -N -R 6443:127.0.0.1:6443 mac2     # on Mac 1, keep it running
   bin/briglet join ... --server-ip 127.0.0.1
   ```

3. **Start a gateway on Mac 2** for the other `/25` half (the
   controller manager hands Mac 2 `10.87.0.128/25` when Mac 1 holds the
   first half). The gateway address is that half's first address. Until
   the netstack handles a `/25` subnet, give it the full `/24`:

   ```sh
   export BRIGLET_STATE_DIR=~/.briglet-mac2
   mkdir -p "$BRIGLET_STATE_DIR/guest-bin"
   cp bin/briglet-guest "$BRIGLET_STATE_DIR/guest-bin/"
   hull --unattended --dnt --store-dir "$BRIGLET_STATE_DIR/hull-store" network-gateway \
     --socket "$BRIGLET_STATE_DIR/gw.sock" --api "$BRIGLET_STATE_DIR/gw.sock.api" \
     --subnet 10.87.0.0/24 --gateway-ip 10.87.0.129 --service-cidr 10.96.0.0/12
   ```

4. **Start the agent on Mac 2**, as `briglet join` printed, with the
   node's own address and gateway:

   ```sh
   bin/briglet node \
     --kubeconfig "$BRIGLET_STATE_DIR/join/node.kubeconfig" \
     --client-ca-file "$BRIGLET_STATE_DIR/join/ca.crt" \
     --tls-cert-file "$BRIGLET_STATE_DIR/join/kubelet-serving-mac2.crt" \
     --tls-private-key-file "$BRIGLET_STATE_DIR/join/kubelet-serving-mac2.key" \
     --name mac2 --node-ip <mac2-ip> --gateway-sock "$BRIGLET_STATE_DIR/gw.sock"
   ```

   The agent adopts its pod CIDR from the apiserver; `--host-ip`
   defaults to that half's first address, which is the gateway's.

5. **Give Mac 2 cluster DNS.** The `kube-dns` Service is ClusterIP only,
   so pods on Mac 2 cannot use the replica on Mac 1:

   ```sh
   kubectl -n kube-system scale deployment/coredns --replicas=2
   ```

   The replica answers cluster names; outside names still go to Mac 1's
   gateway address, which Mac 2 cannot reach.

Then, from Mac 1:

```sh
kubectl run echo2 --image=busybox --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"mac2"}}}' \
  --command -- sh -c 'mkdir /www && echo "hello from $(hostname)" > /www/index.html && httpd -f -p 80 -h /www'
kubectl expose pod echo2 --type=NodePort --port=80 --name=echo2-np
kubectl exec <a pod on mac1> -- wget -qO- http://echo2-np        # stitched through mac2's node port
kubectl logs echo2; kubectl exec echo2 -- hostname               # apiserver -> mac2:10250
curl http://<mac1-ip>:$(kubectl get svc echo2-np -o jsonpath='{.spec.ports[0].nodePort}')/
```

A Service without a node port, and a bare pod address, are not reachable
from the other Mac. Images are pulled on the Mac the pod lands on, so
Mac 2 needs registry access or `imagePullPolicy: IfNotPresent` with the
image already in its hull store.
