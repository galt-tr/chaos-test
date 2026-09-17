package walletsvc

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Config wires the service.
type Config struct {
	URL             string // walletd base URL; "" disables the feature
	Bus             Publisher
	Logger          *slog.Logger
	DefaultMineNode string
	Arcade          ArcadeTx
}

// Service is the orchestrator's handle on the wallet.
//
// Nothing dials during construction: walletd and wallet-infra may start minutes after the
// orchestrator, and every route reports that honestly rather than failing to boot.
type Service struct {
	cfg Config
	cl  *Client
	ctl *Controller
	trk *Tracker
	log *slog.Logger

	mu      sync.RWMutex
	health  WalletdHealth
	checked time.Time
	lastErr string
}

// New builds the service. URL may be empty, in which case the feature reports itself disabled.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Service{cfg: cfg, cl: NewClient(cfg.URL), log: cfg.Logger}
	s.trk = NewTracker(cfg.Arcade, cfg.Bus, 2000)
	return s
}

// Bind supplies the transaction sender, which lives in the api package and is constructed
// after this service. That ordering is what keeps walletsvc free of an import cycle back
// into api.
func (s *Service) Bind(sender TxSender) {
	s.ctl = NewController(sender, s, s.cfg.Bus, s.log)
}

// Client exposes the walletd client for the api layer's own calls.
func (s *Service) Client() *Client { return s.cl }

// Tracker exposes the status tracker.
func (s *Service) Tracker() *Tracker { return s.trk }

// Send exposes the send controller. Nil until Bind is called.
func (s *Service) Send() *Controller { return s.ctl }

// DefaultMineNode is the node auto-mine uses when a request does not name one.
func (s *Service) DefaultMineNode() string { return s.cfg.DefaultMineNode }

// Enabled reports whether a walletd URL was configured at all.
func (s *Service) Enabled() bool { return s.cfg.URL != "" }

// Coins implements CoinCounter for the send controller's gauge.
func (s *Service) Coins(ctx context.Context) (uint32, error) {
	st, err := s.cl.State(ctx)
	if err != nil {
		return 0, err
	}
	return st.Coins, nil
}

// Run keeps the health cache warm and reconciles transaction status.
//
// The cadence follows the work: fast while a send is running or rows are unsettled, slow when
// idle. Polling rather than subscribing to arcade's event stream is deliberate — that stream
// has no status filter, a second consumer on the same token duplicates every event, and
// wallet-infra is already subscribed to it.
func (s *Service) Run(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	for ctx.Err() == nil {
		s.refreshHealth(ctx)
		s.reconcile(ctx)

		wait := 30 * time.Second
		if s.busy() {
			wait = 5 * time.Second
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (s *Service) busy() bool {
	if s.ctl != nil && s.ctl.Status().Running {
		return true
	}
	for _, r := range s.trk.Rows(50) {
		if !r.Terminal {
			return true
		}
	}
	return false
}

func (s *Service) refreshHealth(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	h, err := s.cl.Health(cctx)
	s.mu.Lock()
	s.checked = time.Now()
	if err != nil {
		s.health = WalletdHealth{}
		s.lastErr = err.Error()
	} else {
		s.health, s.lastErr = *h, h.LastError
	}
	s.mu.Unlock()
}

// reconcile pulls wallet-side truth for the labelled actions and then asks arcade about
// whatever is still unsettled.
func (s *Service) reconcile(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if res, err := s.cl.Actions(cctx, nil, 200, true); err == nil {
		for _, a := range res.Actions {
			s.trk.SetWalletStatus(a.TxID, a.Status)
		}
	}
	s.trk.Poll(cctx, 50)
}

// Health returns the cached walletd health plus how stale it is.
func (s *Service) Health() (WalletdHealth, string, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.health, s.lastErr, s.checked
}

// State assembles everything the Wallet page needs in one call.
//
// A failure here is reported inside the payload rather than as an HTTP error: "the wallet is
// not reachable" is a state the page has to render, not an exception.
func (s *Service) State(ctx context.Context) State {
	out := State{Available: s.Enabled()}
	if s.ctl != nil {
		out.Send = s.ctl.Status()
	}
	out.Recent = s.trk.Rows(25)
	out.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	if !out.Available {
		out.Reason = ReasonDisabled
		out.Error = "walletd is not configured (start it and pass -walletd)"
		return out
	}

	h, lastErr, _ := s.Health()
	if !h.OK && !h.Connected {
		out.Reason, out.Error = ReasonUnavailable, "walletd is not reachable"
		if lastErr != "" {
			out.Error = lastErr
		}
		return out
	}
	if !h.Connected {
		out.Reason = ReasonNotConnected
		out.Error = h.LastError
		if out.Error == "" {
			out.Error = "walletd is running but has no connection to wallet-infra"
		}
		out.Network = h.Network
		return out
	}

	st, err := s.cl.State(ctx)
	if err != nil {
		out.Reason, out.Error = ReasonOf(err), err.Error()
		if out.Reason == "" {
			out.Reason = ReasonUpstream
		}
		return out
	}
	out.Connected = true
	out.Network, out.IdentityKey, out.Address = st.Network, st.IdentityKey, st.Address
	out.Balance, out.Coins = st.Balance, st.Coins

	if res, err := s.cl.Actions(ctx, nil, 1000, false); err == nil {
		out.Health = res.Health
	} else {
		out.Health.AcceptRate = -1
	}
	if coins, _, err := s.cl.Outputs(ctx, 50); err == nil {
		out.Coinlist = coins
	}
	return out
}
