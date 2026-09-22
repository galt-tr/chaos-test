# chaos-test simulator

The orchestrator attaches to a running stack (`../bsv-regtest`) and drives it: it observes every
node, delivers alerts, injects chaos and runs scenarios. The React UI is embedded in the same
binary.

```bash
make sim-build     # image localhost/chaos-orchestrator:local (UI=0 skips the UI build)
make sim-up        # joins bsv-regtest_chaosnet/alertnet/ctlnet; API + UI on http://localhost:8600
make sim-logs
```

Requirements: the stack is up (`make up`) and `systemctl --user enable --now podman.socket`
(the orchestrator mounts `$XDG_RUNTIME_DIR/podman/podman.sock` for partitions/pause/stop).

## Pieces

- `cmd/orchestrator` — flags/env: `INVENTORY` (`/config/inventory.json`), `KEYS`, `DATA`
  (alert log, keyring, runs), `KAFKA_BROKERS`, `SCENARIOS` (mounted from `../scenarios`, re-read
  on every list/run so edits apply live), `-host-mode` (dev on the host against published
  ports; no alert delivery).
- `internal/observe` — fleet poller (teranodes: asset API every second; SV nodes: RPC
  `getblockheader`/`getrawmempool`/`getpeerinfo` plus their alert sidecar's `/health`),
  alert-sequence probes (a node's `alertAddr`, which for an SV node is its sidecar), watched
  outpoints (`/utxos/<txid>/json` on teranodes → `FROZEN/OK/SPENT`; `gettxout` +
  `queryBlacklist` on SV nodes), hub, arcade and service health, event bus. Every node carries
  a `kind` (`teranode` | `svnode`); the Fleet page renders an SV card with peers instead of
  FSM and the sidecar's sequence and unprocessed count, and the Chaos page's alert-plane cut
  targets the sidecar container.
- `internal/api` — REST + SSE (`/api/events`), key ring (`miner` = the nodes' coinbase key plus
  named victim keys), raw-signer spend/submit (per node or via arcade, Extended Format).
- `internal/scenario` — YAML scenarios, `${...}` expressions (`tip(A)`, `latest`, vars,
  params, roles), auto/step runs, polled assertions, `should` → findings, automatic healing of
  partitions the run opened, run records in `sim/.data/runs/`. `mark` records an event id and
  a timestamp; `event`/`no_event` take `since: "${m.eventId}"`, `log_count` (a regexp over a
  node's container logs, via the podman socket) takes `since: "${m.at}"` — the only way to see
  behaviour a node never publishes, such as upstream main's block re-validation loop.
- `internal/chaos` — Docker-compatible API over the podman socket (network connect/disconnect
  with pinned IP, pause/unpause, stop/start), serialised and verified.
- `sim/ui` — React + TypeScript + Vite. Dev: `npm run dev` on http://127.0.0.1:5173 proxying
  `/api` to :8600. Build: `npm run build` → `internal/api/uidist` (embedded).

## Arcade in the GUI

Arcade is a first-class entity: the header's **arcade ↗** button opens its landing page
(`http://localhost:18080/`, the API docs) in its own window; the Fleet page has an arcade card
(health document, version, arcade's own height, the embedded chaintracks tip compared with the
node tips, per-datahub health, links to health / chaintracks tip / SSE events / policy) and an
arcade chip in the chain-tips strip. The orchestrator polls arcade every 2 s (`internal/arcade`,
`snapshot.arcade`) and publishes `tip` events with node `arcade`.

**Every transaction id is a link** that opens `http://localhost:18080/tx/<txid>` in a new tab
(arcade's JSON status: `txStatus`, `blockHash`, `blockHeight`, `merklePath`; 404 JSON when
arcade never saw the tx). The `⧉` glyph next to it copies the txid. Structured txids
(watched outpoints, lookups, spend/submit results, mempools, alert funds, run variables) link
directly; hashes inside free text (event messages, assertion details, findings) link only when
they are, or abbreviate, a txid the UI has seen in structured data — so block hashes, alert
hashes and scripts stay plain. `sim/ui/src/txids.ts` holds the key lists and the tokeniser.
Host URLs come from `inventory.json` (`services.arcade.hostURL` and `urls.*`); re-run
`make gen` on an older inventory or the arcade card reports the missing chaintracks URL.

## One alert publisher log per network

Alerts are sequenced; every node accepts only `latest+1`. The orchestrator's log
(`sim/.data/alerts.json`) must hold the network's full history. `make reset` wipes chain,
hub and logs together; if you publish alerts with `alertctl` from the tools container
instead, copy its `bsv-regtest/.data/tools/alerts.json` over before starting the orchestrator.

## API cheat sheet

```
GET  /api/state                 GET  /api/events (SSE)   GET /api/events/recent?n=
POST /api/mine {node,blocks,address?}
GET  /api/alerts                POST /api/alerts/build {type,funds,...}  POST /api/alerts/push {node,sequence}
POST /api/alerts/rpc {node,action,txid,vout,start,stop,policyExpires}
GET  /api/keys  POST /api/keys {name}     POST /api/tx/spend {...}  POST /api/tx/submit {target,hex,efHex}
GET  /api/tx/{txid}?node=       GET  /api/coinbase?node=&height=   POST /api/watch {txid,vout,label}
POST /api/chaos/partition {node,plane:p2p|alert,on}   POST /api/chaos/{pause|unpause|stop|start} {node}
GET  /api/scenarios  POST /api/scenarios/{id}/run {mode,roles,params}   GET /api/runs  GET /api/runs/{id}
POST /api/runs/{id}/next  POST /api/runs/{id}/abort
```
