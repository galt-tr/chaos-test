# chaos-test

A harness for stress-testing BSV consensus and the BSV **alert system** (freeze / unfreeze /
confiscation alerts) against a private network of teranodes and SV Nodes, with a web GUI that
runs and observes scenarios. It brings up a disposable regtest fleet, drives it through scripted
chaos (network partitions, competing miners, alert delivery, container pause/stop), and checks
that the nodes agree, or records exactly where and how they disagree.

Two layers:

| layer | what | how to run |
|---|---|---|
| [`bsv-regtest/`](bsv-regtest/README.md) | the network: N teranodes built from a teranode ref, SV Nodes following them over teranode's legacy service with go-alert-system sidecars, Redpanda, the alert hub, arcade, merkle-service, the BRC-100 wallet (wallet-infra + walletd), tools. Its own Go module and its own README; usable without the simulator | `make build && make up && make wait` |
| [`sim/`](sim/) | orchestrator (Go) + React UI that attaches to the running network: fleet view (nodes, alert hub, arcade with its chaintracks tip), alert builder/delivery, chain & UTXO view, wallet, chaos, scenarios, logs and diagnostics; every txid opens in arcade | `make sim-build && make sim-up` → http://localhost:8600 |

Nothing needs the internet at run time. Everything (keys, IPs, images) is pinned.

**Only want the network?** Use [`bsv-regtest/`](bsv-regtest/README.md) on its own. This
repository's Makefile drives the same targets with the simulator's defaults: teranode built from
upstream `main` (override with `TERANODE_REF`) and open compose networks. Set `INTERNAL=1` for
egress-free networks.

## Quick start

```bash
systemctl --user enable --now podman.socket   # podman only: chaos actions and logs use the engine's API socket (docker: nothing to do)
make build           # tools image, alert-system image, walletd image, teranode (first time 10-20 min)
make gen N=3 SV=2    # bsv-regtest/compose.yaml + bsv-regtest/config (keys created once); SV=0 for teranodes only
make up && make wait # teranodes, SV nodes + alert sidecars, kafka, hub, arcade, merkle-service, wallet, tools
make sim-build && make sim-up         # orchestrator + GUI
open http://localhost:8600            # GUI (or `cd sim/ui && npm run dev` for live UI dev on :5173)
```

`make` picks podman if installed, otherwise docker (`make RUNTIME=docker …`). `make sim-up`
bind-mounts the engine's API socket into the orchestrator: on Docker `/var/run/docker.sock` (or
`DOCKER_HOST`'s unix path, for rootless Docker), on Podman the socket `podman info` reports.
Override with `CONTAINER_SOCKET=/path/to/socket make sim-up`.

`make up` re-renders the compose file first, so a `make gen` setting (`N`, `SV`, `INTERNAL`,
`TERANODE_TAG`) given to `make up` takes effect. Run an example scenario from the Scenarios page,
or:

```bash
curl -s -X POST -H 'Content-Type: application/json' -d '{"mode":"auto"}' \
  localhost:8600/api/scenarios/freeze-timing-skew/run | jq .id
curl -s localhost:8600/api/runs/<id> | jq '.status, .findings, [.steps[] | {name,status}]'
```

`mode: "step"` pauses before every step (Next/Abort in the GUI or `POST /api/runs/<id>/next`).

## Scenarios

Example scenarios live in [`examples/scenarios/`](examples/scenarios/). They are meant to be read,
copied and adapted:

- `freeze-timing-skew.yaml` — A gets a freeze alert early, B late, C never. A block spending the
  coin *below* the freeze window must be accepted by everyone; a block spending it *inside* the
  window must be rejected cleanly by every alert holder, once, with `UTXO_CONSENSUS_FROZEN`.
- `freeze-fork-remine.yaml` — the adversarial fork: C re-mines the below-window spend inside the
  window on an isolated fork; A and B must reject it although they record the coin as spent by
  that very transaction.
- `freeze-skew-rpc.yaml` — delivers an immediate freeze over the admin `freeze` RPC (no heights)
  and compares *how* each node rejects the offending block: verdict published or not, validated
  once or re-fetched in a loop, child block refused at once or not.
- `svnode-dust-policy.yaml` — characterises where the fleet disagrees about dust: a zero-satoshi
  bare `OP_RETURN` output is rejected by the strict-policy SV node and accepted by everyone else.
