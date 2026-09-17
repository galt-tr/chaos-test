package teranode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// HTTPStatusError is returned when teranode answers with a non-2xx status.
//
// It exists because several diagnostic endpoints return a plausible-looking body alongside
// an error status — /catchup/status answers 503 and 500 with `{"is_catching_up": false}` —
// so a caller that decodes without checking the status cannot tell "the service is down"
// from "the node is healthy and idle". Callers use errors.As to keep them apart.
type HTTPStatusError struct {
	Path   string
	Status int
	Body   string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d: %s", e.Path, e.Status, e.Body)
}

// StatusOf returns the HTTP status carried by err, or 0 if it is not a status error.
func StatusOf(err error) int {
	var se *HTTPStatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// getChecked is getJSON with a typed status error.
func (c *AssetClient) getChecked(ctx context.Context, path string, v any) error {
	b, status, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &HTTPStatusError{Path: path, Status: status, Body: truncate(b)}
	}
	return json.Unmarshal(b, v)
}

// FSMInfo is the node's current state plus the transitions legal from it.
type FSMInfo struct {
	State       string   `json:"state"`
	StateValue  int      `json:"stateValue"`
	LegalEvents []string `json:"legalEvents,omitempty"`
}

// FSM reports the blockchain FSM state. Only three states exist — IDLE, RUNNING and
// CATCHINGBLOCKS — and only this endpoint reveals IDLE: the node's /health document
// returns 200 in every one of them.
func (c *AssetClient) FSM(ctx context.Context) (*FSMInfo, error) {
	var st struct {
		State string `json:"state"`
		Value int    `json:"state_value"`
	}
	if err := c.getChecked(ctx, "/fsm/state", &st); err != nil {
		return nil, err
	}
	out := &FSMInfo{State: st.State, StateValue: st.Value}
	// Best effort: the legal-transition list is useful for labelling actions but its
	// absence must not fail the probe.
	var ev struct {
		Events []string `json:"events"`
	}
	if err := c.getChecked(ctx, "/fsm/events", &ev); err == nil {
		out.LegalEvents = ev.Events
	}
	return out, nil
}

// PreviousAttempt records why the last catchup failed. It is the single most direct answer
// to "why is this node not in sync" the node offers, and is absent until one has failed.
type PreviousAttempt struct {
	PeerID            string `json:"peer_id"`
	PeerURL           string `json:"peer_url"`
	TargetBlockHash   string `json:"target_block_hash"`
	TargetBlockHeight uint32 `json:"target_block_height"`
	ErrorMessage      string `json:"error_message"`
	ErrorType         string `json:"error_type"` // validation_failure | network_error | secret_mining | …
	AttemptTime       int64  `json:"attempt_time"`
	DurationMs        int64  `json:"duration_ms"`
	BlocksValidated   int64  `json:"blocks_validated"`
	PeerNode          string `json:"peerNode,omitempty"` // peer_id resolved to an inventory node
}

// CatchupStatus is GET /catchup/status.
type CatchupStatus struct {
	IsCatchingUp        bool   `json:"is_catching_up"`
	PeerID              string `json:"peer_id"`
	PeerURL             string `json:"peer_url"`
	TargetBlockHash     string `json:"target_block_hash"`
	TargetBlockHeight   uint32 `json:"target_block_height"`
	CurrentHeight       uint32 `json:"current_height"`
	TotalBlocks         int64  `json:"total_blocks"`
	BlocksFetched       int64  `json:"blocks_fetched"`
	BlocksValidated     int64  `json:"blocks_validated"`
	ForkDepth           uint32 `json:"fork_depth"`
	CommonAncestorHash  string `json:"common_ancestor_hash"`
	CommonAncestorHeigh uint32 `json:"common_ancestor_height"`
	StartTime           int64  `json:"start_time"`
	DurationMs          int64  `json:"duration_ms"`

	PreviousAttempt *PreviousAttempt `json:"previous_attempt,omitempty"`
	PeerNode        string           `json:"peerNode,omitempty"` // peer_id resolved to a node name
}

