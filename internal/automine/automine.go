// Package automine keeps the chain moving on a fixed cadence.
//
// It is deliberately a metronome, not a heartbeat: it mines on a constant interval regardless
// of what else happened to the chain. Mining by hand, a scenario's own mine step, or a wallet
// send all leave the schedule untouched, so the countdown a user sees is always the truth.
//
// It lives at orchestrator level rather than inside the wallet's send controller because the
// cadence is a property of the harness, not of any one page or run: blocks should keep coming
// whether or not anybody is sending transactions.
package automine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/observe"
)

// Bounds. A very short interval turns a chaos harness into a block spammer; a very long one is
// indistinguishable from off, which is what Enabled is for.
const (
	MinInterval     = 10 * time.Second
	MaxInterval     = 24 * time.Hour
	DefaultInterval = 10 * time.Minute
)

// Miner mines blocks on a node.
type Miner interface {
	Mine(ctx context.Context, node string, blocks int) ([]string, error)
}

// Publisher is the event bus.
type Publisher interface {
	Publish(kind, node, message string, data map[string]any) observe.Event
}

// Config is the mining cadence.
type Config struct {
	Enabled  bool          `json:"enabled"`
	Interval time.Duration `json:"-"`
	Node     string        `json:"node"`
	Blocks   int           `json:"blocks"`
}

// Status is what the UI renders: a countdown plus what happened last time.
type Status struct {
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int    `json:"intervalSeconds"`
	Node            string `json:"node"`
	Blocks          int    `json:"blocks"`
	// NextMineAt is an absolute instant so a client can run its own countdown without
	// depending on poll timing. Now is sent alongside it so a client whose clock disagrees
	// with the container's still counts down correctly.
	NextMineAt  string `json:"nextMineAt,omitempty"`
	Now         string `json:"now"`
	LastMinedAt string `json:"lastMinedAt,omitempty"`
	BlocksMined uint64 `json:"blocksMined"`
	Runs        uint64 `json:"runs"`
	LastError   string `json:"lastError,omitempty"`
}

// Service mines on a fixed schedule.
type Service struct {
	miner Miner
	bus   Publisher
	log   *slog.Logger

	mu          sync.RWMutex
	cfg         Config
	nextAt      time.Time
	lastAt      time.Time
	blocksMined uint64
	runs        uint64
	lastErr     string

	// reset wakes the loop when the configuration changes, so a new interval takes effect
	// immediately instead of after the old one elapses.
	reset chan struct{}
}

// New builds the service. Interval and Blocks are clamped; a zero interval becomes the default.
func New(miner Miner, bus Publisher, log *slog.Logger, cfg Config) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{miner: miner, bus: bus, log: log, reset: make(chan struct{}, 1)}
	s.cfg = clamp(cfg)
	return s
}

func clamp(c Config) Config {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.Interval < MinInterval {
		c.Interval = MinInterval
	}
	if c.Interval > MaxInterval {
		c.Interval = MaxInterval
	}
	if c.Blocks <= 0 {
		c.Blocks = 1
	} else if c.Blocks > 5 {
		c.Blocks = 5
	}
	return c
}

// Run drives the schedule until the context ends.
//
// The next fire time is computed from the previous one, not from when the last mine finished,
// so a slow mine does not drift the cadence.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.nextAt = time.Now().Add(s.cfg.Interval)
	s.mu.Unlock()

	for {
		s.mu.RLock()
		wait := time.Until(s.nextAt)
		enabled, node, blocks, interval := s.cfg.Enabled, s.cfg.Node, s.cfg.Blocks, s.cfg.Interval
		s.mu.RUnlock()
		if wait < 0 {
			wait = 0
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.reset:
			// Configuration changed: Configure already moved nextAt.
			t.Stop()
			continue
		case <-t.C:
		}

		// Advance the schedule first. Doing it before the mine keeps the cadence constant even
		// if mining is slow or fails, which is the whole point of a metronome.
		s.mu.Lock()
		s.nextAt = s.nextAt.Add(interval)
		// If we fell far behind (a suspended process, a long mine), skip forward rather than
		// firing repeatedly to "catch up".
		if now := time.Now(); s.nextAt.Before(now) {
			s.nextAt = now.Add(interval)
		}
		s.mu.Unlock()

		if !enabled || node == "" {
			continue
		}
		s.mineOnce(ctx, node, blocks)
	}
}

func (s *Service) mineOnce(ctx context.Context, node string, blocks int) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	s.mu.RLock()
	m := s.miner
	s.mu.RUnlock()
	if m == nil {
		return
	}
	hashes, err := m.Mine(cctx, node, blocks)
	s.mu.Lock()
	s.runs++
	if err != nil {
		s.lastErr = err.Error()
		s.mu.Unlock()
		s.log.Warn("auto-mine failed", "node", node, "err", err)
		if s.bus != nil {
			s.bus.Publish("error", node, "auto-mine failed: "+err.Error(), nil)
		}
		return
	}
	s.lastErr = ""
	s.lastAt = time.Now()
	s.blocksMined += uint64(len(hashes))
	s.mu.Unlock()

	if s.bus != nil {
		s.bus.Publish("mine", node,
			fmt.Sprintf("%s mined %d block(s) [auto-mine]", node, len(hashes)),
			map[string]any{"hashes": hashes, "source": "automine"})
	}
}

// SetMiner supplies the miner after construction. The orchestrator builds the API server
// (which does the mining) after this service, so one of the two has to be wired second.
func (s *Service) SetMiner(m Miner) {
	s.mu.Lock()
	s.miner = m
	s.mu.Unlock()
}

// Configure changes the cadence and restarts the countdown from now.
func (s *Service) Configure(cfg Config) Status {
	s.mu.Lock()
	s.cfg = clamp(cfg)
	s.nextAt = time.Now().Add(s.cfg.Interval)
	s.mu.Unlock()

	select {
	case s.reset <- struct{}{}:
	default:
	}
	st := s.Status()
	if s.bus != nil {
		msg := fmt.Sprintf("auto-mine every %s on %s", st.IntervalSeconds1(), st.Node)
		if !st.Enabled {
			msg = "auto-mine disabled"
		}
		s.bus.Publish("mine", st.Node, msg, map[string]any{
			"enabled": st.Enabled, "intervalSeconds": st.IntervalSeconds, "node": st.Node,
		})
	}
	return st
}

// IntervalSeconds1 renders the interval for a log line.
func (st Status) IntervalSeconds1() string {
	return (time.Duration(st.IntervalSeconds) * time.Second).String()
}

// Status is a snapshot, safe to call at any time.
func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{
		Enabled: s.cfg.Enabled, IntervalSeconds: int(s.cfg.Interval.Seconds()),
		Node: s.cfg.Node, Blocks: s.cfg.Blocks,
		BlocksMined: s.blocksMined, Runs: s.runs, LastError: s.lastErr,
		Now: time.Now().UTC().Format(time.RFC3339),
	}
	// The countdown is reported even while disabled, so re-enabling is predictable rather
	// than a surprise.
	if !s.nextAt.IsZero() {
		st.NextMineAt = s.nextAt.UTC().Format(time.RFC3339)
	}
	if !s.lastAt.IsZero() {
		st.LastMinedAt = s.lastAt.UTC().Format(time.RFC3339)
	}
	return st
}
