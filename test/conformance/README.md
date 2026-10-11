# Conformance

briglet runs the Kubernetes conformance suite through Sonobuoy. This is not a
certification claim: a cluster whose node is a Mac and whose pods are VMs cannot
pass every test, and `skips.txt` says which ones and why.

## Run

```sh
brew install sonobuoy          # 0.57.5 or later
export KUBECONFIG=~/.briglet/admin.kubeconfig
sonobuoy run \
  --mode=certified-conformance \
  --kube-conformance-image registry.k8s.io/conformance:v1.37.1 \
  --plugin e2e \
  --plugin-env e2e.E2E_PARALLEL=true \
  --wait --timeout 14400
sonobuoy retrieve ~/.briglet/sonobuoy/
sonobuoy results ~/.briglet/sonobuoy/*.tar.gz --plugin e2e
sonobuoy delete --wait
```

Only the e2e plugin: the systemd-logs plugin needs host namespaces, which
briglet refuses. Docker Hub images (Sonobuoy's own) need
`BRIGLET_REGISTRY_MIRROR=mirror.gcr.io` on the node agent when the anonymous
pull limit is exhausted. The conformance image's test images come from
registry.k8s.io.

To skip the known failures, join the patterns in `skips.txt` (comment lines
removed) with `|` and pass them with `--e2e-skip`.

## Results

| date | node | specs | passed | failed | wall time |
|---|---|---|---|---|---|
| 2026-10-10 | Mac Studio, pods booting with `--wait-ip` | 433 | 339 | 92 (+2 errors) | 2 h 40 min |
| 2026-10-11 | Mac Studio, `--wait-ip` removed | 433 | 392 | 41 | 1 h 49 min |
| 2026-10-11 | Mac Studio, nine fixes, overlay-block rootfs | 433 | 401 | 32 | 1 h 29 min |

Passed counts exclude Sonobuoy's own harness entries (9 in the junit file).
Each row is one fresh run on one revision, except 2026-10-10, which had a
34-minute infrastructure incident. The latest run's 32 failures are exactly
the patterns in `skips.txt`: 18 unsupported by design, 13 behind feature
gates this control plane does not enable, and 1 that needs two untainted
nodes.

Results tarballs and extracted junit files live under `~/.briglet/sonobuoy/`.
