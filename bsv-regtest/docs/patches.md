# Teranode patches

`make build-teranode` applies the patches in `patches/teranode/` to `TERANODE_REF` before
building the image. Both are small, both are needed for SV Node to follow a teranode inside a
private network, and neither is upstream yet (the build reports `already in <ref>` on the day
they are).

## 0002 legacy listener without a default route

`services/legacy/Server.go`: the legacy service's `Init` called `GetOutboundIP()`
unconditionally and failed when the container has no default route. That is the case on
`internal` compose networks (`make gen INTERNAL=1`), where nothing here needs a route out.
The patch skips the lookup when an explicit `legacy_listen_addresses` is configured.

## 0003 `legacy_advertiseFullNode`

`settings/legacy_settings.go`, `services/legacy/peer_server.go`: teranode's legacy service
decides its advertised service bits from the block persister's height. With the persister off
(`startBlockPersister=false`, the normal regtest setup) that height is 0, the service
advertises NODE_NETWORK_LIMITED once at startup and never revisits it, and SV Node refuses to
sync from a limited peer (`getpeerinfo` shows `services` ending in `…0420`). The patch adds a
setting, `legacy_advertiseFullNode` (default false), that makes the service announce
NODE_NETWORK from the start; `config/teranode/common.env` sets it to `true`. The startup log
line `Legacy service determined storage mode: full (… advertiseFullNode=true)` confirms it.

## 0001, no longer here

The alert P2P settings patch (configurable bootstrap peer, private-IP policy, discovery
interval and DHT mode for teranode's alert service) was merged upstream as
bsv-blockchain/teranode PR 1767 on 2026-09-18, so `main` no longer needs it. The chaos-test
harness, which builds an older PR branch, keeps it in its own tree and passes it in through
`EXTRA_PATCHES`.

## Rebasing a patch

```bash
make build-teranode TERANODE_REF=<ref>        # stops at "PATCH 000N-… DOES NOT APPLY"
cd upstream/teranode
git apply --3way ../../patches/teranode/000N-*.patch   # resolve conflicts, then:
git diff > ../../patches/teranode/000N-<same-name>.patch
cd ../.. && make build-teranode TERANODE_REF=<ref>
```

Keep the patch a plain `git diff` (no commit headers) and the file name stable; the build
only globs the directory.
