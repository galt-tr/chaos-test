package automine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/observe"
)

type fakeMiner struct {
	calls atomic.Int64
	at    []time.Time
	mu    sync.Mutex
	err   error
	delay time.Duration
}

func (f *fakeMiner) Mine(ctx context.Context, _ string, blocks int) ([]string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.at = append(f.at, time.Now())
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
	out := make([]string, blocks)
	for i := range out {
		out[i] = "hash"
	}
	return out, nil
}

func (f *fakeMiner) times() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.at...)
}

type nopBus struct{}

func (nopBus) Publish(string, string, string, map[string]any) observe.Event { return observe.Event{} }

// newTest builds a service with a sub-second interval.
//
// The public minimum is 10s, which is right for a chaos harness but would make every test here
// take minutes. Tests live in the package, so they set the interval past the clamp directly
// rather than weakening the guard for production.
func newTest(m Miner, cfg Config) *Service {
	raw := cfg.Interval
	s := New(m, nopBus{}, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if raw > 0 && raw < MinInterval {
		s.cfg.Interval = raw
	}
	return s
}

// configureFast is Configure with the same clamp bypass.
func (s *Service) configureFast(cfg Config) {
	s.Configure(cfg)
	s.mu.Lock()
	if cfg.Interval > 0 && cfg.Interval < MinInterval {
		s.cfg.Interval = cfg.Interval
		s.nextAt = time.Now().Add(cfg.Interval)
	}
	s.mu.Unlock()
	select {
	case s.reset <- struct{}{}:
	default:
	}
}

// The core promise: a constant cadence.
func TestMinesOnAConstantInterval(t *testing.T) {
	m := &fakeMiner{}
	s := newTest(m, Config{Enabled: true, Node: "teranode1", Interval: 300 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(1050 * time.Millisecond)
	cancel()

	if n := m.calls.Load(); n < 2 || n > 4 {
		t.Fatalf("want ~3 mines in 1.05s at 300ms, got %d", n)
	}
	// Gaps must stay close to the interval rather than drifting.
	at := m.times()
	for i := 1; i < len(at); i++ {
		gap := at[i].Sub(at[i-1])
		if gap < 200*time.Millisecond || gap > 450*time.Millisecond {
			t.Fatalf("gap %v is not a steady 300ms cadence", gap)
		}
	}
}

// The behaviour the user asked for explicitly: mining by hand must not move the schedule.
// The service has no view of the tip at all, which is how that is guaranteed — this test pins
// the absence of that coupling.
func TestManualMiningDoesNotAffectTheSchedule(t *testing.T) {
	m := &fakeMiner{}
	s := newTest(m, Config{Enabled: true, Node: "teranode1", Interval: 400 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(50 * time.Millisecond) // let Run set the first deadline

	before := s.Status().NextMineAt
	if before == "" {
		t.Fatal("no countdown was set")
	}
	// Simulate somebody mining by hand several times: the chain advances, nothing here does.
	for i := 0; i < 5; i++ {
		_, _ = m.Mine(ctx, "teranode1", 1)
	}
	time.Sleep(120 * time.Millisecond)
	if got := s.Status().NextMineAt; got != before {
		t.Fatalf("manual mining moved the countdown: %s -> %s", before, got)
	}
	cancel()
}

// A slow or failing mine must not push the cadence out.
func TestScheduleDoesNotDriftOnSlowMine(t *testing.T) {
	m := &fakeMiner{delay: 250 * time.Millisecond}
	s := newTest(m, Config{Enabled: true, Node: "teranode1", Interval: 300 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(1100 * time.Millisecond)
	cancel()
	if n := m.calls.Load(); n < 2 {
		t.Fatalf("a slow mine stalled the schedule: only %d runs", n)
	}
}

func TestFailureKeepsTheScheduleAndRecordsTheError(t *testing.T) {
	m := &fakeMiner{err: errors.New("node refused")}
	s := newTest(m, Config{Enabled: true, Node: "teranode1", Interval: 250 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(700 * time.Millisecond)
	cancel()

	st := s.Status()
	if st.LastError == "" {
		t.Fatal("want the failure recorded so a silently broken auto-miner is visible")
	}
	if st.BlocksMined != 0 {
		t.Fatalf("no blocks should be counted, got %d", st.BlocksMined)
	}
	if m.calls.Load() < 2 {
		t.Fatal("a failure must not stop the cadence")
	}
}

func TestDisabledDoesNotMineButStillCountsDown(t *testing.T) {
	m := &fakeMiner{}
	s := newTest(m, Config{Enabled: false, Node: "teranode1", Interval: 200 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(600 * time.Millisecond)
	cancel()

	if m.calls.Load() != 0 {
		t.Fatalf("disabled must not mine, got %d calls", m.calls.Load())
	}
	// The countdown keeps running so re-enabling is predictable rather than a surprise.
	if s.Status().NextMineAt == "" {
		t.Fatal("want a countdown even while disabled")
	}
}

// Changing the interval must take effect at once, not after the old one elapses.
func TestConfigureRestartsTheCountdown(t *testing.T) {
	m := &fakeMiner{}
	s := newTest(m, Config{Enabled: true, Node: "teranode1", Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	s.configureFast(Config{Enabled: true, Node: "teranode1", Interval: 200 * time.Millisecond})
	time.Sleep(700 * time.Millisecond)
	if m.calls.Load() == 0 {
		t.Fatal("a new interval should apply immediately, not after the previous one elapsed")
	}
}

// Configure through the public API must clamp rather than accept anything.
func TestConfigureClamps(t *testing.T) {
	s := newTest(&fakeMiner{}, Config{Enabled: true, Node: "n", Interval: time.Hour})
	if got := s.Configure(Config{Enabled: true, Node: "n", Interval: time.Millisecond}).IntervalSeconds; got != int(MinInterval.Seconds()) {
		t.Fatalf("want the clamped minimum, got %ds", got)
	}
}

func TestClampsAndDefaults(t *testing.T) {
	s := newTest(&fakeMiner{}, Config{})
	st := s.Status()
	if st.IntervalSeconds != int(DefaultInterval.Seconds()) {
		t.Fatalf("want the %v default, got %ds", DefaultInterval, st.IntervalSeconds)
	}
	if st.Blocks != 1 {
		t.Fatalf("want 1 block, got %d", st.Blocks)
	}
	// New, not newTest: the helper deliberately bypasses the clamp for speed, so the clamp
	// itself has to be checked through the real constructor.
	real := New(&fakeMiner{}, nopBus{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{Interval: time.Millisecond})
	if got := real.Status().IntervalSeconds; got != int(MinInterval.Seconds()) {
		t.Fatalf("a sub-minimum interval should clamp up, got %ds", got)
	}
	if got := newTest(&fakeMiner{}, Config{Blocks: 99}).Status().Blocks; got != 5 {
		t.Fatalf("blocks should clamp to 5, got %d", got)
	}
}

// No node configured means nothing to mine on; it must not panic or spin.
func TestNoNodeIsInert(t *testing.T) {
	m := &fakeMiner{}
	s := newTest(m, Config{Enabled: true, Interval: 150 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(500 * time.Millisecond)
	cancel()
	if m.calls.Load() != 0 {
		t.Fatal("with no node configured there is nothing to mine on")
	}
}
