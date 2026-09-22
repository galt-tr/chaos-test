# Wallet: wallet-infra and walletd

Three containers under the `wallet` compose profile:

| container | role |
|---|---|
| `bsv-regtest-wallet-infra` | go-wallet-toolbox **storage server**: the BRC-100 back end (UTXO storage, action building, signing coordination, broadcasting through arcade, proof tracking through arcade's events and chaintracks). Port 18100. |
| `bsv-regtest-wallet-db` | its PostgreSQL (`storage` database, `wallet:wallet`), volume `.data/wallet-db`. Not published. |
| `bsv-regtest-walletd` | a small HTTP wallet written for this network. It holds the dev wallet identity (`walletUserKey` in `config/keys.json`), does the BRC-103 handshake to wallet-infra and exposes the everyday operations as plain JSON on port 18700. |

The user's private key never leaves walletd: wallet-infra stores outputs and builds actions,
walletd signs.

## wallet-infra

`config/wallet-infra/infra-config.yaml` is generated. Points worth knowing:

- `bsv_network: tstn`. go-wallet-toolbox has no regtest profile; `tstn` is testnet-based and
  uses testnet address encoding, which regtest shares, so a regtest node pays a `tstn`
  address without translation.
- `server_private_key`: `walletServerKey` from `keys.json` (the server's BRC-103 identity).
- `wallet_services.arcade`: broadcasting and status events go to arcade (`url`, `events_url`,
  `full_status_updates: true`); `arc`, `whats_on_chain`, `bitails` and `bhs` are off.
- `wallet_services.chaintracks`: `mode: remote`, `remote_url: http://arcade:8083/chaintracks`,
  i.e. arcade's embedded header server; BEEF verification checks merkle roots against it.
- `fee_model: 100 sat/kb`, `change_basket` with 32 desired UTXOs of at least 1000 satoshis,
  `utxo_management.strategy: privacy`.
- `monitor.tasks`: `check_for_proofs` every 15 s, `send_waiting` every 60 s, `fail_abandoned`
  every 300 s, `un_fail` every 600 s.
- `http.cors` allows every origin and the BRC-103/BRC-105 headers so a browser wallet on
  another port can talk to it.

The server speaks BRC-100 over JSON-RPC (`POST /`) and REST (`/storage/v1/*`) behind BRC-103
mutual authentication, so a bare `curl localhost:18100/` returns
`401 {"error":"authentication required"}` by design. Connect with go-wallet-toolbox's storage
client or the TypeScript `StorageClient` from `@bsv/wallet-toolbox`, using
`http://localhost:18100` as the storage URL and any BRC-100 wallet as the identity.

## walletd

Configuration (flags or environment): `LISTEN` (`:8700`), `WALLET_INFRA_URL`, `KEYS`
(`/config/keys.json`, read for `walletUserKey`), `BSV_NETWORK` (`tstn`), `ORIGINATOR`
(`bsv-regtest-wallet.local`). It connects lazily and retries, so it starts before wallet-infra
is ready.

| route | body | answer |
|---|---|---|
| `GET /healthz` | | process liveness |
| `GET /v1/deposit` | | `{network, address, lockingScriptHex, derivationPrefixB64, derivationSuffixB64, suggestedSatoshis}` |
| `GET /v1/state` | | `{connected, network, identityKey, address, balance, coins, storageURL}` |
| `GET /v1/outputs` | | `{outputs: [{outpoint, satoshis, spendable}]}` |
| `GET /v1/actions?labels=a,b&limit=N&include=1` | | `{labels, total, sampled, buckets, accepted, decided, acceptRate, sampledAt, actions: [{txid, status, satoshis, description}]}`; default label `bsv-regtest-wallet`; `buckets` counts toolbox statuses such as `unproven` (broadcast, no proof yet) and `completed` |
| `POST /v1/tx` | `{shape, satoshis, to, outputs, data, dataHex, script, labels, description, noSend}` | `{txid, status, satoshis, rawHex, efHex, labels, noSend}` |
| `POST /v1/internalize` | `{atomicBeefHex, expectedAddress, outputIndex, description}` | `{accepted, outputIndex, balance, coins}` |

Shapes for `POST /v1/tx`:

- `payment`: one P2PKH output of `satoshis` to `to` (an address or a locking-script hex).
- `opreturn`: one zero-satoshi data output from `data` (UTF-8) or `dataHex`.
- `fanout`: `outputs` equal outputs of `satoshis` each, back to the wallet (a coin splitter).
- `custom`: `script` as the locking script, passed through unvalidated.

Every action carries the label `bsv-regtest-wallet` plus what you pass in `labels`.
Broadcasting is wallet-infra's job (it posts to arcade). With `noSend: true` walletd only builds
and signs and returns `rawHex`/`efHex` for you to deliver elsewhere (for example to an SV node
with `stackctl submit --rpc $SVNODE1_RPC`); note that the change of a `noSend` transaction is
parked, not spendable, until the transaction is seen.

```bash
curl -s localhost:18700/v1/state | jq '{connected, address, balance, coins}'
curl -s -X POST localhost:18700/v1/tx -H 'Content-Type: application/json' \
     -d '{"shape":"payment","satoshis":1000,"to":"<address>","labels":["demo"]}' | jq '{txid, status}'
curl -s -X POST localhost:18700/v1/tx -H 'Content-Type: application/json' \
     -d '{"shape":"opreturn","data":"hello bsv-regtest"}' | jq '{txid, status}'
curl -s -X POST localhost:18700/v1/tx -H 'Content-Type: application/json' \
     -d '{"shape":"fanout","outputs":10,"satoshis":2000}' | jq '{txid, status}'
curl -s localhost:18700/v1/outputs | jq '.outputs[] | {outpoint, satoshis}'
curl -s 'localhost:18700/v1/actions?include=1&limit=5' | jq '{total, buckets, actions: [.actions[] | {txid, status}]}'
```

`POST /v1/tx` answers as soon as wallet-infra has built, signed and handed the transaction to
arcade (`status: "unproven"`); the proof arrives through arcade's events and the
`check_for_proofs` task (every 15 s) and the action becomes `completed`.

## Funding the wallet

A BRC-100 wallet credits an incoming payment through `internalizeAction`, which accepts only
**atomic BEEF** (BRC-95) whose merkle path the storage server's chain tracker can verify. Raw
transaction hex is rejected. So the funding transaction has to be mined and proven first.
`stackctl topup` does the whole round trip from the tools container:

```bash
make tools
stackctl topup                       # 100 000 satoshis from the first spendable coinbase
stackctl topup --sats 5000000        # more
# {"deposit":"…","coinbase":"…","coinbaseHeight":1,"txid":"…","blockHeight":102,"accepted":true,"balance":100000,"coins":1}
```

What it does, and how to do it by hand:

1. `GET /v1/deposit` for the address and its locking script. The address is a BRC-29 payment
   address derived from the wallet key with the sender `AnyoneKey` and fixed derivation
   constants, so it is deterministic and can be shown before the wallet is connected.
2. Find a mature, unspent coinbase (block height ≤ tip − 100 whose output 0 is `OK` on
   `/api/v1/utxos/{txid}/json`); all coinbases pay the miner key (`minerWIF` in the inventory).
3. `stackctl spend --txid <coinbase> --vout 0 --key <minerWIF> --to-script <lockingScriptHex>
   --sats 100000`: the change returns to the miner key.
4. `stackctl submit --arcade $ARCADE_URL --hex <efHex>`.
5. Wait until the node reports the transaction (`/api/v1/txmeta/{txid}/json`), mine a block
   (`stackctl mine`), and poll arcade until `GET /tx/{txid}` carries a `merklePath`.
6. Build atomic BEEF from `rawTx` and `merklePath` (`wallet.BuildAtomicBEEF` in this module,
   or go-sdk: set `tx.MerklePath`, `NewBeefFromTransaction`, `AtomicBytes`).
7. `POST /v1/internalize` with `atomicBeefHex` and `expectedAddress` (walletd then picks the
   output that pays the deposit address instead of trusting an index).

The wallet keeps its state in Postgres (`.data/wallet-db`), so it survives `make down`;
`make clean` wipes it along with the chain, which is what you want, since the coins would no
longer exist.

## Protocols, for orientation

- **BRC-100**: the wallet interface (`createAction`, `signAction`, `internalizeAction`,
  `listOutputs`, …) that walletd calls on the toolbox and the storage server implements.
- **BRC-103 / BRC-104**: mutual authentication between walletd and wallet-infra; every request
  is signed by the client identity key, which is why plain HTTP clients get 401.
- **BRC-29**: how a payment address is derived from sender, recipient and a derivation
  prefix/suffix; walletd's deposit uses it with the public `AnyoneKey` as sender.
- **BRC-62 / BRC-95**: BEEF and atomic BEEF, the transaction-plus-proofs envelope the wallet
  ingests; **BRC-74** BUMP is the merkle path format arcade returns.
