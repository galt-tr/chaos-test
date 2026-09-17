package walletsvc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/arcade"
)

// rejectedGrace is how long a REJECTED row keeps being polled.
//
// Arcade calls REJECTED terminal, but a peer that accepts after another refused supersedes it
// with SEEN_ON_NETWORK or SEEN_MULTIPLE_NODES. Freezing on the first rejection would report a
// transaction as dead that the network went on to accept.
const rejectedGrace = 2 * time.Minute

// notFoundGrace is how long a 404 stays worth re-asking. Right after a submit, "arcade has
// never heard of this" is the normal answer, not a verdict.
const notFoundGrace = 10 * time.Minute

// ArcadeTx is the subset of arcade the tracker needs; an interface so it can be faked.
type ArcadeTx interface {
	GetTx(ctx context.Context, txid string) (*arcade.TxRecord, error)
}

type tracked struct {
	row        TxRow
	created    time.Time
	lastPolled time.Time
	rejectedAt time.Time
}

// Tracker reconciles what the wallet believes against what the network did.
//
// The two are never merged into a single status: the wallet knows whether it can spend the
// change, arcade knows whether the network took the transaction, and a disagreement between
// them is the most interesting thing this harness can find.
type Tracker struct {
	arcade ArcadeTx
	bus    Publisher

	mu    sync.Mutex
	rows  map[string]*tracked
	order []string
	max   int
}

// NewTracker returns a tracker keeping at most max rows.
func NewTracker(a ArcadeTx, bus Publisher, max int) *Tracker {
	if max <= 0 {
		max = 2000
	}
	return &Tracker{arcade: a, bus: bus, rows: map[string]*tracked{}, max: max}
}

// Note records a transaction the harness just created.
func (t *Tracker) Note(txid, shape, target string, sats uint64, walletStatus string) {
	if txid == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.rows[txid]; ok {
		if walletStatus != "" {
			e.row.WalletStatus = walletStatus
		}
		return
	}
	now := time.Now()
	t.rows[txid] = &tracked{
		row: TxRow{TxID: txid, Shape: shape, Target: target, Satoshis: int64(sats),
			WalletStatus: walletStatus, CreatedAt: now.UTC().Format(time.RFC3339)},
		created: now,
	}
	t.order = append(t.order, txid)
	for len(t.order) > t.max {
		delete(t.rows, t.order[0])
		t.order = t.order[1:]
	}
}

// SetWalletStatus updates wallet-side truth from a ListActions sweep.
func (t *Tracker) SetWalletStatus(txid, status string) {
	if txid == "" || status == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.rows[txid]
	if !ok {
		now := time.Now()
		e = &tracked{row: TxRow{TxID: txid, CreatedAt: now.UTC().Format(time.RFC3339)}, created: now}
		t.rows[txid] = e
		t.order = append(t.order, txid)
	}
	e.row.WalletStatus = status
	// Re-evaluate: a wallet status arriving after arcade's can create a divergence on its own
	// (a wallet that gives up on a transaction the network already mined, for instance).
	if d, _ := divergence(e.row, e.rejectedAt, time.Now()); d {
		e.row.Diverged = true
	}
}

// Rows returns the newest n rows, newest first.
func (t *Tracker) Rows(n int) []TxRow {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n <= 0 || n > len(t.order) {
		n = len(t.order)
	}
	out := make([]TxRow, 0, n)
	for i := len(t.order) - 1; i >= 0 && len(out) < n; i-- {
		if e, ok := t.rows[t.order[i]]; ok {
			out = append(out, e.row)
		}
	}
	return out
}

// Row returns one row.
func (t *Tracker) Row(txid string) (TxRow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.rows[txid]
	if !ok {
		return TxRow{}, false
	}
	return e.row, true
}

// due lists the rows worth asking arcade about, oldest-polled first, bounded per tick.
func (t *Tracker) due(limit int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	type cand struct {
		id string
		at time.Time
	}
	var cs []cand
	for id, e := range t.rows {
		if e.row.Terminal {
			continue
		}
		cs = append(cs, cand{id, e.lastPolled})
	}
	for i := 0; i < len(cs); i++ { // small n; a full sort is not worth the import
		for j := i + 1; j < len(cs); j++ {
			if cs[j].at.Before(cs[i].at) {
				cs[i], cs[j] = cs[j], cs[i]
			}
		}
	}
	out := make([]string, 0, limit)
	for i := 0; i < len(cs) && len(out) < limit; i++ {
		out = append(out, cs[i].id)
	}
	return out
}

