# Networking

Three compose networks, all with pinned addresses (`ipv4_address`), all recorded in
`config/inventory.json` (`networks`, per-node `chaosIP`/`alertIP`/`ctlIP`, `services.*`).

| network | subnet | carries |
|---|---|---|
| `bsv-regtest_chaosnet` | 10.190.0.0/24 | teranode ↔ teranode libp2p (9905), teranode's legacy Bitcoin-wire service ↔ SV nodes (18444), Kafka (9092), arcade and merkle-service reaching the datahubs and the p2p mesh, wallet-infra ↔ wallet-db |
| `bsv-regtest_alertnet` | 192.0.0.128/26 | the alert network: teranode alert services 9908, hub and sidecars 9906, tools |
| `bsv-regtest_ctlnet` | 10.191.0.0/24 | control plane: teranode RPC 9292, asset 8090, propagation 8833, health 8000; SV RPC 18332; hub and sidecar APIs 3000; arcade, merkle-service, wallet-infra, walletd APIs. Everything with a host port lives here |

alertnet uses a reserved range (192.0.0.0/24 is IETF protocol assignments) that libp2p
classifies as *public*, so go-alert-system's private-IP connection gater and DHT address
filter pass without patches, while `alert_p2p_allow_private_ips=false` keeps the alert hosts
from ever advertising or accepting chaosnet/ctlnet addresses. That is what makes an alert-plane
partition airtight.

Address plan (last octet): teranodes `.11+` (alertnet `.141+`), SV nodes `.21+` (their
sidecars ctlnet `.31+`, alertnet `.151+`), hub alertnet `.130` / ctlnet `.30`, kafka chaosnet
`.5`, arcade `.40`, merkle-service `.41`, wallet-infra `.42`, wallet-db `.43`, walletd `.52`,
tools ctlnet `.50` / alertnet `.180`.

## Host ports

| service | host port |
|---|---|
| teranode N | base `20000 + (N-1)*2000`: health `+0`, asset API and dashboard `+90`, propagation `+833`, RPC `+1292` |
| SV node J | RPC `40000 + (J-1)*1000 + 332`; its alert sidecar API `40000 + (J-1)*1000 + 300` |
| alert hub | `HUB_HOST_PORT` 3000 |
| arcade | `ARCADE_HOST_PORT` 18080, then 18081 health, 18082 events, 18083 chaintracks |
| merkle-service | `MERKLE_HOST_PORT` 18090 |
| wallet-infra | `WALLET_HOST_PORT` 18100 |
| walletd | `WALLETD_HOST_PORT` 18700 |

The node ranges are not variables; pick a smaller `N`/`SV` if they collide. Kafka is not
published: `$RUNTIME exec bsv-regtest-kafka-shared rpk topic list`.

## `INTERNAL=1`

Nothing here needs the internet at run time: no public bootstrap peers on the main p2p network
(DHT off, static mesh), alerts bootstrapped from the hub. One thing still reaches out when it
can: teranode's embedded alert service runs go-alert-system's DHT in client mode, and that
client bootstraps from libp2p's default public peer list as well as from the hub, so with
egress each teranode also holds connections to public DHT nodes (observed: about a dozen per
node; its log says `connected to 44 peers, peerstore has 346 peers`). The alert topic is
regtest-specific and alerts only move over direct sync streams, so nothing of yours is
published there, but it is traffic. `make gen INTERNAL=1` renders `internal: true` on all
three networks so the containers have no egress at all; the alert services then find each
other through the hub alone. Verified on Podman, where host-published ports keep working on
internal networks. On Docker the behaviour of published ports on internal networks has changed
across versions (moby#36174); Docker 28 documents host-local access, but that is unverified
here, so `INTERNAL` is off by default.

## Partitions

```bash
scripts/partition.sh 2 alert on        # teranode 2 leaves alertnet (its alert service stops syncing)
scripts/partition.sh 2 alert off       # rejoins with its pinned IP
scripts/partition.sh 3 p2p on          # teranode 3 leaves chaosnet: no peers, no legacy, no datahub access for arcade
scripts/partition.sh svnode1 alert on  # cuts svnode1's alert SIDECAR from alertnet
scripts/partition.sh svnode1 p2p on    # svnode1 loses its legacy links to the teranodes
```

The script runs `$RUNTIME network disconnect` / `connect --ip <pinned IP>` on the container,
reading the network names and addresses from the inventory (`RUNTIME` defaults to podman if
installed, else docker). ctlnet is never touched, so RPC and asset access survive every
partition. Teranode's intra-node gRPC addresses are pinned to `localhost` in `common.env`
because the image's defaults resolve the container name, which could pick any of its three
addresses and break a node from the inside when a plane is cut.

## The legacy bridge to SV Node

SV Node speaks the classic Bitcoin wire protocol, not teranode's libp2p mesh. Teranode bridges
the two with its **legacy service**, which the generator enables on every teranode when SV
nodes are present (`startLegacy=true`, `legacy_listen_addresses=0.0.0.0:18444`,
`legacy_allowSyncCandidateFromLocalPeers=true`, `legacy_advertiseFullNode=true`). SV Node
downloads blocks only from peers it dialed, so each `config/svnode/svnodeJ.conf` lists every
teranode and the other SV nodes as `connect=` targets (`getpeerinfo` shows them outbound with
`services` ending in `…01`, NODE_NETWORK). Teranode-mined blocks are announced over the wire
protocol and served from the announcing teranode. Chain parameters match teranode's regtest
(go-chaincfg): `genesisactivationheight=100`; teranode's Chronicle activation at height 200
has no SV counterpart, so keep experiments below height 200 between resets. Mining stays on
the teranodes. Two teranode patches make the bridge work in a private network
([`patches.md`](patches.md)).

## Kafka

Redpanda (`bsv-regtest-kafka-shared`, `kafka-shared:9092` on chaosnet) is shared by the
teranodes; each node's topics carry its name: `blocks-`, `blocks-final-`, `subtrees-`,
`txmeta-`, `legacy-inv-`, `invalid-subtrees-`, `rejectedtx-` and `invalid-blocks-teranodeN`.
The last two are the node's verdicts (the inventory carries them as `kafkaRejectedTx` /
`kafkaInvalidBlocks`).

```bash
$RUNTIME exec bsv-regtest-kafka-shared rpk topic list
$RUNTIME exec bsv-regtest-kafka-shared rpk topic consume invalid-blocks-teranode1 -n 1
```

## SELinux labels on bind mounts

Read-only configuration mounts use `:ro,z` (shared label): `./config` is mounted by several
containers, and with the private `:Z` label the last container to start would lock the others
out (`Permission denied` inside the tools container was the symptom). Per-service data
directories under `.data/` use `:Z`. Both flags are no-ops without SELinux and are accepted by
Docker and Podman.
