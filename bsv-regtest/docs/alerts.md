# The alert system

The BSV alert system lets the network's alert keys instruct nodes to freeze or unfreeze
outputs, confiscate them, ban peers, invalidate blocks or rotate the keys. Nodes learn alerts
from each other over a libp2p network; each alert is signed and strictly sequenced. This
network runs a complete private instance of it.

| actor | container | role |
|---|---|---|
| alert hub | `bsv-regtest-alert-system` | go-alert-system node: bootstrap peer of the alert network, DHT server, canonical store. `GET :3000/health`, `GET :3000/alerts`. Its RPC points at teranode 1, which has no SV-style blacklist RPCs, so its `unprocessed_alerts` counter is not an error. |
| teranode alert services | inside `bsv-regtest-teranodeN` | teranode's embedded alert service (`startAlert=true`), bootstrapped to the hub, applying alerts to the node's UTXO store. |
| SV node sidecars | `bsv-regtest-alert-svnodeJ` | one go-alert-system instance per SV node, bootstrapped to the hub, applying alerts to its SV node over RPC. `GET :4J300/health`. |
| tools | `bsv-regtest-tools` | `alertctl`: builds and signs alerts with the genesis keys and delivers them. |

Configuration: `config/alert-system/config.json` (hub), `config/alert-system/svnodeJ.json`
(sidecars), the `alert_*` lines of `config/teranode/common.env`. Keys: `genesis` (five key
pairs; the public keys are every node's `genesis_keys`), `hub`, `nodes`, `svnodes` and
`publisher` (libp2p identities) in `config/keys.json`.

## How alerts move

- Every alert-network member bootstraps to the hub (`alert_p2p_bootstrap_peer`,
  `p2p.bootstrap_peer`) and finds the others through the hub's DHT. Members sync alerts from
  each other on every discovery round (`alert_p2p_peer_discovery_interval` /
  `peer_discovery_interval`, 15 s here): whoever holds a higher sequence is asked for the
  missing alerts.
- `alertctl push --peer ADDR` delivers alerts to one member immediately over that same sync
  protocol; the rest of the network learns them on its next round unless partitioned.
- Gossip publishing (`alertctl broadcast`) is dropped by go-alert-system v0.1.x, the version
  teranode pins (bsv-blockchain/go-alert-system#171, fixed in v0.2.0). On this network alerts
  travel only over the sync stream.
- SV nodes receive alerts through their sidecars, which then call the SV node's RPC: freeze and
  unfreeze → `addToConsensusBlacklist`, confiscate → `addToConfiscationTxidWhitelist`, ban and
  unban → `setban`, invalidate → `invalidateblock`. A failed RPC apply is retried every 30 s
  (`alert_processing_interval`) and counted in the sidecar's `unprocessed_alerts`.
- Freezes can also be applied to a single teranode with its admin RPC (`scripts/rpc.sh N
  freeze '["<txid>", <vout>, ""]'`), bypassing the alert network entirely.

The alert plane is its own compose network (`alertnet`, 192.0.0.128/26). Cutting a node from
it (`scripts/partition.sh 2 alert on`; for an SV node this cuts its sidecar) stops it at its
current sequence until healed.

## Signing and sequencing

`alertctl build` signs with the first three of the five genesis keys (the alert format carries
three signatures) and appends the alert to a publisher log, `/data/alerts.json` in the tools
container, `.data/tools/alerts.json` on the host. Sequence numbers must be contiguous: a
member accepts only `latest + 1`. `build` uses `log latest + 1` unless `--seq` says otherwise;
`push` delivers everything up to `--seq` (default: log latest) that the peer is missing.

After `make clean` both the network and the log are empty and sequence 1 is next. If the log
and the network disagree (`push` reports nothing requested, or a peer refuses), compare
`alertctl hub` (network) with `alertctl log` (yours) and rebuild with an explicit `--seq`.

## alertctl reference

```
alertctl keygen ed25519|secp256k1 [-n N]        fresh keys as JSON
alertctl build <type> [flags]                   build + sign an alert, append it to the log
     freeze | unfreeze   --fund txid:vout:start:stop[:policy]   (repeatable)
     info                --message TEXT
     invalidate          --hash BLOCKHASH [--reason TEXT]
     confiscate          --enforce-at HEIGHT --tx-hex RAWTX
     ban | unban         --peer 192.0.2.1/32 [--reason TEXT]
     setkeys             --pubkeys k1,k2,k3,k4,k5
     common: --seq N  --note TEXT
alertctl push --peer ADDR [--seq N]             make a peer sync alerts up to N from the log
alertctl probe --peer ADDR                      ask a peer for its latest sequence
alertctl hub [--url URL]                        a go-alert-system node's /health and /alerts
alertctl log                                    print the log
alertctl serve                                  stay online answering sync requests
alertctl broadcast --seq N --bootstrap ADDR     gossip publish (ignored by v0.1.x members)
```

Defaults come from `config/tools.env`: `ALERTCTL_KEYS`, `ALERTCTL_LOG`, `ALERTCTL_TOPIC`
(`bitcoin_alert_system_regtest`), `ALERTCTL_PROTOCOL` (`/bitcoin/alert-system/1.0.0`),
`ALERTCTL_HUB`, `ALERTCTL_BOOTSTRAP`, and one `*_ALERT_ADDR` per member: `HUB_ALERT_ADDR`,
`TERANODEN_ALERT_ADDR`, `SVNODEJ_ALERT_ADDR` (an SV node's address is its sidecar's).

## What a freeze does on each node

A freeze fund is `txid:vout:start:stop[:policy]`, the enforcement window in block heights.
The two implementations read it differently (verified on teranode `main` and SV Node 1.2.2):

- **Teranode** (go-alert-system's handler in `services/alert/node.go`): a fund whose `stop` is
  below the node's current height is treated as an *unfreeze*; anything else freezes the output
  at once, `start` ignored. The output shows `status: FROZEN` on `/api/v1/utxos/{txid}/json`,
  a spend is refused with `403 UTXO_FROZEN (72)`, and a block spending it is rejected with
  `UTXO_CONSENSUS_FROZEN`. Upstream PR 1764 makes teranode honour the window instead.
- **SV Node** (via the sidecar's `addToConsensusBlacklist`): the output goes on the policy
  blacklist at once, so the mempool refuses the spend (`bad-txns-inputs-frozen`), and on the
  consensus blacklist for `[start, stop)`, where blocks spending it are rejected. With an
  empty window (`0:0`) only the policy part is left: the mempool refuses, blocks pass.

So `0:0` is *not* "freeze forever": at height 103 it is a no-op on teranode and a mempool-only
block on SV Node. For "frozen everywhere, now" use a far-away stop such as `0:1000000`.

An **unfreeze** alert lifts the freeze on the teranodes immediately. The SV sidecar rewrites
the blacklist entry with an empty window, which SV Node keeps on its policy blacklist, so its
mempool keeps refusing the spend until `scripts/rpc.sh svJ clearBlacklists` (blocks containing
it are accepted). That is go-alert-system v0.1.17 behaviour, not something this network
configures.

## A worked example: freeze a coin everywhere

```bash
scripts/mine.sh 1 101                 # host: coinbases 1.. are spendable
make tools                            # the rest runs in the tools container
CB=$(stackctl coinbase --height 3 | jq -r .txid)                   # a mature coinbase nobody has spent
curl -s $TERANODE1_ASSET/utxos/$CB/json | jq '.[0].status'         # "OK"
alertctl build freeze --fund $CB:0:0:1000000 | jq '{sequence, text}'
alertctl push --peer $HUB_ALERT_ADDR                               # {"before":N-1,"after":N,"delivered":[N]}
sleep 40                                                            # two discovery rounds reach every member
for p in $TERANODE1_ALERT_ADDR $TERANODE2_ALERT_ADDR $TERANODE3_ALERT_ADDR $SVNODE1_ALERT_ADDR $SVNODE2_ALERT_ADDR; do
  alertctl probe --peer $p | jq -c .latestSequence; done            # all N
curl -s $TERANODE1_ASSET/utxos/$CB/json | jq '.[0].status'         # "FROZEN"
curl -s $SVNODE1_SIDECAR/health | jq '{sequence, unprocessed_alerts}'

# try to spend it
S=$(stackctl spend --txid $CB --vout 0 --key "$(jq -r .minerWIF /config/inventory.json)")
stackctl submit --rpc $SVNODE1_RPC --hex $(echo "$S" | jq -r .hex)       # "rpc error -26: 16: bad-txns-inputs-frozen"
stackctl submit --asset $TERANODE2_ASSET --hex $(echo "$S" | jq -r .hex)  # 403 "UTXO_FROZEN (72): … utxo is frozen for <CB>:0"
stackctl submit --arcade $ARCADE_URL --hex $(echo "$S" | jq -r .efHex)    # 202 RECEIVED: arcade accepts, the datahubs refuse it

# lift it: a fund whose stop is below the current height
alertctl build unfreeze --fund $CB:0:0:0 && alertctl push --peer $HUB_ALERT_ADDR
sleep 40; curl -s $TERANODE1_ASSET/utxos/$CB/json | jq '.[0].status'   # "OK" again on the teranodes
```

On the host, `scripts/rpc.sh sv1 queryBlacklist` lists what the sidecar has put on svnode1's
blacklists (`clearBlacklists` wipes them, and is what lets the SV mempool take the spend
again after an unfreeze), and `make status` prints every node's sequence. In this run the
whole fleet was at the new sequence 31 to 38 s after the push.

For experiments where one node must stay unaware, deliver to individual members
(`alertctl push --peer $TERANODE1_ALERT_ADDR`) and cut the others from the alert plane first
(`scripts/partition.sh 2 alert on`); otherwise the next 15 s round spreads the alert. A node
that mines a block spending a coin the others have frozen splits the fleet, which is what a
freeze is supposed to do; `make clean && make up` starts over.
