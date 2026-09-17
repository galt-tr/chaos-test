package arcade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrTxNotFound means arcade has no record of the transaction. Right after a submit this is
// normal, not a failure — it is also the only honest answer for a transaction that was
// broadcast straight to a node, bypassing arcade entirely.
var ErrTxNotFound = errors.New("arcade has no record of this transaction")

// Arcade's transaction lifecycle. The enum is OPEN: arcade may add values, so callers must
// carry unrecognised statuses through verbatim rather than erroring or normalising them away.
const (
	StatusUnknown              = "UNKNOWN"
	StatusReceived             = "RECEIVED"
	StatusSentToNetwork        = "SENT_TO_NETWORK"
	StatusAcceptedByNetwork    = "ACCEPTED_BY_NETWORK"
	StatusSeenOnNetwork        = "SEEN_ON_NETWORK"
	StatusSeenMultipleNodes    = "SEEN_MULTIPLE_NODES" // not SEEN_ON_MULTIPLE_NODES
	StatusDoubleSpendAttempted = "DOUBLE_SPEND_ATTEMPTED"
	StatusRejected             = "REJECTED"
	StatusPendingRetry         = "PENDING_RETRY"
	StatusStumpProcessing      = "STUMP_PROCESSING"
	StatusMined                = "MINED"
	StatusImmutable            = "IMMUTABLE"
	// StatusNotFound is synthetic: arcade answered 404. Kept distinct from an empty status,
	// which means "we never asked".
	StatusNotFound = "NOT_FOUND"
)

// IsSettled reports whether a status can no longer change in a way worth waiting for.
//
// REJECTED is deliberately excluded even though arcade calls it terminal: a peer that accepts
// after another refused supersedes it with SEEN_ON_NETWORK or SEEN_MULTIPLE_NODES, so a
// rejection is provisional until it has been left alone for a while.
func IsSettled(status string) bool {
	switch status {
	case StatusMined, StatusImmutable, StatusDoubleSpendAttempted:
		return true
	}
	return false
}

// IsGood reports network success. Only a block counts — every earlier status is a report of
// progress, not of acceptance.
func IsGood(status string) bool { return status == StatusMined || status == StatusImmutable }

// TxRecord is GET /tx/{txid}.
type TxRecord struct {
	TxID         string   `json:"txid"`
	Status       string   `json:"txStatus"`
	StatusCode   int      `json:"status,omitempty"`
	BlockHash    string   `json:"blockHash,omitempty"`
	BlockHeight  uint64   `json:"blockHeight,omitempty"`
	MerklePath   string   `json:"merklePath,omitempty"`
	ExtraInfo    string   `json:"extraInfo,omitempty"`
	CompetingTxs []string `json:"competingTxs,omitempty"`
	Timestamp    string   `json:"timestamp,omitempty"`
}

// GetTx asks arcade what the network did with a transaction.
func (c *Client) GetTx(ctx context.Context, txid string) (*TxRecord, error) {
	if c.BaseURL == "" {
		return nil, errors.New("arcade URL not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/tx/"+txid, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /tx/%s: %w", txid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrTxNotFound
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET /tx/%s: HTTP %d", txid, resp.StatusCode)
	}
	var rec TxRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, fmt.Errorf("decode tx record: %w", err)
	}
	if rec.TxID == "" {
		rec.TxID = txid
	}
	return &rec, nil
}
