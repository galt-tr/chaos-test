# arcade: a resurrected canonical block is never returned to `active` in `block_processing`

Reproduced on the chaos-test regtest fleet against `ghcr.io/bsv-blockchain/arcade:latest`
(image created 2026-09-15) for
[arcade#339](https://github.com/bsv-blockchain/arcade/issues/339), then re-run against a local
build of [arcade#343](https://github.com/bsv-blockchain/arcade/pull/343)
(`fix/339-reactivate-resurrected-block` @ `a5503ff`).

Scenario: `scenarios/arcade-same-height-flip-flop.yaml`.

## The contradiction

At the end of the run the same arcade instance answered both of these, seconds apart:

```
GET :18080/api/v1/blocks/processing-status/17a772afb893a74b68bdfc7d5f3b6630c4ec637517229b2834cb3d7cd914c572
  { "blockHeight": 3, "status": "orphaned", "orphanedAt": "2026-09-17T19:34:59.640Z" }

GET :18083/chaintracks/v2/header/height/3
  { "height": 3, "hash": "17a772afb893a74b68bdfc7d5f3b6630c4ec637517229b2834cb3d7cd914c572" }
```

Arcade's own chain view says block `17a772…` **is** the canonical block at height 3. Its
durable projection says that block is orphaned. A consumer treating `processing-status` as a
reorg feed demotes every transaction in a canonical block — the impact the issue reports from
mainnet block 965773.

Raw responses: `evidence/arcade-339-latest-contradiction.json`.

## Why it happens

`services/chaintracks_server/block_status_tracker.go` is the only writer that moves a row
between `active` and `orphaned`, and neither of its two inputs can resurrect a block:

- `recordReorg` (`:115-133`) marks `ev.OrphanedHashes` and upserts `ev.NewTip`. Nothing else.
  A block that returns to the active chain sits strictly **between** the common ancestor and
  the new tip, so no `ReorgEvent` ever names it.
- The tie-scan `scanRecentActive` (`:136-188`) skips any row whose status is not `active`
  (`row.Status != models.BlockStatusActive → return nil`). It only ever demotes.

The one remaining healing path is the reconciler's resurrection short-circuit
(`services/bump_builder/reconciler.go:429-443`), whose queue is
`status='orphaned' AND reconciled_at IS NULL`. Once `reconciled_at` is stamped the row leaves
that queue permanently. **The scenario waits for that stamp before triggering the second
reorg**, which is what turns an intermittent race into a deterministic reproduction. PR #343's
own e2e test arms the trap the same way.

## A second finding: the tie-scan cannot see a block that was never the tip

The first draft of this scenario used the issue's literal generic repro — two equal-work blocks
at the same height, the loser arriving second — and it did **not** reproduce. Arcade left the
loser `active` indefinitely instead of orphaning it:

```
18:55:50  Received block: height=144 hash=42bbcb2c… datahub=10.190.0.13
18:55:50  Block added as orphan/alternate chain: height=144        ← chaintracks: alternate
18:55:58  block_processed enqueued  block_hash=42bbcb2c…           ← the only row it ever got

GET /api/v1/blocks/processing-status/42bbcb2c… → { "blockHeight": 0, "status": "active" }
```

An equal-work alternate never becomes the tip, so the tip channel never fires, so
`UpsertBlockHeaderSeen` — the only writer of a real `block_height` — never runs. The row is the
height-0 placeholder `MarkBlockProcessed` creates, and the tie-scan's height filter excludes it
by design (the code says so at `block_status_tracker.go:165`). So the tie-scan added for #279
catches a same-height loser only if that block was the tip *first* and was then displaced; a
block that arrives second and never leads is invisible to it.

That is a detection gap, not the subject of #339, and it is partly covered in practice: the
reconciler's short-circuit resolves a height-0 row's height from its stored BUMP. It is noted
here because it is the reason this scenario builds its fork by **work** rather than by a
same-height race — the block under test is made arcade's chaintracks tip first, so its row
carries a real height, and the orphaning is then driven by a genuine `ReorgEvent`.

## What the scenario does

Roles: `A: teranode1` (rival chain), `B: teranode2` (bystander), `C: teranode3` (subject).
A `p2p` partition drops a node from `chaosnet`, which carries both peer announcements and
arcade's datahub access, while `generate` RPC keeps working on `ctlnet` — so a partitioned node
mines a chain that arcade cannot see.

1. Auto-miner off; converge on B; record `baseH`.
2. Partition A; A privately mines `rival1` (`baseH+1`) and `rival2` (`baseH+2`).
3. C mines `subject` (`baseH+1`). Arcade adopts it as its chaintracks tip →
   `UpsertBlockHeaderSeen` writes a real height and `status=active`. **Asserted**, because
   without it the rest of the run is vacuous.
4. Partition C so it holds `subject` through the coming reorg.
5. Heal A. Its two-block chain outweighs `subject`; arcade's `ReorgEvent` names `subject` in
   `OrphanedHashes` → `status=orphaned`. **Asserted** — correct behaviour at this instant.
6. Wait `armSeconds` (15) so the reconciler stamps `reconciled_at` and the row leaves its queue.
7. C privately mines `revive1`/`revive2`, so its chain outweighs the rival chain.
8. Heal C. The fleet reorgs back; `subject` is canonical again. `arcade_canonical` asserts
   arcade's **own** chaintracks agrees — this is what makes the next failure a self-contradiction
   rather than a claim about the fleet.
9. Assert `subject` is `active`. **No `should:`** — while the bug is present this fails the run.
   The rivals (named by the second `ReorgEvent`) and the new tip are asserted too, so a failure
   is provably specific to the unnamed resurrected block and not a dead reorg pipeline.

Regtest tuning needed to make step 6 fast (`cmd/gen/templates.go`, supported config keys only):
`bump_builder.reconciler.interval_ms: 2000`, `max_defer_attempts: 2` (stock 30s × 10 defers is a
5-minute worst case) and `chaintracks_server.tie_scan_min_interval_ms: 500`.

## PR #343 fixes it

`fix/339-reactivate-resurrected-block` @ `a5503ff` (arcade reports itself as
`v0.13.3-18-ga5503ff`) makes the tie-scan **bidirectional**: as well as demoting `active` rows
whose height is held by another block, it resets to `active` any `orphaned` row that **is** the
active-chain block at its height, and forces that scan after every `ReorgEvent` rather than
letting the debounce drop it. It also walks the new branch on reorg, bounded by
`maxReorgBranchWalk = 1000`. The PR's own docstring describes the sequence this scenario builds:

> when the next block builds on the tie loser, chaintracks' ReorgEvent names only the hashes it
> orphans and the new tip — never the resurrected ancestor — and the reconciler has usually
> already stamped that row reconciled, so nothing else would ever return it to active.

Same scenario, same moment in the run, on the PR build
(`evidence/arcade-339-pr343-agrees.json`):

```
GET :18080/api/v1/blocks/processing-status/653df08982c9…
  { "blockHeight": 3, "status": "active" }        ← and orphanedAt is gone

GET :18083/chaintracks/v2/header/height/3
  { "height": 3, "hash": "653df08982c9…" }
```

The two views agree. The reactivation upsert also clears `orphaned_at`, so the row carries no
stale trace of the demotion.

## Results

Both builds ran the **same scenario file** on a **freshly wiped chain** (`make reset`), so the
two outcomes differ only by the arcade image.

| arcade build | run | outcome |
|---|---|---|
| `ghcr.io/bsv-blockchain/arcade:latest` | `20260917-193439-arcade-same-height-flip-flop-1` | **failed** at step 16/16 — `arcade says 17a772afb893… is "orphaned" at height 3 (orphanedAt 2026-09-17T19:34:59.640Z), want "active"` |
| PR #343 `a5503ff` | `20260917-193050-arcade-same-height-flip-flop-2` | **passed**, 16/16 |

Every step before the final assertion passed identically on both builds — including
`arcade_block_status … orphaned` after the first reorg, which is correct behaviour on both. The
builds diverge only on whether the row comes back.

Earlier runs, kept for the record: `20260917-190414-…-2` (`:latest`, failed on the same
assertion at height 147, before the chain was reset) and `20260917-192828-…-1` (PR build, #339
assertion passed but the run failed on an over-specified assertion of this scenario's own, below).

### One assertion of this scenario was wrong, and the PR build is what exposed it

The first version of the final step also asserted that `rival1` — the rival chain's *lower*
block, mined at the subject's height — ends up `orphaned`. Arcade holds **no row at all** for
it, so the assertion failed with `arcade has no processing status for 5ff1c879d108…`.

That is correct arcade behaviour, not a defect. `rival1` was never arcade's tip (it tied the
subject's height, so no `UpsertBlockHeaderSeen`) and, containing no transactions, was never
processed either — and `MarkBlocksOrphaned` skips hashes with no existing row
(`store/pebble/pebble.go`, `if prev == nil { continue }`). The assertion was removed.

It never surfaced on `:latest` because the engine stops a step at its first failing assertion,
and there the `subject` assertion failed first. The `:latest` result above is unaffected: the
assertion it failed on is byte-identical in both versions of the scenario and is evaluated
before the removed one.
