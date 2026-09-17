package walletsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/bsv-blockchain/chaos-test/internal/observe"
)

// Bounds. These are validated server-side because they size a goroutine pool and a channel
// buffer from user input.
const (
	minTPS, maxTPS         = 0.05, 20.0
	minWorkers, maxWorkers = 1, 8
)

// TxSender builds and broadcasts one transaction. Implemented by *api.Server.
type TxSender interface {
	SendOne(ctx context.Context, req TxRequest) (*TxResult, error)
}

// Publisher is the event bus. The return value is ignored here, but matching observe.Bus's
// signature avoids forcing an adapter at every call site.
type Publisher interface {
	Publish(kind, node, message string, data map[string]any) observe.Event
}

// CoinCounter reports spendable coins, for the gauge and for backpressure reporting.
type CoinCounter interface {
	Coins(ctx context.Context) (uint32, error)
}

// Controller runs the sustained send.
//
// It is a flow generator, not a load generator: the goal is that blocks have transactions in
// them, so the rate is low and the emphasis is on running for hours without lying about what
// happened.
type Controller struct {
	sender TxSender
	coins  CoinCounter
	bus    Publisher
	log    *slog.Logger

	mu       sync.Mutex
	running  bool
	draining bool
	cfg      SendRequest
	labels   []string
	stopProd context.CancelFunc
	done     chan struct{}
	gen      uint64

	startedAt, stoppedAt time.Time
	stopReason           string

	attempted, succeeded, failed, backpressure, canceled atomic.Uint64
	inFlight                                             atomic.Int64

	rateMu                 sync.Mutex
	lastSampleAt           time.Time
	lastSucceeded          uint64
	measuredTPS            float64
	coinsAtStart, coinsNow atomic.Uint32
	waitingSince           atomic.Int64
	lastErr                atomic.Pointer[string]
}

// NewController wires a controller.
//
// It does not mine. Keeping the chain moving is the job of internal/automine, which runs on a
// constant cadence for the whole orchestrator — a send is one reason blocks should be produced,
// not the authority on when.
func NewController(sender TxSender, coins CoinCounter, bus Publisher, log *slog.Logger) *Controller {
	return &Controller{sender: sender, coins: coins, bus: bus, log: log}
}

// Start begins a run. It is an error to start one while another is running or draining —
// the send is a single global resource and two concurrent runs would fight over the coin pool.
func (c *Controller) Start(parent context.Context, req SendRequest) (SendStatus, error) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return c.Status(), &Error{Reason: ReasonAlreadyRunning, Msg: "a send is already running"}
	}
	if c.draining {
		c.mu.Unlock()
		return c.Status(), &Error{Reason: ReasonDraining, Msg: "the previous send is still draining"}
	}
	// The legacy node path builds with NoSend, which parks the change instead of making it
	// spendable. A sustained run would immobilise the whole balance within minutes, so it is a
	// one-shot capability only.
	if req.Target != "" && req.Target != TargetArcade {
		c.mu.Unlock()
		return c.Status(), &Error{Reason: ReasonLegacyInSend,
			Msg: "the sustained send is arcade-only: broadcasting straight to a node parks the change, which would strand the balance within minutes. Use a one-shot transaction for the legacy path."}
	}

	cfg := normalise(req)
	c.cfg = cfg
	c.labels = []string{labelSend, fmt.Sprintf("run-%d", time.Now().Unix())}
	if cfg.Label != "" {
		c.labels = append(c.labels, cfg.Label)
	}
	c.running, c.draining = true, false
	c.startedAt, c.stoppedAt, c.stopReason = time.Now().UTC(), time.Time{}, ""
	c.attempted.Store(0)
	c.succeeded.Store(0)
	c.failed.Store(0)
	c.backpressure.Store(0)
	c.canceled.Store(0)
	c.waitingSince.Store(0)
	c.lastErr.Store(nil)
	c.lastSampleAt, c.lastSucceeded, c.measuredTPS = time.Now(), 0, 0

	// Production is bounded by the duration; the workers are NOT. Expiry must stop scheduling
	// new sends, never abort ones already in flight.
	prodCtx, cancel := context.WithCancel(parent)
	if cfg.DurationSeconds > 0 {
		prodCtx, cancel = context.WithTimeout(parent, time.Duration(cfg.DurationSeconds)*time.Second)
	}
	workCtx := context.WithoutCancel(parent)
	c.stopProd = cancel
	c.gen++
	gen := c.gen
	done := make(chan struct{})
	c.done = done
	c.mu.Unlock()

	if n, err := c.coinCount(workCtx); err == nil {
		c.coinsAtStart.Store(n)
		c.coinsNow.Store(n)
	}
	c.publish("send started", map[string]any{
		"tps": cfg.TPS, "workers": cfg.Workers, "shape": cfg.Shape, "target": TargetArcade,
		"labels": c.labels, "startedBy": cfg.StartedBy,
	})
	go c.run(prodCtx, workCtx, cfg, gen, done)
	return c.Status(), nil
}

