# chaos-test

A harness for stress-testing the BSV **alert system** (freeze / unfreeze / confiscation alerts)
against a network of teranodes and SV Nodes, with a web GUI that runs and observes scenarios.
First target: teranode PR [#1764](https://github.com/bsv-blockchain/teranode/pull/1764)
(height-anchored freezes), reproduced as scenario `freeze-timing-skew`.

Two layers:

| layer | what | how to run |
|---|---|---|
| [`bsv-regtest/`](bsv-regtest/README.md) | the network: N teranodes built from a teranode ref plus patches, SV Nodes following them over teranode's legacy service with go-alert-system sidecars, Redpanda, the alert hub, arcade, merkle-service, the BRC-100 wallet (wallet-infra + walletd), tools. Its own Go module and its own README; usable without the simulator | `make build && make up && make wait` |
| [`sim/`](sim/) | orchestrator (Go) + React UI that attaches to the running network: fleet view (nodes, alert hub, arcade with its chaintracks tip), alert builder/delivery, chain & UTXO view, wallet, chaos, scenarios, logs and diagnostics; every txid opens in arcade | `make sim-build && make sim-up` → http://localhost:8600 |

Nothing needs the internet at run time. Everything (keys, IPs, images) is pinned.

**Only want the network?** Use [`bsv-regtest/`](bsv-regtest/README.md) on its own. This
repository's Makefile drives the same targets with the harness's own defaults: teranode built
from the PR branch (`TERANODE_REF=fix/1422-height-anchored-freeze`, tag `pr1764`, plus the
alert-P2P settings patch in `patches/teranode/` that the branch still needs) and egress-free
networks (`INTERNAL=1`).

## Quick start

```bash
systemctl --user enable --now podman.socket   # chaos actions (partitions, pause, stop) use the container API
make build           # tools image, alert-system image, walletd image, patched teranode (first time 10-20 min)
make gen N=3 SV=2    # bsv-regtest/compose.yaml + bsv-regtest/config (keys created once); SV=0 for teranodes only
make up && make wait # teranodes, SV nodes + alert sidecars, kafka, hub, arcade, merkle-service, wallet, tools
make sim-build && make sim-up         # orchestrator + GUI
open http://localhost:8600            # GUI (or `cd sim/ui && npm run dev` for live UI dev on :5173)
```

`make up` re-renders the compose file first, so a `make gen` setting (`N`, `SV`, `INTERNAL`,
`TERANODE_TAG`) given to `make up` takes effect. Run the reference scenario from the Scenarios
page, or:

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
- `scenarios/svnode-dust-policy.yaml` — characterises where the fleet disagrees about dust: a
  zero-satoshi bare `OP_RETURN` output is rejected by the strict-policy SV node and accepted by
  everyone else.

Run `make reset` between runs: scenarios need a converged fleet with a clean alert history,
and a split caused by a finding cannot be healed by mining.

Scenarios are YAML: ordered steps (`mine`, `spend`, `submit`, `build_alert`, `push_alert`,
`rpc_freeze`, `partition`, `chaos`, `mark`, …) with polled assertions (`same_tip`, `utxo_status`,
`event`, `no_event`, `log_count` on container logs, `tip_is`, …). `should: true` turns a failing
assertion into a recorded finding instead of a stop. `${...}` expressions reference run
variables, params, roles and live node state (`${tip(A) + 2}`). Runs are stored in
`sim/.data/runs/`.

## Comparing builds

```bash
echo TERANODE_IMAGE_1=ghcr.io/bsv-blockchain/teranode:latest > bsv-regtest/.env   # node A on upstream main
make reset && curl -s -X POST -d '{"mode":"auto"}' localhost:8600/api/scenarios/freeze-skew-rpc/run
rm bsv-regtest/.env && make reset                                                # back to the PR image
```

Result of the comparison (2026-09-17, details in
[`docs/comparison-main-vs-pr1764.md`](docs/comparison-main-vs-pr1764.md)): both builds reject
the unaware miner's block, as an immediate freeze demands; `main` publishes no verdict and,
once the miner builds on the block, re-fetches and re-validates it in a loop (16 attempts in
75 s), while the PR image publishes one `invalid-blocks` verdict naming the frozen output,
validates once and refuses the child block immediately. At the time, upstream `main` lacked
the alert-P2P settings patch, so a `main` node's alert service could not join the private
alert network and `freeze-skew-rpc` delivers the freeze over RPC; since teranode PR 1767
(merged 2026-09-18) a current published image does join it. The height-anchored fix itself
(`freeze-timing-skew`, `freeze-fork-remine`) runs on the PR image.

## Upstream work produced here

- teranode PR [#1767](https://github.com/bsv-blockchain/teranode/pull/1767) (merged): make
  alert P2P bootstrap peer, private IPs, discovery interval and DHT mode configurable. The
  patch stays in `patches/teranode/` for the PR-1764 branch, which predates it.
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
- arcade issue (`docs/upstream/arcade-339-resurrected-block-stays-orphaned.md`): a block
  orphaned by a same-height tie and later resurrected stays orphaned in arcade.
- Two teranode patches for running SV Node behind teranode's legacy service in a private
  network (`bsv-regtest/patches/teranode/`, see `bsv-regtest/docs/patches.md`).

## Layout

```
bsv-regtest/     the network: own Go module (github.com/bsv-blockchain/bsv-regtest), Makefile, compose, generator,
                 alertctl/stackctl, walletd, patches, scripts, docs  -> bsv-regtest/README.md
patches/teranode/  0001 alert-P2P settings: still needed by the PR-1764 branch, applied via EXTRA_PATCHES
sim/             orchestrator compose, Dockerfile, React UI (sim/ui)
cmd/orchestrator simulator backend
internal/api     REST/SSE + UI          internal/observe  fleet observer + event bus   internal/scenario  engine
internal/diag    diagnostics rules      internal/logs     container log tailing        internal/chaos     container runtime
internal/svnode  SV Node RPC client     internal/walletsvc wallet service (walletd client, sustained send)
internal/arcade  arcade client          internal/automine
scenarios/       scenario YAML          docs/upstream/    findings for upstream repos
go.work          workspace: this module + ./bsv-regtest (walletd is a nested module outside the workspace)
```

The `internal/{alerts,keys,teranode,topology,wallet}` packages moved into `bsv-regtest/` and
are imported as `github.com/bsv-blockchain/bsv-regtest/...`; the root `go.work` makes the
local copy the source. `make test` runs both modules' tests; `make test-walletd` the nested
one; `make tidy` tidies all three with `GOWORK=off` so each module's `go.sum` stays complete
for standalone builds.