// Catchup reports sync progress. See HTTPStatusError: a non-200 here still carries
// `is_catching_up: false`, so the status must be checked before the body is believed.
func (c *AssetClient) Catchup(ctx context.Context) (*CatchupStatus, error) {
	var out CatchupStatus
	if err := c.getChecked(ctx, "/catchup/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Peer is one entry of GET /peers.
type Peer struct {
	ID         string `json:"id"`
	ClientName string `json:"client_name"`
	Transport  string `json:"transport"`
	Height     uint32 `json:"height"`
	BlockHash  string `json:"block_hash"`
	DataHubURL string `json:"data_hub_url"`

	IsConnected bool  `json:"is_connected"`
	IsBanned    bool  `json:"is_banned"`
	BanScore    int   `json:"ban_score"`
	ConnectedAt int64 `json:"connected_at"`

	CatchupAttempts      int64   `json:"catchup_attempts"`
	CatchupSuccesses     int64   `json:"catchup_successes"`
	CatchupFailures      int64   `json:"catchup_failures"`
	CatchupReputation    float64 `json:"catchup_reputation_score"`
	CatchupAvgResponseMs int64   `json:"catchup_avg_response_ms"`
	LastCatchupError     string  `json:"last_catchup_error"`
	LastCatchupErrorTime int64   `json:"last_catchup_error_time"`

	Node   string `json:"node,omitempty"`   // resolved to an inventory node name
	Behind int    `json:"behind,omitempty"` // fleet max height minus this peer's height
}

// Peers lists the node's P2P peers with their gossiped heights and tips. This is the
// richest peer source: getpeerinfo over RPC is a cached subset.
func (c *AssetClient) Peers(ctx context.Context) ([]Peer, error) {
	var out struct {
		Peers []Peer `json:"peers"`
		Count int    `json:"count"`
	}
	if err := c.getChecked(ctx, "/peers", &out); err != nil {
		return nil, err
	}
	if out.Peers == nil {
		out.Peers = []Peer{}
	}
	return out.Peers, nil
}

// InvalidBlock is one block this node rejected. Note the endpoint carries no reason; the
// reason lives in the container log and in the Kafka verdict topics.
type InvalidBlock struct {
	Height            uint32 `json:"height"`
	Hash              string `json:"hash"`
	PreviousBlockHash string `json:"previousblockhash"`
	Miner             string `json:"miner"`
	Timestamp         string `json:"timestamp"`
	TransactionCount  uint64 `json:"transactionCount"`
	Size              uint64 `json:"size"`
	CoinbaseValue     uint64 `json:"coinbaseValue"`

	MinerNode       string `json:"minerNode,omitempty"` // miner tag resolved to a node name
	RejectReason    string `json:"rejectReason,omitempty"`
	RejectRootCause string `json:"rejectRootCause,omitempty"`
	RejectCode      string `json:"rejectCode,omitempty"`
}

// InvalidBlocks lists blocks this node has rejected, newest first.
//
// This is durable history, not current state: a node can be fully in sync and still hold
// rejections from an earlier fork. Callers must not read a non-empty result as "broken".
func (c *AssetClient) InvalidBlocks(ctx context.Context, count int) ([]InvalidBlock, error) {
	if count <= 0 {
		count = 10
	}
	var out struct {
		Blocks []InvalidBlock `json:"blocks"`
		Count  int            `json:"count"`
	}
	// The parameter is `count`, not `limit`.
	if err := c.getChecked(ctx, "/blocks/invalid?count="+strconv.Itoa(count), &out); err != nil {
		return nil, err
	}
	if out.Blocks == nil {
		out.Blocks = []InvalidBlock{}
	}
	return out.Blocks, nil
}

// ServiceHeights is GET /service/heights; both fields are null when unavailable, which is
// why they are pointers — zero would read as "genesis".
type ServiceHeights struct {
	BlockAssembly  *uint32 `json:"block_assembly_height"`
	BlockPersister *uint32 `json:"block_persister_height"`
}

// Heights reports per-service heights, revealing block assembly lagging the chain tip.
func (c *AssetClient) Heights(ctx context.Context) (*ServiceHeights, error) {
	var out ServiceHeights
	if err := c.getChecked(ctx, "/service/heights", &out); err != nil {
		return nil, err
	}
	return &out, nil
}
