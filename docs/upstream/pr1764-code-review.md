# Code review: teranode PR #1764 — `fix(alert): height-anchor coin freezes and fail frozen blocks cleanly`

Branch `fix/1422-height-anchored-freeze`. Reviewed at head `889a25c3` (66 files, +4452/−350).
The chaos-test runs quoted below were made on the previous head `2d78850`; the two commits
since (`8ef96406`, `889a25c3`) touch the same-block parent check, the Aerospike freeze-record
reader, the legacy-sync consensus bypass and the `freeze` RPC argument semantics. Where a
finding is affected by that delta it says so. The harness ran the SQL backend
(`utxostore=sqlite:///utxostore`, `bsv-regtest/config/teranode/common.env:6`); the Aerospike/Lua
path is covered only by the PR's CI, not by anything below.

Line numbers refer to the PR head for changed files and to the current tree for unchanged
files (`catchup.go`, `SubtreeProcessor.go`, `BlockAssembler.go`, `stores/blockchain/sql/*`).

## 1. Summary verdict

The core change is sound and does what it claims: on a real three-node network the fleet
agrees on a below-window spend, an inside-window block is rejected once with a recorded
`BLOCK_INVALID` verdict, and the re-fetch loop from issue #1422 is gone
(`freeze-timing-skew` 21/21; the main-vs-PR comparison shows `main` re-validating the same
block 16 times in 60 s where the PR publishes one verdict). The store-level design — the
consensus record as a property of the outpoint, checked before the idempotent spent path,
surviving rollback, recorded on spent outputs — is correct and well pinned by the
cross-backend contract. What is not mergeable as-is is the interaction with the rest of the
node: the alert-holding node can be made to leave the honest chain by a single equal-height
sibling (blockchain tie-break) and then mine the frozen spend itself (block-assembly reorg
re-admission, the PR's documented "residual"). Both halves are pre-existing in shape, but the
PR is what makes the freeze a consensus rule, so it is the PR that turns them into a
self-inflicted fleet split. I would ask for (a) the residual to be fixed or at least fenced
in block assembly before this ships as a consensus feature, (b) the checkpoint claim in the
docs to match the code, and (c) the verdict text and the Postgres batching regression to be
addressed; the rest is design notes and tests.

## 2. Findings, by severity

### F1 — HIGH: after judging a fork invalid, the alert holder stays on the fork's equal-height prefix and then mines the consensus-frozen spend itself

Observed (scenario `freeze-fork-remine`, run `20260917-053340`, deterministic from a clean
chain): A and B hold the alert and are on `blockBelow` (height `windowStart−1`, spends the
coin legitimately). Isolated C mines an empty sibling `forkBase` at the same height and
`forkInside` at `windowStart` re-mining the same spend, then re-joins and mines one more
block. A catches up along C's chain, its tip becomes `forkBase`, `forkInside` is rejected with
`BLOCK_INVALID (11): [validOrderAndBlessed] block at height 828 spends consensus-frozen
output …`, A stays on `forkBase`, and A's next mining candidate contains `spendBelow` at
height `windowStart`. B and C reject A's block; A is alone on a chain invalid under its own
rule. Evidence: `docs/upstream/teranode-fork-adopts-sibling-and-remines-frozen-spend.md`,
`docs/upstream/evidence/teranode1-reorg-to-forkbase.log`,
`evidence/teranode1-mines-frozen-spend.log`, `evidence/teranode2-rejects-A-block.log`.

The code explains every step:

1. **Why A adopts `forkBase` (equal height, equal work).** Best-block selection is
   `ORDER BY chain_work DESC, peer_id ASC, id ASC`
   (`stores/blockchain/sql/GetBestBlockHeader.go:133-136`, `GetBestBlockID.go:39`, and the
   design note at `StoreBlock.go:121-127`). Ties are broken by the *peer id string*, not by
   first-seen. Own-mined blocks are stored with `peerID == ""`
   (`services/blockassembly/Server.go:2018`) and so always win; blocks from peers carry the
   announcing or catch-up peer's id (`services/blockvalidation/BlockValidation.go:2033,
   2351` pass `opts.PeerID`; `catchup.go:1725-1731` sets it to the catch-up primary). In the
   harness A=`12D3KooWBTHB…`, B=`12D3KooWGkXu…`, C=`12D3KooWECRd…`
   (`bsv-regtest/config/teranode/teranode{1,2,3}.env`). On A, `blockBelow` carries B's id and
   `forkBase` carries C's; `"…ECRd" < "…GkXu"`, so `forkBase` becomes best the moment it is
   stored. This is deterministic and predicts the inverse: had the fork come from a peer
   sorting after B, A would have stayed on `blockBelow` and the residual would not have
   fired. Bitcoin Core never switches tips on equal work; teranode does, whenever the sibling
   comes from a lexicographically lower peer.
2. **Why there is no fallback after the invalid verdict.** Catch-up validates and stores the
   batch one block at a time (`services/blockvalidation/catchup.go:1667-1790`,
   `validateBlocksOnChannel`); each valid block is `AddBlock`'d immediately and is eligible
   for best. When block N fails with `ErrBlockInvalid` the function returns
   (`catchup.go:~1790`) and blocks 1..N−1 stay stored, valid, and — by the tie-break above —
   best. Nothing re-evaluates whether the previous tip should be restored.
3. **Why the spend is back in A's template.** Block assembly follows the blockchain's best
   header (`services/blockassembly/BlockAssembler.go:936-1049`, `handleReorg` at
   `1938-1960`). `moveBackBlock` (`subtreeprocessor/SubtreeProcessor.go:4103-4160`) reads
   the disconnected block's subtrees from the blob store and `moveBackBlockBulkBuild`
   (`4174`) / `processOwnBlockSubtreeNodes` (`4847`) re-add every node through
   `addNodePreValidated` (`2336`), which by design performs no store check. The log line
   `processing 1 remainder tx hashes into subtrees` is `spendBelow` re-entering.
   `GetMiningCandidate` does not consult freeze state either. The PR's own e2e test observes
   exactly this and logs it instead of asserting
   (`test/e2e/daemon/ready/freeze_enforce_at_height_test.go:553-562`).

Failure scenario in production terms: any miner (honest or not) who produces one
equal-height sibling plus one inside-window block carrying a below-window spend makes every
alert-holding node whose tie-break favours that miner's peer id (i) abandon the honest tip
and (ii) mine an invalid block. That is a cheap, targeted attack on precisely the nodes that
honour the alert, and it needs one block of hashpower, not a majority. It is also reachable
by accident on any network with two miners.

Suggested changes (the first is the one I would block on):

- **Block assembly: never re-admit a policy-frozen spend on reorg.** In `moveBackBlock` /
  `processRemainderTxHashes` (`SubtreeProcessor.go:4103, 5361`), before nodes are re-added,
  filter the disconnected block's transactions against the freeze at the new template height.
  Cheapest faithful check: for each re-admitted tx, one batched
  `utxoStore.Get(parent, fields.UtxoFreezeFrom, fields.UtxoFreezeUntil, fields.UtxoFreezeExp)`
  per distinct parent (the read block validation already makes; `FreezeRecords == nil` for
  every parent with no record, and on Aerospike that costs no extra round trip because the
  bins are simply absent) and drop the tx if
  `utxo.FreezePolicyActiveAt(rec.From, rec.Until, rec.PolicyExpires, tipHeight+1)` holds for
  any spent vout. Alternatively keep an in-memory set of policy-frozen outpoints fed by
  `FreezeUTXOs`/`UnFreezeUTXOs` and consult it here and in `Reset`. Do not reuse
  `validateUnminedTxInputs` as-is (see F8).
- **Blockchain: do not switch tips on equal work.** Either order ties by insertion
  (`id ASC` before `peer_id ASC`) or make the reorg decision in
  `BlockAssembler.go:984` require strictly greater chain work. If the `peer_id` tie-break is
  intentional (e.g. for deterministic agreement between instances sharing a store) it should
  be documented as such and the catch-up path should not commit a fork prefix as best before
  the batch is judged — e.g. store batch blocks with a tentative flag or compare the batch's
  total chain work before the first `AddBlock`.
- **Catch-up: restore the prior tip when a batch is judged invalid** if its prefix displaced
  the tip only by tie-break. This is the most invasive option; the two above make it
  unnecessary.
- In this PR at minimum: turn the e2e `t.Logf` at `:553-562` into an assertion (or a skip
  with the ticket number), state in `docs/topics/services/alert.md` that a reorg onto an
  equal-work fork can put the alert holder's own template in violation until the fix lands,
  and open the block-assembly and blockchain tie-break tickets the description promises.

### F2 — MEDIUM: "checkpoint beats freeze" is implemented only on the quick-validation paths; the docs claim more

The consensus-tier bypass is applied on the below-checkpoint spend paths
(`services/blockvalidation/quick_validate.go:1697-1702`,
`services/legacy/netsync/handle_block.go:1455-1462`, gated in
`services/validator/Validator.go:922-937`). The new block-level check
`checkParentFreezeRecords` (`model/Block.go:2071-2088`, called from
`getParentTxMetaBlockIDs:2059` and `checkSameBlockParentFreeze:1999-2011`) has no checkpoint
awareness, and the normal-validation spends in `CheckBlockSubtrees`
(`services/subtreevalidation/check_block_subtrees.go:707-717, 1287-1302`) carry only
`IgnorePolicyFreeze`. Both run for any block that takes normal validation, which includes
below-checkpoint blocks whenever `blockvalidation_catchup_allow_quick_validation=false`
(`settings/blockvalidation_settings.go:89`; `catchup.go:1157/1190`) or when
`tryQuickValidation` falls back (`catchup.go:1858`).

Failure scenario: an operator issues the unqualified admin `freeze` (stored `(0,0)`,
enforced at every height) or an alert with `start` below the checkpoint, on a coin whose
legitimate spend sits in a checkpointed block. A node syncing with quick validation disabled
marks that checkpointed block invalid and wedges; a node with it enabled does not. Two
configurations of the same build disagree about a checkpointed block.

`docs/topics/services/alert.md:40-42` says "neither tier of a freeze is applied to its spends,
on the block-validation and the legacy-sync catch-up paths alike" — that is true only of the
quick paths. Suggested change: gate `checkParentFreezeRecords` on
`b.Height > blockchain.HighestCheckpointHeight(settings.ChainCfgParams.Checkpoints)` (the
settings are already passed into `Valid`; `checkpointConfirmedAncestor` at
`model/Block.go:125-159` is the existing precedent), add `WithIgnoreConsensusFreeze(true)`
to the `CheckBlockSubtrees` option lists when the block is at or below the checkpoint, and
soften the doc sentence until then.

### F3 — MEDIUM: Postgres block validation loses the batched parent read

`stores/utxo/sql/sql.go:1462-1470`: any request for a freeze field routes `Get` to
`getUnbatched`, which now runs the transaction/block-IDs queries plus a third per-output
freeze query (`sql.go:1782-1823`). `getParentTxMetaBlockIDs` (`model/Block.go:2039-2040`)
asks for the three freeze fields on *every* out-of-block parent of *every* transaction in a
block, and `checkSameBlockParentFreeze` adds one more unbatched `Get` per in-block edge. On
Postgres with `BatchSQLOperations` (`sql.go:240, 487, 566`) these reads previously went
through `getBatched`. The benchmark comparison attached to the PR shows no regression, but
none of those benchmarks touches Postgres. Suggested change: carry the freeze records in the
batched decorator (one `LEFT JOIN outputs … WHERE frozen OR freezeFrom IS NOT NULL`
aggregated per transaction in `sendGetBatch`), or add a dedicated batched
"freeze records for these tx ids" query used by block validation. Please measure a
1k-tx block on Postgres before and after.

### F4 — MEDIUM: block validation admits a policy-frozen spend from a peer's block into this node's own block assembly

`check_block_subtrees.go:707-717` and `:1287-1302` set `WithIgnorePolicyFreeze(true)` and
leave `AddTXToBlockAssembly` at its default `true` (`services/validator/options.go:233`)
whenever the FSM is RUNNING; the validator then stores the tx and hands it to block assembly
(`services/validator/Validator.go:1066-1071`). So a peer block's spend of a coin that this
node policy-froze enters this node's template, and is only removed when the block is marked
mined. Normally that window is short; combined with F1 it is what keeps the spend resident
for the reorg to re-admit, and if the peer block is rejected for an unrelated later
transaction the spend stays in the template outright. The store already knows when it fell
through a policy freeze (`teranode.lua:~545` "fall through and record the spend";
`sql.go:2290-2293` `policyFrozenAt` skipped because `ignorePolicyFreeze`). Suggested change:
have the spend result report "policy tier bypassed" per input, and have
`spendAndCreateInUtxoStore` skip block assembly for such a tx (or mark it in a way
`GetMiningCandidate` excludes). This closes F1's third step from the other side.

### F5 — MEDIUM: verdict text differs between the live path and the block-level path; the Kafka verdict has no structured code

Live path (subtree validation, tx not yet known):
`services/subtreevalidation/SubtreeValidation.go:446-448` → `ErrTxInvalid` wrapping
`UTXO_CONSENSUS_FROZEN (78)`, and `BlockValidation.go:1946-1955` publishes
`reason = "block contains invalid transactions: " + err.Error()`. Block-level path (tx
already known, subtree reused): `model/Block.go:2082-2086` returns
`BLOCK_INVALID (11): [validOrderAndBlessed] block at height N spends consensus-frozen output
…`, and `BlockValidation.go:2216-2219, 2299-2303` publishes `reason = err.Error()`.
`KafkaInvalidBlockTopicMessage` (`BlockValidation.go:2823-2828`) is
`{BlockHash, Reason, PeerId, PeerUrl}` — free text only. In-process both chains satisfy
`errors.Is(err, ErrBlockInvalid/ErrTxInvalid/ErrUtxoConsensusFrozen)`, so classification is
fine; external consumers of `invalid-blocks-*` (dashboards, the harness, operators) have to
pattern-match two shapes. Suggested change: make `checkParentFreezeRecords`'s outer message
start with the same "block contains invalid transactions" prefix and include the code name,
and add a `reason_code` (the root-cause `ERR` enum) to the Kafka message so consumers stop
parsing text.

### F6 — MEDIUM: mempool rejection code flips from 72 to 78 at the window boundary, undocumented

Observed: below the window a mempool spend of the frozen coin is `UTXO_FROZEN (72)`; once the
node's next height is inside the window the same submission is `UTXO_CONSENSUS_FROZEN (78) …
at block height N`. This is intended — `stores/utxo/tests/tests.go:328-332` asserts it
("inside the window a mempool spend gets the consensus verdict too") because both backends
check the consensus tier first (`sql.go:2287-2294`, `teranode.lua:~499-522`). It is not in
`docs/topics/services/rpc.md` or `alert.md`, and `ErrUtxoConsensusFrozen` is deliberately not
`errors.Is(ErrFrozen)` (`tests.go:556-561`). In-repo consumers are covered
(`services/propagation/Server.go:766`, `errors/errors.go:1101`); I found no other
`ErrFrozen` consumer in `services/`. External submitters keying on 72 will misclassify.
Suggested change: on non-block calls (`IgnorePolicyFreeze == false`) evaluate the policy
tier first so the mempool path always reports 72 while the coin is policy-frozen, reserving
78 for block-context spends; otherwise document the flip in `rpc.md` and the `rejectedtx`
topic docs.

### F7 — MEDIUM: `EnforceAtHeightEnd = 0` on the alert wire silently becomes "never consensus-enforced"

`services/alert/node.go:404-423` (`enforceAtHeightWindow`) → `utxo.NormalizeFreezeWindow`
(`stores/utxo/Interface.go:183-189`): `stop <= start`, including `stop == 0`, is an empty
interval, so the coin is never consensus-frozen and only the policy tier holds. The binary
freeze alert has no optional field (`go-alert-system@v0.1.17
app/models/alert_message_freeze_utxo.go:25-27, 59-60`), so an authority expressing "no end"
as 0 gets a freeze that no block ever violates, with no error and no log. The PR's SV-parity
argument (stop is exclusive, 0 is empty) is plausible but I could not verify SV Node's
handling of an explicit `stop: 0` from here; the only in-tree example uses an explicit end
(`go-alert-system hack/publish.go:162`). Suggested change: log at Warn in
`AddToConsensusBlacklist` when a fund's interval normalises to empty while
`policyExpiresWithConsensus` is false (a "freeze" that will never be consensus-enforced), and
state in `alert.md` how an authority must encode "no end" (`math.MaxUint64`, or any stop
beyond the horizon).

### F8 — LOW: `validateUnminedTxInputs` will mark spenders of policy-frozen coins as Conflicting

`services/blockassembly/BlockAssembler.go:3254-3290` compares each input's
`SpendingDatas[vout].TxID` with the tx and, on mismatch, calls `markAsConflicting`. A
policy-frozen unspent output reports `FrozenBytesTxHash` as its spender — SQL did this
before (`sql.go:1795-1800` via `policyFrozenAt`) and Aerospike now does too
(`stores/utxo/aerospike/get.go:1521-1556`, `synthesiseFrozenSpendingData`, and the extra
records at `:1640-1648`). So `ResetWithInputValidation` (`BlockAssembler.go:1666`) will
permanently flag a tx as conflicting for spending a coin that is merely policy-frozen — wrong
if the freeze later lifts (`policyExpiresWithConsensus`) or if the tx is valid below the
window. Newly reachable on Aerospike because of this PR. Suggested change: special-case
`spendingData.TxID.IsEqual(&subtree.FrozenBytesTxHash)` to "exclude from assembly, do not
mark conflicting".

### F9 — LOW: a longer fork learned through `node_status` is not fetched while a sync peer is active

`services/p2p/sync_coordinator.go:1112-1116`: `selectAndActivateNewPeer` returns early if any
sync peer is active. After C's peer plane was healed, A and B logged C's higher tip every
10 s for 90 s and validated nothing until C announced another block. Pre-existing and out of
this PR's scope, but it delays the fleet's verdict on a fork by up to a block interval and is
part of how F1 unfolds. Worth its own ticket.

### F10 — LOW (pre-existing, not introduced): `getrawmempool` lists an all-`ff` txid

`getrawmempool` returns `blockAssemblyClient.GetTransactionHashes`
(`services/rpc/handlers.go:1364`), whose handler appends every node of every subtree
(`SubtreeProcessor.go:849-864`) including the coinbase placeholder node.
`CoinbasePlaceholderHashValue` is 32×`0xFF` — byte-identical to `FrozenBytesTxHash`
(`go-subtree coinbase_placeholder.go`, read at v1.0.3; teranode pins v1.5.1). So the entry
is the coinbase placeholder, not the freeze sentinel, and it predates the PR. A one-line
filter in the handler would remove the confusion; not this PR's job.

### F11 — LOW (pre-existing, improved): the announced block is validated twice

Two `[ValidateBlock]` lines within 15 s on the PR image; `main` showed four for the same
event (`docs/comparison-main-vs-pr1764.md`). Not introduced here; noted so nobody attributes
it to the PR.

## 3. Design and consistency observations

- **Two error codes, one family.** `ErrUtxoConsensusFrozen` (78) is not `errors.Is`
  `ErrFrozen` (72) on purpose (`tests.go:556-561`), which is right for block classification
  but means every future "is this frozen?" check must test both. The Lua side mirrors this
  (`teranode.lua` `ERROR_CODE_CONSENSUS_FROZEN` vs `ERROR_CODE_FROZEN`, `teranode.go`
  `LuaErrorCodeConsensusFrozen`). Consider a helper `errors.IsAnyFrozen(err)` and use it in
  `propagation/Server.go:766` and `errors.go:1101`.
- **Verdict text** — see F5. Also `spendBatchWithRetry` (`quick_validate.go:1697-1715`) can
  produce a third shape (`[spendBatchWithRetry][…] tx … spends a consensus-frozen utxo`),
  unreachable while the bypass is set, as the comment says.
- **RPC parameter semantics (head `889a25c3`).** `services/rpc/bsvjson/chainsvrcmds.go:703-731`
  makes `utxohash`, `enforceAtHeightStart`, `enforceAtHeightStop` pointers without
  `jsonrpcdefault`; `freezeWindowFromRPC` (`handlers.go:2771-2797`) maps both-omitted →
  `(0,0)` "every height", explicit `(0,0)` → empty, `stop` alone → from genesis, `start`
  alone → no end. `TestFreezeWindowFromRPC` pins each. This resolves the edge cases the
  harness flagged on `2d78850` (where `jsonrpcdefault:"0"` made an explicit `0,0`
  indistinguishable from omission). Remaining notes: (i) `main` required exactly 3 params,
  the PR accepts 2..6 — compatible widening, fine; (ii) the supplied `utxohash` is now
  ignored (`handlers.go:2717-2721` derives it), so a wrong hash that used to fail on
  Aerospike now succeeds silently — documented as "Unused" in `rpc.md:80`, acceptable;
  (iii) `stop` alone and the unqualified form both cover history from genesis, so an
  operator freezing a coin with an already-mined spend will reject historical blocks on
  re-validation/re-sync (F2). `rpc.md` should say "always give a `start` at or above the
  current tip plus a propagation margin".
- **Tx-level `transactions.frozen` demoted to policy-only** (`sql.go:2204-2244`,
  `txFrozen`). Nothing in the tree sets it (only `outputs.frozen` is written,
  `stores/utxo/sql/alert_system.go:148, 240`), so the change is inert; the comment "policy
  only, never a consensus record" is accurate.
- **Unfreeze semantics changed on purpose**: an elapsed or empty interval no longer deletes
  the record; with `policyExpiresWithConsensus` clear the coin stays out of this node's
  templates (`services/alert/node.go:331-383`, `alert.md:57-64`). This is a behaviour change
  for any authority that relied on the pre-PR "stop below tip ⇒ unfreeze"; it is documented
  but deserves a line in the release notes, not only in `alert.md`.
- **Markers are never removed** (`stores/utxo/aerospike/alert_system.go:~81-121`,
  `fields.UtxoFreezeRecs`), which is the right call for the race described, at the cost of
  one wasted extra-record read per stale marker forever. Fine.
- **Legacy sentinel not consulted by the block-level read** (`freeze_record.go:69-79`). The
  argument holds — a sentinel-only output is unspent by construction so there is no
  already-validated spender to catch — and its one exception (below-checkpoint bypass) is
  the same hole as F2.
- **Aerospike `readFreezeRecords` runs up to three times per record** when no output carries
  a record (`get.go:1185-1198`: the `FreezeRecords == nil` guard stays true for each of the
  three requested fields). No I/O (no marker → no extra read), just repeated map decoding.
  Cache an "already decoded" flag.
- **`markFreezeExtraRecords` dereferences `*spend.TxID`** without a nil check
  (`alert_system.go:~86`); all current callers set it. Cheap to guard.
- **`FreezePolicyActiveAt` with `until == 0`** returns true regardless of the flag
  (`Interface.go:159-167`), i.e. `policyExpiresWithConsensus` on an open-ended or legacy
  window never lifts. Pinned by `TestFreezePolicyActiveAt`; worth one sentence in `alert.md`.
- **SQL conditional writes** (`observedFreezeState`, `alert_system.go:~110-125`): the
  null-safe `IS` on SQLite compares an integer-stored boolean with a Go `bool` — works
  because SQLite stores booleans as 0/1; a comment would save the next reader the check.

## 4. Test-coverage gaps

Unit / integration:

1. **Block-level check below the checkpoint (F2).** A `model` test that freezes a parent
   `(0,0)`, sets `Checkpoints` above the block height, and asserts `checkParentExistsOnChain`
   does *not* return `ErrBlockInvalid` once the gate exists; plus a `subtreevalidation`
   test that `CheckBlockSubtrees` passes `IgnoreConsensusFreeze` for a block at the
   checkpoint height.
2. **Postgres batching (F3).** A `stores/utxo/sql` test asserting that
   `Get(hash, fields.BlockIDs, fields.UtxoFreezeFrom, …)` goes through `getBatched` when
   `BatchSQLOperations` is on, and a benchmark on a 1k-parent block.
3. **Policy bypass does not feed block assembly (F4).** A validator test with
   `IgnorePolicyFreeze(true)`, `AddTXToBlockAssembly(true)`, a policy-frozen parent, and a
   fake block-assembly client that must not receive the tx.
4. **Reorg re-admission (F1, block assembly).** A `subtreeprocessor` test: freeze parent:0
   with a window starting at the new tip height, `moveBackBlock` a block carrying its
   spend, assert the spend is not in `GetTransactionHashes` (today it is).
5. **`validateUnminedTxInputs` with the frozen sentinel (F8).** Assert no
   `markAsConflicting` call.
6. **Mempool code inside the window (F6)** — already pinned in `tests.go:328-332`; if the
   ordering is changed per F6 that assertion flips and documents the new contract.
7. **Alert with `Stop == 0` (F7)** — `TestEnforceAtHeightWindow` covers the mapping; add an
   assertion on the Warn log (or a returned `NotProcessed` reason) once one exists.
8. **Aerospike parity for every cross-backend case is CI-only**; the description says so.
   The `aerospike` build tag should be part of the PR's required checks, since the Lua path
   is where the two backends diverged in the first round.

End-to-end (the PR's `test/e2e/daemon/ready/freeze_enforce_at_height_test.go`):

9. **Fork/remine over P2P with a tie.** `TestFreezeAlertTimingKeepsFleetInAgreement` hands
   blocks to `ValidateBlock` directly and asserts `requireSameTip`; it never lets the
   blockchain choose between `blockBelow` and `forkBase`, so it cannot see F1. Add a variant
   where node B (higher-sorting peer id) mines `blockBelow`, a third node with a lower-sorting
   id serves `forkBase`+`forkInside` through catch-up, and assert (a) A's tip returns to
   `blockBelow` (or never leaves it), (b) A's next `GetMiningCandidate` does not contain
   `spendBelow`, (c) A's next block is accepted by B. The harness scenario
   `scenarios/freeze-fork-remine.yaml` is a ready-made spec for this.
10. **Turn the residual log into an assertion.** `TestFreezeReorgRejectsReminedSpend:553-562`
    should `require.NotContains(hashes, spendBelow)` once the block-assembly fix is in, or
    `t.Skip` with the ticket until then.
11. **Unfreeze and confiscation were not exercised by the harness.** Add e2e cases for (a)
    an unfreeze alert (empty interval + `policyExpiresWithConsensus`) releasing the coin into
    the mempool, (b) the same without the flag keeping it out, and (c) `ReAssignUTXO` on a
    record-only frozen output (rolled-back below-window spend), given the known
    reassignment regression the description references.
12. **Restart mid-window.** Freeze, restart the node (block assembly `Reset` /
    `loadUnminedTransactions`), assert the policy tier still holds and no unmined tx spending
    the coin is re-admitted or marked conflicting (F8).

## 5. Nits

- `services/blockvalidation/BlockValidation.go:1949-1951` and the two similar comments
  ("One verb, one arg") explain a `%!s(MISSING)` fix three times; once in a helper's doc
  would do.
- `stores/utxo/aerospike/native_op.go:654` startup message names four fenced sub-ops; the
  fence and the settings text (`settings/aerospike_settings.go:24`) name five (reassign was
  added in the delta). Update the log line.
- `stores/utxo/aerospike/native_op_coverage_test.go:22-23` comment still says "four
  freeze-aware sub-ops" while the map lists five.
- `docs/topics/services/alert.md` §2.3 (unchanged) still describes the pre-PR
  "compare `Stop` with the live tip and call `UnFreezeUTXOs`" flow; Copilot flagged this and
  it is still there. Update or delete the procedural section and diagram.
- `errors/error.pb.go` header bumps `protoc v6.33.2 → v7.34.1`; unrelated churn, harmless,
  but confirm the generator pin in CI so the next `make gen` does not flip it back.
- `services/rpc/handlers.go:~2745` `utxoHashForOutput` doc says "keyed on the requested
  txid (see services/alert/node.go for why…)"; the referenced explanation is not in
  `node.go` at head. Point at the right place or inline one sentence.
- `model/block_same_block_freeze_test.go:600-604` name says "outside the window the frozen
  output may be spent" and tests `windowStart-1` and `windowStop`; add `windowStop-1` to pin
  the exclusive end at the block level too (the store test has it; this layer does not).
- Log message at `stores/utxo/sql/alert_system.go:~160` and the Aerospike equivalent both
  say "freeze recorded on spent output" at Info; alerts are rare so fine, but the SQL one
  fires per output per call while the Aerospike one fires per batch record — same event,
  different cardinality.

## What was checked and found correct

Store ordering (consensus check before the idempotent spent path, both backends), the
two-tier split and its option plumbing across gRPC/HTTP, `blessMissingTransaction`'s
`ErrTxInvalid` conversion guarded on `IgnorePolicyFreeze`, the `WrapGRPC` fix in
`CheckBlockSubtrees`, `hardFail` first-write-wins, SQL single-transaction conditional writes
with `RowsAffected`, Lua record-on-spent / unfreeze-clears-record / reassign-clears-record,
`readFreezeRecords` following the marker only, the malformed-bin storage errors, the
`freeze`/`unfreeze` RPC hash derivation, `IsRetryableError` not including either frozen code
(`errors/error_utils.go:34-42`), and the go-alert-system wire model (one 57-byte range per
fund; `Start`/`End` as `uint64`, converted to `int` with a `MaxInt` guard). The harness
confirmed the headline claims on real infrastructure: fleet agreement below the window, one
recorded verdict inside it, no re-fetch loop, and a clean `parent block … is invalid`
rejection of the child block that `main` re-validated 16 times.
