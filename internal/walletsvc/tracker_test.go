package walletsvc

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/arcade"
	"github.com/bsv-blockchain/chaos-test/internal/observe"
)

type fakeArcade struct {
	rec  *arcade.TxRecord
	err  error
	hits int
}

func (f *fakeArcade) GetTx(context.Context, string) (*arcade.TxRecord, error) {
	f.hits++
	if f.err != nil {
		return nil, f.err
	}
	return f.rec, nil
}

type capturingBus struct{ events []string }

func (c *capturingBus) Publish(kind, _, msg string, _ map[string]any) observe.Event {
	c.events = append(c.events, kind+":"+msg)
	return observe.Event{}
}

func TestMinedIsTerminal(t *testing.T) {
	a := &fakeArcade{rec: &arcade.TxRecord{TxID: "a", Status: arcade.StatusMined, BlockHeight: 120}}
	tr := NewTracker(a, nopBus{}, 10)
	tr.Note("a", ShapePayment, TargetArcade, 1000, "sending")
	tr.Poll(context.Background(), 10)
	r, _ := tr.Row("a")
	if !r.Terminal || r.ArcadeStatus != arcade.StatusMined || r.BlockHeight != 120 {
		t.Fatalf("got %+v", r)
	}
	// A settled row must drop out of the polling set.
	before := a.hits
	tr.Poll(context.Background(), 10)
	if a.hits != before {
		t.Fatal("a terminal row should no longer be polled")
	}
}

// A REJECTED transaction is provisional: a later SEEN_* supersedes it, so it must keep being
// polled rather than freezing on the first rejection.
func TestRejectedStaysProvisionalThenRecovers(t *testing.T) {
	a := &fakeArcade{rec: &arcade.TxRecord{TxID: "a", Status: arcade.StatusRejected}}
	tr := NewTracker(a, nopBus{}, 10)
	tr.Note("a", ShapePayment, TargetArcade, 1000, "completed")
	tr.Poll(context.Background(), 10)

	if r, _ := tr.Row("a"); r.Terminal {
		t.Fatal("REJECTED must not be terminal inside the grace window")
	}
	a.rec = &arcade.TxRecord{TxID: "a", Status: arcade.StatusSeenMultipleNodes}
	tr.Poll(context.Background(), 10)
	r, _ := tr.Row("a")
	if r.ArcadeStatus != arcade.StatusSeenMultipleNodes {
		t.Fatalf("recovery not applied: %+v", r)
	}
	if r.Diverged {
		t.Fatal("a recovered transaction must not be left marked as diverged")
	}
}

// Past the grace window with no supersession, a rejection the wallet counts as accepted is a
// genuine divergence and must be announced exactly once.
func TestRejectedPastGraceDiverges(t *testing.T) {
	a := &fakeArcade{rec: &arcade.TxRecord{TxID: "a", Status: arcade.StatusRejected}}
	bus := &capturingBus{}
	tr := NewTracker(a, bus, 10)
	tr.Note("a", ShapePayment, TargetArcade, 1000, "completed")
	tr.Poll(context.Background(), 10)

	tr.mu.Lock()
	tr.rows["a"].rejectedAt = time.Now().Add(-2 * rejectedGrace)
	tr.mu.Unlock()
	tr.Poll(context.Background(), 10)

	r, _ := tr.Row("a")
	if !r.Terminal || !r.Diverged {
		t.Fatalf("want terminal+diverged, got %+v", r)
	}
	n := 0
	for _, e := range bus.events {
		if len(e) > 17 && e[:17] == "wallet_divergence" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one divergence event, got %d (%v)", n, bus.events)
	}
	tr.Poll(context.Background(), 10)
	// Still one: a terminal row is not re-polled, and divergence is announced once.
	if len(bus.events) != n {
		t.Fatalf("divergence re-announced: %v", bus.events)
	}
}

// An unrecognised status is arcade adding a value we have not seen. Carry it verbatim and keep
// polling — never treat it as terminal, and never error.
func TestUnknownStatusIsCarriedAndKeptPolling(t *testing.T) {
	a := &fakeArcade{rec: &arcade.TxRecord{TxID: "a", Status: "SOME_FUTURE_STATE"}}
	tr := NewTracker(a, nopBus{}, 10)
	tr.Note("a", ShapePayment, TargetArcade, 1000, "sending")
	tr.Poll(context.Background(), 10)
	r, _ := tr.Row("a")
	if r.ArcadeStatus != "SOME_FUTURE_STATE" {
		t.Fatalf("unknown status not preserved: %q", r.ArcadeStatus)
	}
	if r.Terminal {
		t.Fatal("an unknown status must not be treated as terminal")
	}
}

func TestNotFoundBecomesUnknownAfterGrace(t *testing.T) {
	a := &fakeArcade{err: arcade.ErrTxNotFound}
	tr := NewTracker(a, nopBus{}, 10)
	tr.Note("a", ShapePayment, TargetArcade, 1000, "sending")
	tr.Poll(context.Background(), 10)
	if r, _ := tr.Row("a"); r.ArcadeStatus != arcade.StatusNotFound || r.Terminal {
		t.Fatalf("a fresh 404 is normal, not a verdict: %+v", r)
	}
	tr.mu.Lock()
	tr.rows["a"].created = time.Now().Add(-2 * notFoundGrace)
	tr.mu.Unlock()
	tr.Poll(context.Background(), 10)
	r, _ := tr.Row("a")
	if r.ArcadeStatus != arcade.StatusUnknown || !r.Terminal {
		t.Fatalf("want UNKNOWN+terminal after the grace window, got %+v", r)
	}
}

// Arcade being unreachable is not information about the transaction.
func TestArcadeErrorLeavesStatusAlone(t *testing.T) {
	a := &fakeArcade{rec: &arcade.TxRecord{TxID: "a", Status: arcade.StatusSeenOnNetwork}}
	tr := NewTracker(a, nopBus{}, 10)
	tr.Note("a", ShapePayment, TargetArcade, 1000, "sending")
	tr.Poll(context.Background(), 10)
	a.err = context.DeadlineExceeded
	tr.Poll(context.Background(), 10)
	if r, _ := tr.Row("a"); r.ArcadeStatus != arcade.StatusSeenOnNetwork {
		t.Fatalf("a failed poll overwrote a known status: %+v", r)
	}
}

// The wallet giving up on a transaction the network mined is a divergence too.
func TestWalletFailedButMinedDiverges(t *testing.T) {
	a := &fakeArcade{rec: &arcade.TxRecord{TxID: "a", Status: arcade.StatusMined, BlockHeight: 5}}
	bus := &capturingBus{}
	tr := NewTracker(a, bus, 10)
	tr.Note("a", ShapePayment, TargetArcade, 1000, "failed")
	tr.Poll(context.Background(), 10)
	if r, _ := tr.Row("a"); !r.Diverged {
		t.Fatalf("want divergence, got %+v", r)
	}
}

func TestRowsEvictsOldest(t *testing.T) {
	tr := NewTracker(&fakeArcade{}, nopBus{}, 2)
	tr.Note("a", "", "", 0, "sending")
	tr.Note("b", "", "", 0, "sending")
	tr.Note("c", "", "", 0, "sending")
	if _, ok := tr.Row("a"); ok {
		t.Fatal("oldest row should have been evicted")
	}
	if rows := tr.Rows(10); len(rows) != 2 || rows[0].TxID != "c" {
		t.Fatalf("want newest-first [c b], got %+v", rows)
	}
}
