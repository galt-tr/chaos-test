# go-alert-system: gossip receive path drops every new alert ("error looking for duplicate alert: alert not found")

Status: **filed 2026-09-17 as https://github.com/bsv-blockchain/go-alert-system/issues/171** (found while bringing up the chaos-test regtest alert network).

Affects every release up to and including v0.1.17 (pinned by teranode). Fixed on `master` / v0.2.0 by
#167 (`495dfbd`, 2026-09-09), which moved the pubsub path into `handleAlert` and compares against
`models.ErrAlertNotFound` (`app/p2p/server.go:538`), with `TestServer_handleAlert` covering the fresh-alert
case; the fix was not called out anywhere, hence the issue. Follow-up for teranode: bump the pin to v0.2.0
(note #167 also tightens signature validation).

## Summary

`Server.Subscribe` (app/p2p/server.go) rejects every alert that arrives over gossipsub and is
*not yet stored*, because its duplicate check treats "not found" as a real error:

```go
// app/p2p/server.go (v0.1.17: L531-545; master/v0.2.0 is fixed, see status)
var dup *models.AlertMessage
if dup, err = models.GetAlertMessageBySequenceNumber(ctx, ak.SequenceNumber, ...); err == nil && dup != nil && len(dup.Hash) > 0 {
    // duplicate → continue
}
// Did we get a real error?
if err != nil && !errors.Is(err, datastore.ErrNoResults) {
    s.config.Services.Log.Errorf("error looking for duplicate alert: %s", err.Error())
    continue
}
```

but `GetAlertMessageBySequenceNumber` (app/models/alert_message.go) maps the datastore's
"no results" into the package's own sentinel, which does not wrap `datastore.ErrNoResults`:

```go
if errors.Is(err, datastore.ErrNoResults) {
    return nil, ErrAlertNotFound   // errors.New("alert not found") in app/models/errors.go
}
```

So for a fresh alert the check `!errors.Is(err, datastore.ErrNoResults)` is always true, the
alert is logged as an error and skipped. The only way an alert reaches a node is the
peer-to-peer *sync stream* (`StreamThread`, on a discovery round or when a peer pushes
`IGotLatest`), which does not perform this check.

## Evidence

Regtest network: one teranode (PR #1764 build, go-alert-system v0.1.17 embedded) and one
go-alert-system node (v0.1.17), private libp2p network, alert #1 (freeze) published on the
topic by a third participant built on `p2p.NewServer`. Both receivers logged, in order:

```
error verifying signature …: address (…) not found - compressed: true   (x2, debug; the loop tries each key)
ERROR | error looking for duplicate alert: alert not found
```

and kept `sequence: 0`. Pushing the identical bytes over the sync stream (`IGotLatest{1}` →
node requests `IWantSequenceNumber(1)`) was accepted, executed (`AddToConsensusBlacklist`
for 2 funds) and stored, and the second node then synced it on its next discovery round.

Impact on mainnet/teratestnet: alert propagation depends entirely on the periodic sync
(`peer_discovery_interval`, default 10 minutes) instead of gossip.

## Fix

Treat the package sentinel as "no duplicate":

```go
if err != nil && !errors.Is(err, datastore.ErrNoResults) && !errors.Is(err, models.ErrAlertNotFound) {
```

or have `GetAlertMessageBySequenceNumber` wrap rather than replace
(`fmt.Errorf("%w: %w", ErrAlertNotFound, err)`), plus a test that publishes a fresh alert on
the topic and asserts it is stored. The same check exists in `genesis_alert.go` and already
uses `ErrAlertNotFound`, which suggests this is the intended sentinel.
