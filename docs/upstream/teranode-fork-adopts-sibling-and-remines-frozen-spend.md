# teranode PR #1764 follow-up: after judging a fork invalid, the alert-holding node stays on the fork's prefix and mines the frozen spend itself

Status: **draft for the PR author**. First observed 2026-09-17 on the PR #1764 build `2d78850`
(regtest, 3 nodes, chaos-test harness scenario `freeze-fork-remine` plus one manual step), then
**reproduced deterministically from a clean chain** by the scenario itself (run
`20260917-053340-freeze-fork-remine-1`, 28/28 steps, 3 findings):

1. `tip_is A == blockBelow` after the fork was judged invalid → **A sits on forkBase** (B stays
   on blockBelow).
2. `no_event invalid_block B contains <A's next block>` → **B rejected A's next block** — A had
   re-admitted `spendBelow` after the reorg and mined it at `windowStart`.
3. `same_tip A B` → A@106 on its own chain, B@105 on blockBelow: **the alert-holding node
   split from the fleet**.

Re-run with `make reset && curl -X POST -d '{"mode":"auto"}' localhost:8600/api/scenarios/freeze-fork-remine/run`.

## Setup

- A, B, C teranodes on a private regtest network; a go-alert-system hub; alerts delivered over
  the alert P2P sync stream. Coinbase maturity 100, `windowStart = parentBlock + 2`.
- `parentTx` pays two "victim" outputs. Alert #9 freezes both over `[828, 838)`.
- A receives the alert first. B (unaware) mines `blockBelow` **0e57487a…** at height 827
  containing `spendBelow` **5f9f680f…** (victim output 0). A accepts it (below the window) — the
  #1422 fix works. B then receives the alert (late) and records the freeze on the spent output.
- C, isolated from the peer plane and never given the alert, mines `forkBase` **645078ff…**
  (827, empty sibling of blockBelow) and `forkInside` **540213c8…** (828, re-mines spendBelow
  inside the window). C's peer plane is healed.

## Observation 1 — a longer fork learned through node_status is not fetched

For 90 s after the heal, A and B log `[handleNodeStatusTopic] Updated block hash 540213c8…
for peer <C>` every 10 s, the sync coordinator reports "Sync peer … already active; skipping
new activation", and the block-processing queue stays at zero. Nothing is validated until C
announces another block. (Expected? A reconnecting miner whose chain is one block longer is
ignored until it mines again.)

## Observation 2 — catch-up adopts the fork's valid prefix before judging the batch, and does not fall back

C mines **25c6a868…** (829). A now catches up along C's chain:

```
05:18:10 [BlockAssembler][645078ff…] new best block header: 827
05:18:10 [BlockAssembler] best block header according to blockchain: 827: 645078ff…   (forkBase)
05:18:10 [BlockAssembler] best block header according to block assembly : 827: 0e57487a…  (blockBelow)
05:18:10 [BlockAssembler] handling reorg, moveBackBlocks: 1, moveForwardBlocks: 1
05:18:10 [moveBackBlock][Block 0e57487a… (height: 827, txCount: 3)] …
05:18:10 [moveForwardBlock][Block 645078ff… (height: 827, txCount: 2)] … processing 1 remainder tx hashes into subtrees
05:18:11 invalid block 540213c8…: BLOCK_INVALID (11): [validOrderAndBlessed] block at height 828 spends consensus-frozen output …
```

The blockchain service made the **equal-height** sibling `forkBase` the best block before
`forkInside` was judged, block assembly reorged onto it (moving `spendBelow` back into the
template as a "remainder tx", without re-validation), and after `forkInside` was rejected
**A stayed on `forkBase`** rather than returning to `blockBelow` (see
`evidence/teranode1-reorg-to-forkbase.log`).

## Observation 3 — A then mines the frozen spend inside the window (the PR's "residual")

```
05:19:51 [handleGenerate] called for 2 blocks
05:19:51 [GetMiningCandidate] Returning mining candidate: height=828, fees=500, subsidy=…, txCount=1 …
05:19:51 [Block:Valid] called for 3ea891a3…   (height 828, txCount 2)
```

A's block **3ea891a3…** at height 828 (= windowStart) contains `spendBelow`. B and C reject it:

```
[ValidateBlock][3ea891a3…] storing block as invalid: BLOCK_INVALID (11): [validOrderAndBlessed] block at height 828 spends consensus-frozen output …
[peer_metrics] Recording malicious attempt from peer <A>: invalid_block_validation
```

End state: A@829 on its own invalid chain, B@827 on blockBelow, C@829 on its fork — a
three-way split caused by the node that **holds** the alert. This is the case the PR
description lists under "Residual (ticketed, not in this PR)": *reorg re-admits the spend into
our own template without re-validation; if the new tip is inside the interval we mine an
invalid block until we reorg back*. On regtest it needs only one isolated miner and a
same-height sibling; and because of observation 2 the node does not "reorg back" on its own.

## Suggested checks

1. Catch-up should not move the active tip onto a fork prefix of equal work before the
   announced batch has been validated; after an invalid verdict the previous tip should be
   restored (or the fork prefix must at least not win ties).
2. Block assembly's `moveBackBlock` re-admission should re-run the policy freeze check
   (`IgnorePolicyFreeze=false`) at the new template height, so a coin frozen for the next
   height never enters the template.

Evidence: `evidence/teranode1-reorg-to-forkbase.log`, `evidence/teranode1-mines-frozen-spend.log`,
`evidence/teranode2-rejects-A-block.log` (full logs kept by the harness run recorder).
