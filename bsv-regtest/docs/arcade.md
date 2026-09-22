# Arcade

Arcade is the transaction broadcaster of this network: it validates a transaction, fans it out
to the teranode datahubs (`datahub_urls` in `config/arcade/config.yaml`, one per teranode),
tracks a status per txid and, once merkle-service has proven the transaction, attaches its
merkle path. It runs in `mode: all` (API, processor, callbacks and the embedded chaintracks
header server in one container).

| listener | host port | what |
|---|---|---|
| API | 18080 | `POST /tx`, `GET /tx/{txid}`, `/policy`, `/health`, and the landing page `GET /` |
| health | 18081 | `GET /health` → `{"status":"ok"}` (liveness only) |
| events | 18082 | `GET /events?callbackToken=…` server-sent status events |
| chaintracks | 18083 | `GET /chaintracks/v2/*` header server used by the wallet |

The landing page at http://localhost:18080/ is generated from the server's route table and is
the authoritative API reference for the image you are running. In-network URLs are
`services.arcade` in `config/inventory.json`; the tools container has `$ARCADE_URL`.

## Submitting

```bash
# any of the three bodies; text/plain hex is what `stackctl submit --arcade` sends
curl -s -X POST localhost:18080/tx -H 'Content-Type: text/plain' --data "$HEX"
curl -s -X POST localhost:18080/tx -H 'Content-Type: application/octet-stream' --data-binary @tx.bin
curl -s -X POST localhost:18080/tx -H 'Content-Type: application/json' -d "{\"rawTx\":\"$HEX\"}"
# → 202 {"txid":"…","status":202,"txStatus":"RECEIVED"}
```

Optional headers: `X-CallbackUrl` (arcade posts status changes there), `X-CallbackToken` (sent
back as a bearer token) and `X-FullStatusUpdates: true` (every status, not just the final
ones). The network accepts standard-format transactions (`GET /policy` reports
`standardFormatSupported: true`, zero fee, no minimum fee); Extended Format (BIP-239, what
`stackctl spend` prints as `efHex`) carries the inputs' source data and spares arcade a
round trip to the datahubs. Arcade only knows transactions it received: a coinbase or a
transaction sent straight to a node returns `404 {"error":"transaction not found"}`.

## Status lifecycle

```
RECEIVED → SENT_TO_NETWORK → ACCEPTED_BY_NETWORK → SEEN_ON_NETWORK → SEEN_MULTIPLE_NODES → MINED → IMMUTABLE
                                                                    ↘ REJECTED · DOUBLE_SPEND_ATTEMPTED · PENDING_RETRY · STUMP_PROCESSING
```

`MINED` is set when merkle-service delivers the proof (see below), not when a node mines the
block, so it trails the block by a few seconds. `UNKNOWN` is the zero value. A transaction
submitted and mined in the same second can miss the block: arcade answers the submit before
the transaction has reached a node's block assembly, and on regtest the next block only comes
when you mine it. `stackctl topup` therefore waits until the node reports the transaction
(`GET /api/v1/txmeta/{txid}/json`) before sealing a block.

```bash
curl -s localhost:18080/tx/$TXID | jq '{txStatus, blockHeight, blockHash, timestamp}'
curl -s localhost:18080/tx/$TXID | jq -r .merklePath     # BUMP (BRC-74) hex, once MINED
curl -s localhost:18080/tx/$TXID | jq -r .rawTx
```

## Events

```bash
curl -N 'localhost:18082/events?callbackToken=demo'
```

One event per status change as SSE; `: keepalive` every 15 s; reconnect with `Last-Event-ID`
to resume. wallet-infra subscribes here (`events_url` in its config) to learn when its
transactions are mined.

## Arcade and merkle-service

Arcade registers every transaction it accepts with merkle-service (`POST /watch` with its
`callback_url` and `callback_token`, both in `config/arcade/config.yaml`; the token comes from
`arcadeCallbackToken` in `config/keys.json`). merkle-service follows the teranodes' blocks and
subtrees over libp2p, builds the proof and calls
`http://arcade:8080/api/v1/merkle-service/callback` with the token as a bearer. Both sides
allow private IPs on this network (`callback.allow_private_ips`, `CALLBACK_ALLOW_PRIVATE_IPS`).

```bash
curl -s localhost:18090/health                       # {"status":"healthy","details":{"backend":"connected"}}
curl -s localhost:18090/api/lookup/$TXID             # {"txid":"…","callbackUrls":["http://10.190.0.40:8080/api/v1/merkle-service/callback"]}
```

`/api/lookup/{txid}` answers with the registered callbacks, not the proof. Proofs are on
arcade (`merklePath`) and on every teranode (`/api/v1/merkle_proof/{txid}/json`, fields
`blockHeight` and `path`; not for coinbases). The merkle-service dashboard at
http://localhost:18090/ visualises STUMP and BUMP structures.

## Chaintracks on regtest

Arcade embeds go-chaintracks. It learns headers only from block announcements on the p2p
network, so a freshly created container would sit at genesis until the *next* block is mined,
and its header state would default to `~/.chaintracks` inside the container, lost on every
recreate. The generated config therefore sets `chaintracks.bootstrap_url` (teranode 1's asset
API, synced once at startup, `bootstrap_mode: api`) and `chaintracks.storage_path:
/data/chaintracks` on the arcade volume.

```bash
curl -s localhost:18083/chaintracks/v2/network                 # "regtest"
curl -s localhost:18083/chaintracks/v2/height
curl -s localhost:18083/chaintracks/v2/tip | jq '{height, hash}'
curl -s localhost:18083/chaintracks/v2/header/height/1 | jq .
curl -s localhost:18083/chaintracks/v2/header/hash/$HASH | jq .
curl -N localhost:18083/chaintracks/v2/tip/stream               # SSE, one event per new tip
curl -s localhost:18080/api/v1/blocks/processing-status | jq .  # arcade's own view of recent blocks
```

`/chaintracks/v2/headers` is a binary range endpoint (80-byte headers), not JSON.

Known limitation outside this repository: the go-chaintracks HTTP client used by wallet-infra
subscribes to `/v2/tip/stream` once and never reconnects. After an arcade restart the wallet's
cached tip goes stale until wallet-infra is restarted (`$RUNTIME restart
bsv-regtest-wallet-infra`); BEEF verification keeps working because it fetches headers live.

## Regtest tuning in the generated config

- `validator.accept_zero_fee: true`, `min_fee_per_kb: 0`: teranode mines with
  `minminingtxfee=0`; `stackctl spend` still pays a 500-satoshi fee by default.
- `validator.standard_format_supported: true`: standard-format submissions are accepted and
  the inputs are fetched from the datahubs.
- `chaintracks_server.tie_scan_min_interval_ms: 500` and `bump_builder.reconciler.interval_ms:
  2000`: arcade's defaults assume ten-minute blocks; on regtest blocks arrive on demand.
- `p2p.bootstrap_peers`: the three teranodes, `dht_mode: "off"`, `allow_private_urls: true`.
- `kafka.backend: memory`, `store.backend: pebble` under `/data`: arcade keeps its own state on
  the `.data/arcade` volume and does not use the teranodes' Redpanda.