func normalise(r SendRequest) SendRequest {
	if r.TPS <= 0 {
		r.TPS = 1
	}
	r.TPS = clampF(r.TPS, minTPS, maxTPS)
	if r.Workers <= 0 {
		r.Workers = 2
	}
	r.Workers = clampI(r.Workers, minWorkers, maxWorkers)
	if r.Shape == "" {
		r.Shape = ShapeOpReturn
	}
	r.Target = TargetArcade
	return r
}

func (c *Controller) run(prodCtx, workCtx context.Context, cfg SendRequest, gen uint64, done chan struct{}) {
	defer close(done)

	jobs := make(chan struct{}, cfg.Workers)
	var wg sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				c.sendOne(workCtx, cfg)
			}
		}()
	}

	stopSample := c.sampleRate(prodCtx)

	// Burst 1 is deliberate. A larger burst banks unused capacity while the wallet is starved
	// of coins and then releases all of it at once — precisely when the mempool is deepest.
	lim := rate.NewLimiter(rate.Limit(cfg.TPS), 1)
	for {
		if err := lim.Wait(prodCtx); err != nil {
			break
		}
		select {
		case <-prodCtx.Done():
		case jobs <- struct{}{}:
			continue
		}
		break
	}
	close(jobs)
	wg.Wait()
	stopSample()

	c.mu.Lock()
	if c.gen == gen { // a newer run may have started; never tear that one down
		c.running, c.draining = false, false
		c.stoppedAt = time.Now().UTC()
		if c.stopReason == "" {
			c.stopReason = "duration"
			if prodCtx.Err() == context.Canceled {
				c.stopReason = "stopped"
			}
		}
	}
	c.mu.Unlock()
	c.publish("send finished", map[string]any{
		"attempted": c.attempted.Load(), "succeeded": c.succeeded.Load(), "failed": c.failed.Load(),
		"backpressure": c.backpressure.Load(), "canceled": c.canceled.Load(),
	})
}

func (c *Controller) sendOne(ctx context.Context, cfg SendRequest) {
	c.attempted.Add(1)
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)

	req := TxRequest{
		Shape: cfg.Shape, Target: TargetArcade, Satoshis: cfg.Satoshis, Outputs: cfg.Outputs,
		To: cfg.To, Data: cfg.Data, Label: joinLabels(c.labels),
		Description: "chaos sustained send",
	}
	if req.Shape == ShapeOpReturn && req.Data == "" {
		req.Data = fmt.Sprintf("chaos-test %d", time.Now().UnixNano())
	}
	_, err := c.sender.SendOne(ctx, req)
	switch {
	case err == nil:
		c.succeeded.Add(1)
		c.waitingSince.Store(0)

	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// This is what pressing Stop looks like from inside a worker. Counting it as a failure
		// would make every clean shutdown look like an outage.
		c.canceled.Add(1)

	case IsInsufficientFunds(err):
		// The coin pool is empty. The send is idle, not broken: it resumes when change
		// confirms or someone tops up. Reported separately and never mixed into failures.
		n := c.backpressure.Add(1)
		if c.waitingSince.Load() == 0 {
			c.waitingSince.Store(time.Now().Unix())
		}
		if n == 1 || n%50 == 0 {
			c.publish("waiting for spendable coins", map[string]any{"backpressure": n})
		}
		sleepCtx(ctx, 2*time.Second)

	default:
		n := c.failed.Add(1)
		msg := err.Error()
		c.lastErr.Store(&msg)
		if n <= 3 || n%100 == 0 {
			c.log.Warn("send failed", "err", err, "count", n)
			c.publish("send error: "+msg, map[string]any{"failed": n})
		}
	}
	if n, err := c.coinCount(ctx); err == nil {
		c.coinsNow.Store(n)
	}
}

