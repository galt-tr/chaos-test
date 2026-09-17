package walletsvc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/chaos-test/internal/observe"
	"time"
)

type fakeSender struct {
	mu    sync.Mutex
	calls int
	err   error
	delay time.Duration
	seen  []time.Time
}

func (f *fakeSender) SendOne(ctx context.Context, _ TxRequest) (*TxResult, error) {
	f.mu.Lock()
	f.calls++
	f.seen = append(f.seen, time.Now())
	err, delay := f.err, f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	if err != nil {
		return nil, err
	}
	return &TxResult{TxID: "deadbeef"}, nil
}

func (f *fakeSender) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type fakeCoins struct{ n atomic.Uint32 }

func (c *fakeCoins) Coins(context.Context) (uint32, error) { return c.n.Load(), nil }

type nopBus struct{}

func (nopBus) Publish(string, string, string, map[string]any) observe.Event { return observe.Event{} }

// The controller no longer mines: the cadence belongs to internal/automine, which is tested
// there. These tests are only about pacing, counting and lifecycle.
func newTestController(s TxSender) *Controller {
	return NewController(s, &fakeCoins{}, nopBus{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestStartRejectsSecondRun(t *testing.T) {
	c := newTestController(&fakeSender{delay: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.Start(ctx, SendRequest{TPS: 1}); err != nil {
		t.Fatal(err)
	}
	defer c.Stop(time.Second)
	_, err := c.Start(ctx, SendRequest{TPS: 1})
	if ReasonOf(err) != ReasonAlreadyRunning {
		t.Fatalf("want %s, got %v", ReasonAlreadyRunning, err)
	}
}

// The sustained send must refuse a node target: that path builds with NoSend, whose change is
// parked rather than spendable, so a continuous run would strand the whole balance.
func TestStartRejectsLegacyTarget(t *testing.T) {
	c := newTestController(&fakeSender{})
	_, err := c.Start(context.Background(), SendRequest{TPS: 1, Target: "svnode1"})
	if ReasonOf(err) != ReasonLegacyInSend {
		t.Fatalf("want %s, got %v", ReasonLegacyInSend, err)
	}
	if c.Status().Running {
		t.Fatal("a rejected start must not leave the controller running")
	}
}

// Pressing Stop cancels in-flight sends. Those must not be booked as failures, or every clean
// shutdown reads as an outage.
func TestCanceledIsNotAFailure(t *testing.T) {
	c := newTestController(&fakeSender{err: context.Canceled})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.Start(ctx, SendRequest{TPS: 20, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	s := c.Stop(2 * time.Second)
	if s.Failed != 0 {
		t.Fatalf("context.Canceled booked as %d failures", s.Failed)
	}
	if s.Canceled == 0 {
		t.Fatal("want cancellations counted separately")
	}
}

// An empty coin pool is the send waiting, not the send failing.
func TestInsufficientFundsIsBackpressure(t *testing.T) {
	c := newTestController(&fakeSender{err: errors.New("not enough funds in the default basket")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.Start(ctx, SendRequest{TPS: 20, Workers: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	s := c.Stop(3 * time.Second)
	if s.Backpressure == 0 {
		t.Fatal("want backpressure counted")
	}
	if s.Failed != 0 {
		t.Fatalf("insufficient funds must not count as failure, got %d", s.Failed)
	}
	if !s.WaitingFunds {
		t.Fatal("want waitingForFunds surfaced so the UI can say 'idle, not failed'")
	}
}

// Burst 1: after an idle gap the limiter must not release a banked herd. With burst>1 a 1s
// pause at 10 tx/s would admit ~10 sends instantly.
func TestPacingDoesNotBurstAfterIdle(t *testing.T) {
	f := &fakeSender{}
	c := newTestController(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.Start(ctx, SendRequest{TPS: 10, Workers: 4}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	c.Stop(2 * time.Second)
	// ~7 expected at 10/s; allow generous slack but catch a burst of the full bucket.
	if n := f.count(); n > 14 {
		t.Fatalf("burst detected: %d sends in ~0.7s at 10 tx/s", n)
	}
}

// Stop must let in-flight work finish rather than aborting it.
func TestStopDrainsInFlight(t *testing.T) {
	f := &fakeSender{delay: 300 * time.Millisecond}
	c := newTestController(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.Start(ctx, SendRequest{TPS: 20, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	s := c.Stop(5 * time.Second)
	if s.Running || s.Draining {
		t.Fatalf("want a completed drain, got running=%v draining=%v", s.Running, s.Draining)
	}
	if s.Succeeded == 0 {
		t.Fatal("in-flight sends should have completed, not been aborted")
	}
}

// Duration expiry stops scheduling; it must not cancel the workers.
func TestDurationStopsProduction(t *testing.T) {
	f := &fakeSender{}
	c := newTestController(f)
	if _, err := c.Start(context.Background(), SendRequest{TPS: 20, Workers: 2, DurationSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		if !c.Status().Running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("run did not stop when its duration elapsed")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if s := c.Status(); s.Failed != 0 || s.Canceled != 0 {
		t.Fatalf("duration expiry must not abort workers: failed=%d canceled=%d", s.Failed, s.Canceled)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	c := newTestController(&fakeSender{})
	if s := c.Stop(time.Second); s.Running {
		t.Fatal("stopping an idle controller should be a no-op")
	}
}

func TestNormaliseClamps(t *testing.T) {
	got := normalise(SendRequest{TPS: 9999, Workers: 99})
	if got.TPS != maxTPS {
		t.Fatalf("tps not clamped: %v", got.TPS)
	}
	if got.Workers != maxWorkers {
		t.Fatalf("workers not clamped: %v", got.Workers)
	}
	if got.Target != TargetArcade {
		t.Fatalf("target must be forced to arcade, got %q", got.Target)
	}
}
