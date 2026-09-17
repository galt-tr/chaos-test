// Package main is walletd: a thin HTTP front end over a go-wallet-toolbox BRC-100 wallet.
//
// It is a separate Go module and a separate process on purpose. The toolbox pulls in ~300
// modules and pins gorm's sqlite driver, while chaos-test replaces that driver with
// glebarez/sqlite (which has no Config/New) for its alert datastore — so importing the
// toolbox into the orchestrator's module cannot compile. Keeping it out here also means the
// toolbox can be bumped without touching chaos-test's go.sum.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/defs"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/storage"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
)

// ErrNotConnected is returned while the wallet has no storage behind it.
var ErrNotConnected = errors.New("wallet is not connected to storage")

// Wallet owns the toolbox wallet and its connection lifecycle.
//
// Nothing dials during construction: walletd must start and answer /healthz and /v1/deposit
// even when wallet-infra is down, because those are the two things an operator needs in
// order to see *why* it is down.
type Wallet struct {
	cfg Config
	log *slog.Logger

	deposit Deposit // derived at construction; served even while disconnected

	mu        sync.RWMutex
	w         *wallet.Wallet
	connected bool
	lastErr   string
	attempts  int
	since     time.Time
}

// Config is walletd's runtime configuration.
type Config struct {
	StorageURL string // go-wallet-toolbox storage server (wallet-infra)
	PrivateKey string // 64-hex; the harness's wallet identity (keys.json walletUserKey)
	Network    defs.BSVNetwork
	Originator string
}

// NewWallet validates the config. It does not connect.
func NewWallet(cfg Config, log *slog.Logger) (*Wallet, error) {
	if cfg.StorageURL == "" {
		return nil, errors.New("storage URL is required")
	}
	if len(cfg.PrivateKey) != 64 {
		return nil, fmt.Errorf("private key must be 64 hex characters, got %d", len(cfg.PrivateKey))
	}
	dep, err := DeriveDeposit(cfg.PrivateKey, cfg.Network)
	if err != nil {
		return nil, err
	}
	return &Wallet{cfg: cfg, log: log, deposit: dep}, nil
}

// Run keeps a connection to storage, retrying for as long as the context lives.
//
// The backoff is capped rather than bounded by a total deadline: wallet-infra may legitimately
// start minutes after the orchestrator (it waits on postgres), and a harness that gave up would
// need a restart to notice. After connecting it keeps a slow liveness probe so a dropped
// connection flips `connected` once rather than surfacing as a storm of per-request errors.
func (w *Wallet) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := w.connect(ctx); err != nil {
			w.setErr(err)
			w.log.Warn("wallet storage not reachable", "err", err, "backoff", backoff, "url", w.cfg.StorageURL)
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff *= 2; backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		backoff = time.Second
		w.log.Info("wallet connected", "url", w.cfg.StorageURL, "network", w.cfg.Network)
		w.probeUntilLost(ctx)
	}
}

// connect builds the wallet and proves it can reach storage.
//
// storage.NewClient does not dial, so a successful construction says nothing; Balance() is the
// cheapest call that forces the BRC-103 handshake and a real round trip.
func (w *Wallet) connect(ctx context.Context) error {
	w.mu.Lock()
	w.attempts++
	w.mu.Unlock()

	nw, err := wallet.NewWithStorageFactory(w.cfg.Network, w.cfg.PrivateKey,
		func(user sdk.Interface) (wdk.WalletStorageProvider, func(), error) {
			return storage.NewClient(w.cfg.StorageURL, user)
		})
	if err != nil {
		return fmt.Errorf("build wallet: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := nw.Balance(pctx); err != nil {
		return fmt.Errorf("reach storage: %w", err)
	}

	w.mu.Lock()
	w.w, w.connected, w.lastErr, w.since = nw, true, "", time.Now().UTC()
	w.mu.Unlock()
	return nil
}

// probeUntilLost returns once the connection stops answering, so Run can rebuild it.
func (w *Wallet) probeUntilLost(ctx context.Context) {
	for sleepCtx(ctx, 30*time.Second) {
		h, err := w.Handle()
		if err != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = h.Balance(pctx)
		cancel()
		if err != nil {
			w.setErr(fmt.Errorf("liveness probe: %w", err))
			w.log.Warn("wallet connection lost", "err", err)
			return
		}
	}
}

func (w *Wallet) setErr(err error) {
	w.mu.Lock()
	w.connected, w.lastErr = false, err.Error()
	w.mu.Unlock()
}

// Handle returns the connected wallet, or ErrNotConnected.
func (w *Wallet) Handle() (*wallet.Wallet, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.connected || w.w == nil {
		return nil, ErrNotConnected
	}
	return w.w, nil
}

// Health is the /healthz document: always served, even when disconnected.
type Health struct {
	OK          bool   `json:"ok"`
	Connected   bool   `json:"connected"`
	Attempts    int    `json:"attempts"`
	LastError   string `json:"lastError,omitempty"`
	StorageURL  string `json:"storageURL"`
	Network     string `json:"network"`
	ConnectedAt string `json:"connectedAt,omitempty"`
}

// Health reports the connection state.
func (w *Wallet) Health() Health {
	w.mu.RLock()
	defer w.mu.RUnlock()
	h := Health{OK: true, Connected: w.connected, Attempts: w.attempts, LastError: w.lastErr,
		StorageURL: w.cfg.StorageURL, Network: string(w.cfg.Network)}
	if w.connected {
		h.ConnectedAt = w.since.Format(time.RFC3339)
	}
	return h
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
