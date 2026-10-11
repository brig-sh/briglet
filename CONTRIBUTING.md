# Contributing to briglet

Thanks for taking the time. briglet is experimental, so the most useful
contributions are reproducible reports and small, focused patches.

## Before you open a pull request

- Build and test: `make test` runs the unit tests and vets the linux/arm64
  guest supervisor. `make` builds both binaries.
- For changes that touch the runtime path, run the relevant smoke check
  from `hack/` against a disposable `BRIGLET_STATE_DIR`. The scripts wipe
  the state directory they are given.
- Keep the README and `docs/` in step with behavior you change.

## Commits

Use Conventional Commits with a capitalized imperative summary, and a body
that says why:

```
fix(node): Dial unknown egress destinations directly

The proxy 403'd anything outside the service and pod CIDRs, which
blocked the apiserver's dials to another node's kubelet.
```

Scopes in use: `cp`, `node`, `podvm`, `guest`, `hull`, `services`, `hack`,
`docs`.

Every commit needs a `Signed-off-by` trailer (`git commit -s`), which
certifies the [Developer Certificate of Origin](https://developercertificate.org/).
Contributions are accepted under the Apache License 2.0 in `LICENSE`.

## Reporting a problem

Include the macOS and hull versions, the `briglet status` output, and the
relevant file under `~/.briglet/logs/`. For a pod that does not start, the
pod's directory under `~/.briglet/pods/<uid>/` holds the record the agent
handed to the VM.
