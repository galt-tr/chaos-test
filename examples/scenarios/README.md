# Writing scenarios

This is a reference for generating a chaos-test scenario from a description. It is written to be
read by an AI agent: it states the exact file schema, every action and its `with:` fields, every
assertion and its pass condition, and the `${...}` expression language with the mistakes that
actually break scenarios. The authority for all of it is `internal/scenario/engine.go`
(schema, lifecycle) and `internal/scenario/executor.go` (actions, checks, expressions). If this
document and that code disagree, the code wins; fix this document.

A scenario is one YAML file in a scenario directory (`examples/scenarios/` by default). Its `id`
defaults to the filename without extension. Drop a file in, and it is picked up on the next
`GET /api/scenarios` or run start, no restart. A file that fails to parse is logged and skipped,
so a broken scenario never hides the others, but it also never runs: check the orchestrator log
if a scenario you added does not appear.

## How a run works

1. `POST /api/scenarios/{id}/run` starts a run. Body: `{"mode": "...", "roles": {...}, "params": {...}}`
   (all optional). `mode` is `auto` (default) or `step`. `roles` and `params` override the
   file's defaults for this run only.
2. Steps run in order. Each step resolves its `with:` block (expanding `${...}`), runs its
   `action`, optionally stores the result under `as:`, then evaluates its `assert:` list.
3. A step's `action` failing (an error, e.g. a rejected RPC) **fails the step and aborts the
   run** unless that failure is what you are asserting on.
4. An assertion that does not pass behaves by its `should:` flag:
   - `should:` omitted or `false` → **hard failure**: the step fails and the run aborts.
   - `should: true` → **finding**: the mismatch is recorded in the run's `findings[]` and the
     run continues. This is how you capture a disagreement without stopping the scenario. A run
     that completes with findings has `status: passed` and a non-empty `findings[]`.
5. On exit (success, failure or abort) every network partition the run opened is healed, unless
   the run has `params.cleanup: false`.
6. Only one run exists at a time. Starting a run while another is `running` or `paused` is
   refused; abort it first (`POST /api/runs/{id}/abort`).

`mode: step` pauses before every step after the first; release with `POST /api/runs/{id}/next`.
The run is saved as JSON under `sim/.data/runs/` after every step.

Scenarios assume a **converged fleet with clean alert history**. Run `make reset` between runs: a
chain split caused by a finding cannot be healed by mining, and stale alerts leak across runs.

## File schema

```yaml
id: my-scenario            # optional; defaults to the filename stem. Keep it unique.
name: One-line human title # shown in the UI and run log
description: >             # free text; a folded scalar (>) is the usual form for a paragraph
  What this scenario does and what a finding would mean.
requires: [regtest]        # informational tags only; not enforced
roles:                     # role name -> default node name
  A: teranode1
  B: teranode2
  C: svnode1
params:                    # default parameters, overridable per run; values of any type
  windowLen: 6
  cleanup: true
steps:
  - name: human label for the step     # required, shown in output
    action: mine                       # required, one of the actions below
    with: { node: A, blocks: 1 }       # action inputs; values may contain ${...}
    as: blockBelow                     # optional: store the action's output map under this name
    timeout: 2m                        # optional step timeout (default 5m); Go duration or bare seconds
    note: why this step exists         # optional, free text
    assert:                            # optional list of checks run after the action
      - check: same_tip
        with: { nodes: [A, B, C] }
        timeout: 45s                   # optional assertion timeout (default 30s)
        should: true                   # optional: true = record a finding instead of aborting
        message: "fleet split after X" # optional prefix on the recorded detail
```

Durations (`timeout`) are a Go duration (`45s`, `2m`, `1m30s`) or a bare integer read as seconds.

## The `${...}` expression language

Any string in a `with:` value may contain `${...}` expressions. They are resolved just before the
action or assertion runs, against this run's variables, params, roles and live node state.

