# Building the teranode image

Teranode is built from source so that the two patches in `patches/teranode/` can be applied
([`patches.md`](patches.md)). `make build-teranode` does everything:

1. clones `TERANODE_REPO` at `TERANODE_REF` into `upstream/teranode` (shallow), or, if the
   checkout exists, fetches the ref and resets the tree to it (`upstream/teranode` is a
   disposable build checkout: local changes are dropped);
2. applies every `patches/teranode/*.patch` and, when `EXTRA_PATCHES` names a directory, every
   patch in it. A patch that is already contained in the ref is reported and skipped; one that
   does not apply stops the build with `PATCH … DOES NOT APPLY`;
3. builds the image with teranode's own Dockerfile and the published base images
   (`ghcr.io/bsv-blockchain/teranode-base:build-latest` / `run-latest`) as
   `localhost/bsv-regtest/teranode:$(TERANODE_TAG)`, with `GIT_VERSION` set to
   `<tag>-bsv-regtest` so a node's dashboard says what it runs.

| variable | default | meaning |
|---|---|---|
| `TERANODE_REF` | `main` | branch, tag or commit to build |
| `TERANODE_TAG` | the ref with `/` replaced by `-` | image tag; `make gen` writes `localhost/bsv-regtest/teranode:$(TERANODE_TAG)` into `compose.yaml`, so pass the same value to both |
| `TERANODE_REPO` | `https://github.com/bsv-blockchain/teranode` | |
| `EXTRA_PATCHES` | empty | a second patch directory (the chaos-test harness uses it for a patch its PR branch needs) |

```bash
make build-teranode                                   # main
make build-teranode TERANODE_REF=v1.4.0               # a release tag → localhost/bsv-regtest/teranode:v1.4.0
make gen TERANODE_TAG=v1.4.0 && make clean && make up  # run it
```

The first build compiles teranode and takes 10 to 20 minutes and several GB; later builds reuse
the checkout and the layer cache. Network access is needed only here (github.com, ghcr.io).

## Switching images

Never swap a teranode image under an existing `.data/teranodeN`: builds differ in their sqlite
schemas, and a node that opens another build's database fails at startup. `make clean` first.

One node on another image, without rebuilding: `.env`

```
TERANODE_IMAGE_2=ghcr.io/bsv-blockchain/teranode:latest
```

Compose pulls that image for node 2 (the teranode service has no `pull_policy: never`). What a
published image can and cannot do in this network:

- Since teranode PR 1767 (merged 2026-09-18) the alert P2P settings this network relies on
  (`alert_p2p_bootstrap_peer`, `alert_p2p_allow_private_ips`, `alert_p2p_peer_discovery_interval`,
  `alert_p2p_dht_mode`) are upstream, so a published image built after that date joins the
  private alert network. An older image ignores them and its alert service never bootstraps;
  freeze it through the admin RPC instead (`scripts/rpc.sh N freeze …`).
- Without patch 0003 (`legacy_advertiseFullNode`) SV nodes do not sync from that node, because
  its legacy service advertises NODE_NETWORK_LIMITED while the block persister is off. They
  still sync from the patched nodes.
- Without patch 0002 the legacy service fails at startup when the container has no default
  route, i.e. with `INTERNAL=1`.

## The other images

- `make build-alert-system`: go-alert-system at `ALERT_SYSTEM_REF` (`v0.1.17`, the version
  teranode pins, so hub, sidecars and the embedded services speak the same protocol) from
  `docker/alert-system.Dockerfile`, which uses fully qualified base images. Image
  `localhost/bsv-regtest/alert-system:<ref>`.
- `make build-tools`: `alertctl` and `stackctl` from this module (`docker/tools.Dockerfile`,
  `CGO_ENABLED=0`). Image `localhost/bsv-regtest/tools:local`.
- `make build-walletd`: `walletd/Dockerfile`, its own Go module. Image
  `localhost/bsv-regtest/walletd:local`.
- Pulled: `docker.io/bitcoinsv/bitcoin-sv:1.2.2` (`SVNODE_IMAGE`, a `make gen` setting;
  per-node `SVNODE_IMAGE_J` in `.env`), `ghcr.io/bsv-blockchain/arcade:latest`
  (`ARCADE_IMAGE`), merkle-service at a pinned digest, `ghcr.io/bsv-blockchain/go-wallet-toolbox`,
  `docker.io/library/postgres`, `docker.io/redpandadata/redpanda` (`REDPANDA_IMAGE`) and
  `docker.io/library/busybox` for `make clean`.

Locally built images carry `pull_policy: never`: if one is missing, compose fails with
"image not found" instead of trying a registry called `localhost`. Run the matching
`make build-*`.
