# Spikes

## Milestone 0 (2026-09-17): one patched teranode + alert hub, offline
`compose.yaml`, `gen-env.sh`: the first proof that teranode's alert service bootstraps to a
go-alert-system hub on the reserved alertnet subnet with no internet. Kept for reference.

## SV Node alongside teranode (2026-09-17)
Goal: a Bitcoin SV Node container follows the teranodes' regtest chain and receives alerts
through a go-alert-system sidecar. Run by hand before the generator learned about SV nodes.

Findings:
- `startLegacy=true` on the current compose networks makes teranode fail at start: the legacy
  service calls `GetOutboundIP()` (a UDP dial to 8.8.8.8, i.e. a route lookup) before listening,
  and our networks are always `internal` (podman-compose treats `${INTERNAL:-false}` as the
  truthy string "false"). Patch `0002-legacy-listen-without-default-route.patch` only consults
  the outbound IP when no `legacy_listen_addresses` is configured.
- With `legacy_listen_addresses=0.0.0.0:18444` the SV node (image 1.2.2, `connect=` to every
  teranode) completes the version handshake with all three teranodes but never asks for
  headers: teranode advertised service bits `0x420` = NODE_NETWORK_LIMITED | BSV, because the
  legacy service derives "full vs pruned" from the block persister height (0 = pruned) once at
  start-up, and SV Node treats a peer without NODE_NETWORK as a client. Patch
  `0003-legacy-advertise-full-node-setting.patch` adds `legacy_advertiseFullNode` (the upstream
  e2e test carries this exact line commented out).
- Block timestamps are not a problem: teranode floors the candidate time at median-time-past
  plus one, so burst `generate` is valid for SV Node.
- A hand-run go-alert-system v0.1.17 sidecar (`alert-svnode1.json`: bootstrap = hub, RPC =
  the SV node on ctlnet) joined the alert network in ~12 s, synced the network's alert #1 and
  applied it: `queryBlacklist` on the SV node listed both funds with their [106, 116) window,
  `/health.unprocessed_alerts` = 0. `queryBlacklist`, `clearBlacklists`,
  `addToConsensusBlacklist` exist on SV Node 1.2.2 (`bitcoin-cli help`).
Files: `svnode1.conf` (the conf the generator template was derived from), `alert-svnode1.json`.