// sampleRate keeps the measured rate honest: successes divided by elapsed time actually
// observed, never by the nominal tick, which silently overstates throughput when ticks are
// dropped under load.
func (c *Controller) sampleRate(ctx context.Context) func() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		progress := time.NewTicker(60 * time.Second)
		defer progress.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-t.C:
				c.rateMu.Lock()
				now, done := time.Now(), c.succeeded.Load()
				if el := now.Sub(c.lastSampleAt).Seconds(); el > 0 {
					c.measuredTPS = float64(done-c.lastSucceeded) / el
				}
				c.lastSampleAt, c.lastSucceeded = now, done
				c.rateMu.Unlock()
			case <-progress.C:
				s := c.Status()
				c.publish(fmt.Sprintf("send %d attempted / %d ok / %d failed / %d waiting (%.2f tx/s)",
					s.Attempted, s.Succeeded, s.Failed, s.Backpressure, s.MeasuredTPS), nil)
			}
		}
	}()
	return func() { close(stop) }
}

// Stop halts scheduling and waits up to d for in-flight sends to finish.
func (c *Controller) Stop(d time.Duration) SendStatus {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return c.Status() // idempotent: stopping a stopped send is not an error
	}
	c.draining, c.stopReason = true, "stopped"
	cancel, done := c.stopProd, c.done
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(d):
		// Still draining. Returning rather than blocking keeps the HTTP handler responsive;
		// the UI shows "stopping…" and polls.
	}
	return c.Status()
}

// Status is a snapshot. Safe to call at any time.
func (c *Controller) Status() SendStatus {
	c.mu.Lock()
	s := SendStatus{
		Running: c.running, Draining: c.draining, TPS: c.cfg.TPS, Workers: c.cfg.Workers,
		Shape: c.cfg.Shape, Target: TargetArcade, Labels: c.labels, StartedBy: c.cfg.StartedBy,
		StopReason: c.stopReason,
	}
	if !c.startedAt.IsZero() {
		s.StartedAt = c.startedAt.Format(time.RFC3339)
		end := time.Now()
		if !c.stoppedAt.IsZero() {
			end = c.stoppedAt
			s.StoppedAt = c.stoppedAt.Format(time.RFC3339)
		}
		s.ElapsedSec = end.Sub(c.startedAt).Seconds()
	}
	c.mu.Unlock()

	s.Now = time.Now().UTC().Format(time.RFC3339)
	s.InFlight = int(c.inFlight.Load())
	s.Attempted, s.Succeeded = c.attempted.Load(), c.succeeded.Load()
	s.Failed, s.Backpressure, s.Canceled = c.failed.Load(), c.backpressure.Load(), c.canceled.Load()
	s.CoinsAtStart, s.CoinsNow = c.coinsAtStart.Load(), c.coinsNow.Load()
	c.rateMu.Lock()
	s.MeasuredTPS = c.measuredTPS
	c.rateMu.Unlock()
	if w := c.waitingSince.Load(); w > 0 {
		s.WaitingFunds = true
		s.WaitingSince = time.Unix(w, 0).UTC().Format(time.RFC3339)
	}
	if e := c.lastErr.Load(); e != nil {
		s.LastError = *e
	}
	return s
}

func (c *Controller) coinCount(ctx context.Context) (uint32, error) {
	if c.coins == nil {
		return 0, errors.New("no coin counter")
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.coins.Coins(cctx)
}

// publish emits a wallet event. The bus is a 5000-entry ring shared by every page, and a run
// lasts hours, so the send never emits per-transaction events — only lifecycle, periodic
// progress, and sampled failures.
func (c *Controller) publish(msg string, data map[string]any) {
	if c.bus != nil {
		c.bus.Publish("wallet", "", msg, data)
	}
}

func joinLabels(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampI(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

const labelSend = "chaos-wallet-send"
