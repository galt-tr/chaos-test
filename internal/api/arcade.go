package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/arcade"
)

// ArcadeBlockStatus reads arcade's block_processing row for a block.
//
// This is arcade's durable projection of the reorg stream — the thing consumers point at — and
// is deliberately read separately from what arcade's chaintracks believes, because the two
// disagreeing is a real and reportable defect rather than an implementation detail.
func (s *Server) ArcadeBlockStatus(ctx context.Context, hash string) (*arcade.BlockStatus, error) {
	if s.d.ArcadeClient == nil {
		return nil, errors.New("arcade is not configured in the inventory")
	}
	if hash == "" {
		return nil, errors.New("a block hash is required")
	}
	return s.d.ArcadeClient.BlockStatus(ctx, hash)
}

// ArcadeCanonicalHashAt returns the block arcade's own chaintracks considers canonical at a
// height.
func (s *Server) ArcadeCanonicalHashAt(ctx context.Context, height uint32) (string, error) {
	if s.d.ArcadeClient == nil {
		return "", errors.New("arcade is not configured in the inventory")
	}
	h, err := s.d.ArcadeClient.HeaderAtHeight(ctx, height)
	if err != nil {
		return "", err
	}
	if h.Hash == "" {
		return "", fmt.Errorf("arcade returned no header at height %d", height)
	}
	return h.Hash, nil
}

// ArcadeTx reads arcade's GET /tx/{txid}. arcade.ErrTxNotFound passes through unchanged so a
// polling caller can tell "not yet" from a broken arcade.
func (s *Server) ArcadeTx(ctx context.Context, txid string) (*arcade.TxRecord, error) {
	if s.d.ArcadeClient == nil {
		return nil, errors.New("arcade is not configured in the inventory")
	}
	if txid == "" {
		return nil, errors.New("a txid is required")
	}
	return s.d.ArcadeClient.GetTx(ctx, txid)
}

// ArcadePolicy reads what arcade advertises on GET /policy.
func (s *Server) ArcadePolicy(ctx context.Context) (*arcade.Policy, error) {
	if s.d.ArcadeClient == nil {
		return nil, errors.New("arcade is not configured in the inventory")
	}
	return s.d.ArcadeClient.GetPolicy(ctx)
}

// sseLifetime bounds a scenario's /events subscription: long enough for any run, short enough
// that an abandoned listener does not hold a connection on arcade for the life of the process.
const sseLifetime = 20 * time.Minute

// ArcadeSSEListen opens a token-scoped subscription to arcade's /events stream, replacing any
// earlier listener for the same token, and returns once it is registered.
func (s *Server) ArcadeSSEListen(token string) (*arcade.SSEListener, error) {
	if s.d.ArcadeEvents == "" {
		return nil, errors.New("arcade events URL is not configured in the inventory")
	}
	if token == "" {
		return nil, errors.New("a token is required")
	}
	base := s.d.BaseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, sseLifetime)
	l, err := arcade.ListenSSE(ctx, s.d.ArcadeEvents, token)
	if err != nil {
		cancel()
		return nil, err
	}
	go func() { <-ctx.Done(); cancel() }()
	s.sseListenMu.Lock()
	defer s.sseListenMu.Unlock()
	if old, ok := s.sseListeners[token]; ok {
		old.Close()
	}
	if s.sseListeners == nil {
		s.sseListeners = map[string]*arcade.SSEListener{}
	}
	s.sseListeners[token] = l
	return l, nil
}

// ArcadeSSE returns the listener opened for token, if any.
func (s *Server) ArcadeSSE(token string) (*arcade.SSEListener, bool) {
	s.sseListenMu.Lock()
	defer s.sseListenMu.Unlock()
	l, ok := s.sseListeners[token]
	return l, ok
}