| Form | Meaning |
|---|---|
| `${name}` | a run variable (set by `as:` or the `set` action), else a param, else a role. Resolution order is **variable > param > role**. |
| `${name.field}` | a field of a map-valued variable, e.g. `${blockBelow.last}`, `${alert.sequence}`. Chainable: `${x.a.b}`. |
| `${a + b}`, `${a - b}` | integer arithmetic. **The spaces around `+`/`-` are required** and operands must be integers. |
| `${tip(A)}` | live block height of role/node `A` (read fresh, not from the lagging snapshot). |
| `${tiphash(A)}` | live tip block hash of role/node `A`. |
| `${latest}` | the highest alert sequence currently in the alert log. |
| integer literal | `${6}` is the int `6`; used as an arithmetic operand. |

Gotchas that break scenarios:

- **Type preservation.** If a value is *exactly* one `${...}` (e.g. `height: "${tip(A) + 2}"`),
  the resolved value keeps its native type (here, an int). If `${...}` is embedded in a larger
  string (e.g. `pattern: "block ${x.last} rejected"`), every match is stringified. So pass a
  number as a whole-string expression when the field needs a number.
- **Spaces in arithmetic are load-bearing.** `${tip(A)+2}` does **not** parse as arithmetic (no
  surrounding spaces); it is treated as a single variable name and fails. Write `${tip(A) + 2}`.
- **Integer only.** `${a + b}` errors if either side is not an integer. There is no float, string
  concatenation, multiplication or comparison in `${...}`; comparisons are done by the `expr`
  check, not here.
- **A role resolves to a node name.** `${A}` yields `teranode1`. Actions that take a `node:` also
  accept a role name directly (they resolve roles themselves), so `node: A` and `node: ${A}` are
  equivalent; prefer `node: A`.
- **Unknown name is an error**, which fails the step at resolve time with `resolve: unknown
  variable "x"`. Set a variable before you reference it.

## Actions

Every action takes a `with:` map and returns an output map (stored by `as:`). Output fields are
what you reference later as `${name.field}`. `node:` fields accept a role or a node name.

| action | key `with:` fields | output fields (for `as:`) |
|---|---|---|
| `note` | `message` | the message string |
| `mark` | — | `eventId`, `at` (RFC3339). Anchor for later `event`/`log_count` `since:`. |
| `set` | any `key: value` pairs | the same map; each key becomes a run variable |
| `wait` | `seconds` (default 1) | duration string |
| `mine` | `node`, `blocks` (default 1), `address` (optional; uses `generatetoaddress`) | `node`, `hashes[]`, `count`, `last` (last block hash) |
| `coinbase` | `node`, `height` (default 1) | `txid`, `hex`, `height`, `block` |
| `newkey` | `name`, `note` | `name`, `address`, `lockingScript` |
| `spend` | `node`, `txid`, `vout` (0), `key`, `to`, `toScript`, `satoshis` (0), `fee` (0), `outputs` (1), or `outs: [{script,to,satoshis}]` | `txid`, `hex`, `efHex`, `size` |
| `submit` | `target` (a node, or `arcade`), and `hex`+`efHex` **or** `tx: {hex, efHex}` | `target`, `accepted`, `status`, `body`, `txid` |
| `build_alert` | `type` (e.g. `freeze`), `message`, `reason`, `blockHash`, `enforceAt`, `txHex`, `peer`, `note`, `funds: [{txid, vout, start, stop, policyExpires}]` | `sequence`, `hash`, `type`, `text` |
| `push_alert` | `node`, `sequence` | `node`, `delivered` |
| `rpc_freeze` | `node`, `txid`, `vout` (0), `start`, `stop`, `policyExpires` | `ok`. Teranode: admin freeze RPC. SV node: consensus blacklist. |
| `rpc_unfreeze` | `node`, `txid`, `vout` (0) | `ok` |
| `partition` | `nodes` (list or comma string; default all), `plane` (`p2p`/`alert`), `on` (bool) | `nodes`, `plane`, `on`. Healed on run exit. |
| `chaos` | `node`, `action` (`pause`/`unpause`/`stop`/`start`) | `ok` |
| `automine` | `enabled` (default true if omitted), `intervalSeconds`, `node`, `blocks` | `enabled`, `intervalSeconds`, `node`, `nextMineAt`, `blocksMined` |
| `wallet_state` | — | `available`, `connected`, `balance`, `coins`, `address`, `acceptRate`, `decided` |
| `wallet_topup` | `node`, `satoshis`, `count`, `fee`, `waitSeconds`, `mine` (bool) | `txid`, `address`, `satoshis`, `outputIndex`, `internalized`, `balance`, `coins`, `fanoutTxid` |
| `wallet_tx` | `shape` (e.g. `opreturn`), `target` (node or `arcade`), `satoshis`, `to`, `outputs`, `data`, `dataHex`, `script`, `label`, `description`, `delayed` | `txid`, `shape`, `target`, `accepted`, `walletStatus`, `status`, `body`, `noSend` |
| `wallet_send_start` | `tps` (float, default 1), `workers`, `shape`, `satoshis`, `outputs`, `to`, `data`, `label`, `durationSeconds`, `autoMineSeconds`, `mineNode`, `mineBlocks` | `running`, `tps`, `workers`, `labels`, `startedAt` |
| `wallet_send_stop` | `timeoutSeconds` (default 20) | `running`, `draining`, `attempted`, `succeeded`, `failed`, `backpressure`, `canceled`, `measuredTps`, `elapsedSeconds` |
| `watch` | `txid`, `vout` (0), `label` | `ok`. Adds the output to the fleet UTXO watch list. |
| `wait_for` | `check`, `with: {...}`, `timeout` | the assertion's detail string. Runs one assertion as a blocking step; the step **fails** if it does not pass. |

