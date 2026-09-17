# chaos-test

A harness for stress-testing the BSV **alert system** (freeze / unfreeze / confiscation alerts)
against a network of teranodes, with a web GUI that runs and observes scenarios. First target:
teranode PR [#1764](https://github.com/bsv-blockchain/teranode/pull/1764) (height-anchored
freezes), reproduced as scenario `freeze-timing-skew`.

Two independent layers:

| layer | what | how to run |
|---|---|---|
| [`stack/`](stack/README.md) | standalone, fully private regtest network: N teranodes (built from a teranode ref + a small alert-P2P settings patch), Redpanda, go-alert-system hub, arcade, merkle-service, wallet-infra, tools | `make build && make gen && make up` |
| [`sim/`](sim/) | orchestrator (Go) + React UI that attaches to the running stack: fleet view (nodes, alert hub, arcade with its chaintracks tip), alert builder/delivery, chain & UTXO view, chaos, scenarios; every txid opens in arcade | `make sim-build && make sim-up` → http://localhost:8600 |

Nothing needs the internet at run time. Everything (keys, IPs, images) is pinned.

**Only want the network?** [`stack/README.md`](stack/README.md) is self-contained: build the
images, `make gen`, `make up PROFILES=` (nodes, Kafka, alert hub) or `make up` (plus arcade,
merkle-service, wallet, tools), then drive it with `stack/scripts/*` and the `alertctl` /
`stackctl` CLIs in the tools container. The simulator and GUI are an optional layer.

## Quick start

```bash
systemctl --user enable --now podman.socket   # chaos actions (partitions) use the podman API
make build           # tools image, go-alert-system hub, patched teranode (first time 10-20 min)
make gen N=3         # stack/compose.yaml + stack/config (keys created once)
make up && make wait # nodes, kafka, hub, arcade, merkle-service, wallet-infra, tools (stack only: stop here)
make sim-build && make sim-up         # optional: orchestrator + GUI (needs the podman socket)
open http://localhost:8600            # GUI (or `cd sim/ui && npm run dev` for live UI dev on :5173)
```

Run the reference scenario from the Scenarios page, or:

```bash
curl -s -X POST -H 'Content-Type: application/json' -d '{"mode":"auto"}' \
  localhost:8600/api/scenarios/freeze-timing-skew/run | jq .id
curl -s localhost:8600/api/runs/<id> | jq '.status, .findings, [.steps[] | {name,status}]'
```

`mode: "step"` pauses before every step (Next/Abort in the GUI or `POST /api/runs/<id>/next`).

## Scenarios

- `scenarios/freeze-timing-skew.yaml` — A gets the alert early, B late, C never. A block spending
  the coin *below* the window must be accepted by everyone; a block spending it *inside* the
  window must be rejected cleanly by every alert holder, once, with `UTXO_CONSENSUS_FROZEN`.
- `scenarios/freeze-fork-remine.yaml` — the adversarial fork: C re-mines the below-window spend
  inside the window on an isolated fork; A and B must reject it although they record the coin
  as spent by that very transaction.
- `scenarios/freeze-skew-rpc.yaml` — the issue #1422 shape over the admin `freeze` RPC (no
  heights = legacy immediate freeze), so it runs unchanged on upstream `main` and on the PR
  image and compares *how* the node rejects the block: verdict published or not, validated once
  or re-fetched in a loop, child block refused at once or not.

Run `make reset` between runs: scenarios need a converged fleet with a clean alert history,
and a split caused by a finding cannot be healed by mining.

Scenarios are YAML: ordered steps (`mine`, `spend`, `submit`, `build_alert`, `push_alert`,
`rpc_freeze`, `partition`, `chaos`, `mark`, …) with polled assertions (`same_tip`, `utxo_status`,
`event`, `no_event`, `log_count` on container logs, `tip_is`, …). `should: true` turns a failing assertion into a recorded finding instead of a
stop. `${...}` expressions reference run variables, params, roles and live node state
(`${tip(A) + 2}`). Runs are stored in `sim/.data/runs/`.

## Comparing builds

```bash
echo TERANODE_IMAGE_1=ghcr.io/bsv-blockchain/teranode:latest > stack/.env   # node A on upstream main
make reset && curl -s -X POST -d '{"mode":"auto"}' localhost:8600/api/scenarios/freeze-skew-rpc/run
rm stack/.env && make reset                                                  # back to the PR image
```

The stack has no internet egress, and upstream `main` lacks the alert-P2P settings patch, so a
`main` node's alert service never starts there. `freeze-skew-rpc` therefore delivers the freeze
over RPC. Result of the comparison (2026-09-17, details in
[`docs/comparison-main-vs-pr1764.md`](docs/comparison-main-vs-pr1764.md)): both builds reject
the unaware miner's block, as an immediate freeze demands; `main` publishes no verdict and,
once the miner builds on the block, re-fetches and re-validates it in a loop (16 attempts in
75 s), while the PR image publishes one `invalid-blocks` verdict naming the frozen output,
validates once and refuses the child block immediately. The height-anchored fix itself
(`freeze-timing-skew`, `freeze-fork-remine`) needs the alert network and runs on the PR image.

## Upstream work produced here

- teranode PR [#1767](https://github.com/bsv-blockchain/teranode/pull/1767): make alert P2P
  bootstrap peer, private IPs, discovery interval and DHT mode configurable
  (`stack/patches/teranode/`).
- go-alert-system issue [#171](https://github.com/bsv-blockchain/go-alert-system/issues/171):
  v0.1.x (teranode's pin) drops every alert received over gossipsub; fixed silently in v0.2.0
  (`docs/upstream/go-alert-system-gossip-duplicate-check.md`).
- teranode PR #1764 finding: after an isolated fork, the alert-holding node adopts the empty
  sibling, re-mines the frozen spend inside the window and splits from the fleet
  (`docs/upstream/teranode-fork-adopts-sibling-and-remines-frozen-spend.md`, with logs);
  reproduced deterministically by `freeze-fork-remine`.
- Code review of PR #1764 with the harness findings traced to code
  (`docs/upstream/pr1764-code-review.md`): 11 findings, test suggestions, nits. Written against
  head `2d78850`; the PR has since moved to `889a25c3`, noted per finding.

## Layout

```
stack/           compose stack, generated config, patches, scripts, README
sim/             orchestrator compose, Dockerfile, React UI (sim/ui)
cmd/gen          stack generator          cmd/alertctl   alert CLI (build/sign/push/probe/broadcast)
cmd/stackctl     mine/spend/submit CLI    cmd/orchestrator  simulator backend
internal/alerts  alert wire format, sync-stream host, gossip participant, hub client
internal/observe fleet observer + event bus   internal/scenario  engine   internal/api  REST/SSE + UI
internal/teranode RPC/asset/Kafka clients  internal/chaos  podman runtime   internal/wallet raw signer
scenarios/       scenario YAML             docs/upstream/  findings for upstream repos
```