- `wallet-sustained-flow.yaml` — drives the BRC-100 wallet to send a sustained stream of
  transactions and checks the fleet keeps a single converged tip under load.
- `arcade-same-height-flip-flop.yaml` — mines competing same-height blocks to exercise arcade's
  view of a chain tip that flips between siblings.

Run `make reset` between runs: scenarios need a converged fleet with a clean alert history, and a
split caused by a finding cannot be healed by mining.

Scenarios are YAML: ordered steps (`mine`, `spend`, `submit`, `build_alert`, `push_alert`,
`rpc_freeze`, `partition`, `chaos`, `mark`, …) with polled assertions (`same_tip`, `utxo_status`,
`event`, `no_event`, `log_count` on container logs, `tip_is`, …). `should: true` turns a failing
assertion into a recorded finding instead of a stop. `${...}` expressions reference run variables,
params, roles and live node state (`${tip(A) + 2}`). Runs are stored in `sim/.data/runs/`.

**Writing your own:** [`examples/scenarios/README.md`](examples/scenarios/README.md) is a complete
authoring reference — every action, every assertion, the `${...}` expression language and its
gotchas, roles/params and the per-run override body. It is written for an AI agent generating a
scenario from a prose description.

**Custom scenario locations:** the orchestrator loads `examples/scenarios` by default. Point it
elsewhere, or load your own alongside the examples, with the `SCENARIOS` environment variable (a
comma-separated list; later directories override earlier ones by scenario id):

```bash
# raw binary
SCENARIOS=examples/scenarios,/path/to/my/scenarios ./orchestrator
# containerised simulator: mount a host directory read-only in place of the examples
SCENARIOS_DIR=/path/to/my/scenarios make sim-up
```

## Comparing builds

Pin a different image for one node and run the same scenario against both, to compare behaviour
between teranode versions (or between teranode and SV Node):

```bash
echo TERANODE_IMAGE_1=ghcr.io/bsv-blockchain/teranode:latest > bsv-regtest/.env   # node A on a published image
make reset && curl -s -X POST -d '{"mode":"auto"}' localhost:8600/api/scenarios/freeze-skew-rpc/run
rm bsv-regtest/.env && make reset                                                 # back to your local build
```

`freeze-skew-rpc` is built for this: it delivers the freeze over RPC (so it needs no shared alert
network) and reports *how* a node rejects the offending block, which is where builds tend to
differ.

## SV Node following

The SV Nodes follow the teranode chain over teranode's legacy (Bitcoin-wire) service. SV Node
syncs only from peers advertising `NODE_NETWORK`, which teranode's legacy service does when its
block persister has stored a block, and it decides that once, at start-up. The generator
therefore runs the block persister whenever SV Nodes are present, and on a fresh chain
`make wait` mines block 1 and restarts the teranodes once so they come back advertising
`NODE_NETWORK` (`make reset` goes through the same step). This works with a stock teranode build;
no patches needed. The SV Nodes can still be mined on directly to create forks, as several
scenarios do. Details: [`bsv-regtest/README.md`](bsv-regtest/README.md#sv-nodes).

## Layout

```
bsv-regtest/     the network: own Go module (github.com/bsv-blockchain/bsv-regtest), Makefile, compose, generator,
                 alertctl/stackctl, walletd, scripts, docs  -> bsv-regtest/README.md
sim/             orchestrator compose, Dockerfile, React UI (sim/ui)
cmd/orchestrator simulator backend
internal/api     REST/SSE + UI          internal/observe  fleet observer + event bus   internal/scenario  engine
internal/diag    diagnostics rules      internal/logs     container log tailing        internal/chaos     container runtime
internal/svnode  SV Node RPC client     internal/walletsvc wallet service (walletd client, sustained send)
internal/arcade  arcade client          internal/automine
examples/scenarios/  example scenario YAML + the authoring guide
go.work          workspace: this module + ./bsv-regtest (walletd is a nested module outside the workspace)
```

The `internal/{alerts,keys,teranode,topology,wallet}` packages live in `bsv-regtest/` and are
imported as `github.com/bsv-blockchain/bsv-regtest/...`; the root `go.work` makes the local copy
the source. `make test` runs both modules' tests; `make test-walletd` the nested one; `make tidy`
tidies all three with `GOWORK=off` so each module's `go.sum` stays complete for standalone builds.