Notes on the fiddly ones:

- **Zero-satoshi outputs.** The `satoshis` shorthand reads `0` as "all inputs minus fee", and
  `outputs:` splits one amount evenly. To create a genuine zero-value output (for dust-policy
  tests), spell outputs out under `outs:` with `satoshis: 0`. `spend` always appends a change
  output.
- **`submit` target `arcade`** submits to the arcade broadcaster instead of a node; any other
  target is resolved as a node/role.
- **`rpc_freeze` with no `start`/`stop`** is an immediate (legacy) freeze enforced at every
  height. Add `start`/`stop` for a height-anchored window.
- **`automine`** sets the orchestrator-wide mining cadence. A scenario that needs a static chain
  should turn it off first: `action: automine` with `with: { enabled: false }`.

## Assertions

An assertion polls until it passes or its `timeout` elapses (default 30s), **except**: `no_event`
and `expr` are evaluated once, and `log_count`/`no_event` wait out their window internally. Poll
interval is ~0.75s. On a non-pass, `should: true` records a finding; otherwise the run aborts.

| check | `with:` fields | passes when |
|---|---|---|
| `same_tip` | `nodes` (default all) | every listed node reports the same tip hash |
| `tip_is` | `node`, `hash` | node's tip hash equals `hash` |
| `tip_not` | `node`, `hash` | node's tip hash differs from `hash` |
| `height` | `node`, `equals` **or** `atLeast` | node height equals `equals`, or is ≥ `atLeast` |
| `alert_seq` | `node`, `equals` | node's applied alert sequence equals `equals` |
| `utxo_status` | `node`, `txid`, `vout` (0), `status` | node reports that output's status as `status` (e.g. `OK`, `FROZEN`) |
| `mempool_has` | `node`, `txid` | `txid` is in node's mempool |
| `mempool_lacks` | `node`, `txid` | `txid` is not in node's mempool |
| `event` | `kind`, `node`, `contains`, `pattern` (regexp), `since` (event id), `min` (default 1) | at least `min` matching bus events exist since `since` (default: this step's start) |
| `no_event` | same filters + `max` (default 0) | waits the full window, then at most `max` matching events exist |
| `log_count` | `node`, `pattern` (regexp) and/or `contains`, `since` (RFC3339, default step start), `min` (default 1) **or** `max` | with `min`: polls until ≥ `min` matching container-log lines. With `max`: waits the window, then ≤ `max`. |
| `wallet_balance` | `atLeast` and/or `coinsAtLeast` | wallet balance / spendable-coin count meet the floors |
| `wallet_tx_status` | `txid`, `wallet` (expected), `arcade` (expected) | the tx's wallet status and/or arcade status match (each checked only if given) |
| `arcade_block_status` | `hash`, `status` | arcade's processing status for the block equals `status` |
| `arcade_canonical` | `height`, `hash` | arcade's chaintracks has `hash` as the active-chain block at `height` |
| `expr` | `left`, `right`, `op` (`==` default, `!=`, `contains`) | the string comparison holds (one-shot) |

### Events, `kind`, and the `mark`/`since` pattern

`event`/`no_event` match against the orchestrator's event bus. `kind` is one of: `tip`,
`alert_seq`, `utxo`, `rejected_tx`, `invalid_block`, `chaos`, `alert`, `mine`, `tx`, `scenario`,
`log`, `error`. `contains` matches a literal substring of the event message or any data value;
`pattern` is a Go regexp over the same text (e.g. `(?i)consensus[ _-]?frozen`).

Events accumulate across the run, so "no event happened **after** step X" needs an anchor.
Place a `mark` action before the action under test and pass its `eventId` as `since`:

```yaml
- name: anchor before the fork heals
  action: mark
  as: forkMark
- name: heal the fork
  action: partition
  with: { nodes: [C], plane: p2p, on: false }
  assert:
    - check: event        # A rejected the healed-in block, once, after the anchor
      with: { kind: invalid_block, node: A, pattern: "(?i)consensus[ _-]?frozen", since: "${forkMark.eventId}" }
```

`log_count` is for behaviour a node never emits as an event: it counts container-log lines
directly (use `max` to assert a loop did **not** happen, `min` to assert it did). Anchor it with
`since: "${someMark.at}"` (the RFC3339 timestamp from a `mark`).

## Roles and params

- **Roles** name the nodes a scenario acts on, so one file runs against different nodes. Default
  them in `roles:`; override per run in the POST body's `roles`. Any `node:` field takes a role.
- **Params** are typed defaults in `params:`, overridden per run in the POST body's `params`,
  referenced as `${paramName}`. One param is special: `cleanup: false` leaves partitions open
  after the run for inspection.

Per-run override example:

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"mode":"auto","roles":{"C":"svnode2"},"params":{"windowLen":10}}' \
  localhost:8600/api/scenarios/freeze-timing-skew/run
