# chaos-test stack: a standalone private BSV regtest network

This directory is a complete, self-contained regtest network for alert-system testing:

- **N teranodes** (default 3), built from a teranode ref plus a small alert-P2P settings patch
  (upstream PR bsv-blockchain/teranode#1767), meshed over a private network;
- **Redpanda** (Kafka) shared by the nodes;
- a **go-alert-system** node acting as the hub of a private alert network;
- optionally **arcade** (transaction processor with embedded chaintracks), **merkle-service**,
  and a **go-wallet-toolbox** wallet server on Postgres;
- a **tools** container with `alertctl` (build, sign, deliver alerts) and `stackctl` (mine,
  spend, submit).

Nothing here needs the internet at run time, and nothing here depends on the simulator/GUI in
`../sim`. You can use this network on its own with the scripts and CLIs described below; the
simulator is an optional layer that attaches to it later.

## Prerequisites

- **podman 5.x** with `podman compose` (podman-compose 1.4+); rootless is fine. Docker with
  `docker compose` is untested (the compose file uses only standard keys plus one
  `x-podman` extension).
- **Go 1.24+** on the host, only to run the config generator (`go run` fetches the toolchain
  the module asks for). Node.js is **not** needed for the stack.
- **RAM**: about 8 GB free for 3 nodes (each node is capped at 3 GB by `TERANODE_MEM_LIMIT`),
  plus ~1 GB for Redpanda, arcade, merkle-service and the wallet.
- **Disk**: the teranode build image is large (several GB during the build); `.data/` grows
  slowly on regtest.
- **Network access at build time only** (ghcr.io, docker.io, github.com) to pull base images
  and clone teranode / go-alert-system. After `make build` the stack runs offline.
- SELinux enforcing is fine (bind mounts use `:Z`). The podman API socket is *not* required
  for the stack (only the simulator's partition/pause actions use it).

## Quick start

```bash
git clone git@github.com:galt-tr/chaos-test.git && cd chaos-test
make build          # images: tools, go-alert-system hub, patched teranode (10-20 min the first time)
make gen N=3        # writes stack/compose.yaml + stack/config/ (keys.json is created once and kept)
make up             # nodes, kafka, hub + the tools, arcade, merkle and wallet profiles
make wait           # blocks until every node answers its health port
make status         # best block of every node + hub alert sequence
```

Only the network, without arcade / merkle-service / wallet:

```bash
make up PROFILES=                      # teranodes, kafka, alert hub
make up PROFILES="--profile tools"     # + the tools container
```

Everything is `podman compose` underneath; `cd stack && podman compose --profile tools up -d`
does the same as `make up PROFILES="--profile tools"`.

Stop / reset:

```bash
make down     # stop and remove containers, keep stack/.data (chain, alerts, wallet DB)
make clean    # down + wipe stack/.data  (podman unshare is used because .data is owned by subuids)
```

`make gen N=5` regenerates everything for another node count (2..10); node identities and
alert genesis keys in `config/keys.json` are preserved, new nodes get new keys.

## Where things are

Host-published ports (`localhost`):

| service | ports | notes |
|---|---|---|
| teranode N | base `20000 + (N-1)*2000`: health `+0`, asset API + dashboard `+90`, propagation `+833`, RPC `+1292` | node 1: http://localhost:20090 (dashboard), http://localhost:20090/api/v1 (asset), http://localhost:21292 (RPC, basic auth `bitcoin:bitcoin`) |
| alert hub | 3000 | `GET /health` (`sequence`, `active_peers`), `GET /alerts` |
| arcade | 18080 API + landing page, 18081 health, 18082 SSE `/events`, 18083 chaintracks `/chaintracks/v2/*` | `GET /tx/{txid}`, `POST /tx` (Extended Format hex) |
| merkle-service | 18090 | |
| wallet-infra | 18100 | go-wallet-toolbox JSON-RPC |

In-network addresses (pinned IPs, peer ids, multiaddrs, URLs) are in `config/inventory.json`,
the contract every tool reads. Kafka is not published to the host; use
`podman exec chaos-kafka-shared rpk …` (topics `rejectedtx-teranodeN`, `invalid-blocks-teranodeN`).

## Everyday operations

```bash
stack/scripts/mine.sh 1 101                    # mine 101 blocks on node 1 (regtest coinbase maturity is 100)
stack/scripts/mine.sh 2 1 <address>            # mine to a specific address
stack/scripts/rpc.sh 1 getblockchaininfo       # any JSON-RPC: rpc.sh <node> <method> '[params]'
stack/scripts/rpc.sh 1 freeze '["<txid>", 0, ""]'          # admin RPC freeze (immediate, every height)
stack/scripts/rpc.sh 1 freeze '["<txid>", 0, "", 120, 130, false]'   # height-anchored (PR #1764 image only)
stack/scripts/tips.sh                          # every node's tip (asset API, uncached) + hub sequence
stack/scripts/partition.sh 2 alert on          # node 2 stops seeing the alert network (control plane untouched)
stack/scripts/partition.sh 2 alert off         # reconnect with its pinned IP
stack/scripts/partition.sh 3 p2p on            # node 3 loses node-to-node p2p + datahub access
make logs                                      # follow all container logs
podman logs -f chaos-teranode1                 # one node
```

Read RPCs are served from a cache (`getchaintips` for 300 s), so observe chain state through the
asset API instead:

```bash
curl -s localhost:20090/api/v1/bestblockheader/json | jq '{height, hash}'
curl -s localhost:20090/api/v1/utxos/<txid>/json | jq '.[] | {vout, status}'   # OK | FROZEN | SPENT per output
curl -s localhost:20090/api/v1/txmeta/<txid>/json | jq '{blockHeights, frozen}'
curl -s localhost:20090/api/v1/block/height/105/json | jq .hash
```

### Alerts and transactions from the tools container

`make tools` opens a shell in `chaos-tools`, where `alertctl` and `stackctl` have the stack's
addresses pre-set from `config/tools.env` (`$TERANODE1_ASSET`, `$TERANODE1_ALERT_ADDR`,
`$HUB_ALERT_ADDR`, `$RPC_USER`, …). Alerts are signed with the three genesis keys in
`config/keys.json`; the log of everything built lives in `/data/alerts.json`
(`stack/.data/tools/alerts.json` on the host) and must stay in sequence with the network.

```bash
alertctl                                        # usage
CB=$(stackctl coinbase --asset $TERANODE1_ASSET --height 1 | jq -r .txid)
alertctl build freeze --fund $CB:0:120:130      # sign alert #N freezing CB:0 over heights [120, 130)
alertctl push --peer $TERANODE1_ALERT_ADDR --seq 1   # deliver to ONE node over the alert sync stream
alertctl probe --peer $TERANODE2_ALERT_ADDR     # which sequence does node 2 hold?
alertctl hub                                    # hub /health + /alerts
alertctl log                                    # what has been built so far
stackctl newkey                                 # fresh key (WIF, hex, address, locking script)
stackctl spend --asset $TERANODE1_ASSET --txid $CB --vout 0 --key <WIF|hex> --to <key|address>
stackctl submit --asset $TERANODE2_ASSET --hex <rawtx>       # to ONE node (403 + UTXO_FROZEN when frozen there)
stackctl submit --arcade $ARCADE_URL --hex <efHex>          # via arcade, which needs the Extended Format hex that `spend` prints as efHex
```

The coinbase key (`minerWIF` in `config/inventory.json` / `keys.json`) unlocks every mined
coinbase, so a funded transaction is one `stackctl spend` away after 101 blocks.

### A worked example: one node freezes, the others do not

This is the shape of teranode issue #1422 done by hand (the simulator's scenarios automate it).

```bash
make tools
stack/scripts/mine.sh 1 101 &&                             # (on the host) fund the miner key
K=$(stackctl newkey); KEY=$(echo "$K" | jq -r .privateKeyHex)
CB=$(stackctl coinbase --asset $TERANODE1_ASSET --height 1 | jq -r .txid)
P=$(stackctl spend --asset $TERANODE1_ASSET --txid $CB --vout 0 --key "$(jq -r .minerWIF /config/inventory.json)" --to $KEY)
stackctl submit --asset $TERANODE1_ASSET --hex $(echo "$P" | jq -r .hex)
# mine it (host): stack/scripts/mine.sh 1 1 ; then freeze P:0 on node 1 only, over a future window
PT=$(echo "$P" | jq -r .txid)
alertctl build freeze --fund $PT:0:110:120
alertctl push --peer $TERANODE1_ALERT_ADDR --seq 1
curl -s $TERANODE1_ASSET/utxos/$PT/json | jq '.[0].status'   # FROZEN on node 1
curl -s $TERANODE2_ASSET/utxos/$PT/json | jq '.[0].status'   # OK on node 2 (until its next 15 s sync round)
S=$(stackctl spend --asset $TERANODE2_ASSET --txid $PT --vout 0 --key $KEY)
stackctl submit --asset $TERANODE1_ASSET --hex $(echo "$S" | jq -r .hex)   # 403 UTXO_FROZEN (72)
stackctl submit --asset $TERANODE2_ASSET --hex $(echo "$S" | jq -r .hex)   # accepted (node 2 is unaware)
```

Mine the spend on node 2 (`stack/scripts/mine.sh 2 1`) below the window and node 1 accepts the
block; mine it inside the window and node 1 rejects it with a `UTXO_CONSENSUS_FROZEN` verdict
on `invalid-blocks-teranode1`. Use `partition.sh 2 alert on` before the push to keep node 2
unaware for longer than one sync round.

## Configuration

`make gen` renders everything from `cmd/gen/templates.go`; edit the templates, not the outputs.

| Path | What |
|---|---|
| `compose.yaml` | generated for N nodes; profiles: default (nodes, kafka, hub), `tools`, `arcade`, `merkle`, `wallet` |
| `.env` (see `.env.example`) | `TERANODE_IMAGE` / `TERANODE_IMAGE_N` (per-node image), `TERANODE_MEM_LIMIT`, `ALERT_SYSTEM_IMAGE`, `TOOLS_IMAGE`, `HUB_HOST_PORT`, `INTERNAL` |
| `config/keys.json` | all identities and alert genesis keys — **regtest dev keys, committed on purpose**; never reuse them elsewhere |
| `config/inventory.json` | names, pinned IPs, ports, peer ids, multiaddrs, in-network and host URLs, Kafka topics |
| `config/teranode/common.env`, `teranodeN.env` | node settings; any teranode setting can be given as an env var of the same name (shown as `[ENV]` in the startup dump) |
| `config/alert-system/config.json` | hub config (genesis keys, bootstrap = node 1, private-IP gater off) |
| `config/arcade/config.yaml`, `config/merkle-service.env`, `config/wallet-infra/` | the optional services |
| `config/tools.env` | defaults for `alertctl` / `stackctl` in the tools container |
| `patches/teranode/` | the alert-P2P settings patch applied at image build |
| `docker/go-alert-system.Dockerfile` | hub image (fully-qualified base images) |
| `scripts/` | `rpc.sh`, `mine.sh`, `tips.sh`, `partition.sh` |
| `spike/` | the milestone-0 single-node spike, kept for reference |

Build variables (`make build-teranode TERANODE_REF=main TERANODE_TAG=main`): `TERANODE_REF`
(default `fix/1422-height-anchored-freeze`), `TERANODE_TAG` (image tag, default `pr1764`),
`TERANODE_REPO`, `ALERT_SYSTEM_REF` (default `v0.1.17`, the version teranode pins). The patch
must apply to the ref or the build stops with a clear message.

## Networks

| network | subnet | carries |
|---|---|---|
| `chaos_chaosnet` | 10.190.0.0/24 | node↔node P2P (9905), Kafka, datahub access for arcade/merkle |
| `chaos_alertnet` | 192.0.0.128/26 | alert P2P: teranodes 9908, hub 9906, tools. A reserved range libp2p treats as *public*, so go-alert-system's private-IP gater and DHT filter pass |
| `chaos_ctlnet` | 10.191.0.0/24 | control plane: RPC 9292, asset 8090, propagation 8833, health 8000, hub 3000, arcade/merkle/wallet APIs |

All IPs are pinned (`ipv4_address`) and recorded in the inventory. Partitioning a node means
disconnecting it from `chaosnet` (peer plane) or `alertnet` (alert plane); `ctlnet` always stays,
so RPC and asset access survive every partition. Set `INTERNAL=true` in `.env` to make every
network egress-free (host ports then stop working; use `podman exec chaos-tools …`).

## How alerts move

- Every teranode's alert service bootstraps to the hub (`alert_p2p_bootstrap_peer`) and finds
  the other nodes through the hub's DHT. Nodes and hub sync alerts from each other on every
  discovery round (`alert_p2p_peer_discovery_interval`, 15 s here).
- `alertctl push` delivers alerts to one chosen node immediately over the same sync protocol;
  the rest of the fleet learns them on its next round unless partitioned.
- Gossip publishing (`alertctl broadcast`) is dropped by go-alert-system v0.1.x, the version
  teranode pins (bsv-blockchain/go-alert-system#171, fixed in v0.2.0). On this stack alerts
  therefore travel only over the sync stream.
- Freezes can also be applied per node with the admin RPC (`rpc.sh N freeze …`), bypassing
  the alert network; that is the only path for an unpatched upstream image, whose alert
  service cannot bootstrap without internet access.

## Arcade's chaintracks on regtest

arcade embeds go-chaintracks, which learns headers only from p2p block announcements (and
backfills from the announcing node's asset API when a parent is missing). A freshly created
container therefore sits at genesis (height 0) until the *next* block is mined - forever on an
idle regtest - and the header state defaulted to `~/.chaintracks` inside the container, lost
on every recreate. The generated `config/arcade/config.yaml` sets `chaintracks.bootstrap_url`
(node 1's asset API, synced at startup) and `chaintracks.storage_path: /data/chaintracks` (on
the arcade volume). Check with `curl localhost:18083/chaintracks/v2/tip`. Known limitation
outside this repo: the go-chaintracks HTTP client used by wallet-infra subscribes to
`/v2/tip/stream` once and never reconnects, so after an arcade restart the wallet's cached tip
goes stale until wallet-infra is restarted (BEEF verification still fetches headers live).

## Comparing builds

`TERANODE_IMAGE_<N>=ghcr.io/bsv-blockchain/teranode:latest` in `.env` runs node N on upstream
`main` (pre-fix); `make clean && make up` (or `make reset` when the simulator is installed)
recreates the node on the new image with a fresh chain - the sqlite schemas of the two builds
differ, so never swap the image under an existing data directory. That image lacks the settings
patch, so inside the private stack (no egress) its alert service never bootstraps: drive
freezes on it with the admin RPC (`scripts/rpc.sh N freeze '["<txid>", <vout>, ""]'`) or, from
the simulator, scenario `freeze-skew-rpc`.

## Troubleshooting

- **`short-name resolution enforced but cannot prompt`** during a build: an image reference
  without a registry. Every image here is fully qualified; if you add one, qualify it.
- **A node never becomes healthy**: `podman logs chaos-teranodeN`. The most common cause is
  memory pressure (lower N or raise `TERANODE_MEM_LIMIT`). The generated settings already
  include the admin gRPC key and raised asset rate limits that catch-up between nodes needs.
- **Nodes disagree after an experiment**: that is usually the experiment working. To start
  clean, `make clean && make up && make wait`.
- **`Permission denied` on `stack/.data`**: files are owned by a subuid; `make clean` uses
  `podman unshare rm -rf`.
- **Changed N or a template**: `make gen N=… && make up` (containers whose config changed are
  recreated; `make clean` first if the node set shrank).
- **Alert sequence mismatch** (`push` says "nothing requested"): the tools log and the network
  must agree on the sequence; `alertctl hub` shows the network's latest, `alertctl log` yours.
  After `make clean`, also remove `stack/.data/tools/alerts.json` (part of `.data`, so already
  gone) - and the simulator's `sim/.data/alerts.json` if it was used.

## Reusing the stack on another network

The inventory is network-agnostic (`network` is `regtest` today). A teratestnet variant drops
the local teranodes and points arcade / merkle-service at public nodes, with real alert keys
supplied through the environment instead of `keys.json`. That is planned, not done: the keys in
this repository are for regtest only.
