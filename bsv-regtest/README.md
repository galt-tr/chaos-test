# bsv-regtest

A private BSV regtest network in containers: **teranode**, **Bitcoin SV Node**, **arcade**
(transaction broadcaster), **merkle-service**, a **BRC-100 wallet** (go-wallet-toolbox storage
server plus a small HTTP wallet) and the **alert system**, on Docker or Podman. Clone, build,
mine, broadcast.

What you get by default:

- 3 teranodes built from source (plus two small patches, see [`docs/patches.md`](docs/patches.md)),
  meshed over a private network;
- 2 SV Nodes that follow the teranodes' chain over teranode's legacy (Bitcoin wire) service, each
  with a **go-alert-system sidecar** that applies alerts to it over RPC;
- Redpanda (Kafka) for the teranodes; a go-alert-system **alert hub** bootstrapping a private alert
  network;
- **arcade** with its embedded chaintracks, **merkle-service**, **wallet-infra** (go-wallet-toolbox
  storage server on Postgres) and **walletd** (an HTTP wallet holding the dev wallet identity);
- a **tools** container with `alertctl` (build, sign and deliver alerts) and `stackctl` (mine,
  spend, submit).

Everything is pinned (keys, IPs, images) and rendered from one generator; after `make build`
nothing needs the internet. The network is also the layer under the
[chaos-test](https://github.com/galt-tr/chaos-test) harness, which drives it with scenarios.

License: Apache-2.0. **The keys in this repository are development keys; read
[Security](#security-development-keys) before exposing anything.**

## Architecture

```
 ┌──────────────────────── chaosnet 10.190.0.0/24 (peer plane) ───────────────────────────┐
 │  teranode1 ◄── libp2p 9905 ──► teranode2 ◄──► teranode3          kafka-shared (redpanda) │
 │      │  legacy service = Bitcoin wire :18444  (SV nodes dial in, connect= only)          │
 │  svnode1 ◄─────────────────► svnode2                                                    │
 │                                                                                         │
 │  arcade ──── datahub /api/v1 ────► teranodes      merkle-service ── libp2p ──► teranodes│
 │    ▲ ▲──── /watch registration + bearer-token callbacks ────────────┘                   │
 │    │ POST /tx  +  /chaintracks (headers)                                                │
 │  wallet-infra (BRC-100 storage server, postgres) ◄── BRC-103 ── walletd (HTTP wallet)   │
 └─────────────────────────────────────────────────────────────────────────────────────────┘
 ┌──────── alertnet 192.0.0.128/26 (alert plane) ────────┐  ┌──── ctlnet 10.191.0.0/24 ────┐
 │  alert hub ◄── sync stream every 15 s ──► teranodes'  │  │ RPC, asset API, health,      │
 │  alert services, alert-svnodeJ (──RPC──► svnodeJ),    │  │ sidecar APIs, tools container│
 │  tools (alertctl push / probe)                        │  │ (all host-published ports)   │
 └───────────────────────────────────────────────────────┘  └──────────────────────────────┘
 host: 20090 teranode1 dashboard · 18080 arcade · 18090 merkle · 18100 wallet-infra · 18700 walletd · 3000 hub
```

Three planes: peer traffic (chaosnet), alert traffic (alertnet, a reserved range libp2p treats
as public so go-alert-system's private-IP gater passes) and control (ctlnet, everything with a
host port). Mining happens on teranodes; SV nodes are followers.

## Prerequisites

- **Docker Engine with `docker compose` v2, or Podman 5.x with podman-compose 1.4+** (rootless is
  fine; Docker Desktop works). `make` picks podman if installed, otherwise docker; force one with
  `make RUNTIME=docker …`.
- **Go 1.26+** only to regenerate the configuration (`make gen`). Not needed to build or run.
- `curl` and `jq`; `python3` for `scripts/tips.sh` and `scripts/partition.sh`.
- **RAM:** about 10 GB free for the default fleet (3 × 3 GB teranode caps, 2 × 2 GB SV Node,
  ~1.5 GB for Redpanda, arcade, merkle-service and the wallet). Lower `N`/`SV` on smaller hosts.
- **Disk:** several GB for the teranode build; `.data/` grows slowly on regtest.
- **Time:** the first `make build` takes 10–20 minutes (teranode compiles from source); later
  builds reuse the checkout and layer cache. `make up && make wait` takes a few minutes.
- Network access at **build time only** (ghcr.io, docker.io, github.com).
- SELinux enforcing is fine (bind mounts carry `:z` / `:Z` labels, a no-op elsewhere).

## Quick start

```bash
git clone https://github.com/bsv-blockchain/bsv-regtest.git && cd bsv-regtest
make build          # tools, alert-system and walletd images + teranode from source (10-20 min, once)
make up             # 3 teranodes, 2 SV nodes + alert sidecars, kafka, alert hub, arcade, merkle-service, wallet, tools
make wait           # blocks until the teranodes and SV nodes answer (then checks the sidecar APIs, best effort)
scripts/mine.sh 1 101   # regtest coinbase maturity is 100: block 1's coinbase is now spendable
make status         # every node's tip, SV peers, alert sequences
```

Smaller fleets:

```bash
make up PROFILES=                      # network only: teranodes, SV nodes + sidecars, kafka, alert hub
make up PROFILES="--profile tools"     # + the tools container
make gen SV=0 && make up               # teranodes only (no SV nodes; legacy service off)
```

Your first transaction, broadcast through arcade. `make tools` opens a shell in the tools
container, where `stackctl` and `alertctl` default to teranode 1 and `$ARCADE_URL`,
`$TERANODE2_ASSET`, `$SVNODE1_RPC`, … are preset from `config/tools.env`:

```bash
make tools
CB=$(stackctl coinbase --height 1 | jq -r .txid)                 # block 1's coinbase, paid to the miner key
KEY=$(stackctl newkey | jq -r .privateKeyHex)
TX=$(stackctl spend --txid $CB --vout 0 --key "$(jq -r .minerWIF /config/inventory.json)" --to $KEY)
stackctl submit --arcade $ARCADE_URL --hex $(echo "$TX" | jq -r .efHex)
# {"via":"arcade","status":202,"accepted":true,"body":{"txid":"<txid>","status":202,"txStatus":"RECEIVED"}}
```

Then, on the host:

```bash
curl -s localhost:18080/tx/<txid> | jq '{txStatus, blockHeight}'     # ACCEPTED_BY_NETWORK within a second
scripts/mine.sh 1 1
curl -s localhost:18080/tx/<txid> | jq '{txStatus, blockHeight, merklePath}'
# {"txStatus":"MINED","blockHeight":102,"merklePath":"…"}      # a few seconds later; merklePath is a BUMP (hex)
```

Fund the wallet and spend from it:

```bash
stackctl topup                                                     # in the tools container: coinbase → deposit address → mined → proven → credited
curl -s localhost:18700/v1/state | jq '{balance, coins}'           # host: {"balance":100000,"coins":1}
curl -s -X POST localhost:18700/v1/tx -H 'Content-Type: application/json' \
     -d '{"shape":"opreturn","data":"hello bsv-regtest"}' | jq '{txid, status}'
```

Dashboards: http://localhost:20090 (teranode1), http://localhost:18080/ (arcade: its landing page
is the API reference), http://localhost:18090/ (merkle-service, with a STUMP/BUMP visualizer).

## What is running

| container | host ports | notes |
|---|---|---|
| `bsv-regtest-teranodeN` | base `20000+(N-1)*2000`: health `+0`, asset API + dashboard `+90`, propagation `+833`, RPC `+1292` | node 1: `:20000/health`, `:20090` dashboard and `:20090/api/v1/…`, `:21292` RPC (basic auth `bitcoin:bitcoin`); node 2: 22000/22090/22833/23292; node 3: 24000/24090/24833/25292 |
| `bsv-regtest-svnodeJ` | `40000+(J-1)*1000+332` | RPC 40332, 41332 (`bitcoin:bitcoin`) |
| `bsv-regtest-alert-svnodeJ` | `40000+(J-1)*1000+300` | the SV node's alert sidecar: `/health`, `/alerts` on 40300, 41300 |
| `bsv-regtest-alert-system` | 3000 | alert hub: `/health` → `{sequence, active_peers, unprocessed_alerts, …}`, `/alerts` |
| `bsv-regtest-arcade` | 18080 API + landing page, 18081 liveness, 18082 SSE `/events`, 18083 `/chaintracks/v2/*` | |
| `bsv-regtest-merkle-service` | 18090 | `GET /` dashboard, `/health`, `/api/lookup/{txid}`, `POST /watch` |
| `bsv-regtest-wallet-infra` | 18100 | BRC-100 storage server; a bare `GET /` returns `401 {"error":"authentication required"}` by design |
| `bsv-regtest-walletd` | 18700 | `/healthz`, `/v1/deposit`, `/v1/state`, `/v1/outputs`, `/v1/actions`, `POST /v1/tx`, `POST /v1/internalize` |
| `bsv-regtest-kafka-shared` | not published | `$RUNTIME exec bsv-regtest-kafka-shared rpk topic list` |
| `bsv-regtest-tools` | – | `make tools` |

In-network addresses (pinned IPs, peer ids, multiaddrs, URLs) are in `config/inventory.json`,
the file every tool and script reads. Teranode's health endpoint is on the health port, not
under `/api/v1`.

## Playing with the components

### Arcade

Arcade takes a transaction, validates it, fans it out to the teranode datahubs and tracks a
status per txid. Its landing page (http://localhost:18080/) is generated from the server's
route table and is the authoritative API reference. Standard-format transactions are accepted
on this network (`/policy` reports `standardFormatSupported: true`); Extended Format saves
arcade a round trip to the datahubs and is what `stackctl spend` prints as `efHex`. Arcade only
knows transactions it received: a coinbase txid returns `404 {"error":"transaction not found"}`.

```bash
curl -s localhost:18080/health | jq '{healthy, blockHeight, datahub_urls}'
curl -s -X POST localhost:18080/tx -H 'Content-Type: text/plain' --data "$HEX"        # or application/octet-stream, or JSON {"rawTx":…}
curl -s localhost:18080/tx/$TXID | jq '{txStatus, blockHeight, blockHash, merklePath}'
curl -N 'localhost:18082/events?callbackToken=demo'          # status events as SSE; ": keepalive" every 15 s
curl -s localhost:18083/chaintracks/v2/tip | jq '{height, hash}'
```

Status lifecycle: `RECEIVED → SENT_TO_NETWORK → ACCEPTED_BY_NETWORK → SEEN_ON_NETWORK →
SEEN_MULTIPLE_NODES → MINED → IMMUTABLE`, with `REJECTED`, `DOUBLE_SPEND_ATTEMPTED`,
`PENDING_RETRY` and `STUMP_PROCESSING` on the side. Details, callback headers, events and the
chaintracks endpoints: [`docs/arcade.md`](docs/arcade.md).

### merkle-service

merkle-service follows teranode's blocks and subtrees over libp2p, builds STUMP/BUMP proofs
and calls arcade back for the transactions arcade registered. Its `/api/lookup/{txid}` returns
callback **registrations**, not proofs; proofs come from any teranode
(`/api/v1/merkle_proof/<txid>/json`) and as the `merklePath` BUMP on arcade's `GET /tx/{txid}`.

```bash
curl -s localhost:18090/health                                     # {"status":"healthy","details":{"backend":"connected"}}
curl -s localhost:18090/api/lookup/$TXID                           # {"txid":"…","callbackUrls":[…]}
curl -s localhost:20090/api/v1/merkle_proof/$TXID/json | jq '{blockHeight, path}'
```

### Wallet: wallet-infra and walletd

wallet-infra is go-wallet-toolbox's BRC-100 **storage server** (`POST /` JSON-RPC and
`/storage/v1/*` REST) behind BRC-103 mutual authentication, so plain `curl` gets a 401 by
design; connect with go-wallet-toolbox's storage client or the TypeScript `StorageClient`.
walletd is a small HTTP wallet that holds `walletUserKey` from `config/keys.json`, does the
BRC-103 handshake for you and exposes the everyday operations. wallet-infra broadcasts through
arcade and reads headers from arcade's chaintracks. A BRC-100 wallet credits a payment only
from atomic BEEF with a verifiable merkle proof, so funding means paying the deposit address,
mining, fetching the proof from arcade and internalizing; `stackctl topup` does all of it.

```bash
curl -s localhost:18700/healthz
curl -s localhost:18700/v1/deposit | jq '{address, suggestedSatoshis}'   # what `stackctl topup` pays
curl -s localhost:18700/v1/state | jq '{connected, network, address, balance, coins}'
curl -s localhost:18700/v1/outputs | jq '.outputs[] | {outpoint, satoshis}'
curl -s -X POST localhost:18700/v1/tx -H 'Content-Type: application/json' \
     -d '{"shape":"payment","satoshis":1000,"to":"<address>","labels":["demo"]}' | jq '{txid, status}'
curl -s 'localhost:18700/v1/actions?include=1&limit=5' | jq '{total, buckets, actions: [.actions[] | {txid, status}]}'
```

Shapes, funding flow and the storage-server protocols: [`docs/wallet.md`](docs/wallet.md).

### Teranode

Each teranode publishes a dashboard and the asset API on the same port, JSON-RPC on `+1292`,
health on `+0`. Read RPCs are served from a cache (`getchaintips` for 300 s), so observe chain
state through the asset API. Any teranode setting can be given as an environment variable of
the same name in `config/teranode/*.env` (the startup log shows `[ENV]` next to it); the keys
are documented in the teranode repository under `docs/references/settings/`.

```bash
curl -s localhost:20000/health | jq '{status, services: [.services[].service]}'
curl -s localhost:20090/api/v1/bestblockheader/json | jq '{height, hash}'
curl -s localhost:20090/api/v1/block/height/1/json | jq '{height, coinbase: .coinbase_tx.txid}'
curl -s localhost:20090/api/v1/utxos/$TXID/json | jq '.[] | {vout, status, satoshis}'     # OK | FROZEN | SPENT
curl -s localhost:20090/api/v1/txmeta/$TXID/json | jq '{blockHeights, fee, isCoinbase}'
scripts/rpc.sh 1 getblockchaininfo | jq .result
scripts/rpc.sh 1 freeze '["<txid>", 0, ""]'          # admin RPC freeze on ONE node, bypassing the alert network
scripts/mine.sh 2 1 <address>                          # mine one block on node 2 paying an address
```

### SV Nodes

SV Node speaks the classic Bitcoin wire protocol; teranode bridges it with its legacy service,
which the generator enables whenever SV nodes are present. SV Node downloads blocks only from
peers it dialed, so each node's `bitcoin.conf` (`config/svnode/`) lists every teranode as a
`connect=` target. `genesisactivationheight=100` matches teranode's regtest parameters. Mining
stays on the teranodes. The **last** SV node is generated with `acceptnonstdoutputs=0`, so the
fleet always contains one node with strict standard-output policy: a zero-satoshi bare
`OP_RETURN <data>` output is dust there (`64: dust`) while every other node, teranode included,
accepts it. Two teranode patches make this bridge work in a private network
([`docs/patches.md`](docs/patches.md)).

```bash
scripts/rpc.sh sv1 getblockchaininfo | jq '.result | {blocks, bestblockhash}'
scripts/rpc.sh sv1 getpeerinfo | jq '.result[] | {addr, services, inbound}'   # services …0021: NODE_NETWORK bit set
scripts/rpc.sh sv1 queryBlacklist                                             # what the sidecar has frozen here
```

### Alert system

The hub is a go-alert-system node acting as the alert network's bootstrap peer, DHT server and
canonical store; the teranodes run their embedded alert service; each SV node gets a
go-alert-system sidecar that applies alerts over RPC (freeze → `addToConsensusBlacklist`,
confiscate → `addToConfiscationTxidWhitelist`, ban → `setban`, invalidate → `invalidateblock`).
Alerts are signed with three of the five genesis keys in `config/keys.json` and are strictly
sequenced; the tools container keeps the publisher log in `.data/tools/alerts.json`. They
travel over the peer-to-peer sync stream (gossip publishing is dropped by go-alert-system
v0.1.x, the version teranode pins).

```bash
curl -s localhost:3000/health | jq '{sequence, active_peers, unprocessed_alerts}'
make tools
CB=$(stackctl coinbase --height 3 | jq -r .txid)   # a mature coinbase nobody has spent
alertctl build freeze --fund $CB:0:0:1000000       # sign alert #N freezing CB:0 (window in heights; see docs/alerts.md)
alertctl push --peer $HUB_ALERT_ADDR               # the hub; every member has it within ~40 s
alertctl push --peer $TERANODE1_ALERT_ADDR         # …or one member only, right now
alertctl probe --peer $SVNODE1_ALERT_ADDR          # an SV node is addressed through its sidecar
scripts/rpc.sh sv1 queryBlacklist                  # host: the sidecar applied it to svnode1
```

The worked example (freeze a coin everywhere, watch every node refuse the spend, unfreeze),
what the height window means on each implementation, per-node delivery and the full
`alertctl` reference: [`docs/alerts.md`](docs/alerts.md).

## Configuration and regeneration

`compose.yaml` and everything under `config/` are **generated** by `cmd/gen` from
`cmd/gen/templates.go` and committed, so `make up` works without Go. Edit the templates, never
the outputs.

```bash
make gen N=5 SV=1        # teranodes 2..10, SV nodes 0..5; keys.json is created once and kept
make gen INTERNAL=1      # egress-free networks (see below)
```

`make` variables: `N`, `SV`, `RUNTIME` (`podman`|`docker`), `PROFILES` (default
`--profile tools --profile arcade --profile merkle --profile wallet`), `TERANODE_REF` (default
`main`), `TERANODE_TAG` (default: the ref with `/` replaced), `TERANODE_REPO`, `ALERT_SYSTEM_REF`
(`v0.1.17`, the version teranode pins), `SVNODE_IMAGE`, `INTERNAL`, `EXTRA_PATCHES`.

`.env` (copy `.env.example`; read by compose): per-node image overrides `TERANODE_IMAGE_N` /
`SVNODE_IMAGE_J`, memory caps (`TERANODE_MEM_LIMIT` 3g, `SVNODE_MEM_LIMIT` 2g, …), the locally
built image names, host ports (`HUB_HOST_PORT`, `ARCADE_HOST_PORT`, `MERKLE_HOST_PORT`,
`WALLET_HOST_PORT`, `WALLETD_HOST_PORT`).

Images: built here (`localhost/bsv-regtest/{teranode,alert-system,tools,walletd}`), pulled
(arcade, merkle-service at a pinned digest, go-wallet-toolbox, postgres, redpanda, bitcoin-sv).
Building teranode: [`docs/building-teranode.md`](docs/building-teranode.md).

**`INTERNAL=1`** renders the three compose networks as `internal` (no egress at all). Nothing
in the network *needs* the internet at run time, but with egress available teranode's embedded
alert service also dials public libp2p DHT peers (go-alert-system's DHT client bootstraps from
libp2p's default peer list as well as from the hub; on this host each teranode held connections
to about a dozen public addresses). Nothing of yours travels there, since the alert topic is
regtest-specific and alerts only move over direct sync streams, but for a network that cannot
reach the internet at all use `INTERNAL=1`. Verified on Podman, where host-published ports keep
working on internal networks; on Docker 28 published ports on internal networks should be
reachable from the host itself, but that is unverified here, which is why it is opt-in.

## How the pieces connect

- **Networks:** chaosnet 10.190.0.0/24 (teranodes `.11+`, SV nodes `.21+`, kafka `.5`, arcade
  `.40`, merkle `.41`, wallet-infra `.42`, wallet-db `.43`, walletd `.52`), alertnet
  192.0.0.128/26 (hub `.130`, teranodes `.141+`, sidecars `.151+`, tools `.180`), ctlnet
  10.191.0.0/24 (same last octets; sidecars `.31+`, tools `.50`). Names are
  `bsv-regtest_chaosnet` etc. and are recorded in the inventory.
- **Legacy bridge:** teranode's legacy service listens on chaosnet `:18444`; SV nodes dial it.
- **Alert sidecars:** speak the same sync protocol as teranode's alert service; an SV node's
  alert address in the inventory is its sidecar's multiaddr.
- **Arcade ↔ merkle-service:** arcade registers `/watch` with its callback URL and token (from
  `keys.json`); merkle-service calls back with the token as a bearer.
- **Wallet:** wallet-infra posts to arcade and follows arcade's chaintracks; walletd talks to
  wallet-infra over BRC-103. On regtest arcade's chaintracks bootstraps from a teranode's asset
  API at startup and keeps its headers on the arcade volume.

More in [`docs/networking.md`](docs/networking.md).

## Stopping, wiping, upgrading

```bash
make down                    # stop and remove the containers, keep .data/ (chain, alerts, wallet DB)
make clean                   # down + wipe .data/ through a busybox container (files belong to other uids)
make build-teranode TERANODE_REF=v1.4.0 && make clean && make up   # another teranode build
make gen N=4 SV=1 && make up # a different fleet; make clean first if the node set shrank
```

Never swap a teranode image under an existing `.data/teranodeN`: builds differ in their sqlite
schemas. `make clean` first.

## Troubleshooting

- **First `make build` is slow (10–20 min, several GB).** Teranode compiles from source. Later
  builds reuse `upstream/teranode` and the layer cache.
- **Both SV nodes exited (139) with `boost::condition_variable::do_wait_until failed in
  pthread_cond_timedwait: Invalid argument`** after the host suspended or its clock jumped. A
  Boost issue in bitcoind, no data loss; the services restart on failure, or run `make up`.
- **An SV node stays at height 0.** `scripts/rpc.sh sv1 getpeerinfo`: each peer's `services`
  must have the NODE_NETWORK bit (last hex digit odd, `…0021` here). `…0420` means the
  teranode image lacks patch 0003 or
  `legacy_advertiseFullNode=true` is missing from `config/teranode/common.env`. SV Node 1.2.0 has
  an intermittent initial-sync stall; the default image is 1.2.2.
- **`alert-svnodeJ: API not up yet`** from `make wait`. go-alert-system opens its web server only
  after two alert peers are connected; it catches up on the next 15 s discovery round.
- **`Permission denied` under `.data/`.** SV Node and Postgres run as their own users, so their
  files belong to subordinate uids on the host. `make clean` wipes through a container. Do not
  `chown` the tree while the network runs.
- **Port already in use.** Every service port except the teranode/SV node ranges is a variable in
  `.env`; for the node ranges pick a smaller `N`/`SV` or stop what holds the port.
- **Docker: `localhost/bsv-regtest/…` not found.** The image was not built on this host; run the
  matching `make build-*` target. Locally built images carry `pull_policy: never`, so nothing is
  ever pulled from a registry called `localhost`.
- **`short-name resolution enforced but cannot prompt`** (Podman build). An image reference without
  a registry; every image here is fully qualified, so qualify any you add.
- **A teranode never becomes healthy.** `make wait` prints the last log lines after five minutes;
  the usual cause is memory pressure (lower `N` or raise `TERANODE_MEM_LIMIT`).
- **`alertctl push` says "nothing requested".** The publisher log (`alertctl log`) and the network
  (`alertctl hub`) disagree on the latest sequence; after `make clean` both start empty.
- **A teranode still reports the old alert sequence** a few seconds after a push to the hub.
  Members pull alerts on their discovery rounds (15 s); the whole fleet is level within about
  half a minute. `alertctl push --peer $TERANODEn_ALERT_ADDR` delivers to one member at once.
- **The hub shows `unprocessed_alerts` > 0.** Its RPC points at a teranode, which has no SV-style
  blacklist RPCs; that counter is not an error. The sidecars' counters are the ones to watch.
- **Nodes disagree after an experiment.** Usually the experiment working; `make clean && make up
  && make wait` starts over.

## Layout

```
Makefile              runtime detection (RUNTIME=podman|docker); gen/build/up/down/wait/status/clean/test
compose.yaml          GENERATED for N teranodes + SV nodes; profiles: default, tools, arcade, merkle, wallet
config/               GENERATED: inventory.json (the contract), keys.json (dev keys, kept), teranode/*.env,
                      svnode/*.conf, alert-system/*.json, arcade/config.yaml, merkle-service.env,
                      wallet-infra/infra-config.yaml, tools.env
cmd/gen               the generator (templates.go is the source of truth)
cmd/alertctl          build/sign/push/probe alerts        cmd/stackctl   mine/spend/submit/topup
alerts/ keys/ teranode/ topology/ wallet/    Go packages behind the CLIs (importable: github.com/bsv-blockchain/bsv-regtest/...)
walletd/              HTTP wallet (own Go module on go-wallet-toolbox)
patches/teranode/     0002 legacy listener without a default route, 0003 legacy_advertiseFullNode  -> docs/patches.md
docker/               alert-system.Dockerfile, tools.Dockerfile
scripts/              rpc.sh mine.sh tips.sh partition.sh
docs/                 arcade.md wallet.md alerts.md networking.md building-teranode.md patches.md
.data/                runtime state (gitignored)          upstream/  source checkouts for image builds (gitignored)
```

History: this network grew out of the chaos-test harness's stack; the legacy-service findings
recorded in `docs/patches.md` came from a hand-run spike in September 2026.

## Security: development keys

`config/keys.json` is committed **on purpose** and contains private keys: the alert genesis
keys, the libp2p identities of the hub, nodes and sidecars, the miner key, the wallet server and
user keys, arcade's callback token and the teranode admin API key. Every RPC is `bitcoin:bitcoin`.
Every container uses these values verbatim.

- Never reuse any of them outside a throwaway regtest.
- Never expose the host ports beyond localhost.
- For fresh keys: `rm config/keys.json && make gen` (then `make clean && make up`).

## Contributing and license

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Apache License 2.0, see [`LICENSE`](LICENSE) and
[`NOTICE`](NOTICE); the patches under `patches/teranode/` modify Teranode and remain under
Teranode's license.