```

## Worked examples in this directory

- **`svnode-dust-policy.yaml`** — the clearest small scenario: `newkey` → `mine` to fund →
  `spend` with an explicit zero-satoshi `outs` output → `submit` to two nodes → assert the
  strict-policy SV node rejects it (`accepted: false`) while the others accept it. Read this first.
- **`freeze-skew-rpc.yaml`** — `rpc_freeze`, `mark`, `event`/`no_event` and `log_count` together,
  including `log_count` with `max:` to assert a node did *not* enter a re-validation loop. The
  model for comparing how two builds behave.
- **`wallet-sustained-flow.yaml`** — `wallet_send_start` / `wallet_send_stop` driving a sustained
  transaction stream, with `same_tip` and `wallet_balance` assertions under load.
- **`freeze-timing-skew.yaml`** / **`freeze-fork-remine.yaml`** — multi-node alert delivery with
  `build_alert` / `push_alert`, roles for early/late/never nodes, and `${tip(A) + N}` height math.

## Adding and loading your own

Drop a `.yaml` file in a scenario directory; it loads on the next list or run start. To keep your
scenarios outside this repository, point the orchestrator at your directory with the `SCENARIOS`
environment variable, a comma-separated list where **later directories override earlier ones by
scenario id**:

```bash
# examples plus your own (yours win on an id clash)
SCENARIOS=examples/scenarios,/path/to/my/scenarios ./orchestrator
# only your own
SCENARIOS=/path/to/my/scenarios ./orchestrator
```

For the containerised simulator, mount a host directory read-only in place of the examples:

```bash
SCENARIOS_DIR=/path/to/my/scenarios make sim-up
```
