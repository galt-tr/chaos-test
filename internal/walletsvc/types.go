// Package walletsvc drives the walletd sidecar: wallet state, funding, transaction building,
// a sustained low-rate send, and reconciliation of what the wallet believes against what the
// network actually did.
//
// It deliberately contains no go-wallet-toolbox imports. The toolbox cannot be compiled into
// this module (chaos-test replaces gorm's sqlite driver for its alert datastore, and the
// replacement lacks the API the toolbox needs), so the wallet lives in its own process and
// this package speaks to it over HTTP.
package walletsvc

// Failure reasons. Callers — including the UI — branch on these, never on message text.
const (
	ReasonBadRequest     = "bad_request"
	ReasonUnknownNode    = "unknown_node"
	ReasonDisabled       = "wallet_disabled"
	ReasonUnavailable    = "wallet_unavailable"
	ReasonNotConnected   = "wallet_not_connected"
	ReasonInsufficient   = "insufficient_funds"
	ReasonNoCoinbase     = "no_spendable_coinbase"
	ReasonImmatureChain  = "immature_chain"
	ReasonFundingReject  = "funding_broadcast_rejected"
	ReasonProofTimeout   = "proof_timeout"
	ReasonInternalize    = "internalize_failed"
	ReasonArcade         = "arcade_unavailable"
	ReasonTxNotFound     = "tx_not_found"
	ReasonAlreadyRunning = "send_already_running"
	ReasonDraining       = "send_draining"
	ReasonLegacyInSend   = "legacy_not_supported_in_send"
	ReasonUpstream       = "upstream_error"
	ReasonTimeout        = "timeout"
)

// Transaction shapes, mirroring walletd.
const (
	ShapePayment  = "payment"
	ShapeOpReturn = "opreturn"
	ShapeFanout   = "fanout"
	ShapeCustom   = "custom"
)

// TargetArcade is the default broadcast route. Anything else names a node and is the legacy path.
const TargetArcade = "arcade"

// Deposit is the wallet's BRC-29 receive address.
type Deposit struct {
	Network             string `json:"network"`
	Address             string `json:"address"`
	LockingScriptHex    string `json:"lockingScriptHex"`
	DerivationPrefixB64 string `json:"derivationPrefixB64"`
	DerivationSuffixB64 string `json:"derivationSuffixB64"`
	SuggestedSatoshis   uint64 `json:"suggestedSatoshis"`
}

// Coin is one spendable output.
type Coin struct {
	Outpoint  string `json:"outpoint"`
	Satoshis  uint64 `json:"satoshis"`
	Spendable bool   `json:"spendable"`
}

// Health is the wallet's own view of the actions it created, bucketed by status.
//
// This replaces "createAction returned 200" as the headline number. CreateAction succeeds once
// an action is stored, which can happen before anything touches the network, so a count of
// successful calls says nothing. A count of actions the wallet later resolved does.
type Health struct {
	Labels     []string          `json:"labels"`
	Total      uint64            `json:"total"`
	Sampled    uint64            `json:"sampled"`
	Buckets    map[string]uint64 `json:"buckets"`
	Accepted   uint64            `json:"accepted"`
	Decided    uint64            `json:"decided"`
	AcceptRate float64           `json:"acceptRate"` // -1 when nothing is decided; never invent one
	SampledAt  string            `json:"sampledAt"`
}

// TxRow carries wallet truth and network truth side by side. They are never merged: the wallet
// knows whether it can spend the change, arcade knows whether the network took the transaction,
// and the interesting bugs live exactly where the two disagree.
type TxRow struct {
	TxID         string   `json:"txid"`
	Shape        string   `json:"shape,omitempty"`
	Target       string   `json:"target,omitempty"`
	Satoshis     int64    `json:"satoshis"`
	WalletStatus string   `json:"walletStatus,omitempty"`
	ArcadeStatus string   `json:"arcadeStatus,omitempty"` // "" = never asked, not "fine"
	BlockHeight  uint64   `json:"blockHeight,omitempty"`
	ExtraInfo    string   `json:"extraInfo,omitempty"`
	CompetingTxs []string `json:"competingTxs,omitempty"`
	Diverged     bool     `json:"diverged"`
	Terminal     bool     `json:"terminal"`
	CreatedAt    string   `json:"createdAt"`
	CheckedAt    string   `json:"arcadeCheckedAt,omitempty"`
}

// State is everything the Wallet page needs in one call.
type State struct {
	Available bool   `json:"available"`
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	Reason    string `json:"reason,omitempty"`

	Network     string `json:"network,omitempty"`
	IdentityKey string `json:"identityKey,omitempty"`
	Address     string `json:"address,omitempty"`
	Balance     uint64 `json:"balance"`
	// Coins is the gauge that explains almost every stall: a send is limited by spendable
	// outputs, not by balance.
	Coins     uint32     `json:"coins"`
	Health    Health     `json:"health"`
	Send      SendStatus `json:"send"`
	Recent    []TxRow    `json:"recent"`
	Coinlist  []Coin     `json:"coins_list,omitempty"`
	CheckedAt string     `json:"checkedAt,omitempty"`
}

