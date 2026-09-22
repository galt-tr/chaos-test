# Upstream `main` vs PR #1764: how a node handles a block spending a coin it froze

Scenario `freeze-skew-rpc`, 2026-09-17, regtest, 3 nodes, same steps on both fleets. The
freeze reaches node A over the admin `freeze` RPC in its 3-parameter form (the only form
`main` accepts; the PR stores it as (0, 0) = "enforced at every height", i.e. the legacy
immediate consensus freeze). Node B never learns of it and mines a block spending the coin
(`blockBelow`), then one more block on top of it. Node C follows B. On both builds A must and
does reject `blockBelow`; the comparison is in how.

| observation (node A) | upstream `main` (`ghcr.io/bsv-blockchain/teranode:latest`, RPC version 1.3.0) | PR #1764 (`fix/1422-height-anchored-freeze` @ `2d78850`) |
|---|---|---|
| run | `20260917-060344-freeze-skew-rpc-1`: passed, **5 findings** | `20260917-060041-freeze-skew-rpc-1`: passed, **0 findings** |
| mempool (policy) | 403 `UTXO_FROZEN (72)` | 403 `UTXO_FROZEN (72)` |
| verdict for `blockBelow` on Kafka `invalid-blocks-teranode1` | **none**; log: `SERVICE_ERROR (59): failed block validation BlockFound … UTXO_FROZEN (72): [Spend] utxo is frozen for <txid>:0` | one message: `block contains invalid transactions … TX_INVALID (31) … [Spend] utxo is frozen for <txid>:0` |
| validations of `blockBelow` in the 15 s after its announcement | 4 | 2 |
| B mines a child block: re-validations of `blockBelow` in the next 60 s | **16** (catch-up fetches headers from B, re-downloads and re-validates the same block every ~4 s; 22 over the run) | 0 |
| verdict for the child block | none | immediate: `parent block <blockBelow> is invalid`, ban score added to B |
| A's tip | stays on its own tip (fleet split A vs B+C) | stays on its own tip (fleet split A vs B+C, by the operator's intent) |
| the frozen output on A | FROZEN | FROZEN |

Evidence: `docs/upstream/evidence/main-rejects-blockBelow-and-refetch-loop.log` (first
validation attempt on `main` and the loop after the child block, from the earlier manual
reproduction, run `20260917-054807-freeze-skew-rpc-2`); the run records under
`sim/.data/runs/` carry every assertion detail.

What the comparison shows: the PR turns "spends a frozen output" from an internal
`SERVICE_ERROR` that leaves the block in limbo (so catch-up keeps re-fetching it, issue
#1422's loop) into a proper block-invalid verdict that is stored, published and inherited by
descendants. The fleet split itself is unavoidable when only one node holds an immediate
freeze; removing it is what the height-anchored alert window is for, which `freeze-timing-skew`
exercises on the PR image (fleet agreement below the window, one clean rejection inside it).

Reproduce:

```bash
make reset && curl -s -X POST -d '{"mode":"auto"}' localhost:8600/api/scenarios/freeze-skew-rpc/run
echo TERANODE_IMAGE_1=ghcr.io/bsv-blockchain/teranode:latest > bsv-regtest/.env
make reset && curl -s -X POST -d '{"mode":"auto"}' localhost:8600/api/scenarios/freeze-skew-rpc/run
rm bsv-regtest/.env && make reset
```
