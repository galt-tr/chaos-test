package arcade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrBlockNotFound means arcade has no block_processing row for the hash. Distinct from a row
// that exists and says "orphaned" — telling those apart is the whole point of the assertion.
var ErrBlockNotFound = errors.New("arcade has no processing status for this block")

// Block processing statuses. These are the only three arcade defines; the self-documenting API
// catalogue shows a "compound-bump-built" example that does not exist in the code.
const (
	BlockActive   = "active"
	BlockOrphaned = "orphaned"
	BlockParked   = "parked"
)

// BlockStatus is GET /api/v1/blocks/processing-status/{hash} — arcade's durable projection of
// the reorg stream, and what consumers point at.
type BlockStatus struct {
	BlockHash    string `json:"blockHash"`
	BlockHeight  uint64 `json:"blockHeight"`
	HeaderSeenAt string `json:"headerSeenAt,omitempty"`
	ProcessedAt  string `json:"processedAt,omitempty"`
	BUMPBuiltAt  string `json:"bumpBuiltAt,omitempty"`
	Status       string `json:"status"`
	OrphanedAt   string `json:"orphanedAt,omitempty"`
	// ReconciledAt tells "orphaned, still queued for reconcile" apart from "orphaned and off
	// the queue". Arcade builds before the #339 fix do not expose it at all, so an empty value
	// means either not-yet-reconciled or an older build.
	ReconciledAt      string `json:"reconciledAt,omitempty"`
	HasBlockProcessed bool   `json:"hasBlockProcessed"`
	HasCompoundBUMP   bool   `json:"hasCompoundBUMP"`
}

// BlockStatus reads one block's processing status.
func (c *Client) BlockStatus(ctx context.Context, hash string) (*BlockStatus, error) {
	if c.BaseURL == "" {
		return nil, errors.New("arcade URL not configured")
	}
	// BaseURL carries no /api/v1 prefix, unlike the /tx route.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/api/v1/blocks/processing-status/"+hash, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET processing-status/%s: %w", hash, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrBlockNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET processing-status/%s: HTTP %d", hash, resp.StatusCode)
	}
	var out BlockStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode processing status: %w", err)
	}
	if out.BlockHash == "" {
		out.BlockHash = hash
	}
	return &out, nil
}

// HeaderAtHeight asks arcade's own chaintracks which block it considers canonical at a height.
//
// Read alongside BlockStatus this is what exposes issue #339: the same instance reporting a
// block as the active-chain block here while its block_processing row still says orphaned.
func (c *Client) HeaderAtHeight(ctx context.Context, height uint32) (*Tip, error) {
	if c.ChaintracksURL == "" {
		return nil, errors.New("chaintracks URL not in inventory (re-run make gen)")
	}
	var h Tip
	if err := c.get(ctx, fmt.Sprintf("%s/chaintracks/v2/header/height/%d", c.ChaintracksURL, height), &h); err != nil {
		return nil, err
	}
	return &h, nil
}