// Poll asks arcade about up to limit non-terminal transactions.
func (t *Tracker) Poll(ctx context.Context, limit int) {
	if t.arcade == nil {
		return
	}
	for _, id := range t.due(limit) {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		rec, err := t.arcade.GetTx(cctx, id)
		cancel()
		switch {
		case errors.Is(err, arcade.ErrTxNotFound):
			t.apply(id, &arcade.TxRecord{TxID: id, Status: arcade.StatusNotFound})
		case err != nil:
			// Arcade being briefly unavailable is not information about the transaction.
			// Leave the row untouched rather than recording a status it did not report.
			t.touch(id)
		default:
			t.apply(id, rec)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (t *Tracker) touch(txid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.rows[txid]; ok {
		e.lastPolled = time.Now()
	}
}

func (t *Tracker) apply(txid string, rec *arcade.TxRecord) {
	t.mu.Lock()
	e, ok := t.rows[txid]
	if !ok {
		t.mu.Unlock()
		return
	}
	now := time.Now()
	e.lastPolled = now
	prev := e.row.ArcadeStatus

	// Unrecognised statuses are stored verbatim and kept in the polling set. Arcade's enum is
	// open, so treating an unknown value as terminal — or as an error — would silently drop
	// transactions the next arcade release starts reporting differently.
	e.row.ArcadeStatus = rec.Status
	e.row.CheckedAt = now.UTC().Format(time.RFC3339)
	if rec.BlockHeight > 0 {
		e.row.BlockHeight = rec.BlockHeight
	}
	if rec.ExtraInfo != "" {
		e.row.ExtraInfo = rec.ExtraInfo
	}
	if len(rec.CompetingTxs) > 0 {
		e.row.CompetingTxs = rec.CompetingTxs
	}

	switch {
	case arcade.IsSettled(rec.Status):
		e.row.Terminal = true
	case rec.Status == arcade.StatusRejected:
		if e.rejectedAt.IsZero() {
			e.rejectedAt = now
		} else if now.Sub(e.rejectedAt) > rejectedGrace {
			// Left alone long enough that no peer superseded it.
			e.row.Terminal = true
		}
	case rec.Status == arcade.StatusNotFound:
		if now.Sub(e.created) > notFoundGrace {
			e.row.ArcadeStatus = arcade.StatusUnknown
			e.row.Terminal = true
		}
	default:
		// Any forward progress clears a previous rejection: this is the recovery edge.
		e.rejectedAt = time.Time{}
	}

	diverged, why := divergence(e.row, e.rejectedAt, now)
	first := diverged && !e.row.Diverged
	e.row.Diverged = diverged
	row := e.row
	t.mu.Unlock()

	if first && t.bus != nil {
		t.bus.Publish("wallet_divergence", "",
			"wallet and network disagree about "+shortID(txid)+": "+why,
			map[string]any{"txid": txid, "walletStatus": row.WalletStatus,
				"arcadeStatus": row.ArcadeStatus, "blockHeight": row.BlockHeight,
				"extraInfo": row.ExtraInfo, "competingTxs": row.CompetingTxs})
	}
	_ = prev
}

// divergence reports whether the two sources contradict each other, and how.
func divergence(r TxRow, rejectedAt time.Time, now time.Time) (bool, string) {
	switch {
	case r.ArcadeStatus == arcade.StatusDoubleSpendAttempted:
		return true, "arcade saw a conflicting spend of the same input"
	case r.ArcadeStatus == arcade.StatusRejected && !rejectedAt.IsZero() && now.Sub(rejectedAt) > rejectedGrace &&
		(r.WalletStatus == "completed" || r.WalletStatus == "unproven"):
		// The wallet believes this is spendable; the network refused it and nothing
		// superseded the rejection. This is the bug the harness exists to find.
		return true, "the wallet counts it as accepted but the network rejected it"
	case r.WalletStatus == "failed" && arcade.IsGood(r.ArcadeStatus):
		return true, "the wallet gave up on a transaction the network mined"
	}
	return false, ""
}

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}