// TxRequest builds one transaction.
type TxRequest struct {
	Shape       string `json:"shape"`
	Target      string `json:"target"` // "arcade" (default) or a node name (legacy)
	Satoshis    uint64 `json:"satoshis"`
	To          string `json:"to"`
	Outputs     int    `json:"outputs"`
	Data        string `json:"data"`
	DataHex     string `json:"dataHex"`
	Script      string `json:"script"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Delayed     bool   `json:"delayed"`
}

// TxResult reports what happened. Accepted means the broadcast target took it — which is still
// not the same as the network having mined it.
type TxResult struct {
	TxID         string `json:"txid"`
	Shape        string `json:"shape"`
	Target       string `json:"target"`
	Accepted     bool   `json:"accepted"`
	WalletStatus string `json:"walletStatus,omitempty"`
	Status       int    `json:"status,omitempty"`
	Body         string `json:"body,omitempty"`
	RawHex       string `json:"rawHex,omitempty"`
	Satoshis     uint64 `json:"satoshis"`
	NoSend       bool   `json:"noSend"`
}

// Step is one stage of a multi-stage operation, surfaced so a failed top-up shows where it broke.
type Step struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
	Elapsed string `json:"elapsed,omitempty"`
}

// TopUpRequest funds the wallet from a regtest coinbase.
type TopUpRequest struct {
	Node        string `json:"node"`
	Satoshis    uint64 `json:"satoshis"`
	Count       int    `json:"count"` // coins to end up with; >1 fans out after funding
	Mine        *bool  `json:"mine"`
	Fee         uint64 `json:"fee"`
	WaitSeconds int    `json:"waitSeconds"`
}

// TopUpResult reports the funding round trip.
type TopUpResult struct {
	TxID           string `json:"txid"`
	Address        string `json:"address"`
	Satoshis       uint64 `json:"satoshis"`
	OutputIndex    uint32 `json:"outputIndex"`
	CoinbaseTxID   string `json:"coinbaseTxid,omitempty"`
	CoinbaseHeight uint32 `json:"coinbaseHeight,omitempty"`
	Internalized   bool   `json:"internalized"`
	FanoutTxID     string `json:"fanoutTxid,omitempty"`
	Coins          uint32 `json:"coins"`
	Balance        uint64 `json:"balance"`
	Steps          []Step `json:"steps"`
}

// SendRequest configures the sustained send.
type SendRequest struct {
	TPS     float64 `json:"tps"`
	Workers int     `json:"workers"`
	Shape   string  `json:"shape"`
	// Target exists so an explicit non-arcade value can be REJECTED with a clear reason
	// rather than silently ignored; the sustained send is arcade-only by design.
	Target          string `json:"target,omitempty"`
	Satoshis        uint64 `json:"satoshis"`
	Outputs         int    `json:"outputs"`
	To              string `json:"to"`
	Data            string `json:"data"`
	Label           string `json:"label"`
	DurationSeconds int    `json:"durationSeconds"`
	AutoMineSeconds int    `json:"autoMineSeconds"`
	MineNode        string `json:"mineNode"`
	MineBlocks      int    `json:"mineBlocks"`
	StartedBy       string `json:"startedBy,omitempty"`
}

// SendStatus is the live state of the send. The lifecycle belongs to the orchestrator, not to
// any browser tab: a page that reloads resyncs from here.
type SendStatus struct {
	Running  bool `json:"running"`
	Draining bool `json:"draining"`
	InFlight int  `json:"inFlight"`

	TPS       float64  `json:"tps"`
	Workers   int      `json:"workers"`
	Shape     string   `json:"shape,omitempty"`
	Target    string   `json:"target,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	StartedBy string   `json:"startedBy,omitempty"`

	StartedAt  string  `json:"startedAt,omitempty"`
	StoppedAt  string  `json:"stoppedAt,omitempty"`
	ElapsedSec float64 `json:"elapsedSeconds"`
	StopReason string  `json:"stopReason,omitempty"`
	Now        string  `json:"now"`

	Attempted    uint64  `json:"attempted"`
	Succeeded    uint64  `json:"succeeded"` // the wallet took it — NOT network acceptance
	Failed       uint64  `json:"failed"`
	Backpressure uint64  `json:"backpressure"` // no spendable coin: idle, not failed
	Canceled     uint64  `json:"canceled"`     // in flight when Stop was pressed; not a failure
	MeasuredTPS  float64 `json:"measuredTps"`
	LastError    string  `json:"lastError,omitempty"`

	CoinsAtStart uint32 `json:"coinsAtStart"`
	CoinsNow     uint32 `json:"coinsRemaining"`
	WaitingFunds bool   `json:"waitingForFunds"`
	WaitingSince string `json:"waitingSince,omitempty"`
}
