# chaos-test stack: a standalone private BSV regtest network

This directory is a complete, self-contained regtest network for alert-system testing:

- **N teranodes** (default 3), built from a teranode ref plus three small patches (alert-P2P
  settings, upstream PR bsv-blockchain/teranode#1767; two legacy-service fixes, see
  `patches/teranode/`), meshed over a private network;
- **SV Bitcoin SV Node instances** (default 2) that follow the teranodes' chain over teranode's
  legacy (Bitcoin wire) service, each with its own **go-alert-system sidecar** that applies
  alerts to it over RPC - the reference implementation to compare teranode against;
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
make gen N=3 SV=2   # writes stack/compose.yaml + stack/config/ (keys.json is created once and kept)
make up             # teranodes, SV nodes + alert sidecars, kafka, hub + the tools, arcade, merkle, wallet profiles
make wait           # blocks until every node answers (teranode health, SV node RPC, sidecar API)
make status         # best block of every node, SV peers and sidecar alert sequences, hub sequence
```

Only the network, without arcade / merkle-service / wallet:

```bash
make up PROFILES=                      # teranodes, SV nodes + sidecars, kafka, alert hub
make up PROFILES="--profile tools"     # + the tools container
make gen SV=0 && make up PROFILES=     # teranodes only (no SV nodes, legacy service off)
```

Everything is `podman compose` underneath; `cd stack && podman compose --profile tools up -d`
does the same as `make up PROFILES="--profile tools"`.

Stop / reset:

```bash
make down     # stop and remove containers, keep stack/.data (chain, alerts, wallet DB)
make clean    # down + wipe stack/.data  (podman unshare is used because .data is owned by subuids)
```

`make gen N=5 SV=1` regenerates everything for other node counts (teranodes 2..10, SV nodes
0..5); node identities, sidecar identities and alert genesis keys in `config/keys.json` are
preserved, new nodes get new keys. The SV Node image (`SVNODE_IMAGE`, default
`docker.io/bitcoinsv/bitcoin-sv:1.2.2`) is pulled, not built.

## Where things are

Host-published ports (`localhost`):

| service | ports | notes |
|---|---|---|
| teranode N | base `20000 + (N-1)*2000`: health `+0`, asset API + dashboard `+90`, propagation `+833`, RPC `+1292` | node 1: http://localhost:20090 (dashboard), http://localhost:20090/api/v1 (asset), http://localhost:21292 (RPC, basic auth `bitcoin:bitcoin`) |
| svnode J | base `40000 + (J-1)*1000`: RPC `+332`, alert sidecar API `+300` | svnode1: RPC http://localhost:40332 (`bitcoin:bitcoin`), sidecar http://localhost:40300/health |
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
stack/scripts/rpc.sh sv1 getblockchaininfo     # SV nodes: sv1 / svnode1 (RPC 40332, 41332, ...)
stack/scripts/rpc.sh sv1 queryBlacklist        # what the sidecar has frozen on svnode1
stack/scripts/rpc.sh sv1 getpeerinfo           # its outbound links to the teranodes' legacy service
stack/scripts/tips.sh                          # every node's tip (+ SV peers and sidecar alert sequence) + hub sequence
stack/scripts/partition.sh 2 alert on          # node 2 stops seeing the alert network (control plane untouched)
stack/scripts/partition.sh 2 alert off         # reconnect with its pinned IP
stack/scripts/partition.sh 3 p2p on            # node 3 loses node-to-node p2p + datahub access
stack/scripts/partition.sh svnode1 alert on    # cuts svnode1's alert SIDECAR from alertnet
stack/scripts/partition.sh svnode1 p2p on      # svnode1 loses its legacy links to the teranodes
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
| `compose.yaml` | generated for N teranodes and SV SV nodes; profiles: default (nodes, SV nodes + sidecars, kafka, hub), `tools`, `arcade`, `merkle`, `wallet` |
| `.env` (see `.env.example`) | `TERANODE_IMAGE` / `TERANODE_IMAGE_N` (per-node image), `TERANODE_MEM_LIMIT`, `SVNODE_IMAGE` / `SVNODE_IMAGE_J`, `SVNODE_MEM_LIMIT`, `ALERT_SYSTEM_IMAGE`, `TOOLS_IMAGE`, `HUB_HOST_PORT` |
| `config/keys.json` | all identities and alert genesis keys — **regtest dev keys, committed on purpose**; never reuse them elsewhere |
| `config/inventory.json` | names, pinned IPs, ports, peer ids, multiaddrs, in-network and host URLs, Kafka topics |
| `config/teranode/common.env`, `teranodeN.env` | node settings; any teranode setting can be given as an env var of the same name (shown as `[ENV]` in the startup dump) |
| `config/alert-system/config.json` | hub config (genesis keys, bootstrap = node 1, private-IP gater off) |
| `config/svnode/svnodeJ.conf` | each SV node's bitcoin.conf (regtest follower: `connect=` every teranode's legacy port, `genesisactivationheight=100` to match teranode) |
| `config/alert-system/svnodeJ.json` | each SV node's alert sidecar config (bootstrap = hub, `rpc_connections` = that SV node, retry every 30 s) |
| `config/arcade/config.yaml`, `config/merkle-service.env`, `config/wallet-infra/` | the optional services |
| `config/tools.env` | defaults for `alertctl` / `stackctl` in the tools container |
| `patches/teranode/` | patches applied at image build: 0001 alert-P2P settings (PR 1767), 0002 legacy listener without a default route, 0003 `legacy_advertiseFullNode` |
| `docker/go-alert-system.Dockerfile` | hub image (fully-qualified base images) |
| `scripts/` | `rpc.sh`, `mine.sh`, `tips.sh`, `partition.sh` |
| `spike/` | the milestone-0 single-node spike, kept for reference |

Build variables (`make build-teranode TERANODE_REF=main TERANODE_TAG=main`): `TERANODE_REF`
(default `fix/1422-height-anchored-freeze`), `TERANODE_TAG` (image tag, default `pr1764`),
`TERANODE_REPO`, `ALERT_SYSTEM_REF` (default `v0.1.17`, the version teranode pins). Every patch
in `patches/teranode/` must apply to the ref or the build stops with a clear message.

## Networks

| network | subnet | carries |
|---|---|---|
| `chaos_chaosnet` | 10.190.0.0/24 | node↔node P2P (9905), legacy Bitcoin-wire P2P teranode↔SV node (18444), Kafka, datahub access for arcade/merkle |
| `chaos_alertnet` | 192.0.0.128/26 | alert P2P: teranodes 9908, hub and SV sidecars 9906, tools. A reserved range libp2p treats as *public*, so go-alert-system's private-IP gater and DHT filter pass |
| `chaos_ctlnet` | 10.191.0.0/24 | control plane: RPC 9292 (teranode) / 18332 (SV), asset 8090, propagation 8833, health 8000, hub and sidecar APIs 3000, arcade/merkle/wallet APIs |

All IPs are pinned (`ipv4_address`) and recorded in the inventory: teranodes `.11+`, SV nodes
`.21+`, sidecars ctlnet `.31+` / alertnet `.151+`. Partitioning a node means disconnecting it
from `chaosnet` (peer plane) or `alertnet` (alert plane; for an SV node that is its sidecar);
`ctlnet` always stays, so RPC and asset access survive every partition. All three networks are
`internal` (no egress) and host-published ports still work.

## SV nodes and alert sidecars

SV Node (`bitcoind`) speaks the classic Bitcoin wire protocol, not teranode's libp2p mesh.
Teranode bridges the two with its **legacy service**, enabled on every teranode when SV nodes
are generated (`startLegacy=true`, listener `0.0.0.0:18444` on chaosnet). SV Node downloads
blocks only from peers it dialed, so each SV node's `bitcoin.conf` lists every teranode (and
the other SV nodes) as `connect=` targets; teranode-mined blocks are announced to it over the
wire protocol and its bodies are served from the announcing teranode's asset API. Two teranode
behaviours needed patches for this to work in the private stack: the legacy service refused
to start without a default route (0002), and it announced itself as a pruned peer whenever the
block persister height was zero, which SV Node never syncs from (0003, `legacy_advertiseFullNode`).

SV Node has no embedded alert service; on a real network an operator runs go-alert-system
next to it. The stack does the same: `alert-svnodeJ` is a go-alert-system instance that joins
the private alert network (bootstrap = the hub) and applies every alert to its SV node over RPC
- freeze/unfreeze → `addToConsensusBlacklist`, confiscate → `addToConfiscationTxidWhitelist`,
ban → `setban`, invalidate → `invalidateblock`. Its `/health` shows the `sequence` it holds and
`unprocessed_alerts`, the alerts whose RPC apply failed (retried every 30 s). Because the sidecar
speaks the same sync protocol as the teranodes' alert services, `alertctl push/probe` and the
fleet's alert probe address the SV node through its sidecar's multiaddr (`SVNODEJ_ALERT_ADDR`).
Check what is frozen on an SV node with `rpc.sh svJ queryBlacklist`.

Chain parameters: SV Node's regtest defaults match teranode's (go-chaincfg) except the
Genesis-rules activation height, set to 100 in the conf to match teranode; teranode's
Chronicle activation at height 200 has no SV counterpart, so keep experiments below height
200 between resets. Mining stays on the teranodes; SV nodes are followers.

### One SV node enforces standard-output policy

The **last** SV node (`svnode2` in the default fleet) is generated with
`acceptnonstdoutputs=0`; every other node keeps SV Node's permissive post-Genesis default. With
only one SV node (`-sv 1`) nothing is made strict, so a permissive node always remains for
comparison.

This exists because the fleet genuinely disagrees about dust. After Genesis a bare
`OP_RETURN <data>` output is **spendable**, so a zero-satoshi one is dust and the strict node
refuses it with `64: dust`; the provably unspendable `OP_FALSE OP_RETURN <data>` form is exempt
at any value. Teranode implements no dust rule and takes both, and arcade relays both happily
(`RECEIVED` → `ACCEPTED_BY_NETWORK` → `MINED`) — so a transaction the rest of the fleet mines
never enters the strict node's mempool, not even by relay from a peer holding it.

`scenarios/svnode-dust-policy.yaml` pins that down as a characterisation test. Note that
`-dustrelayfee` and `-dustlimitfactor` are rejected by SV Node 1.2.2 as removed options, so
`acceptnonstdoutputs` is the only remaining lever over the dust rule. If a scenario needs a
fleet that agrees about dust, drop the flag from `cmd/gen` rather than editing the generated
conf, which `make gen` overwrites.

## How alerts move

- Every teranode's alert service bootstraps to the hub (`alert_p2p_bootstrap_peer`) and finds
  the other nodes through the hub's DHT. Nodes and hub sync alerts from each other on every
  discovery round (`alert_p2p_peer_discovery_interval`, 15 s here).
- `alertctl push` delivers alerts to one chosen node immediately over the same sync protocol;
  the rest of the fleet learns them on its next round unless partitioned.
- Gossip publishing (`alertctl broadcast`) is dropped by go-alert-system v0.1.x, the version
  teranode pins (bsv-blockchain/go-alert-system#171, fixed in v0.2.0). On this stack alerts
  therefore travel only over the sync stream.
- SV nodes receive alerts through their sidecars, which sync like any other alert-network
  member and then call the SV node's RPC; a sidecar cut from alertnet stops at its current
  sequence until healed.
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
- **An SV node stays at height 0**: `rpc.sh svJ getpeerinfo` must show outbound peers whose
  `services` include NODE_NETWORK (`…01`); `…0420` means the teranodes announce
  NODE_NETWORK_LIMITED, i.e. the image lacks patch 0003 or `legacy_advertiseFullNode=true`
  is missing from `common.env`. Teranode's own tests document an intermittent IBD stall on SV
  Node 1.2.0 (headers received, `getdata` never sent); the default image is 1.2.2.
- **`alert-svnodeJ` API not up**: go-alert-system starts its web server only after it has
  connected to two alert peers; `make wait` reports this and moves on. It catches up on the
  next discovery round.
- **Alert sequence mismatch** (`push` says "nothing requested"): the tools log and the network
  must agree on the sequence; `alertctl hub` shows the network's latest, `alertctl log` yours.
  After `make clean`, also remove `stack/.data/tools/alerts.json` (part of `.data`, so already
  gone) - and the simulator's `sim/.data/alerts.json` if it was used.

## Reusing the stack on another network

The inventory is network-agnostic (`network` is `regtest` today). A teratestnet variant drops
the local teranodes and points arcade / merkle-service at public nodes, with real alert keys
supplied through the environment instead of `keys.json`. That is planned, not done: the keys in
this repository are for regtest only.
