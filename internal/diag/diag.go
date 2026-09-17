// Package diag explains why a teranode is in the state it is in.
//
// It fans out to the node's own diagnostic endpoints, joins the answers against the fleet
// snapshot, and reduces the result to a single verdict. The governing rule is that missing
// evidence is never read as good news: a probe that failed leaves its section nil and is
// recorded in Sources, and the "in sync" verdict requires that nothing failed at all.
package diag

import (
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/teranode"
)

// Source names, in the order they are reported.
const (
	SrcContainer = "container"
	SrcTip       = "tip"
	SrcFSM       = "fsm"
	SrcCatchup   = "catchup"
	SrcPeers     = "peers"
	SrcHeights   = "heights"
	SrcInvalid   = "invalid"
	SrcHealth    = "health"
)

// SourceOrder is the fixed order every response reports sources in.
var SourceOrder = []string{SrcContainer, SrcTip, SrcFSM, SrcCatchup, SrcPeers, SrcHeights, SrcInvalid, SrcHealth}

// Failure reasons.
const (
	ReasonTimeout       = "timeout"
	ReasonUnreachable   = "unreachable"
	ReasonHTTPStatus    = "http_status"
	ReasonDecode        = "decode"
	ReasonNotConfigured = "not_configured"
	ReasonNoRuntime     = "no_runtime"
	ReasonError         = "error"
)

// SourceStatus reports how one probe went. Every source carries one on every response, so
// a source that failed can never be mistaken for an empty-but-healthy result.
type SourceStatus struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	Reason     string `json:"reason,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Elapsed    string `json:"elapsed"`
}

// TipInfo is the node's own chain tip.
type TipInfo struct {
	Height uint32 `json:"height"`
	Hash   string `json:"hash"`
}

// Verdict classes.
const (
	ClassUnreachable    = "unreachable"
	ClassContainerDown  = "container_down"
	ClassPartitioned    = "partitioned"
	ClassFSMIdle        = "fsm_idle"
	ClassCatchupStalled = "catchup_stalled"
	ClassCatchingUp     = "catching_up"
	ClassCatchupFailed  = "catchup_failed"
	ClassNoPeers        = "no_peers"
	ClassMinorityTip    = "minority_tip"
	ClassBehind         = "behind"
	ClassUnhealthy      = "unhealthy"
	ClassInSync         = "in_sync"
	ClassUnknown        = "unknown"
)

// Verdict is the computed explanation. Evidence and Missing make a verdict derived from
// partial data auditable instead of merely authoritative.
type Verdict struct {
	Class      string   `json:"class"`
	Severity   string   `json:"severity"` // ok | info | warn | error | unknown
	Headline   string   `json:"headline"`
	Details    []string `json:"details,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`
	Missing    []string `json:"missing,omitempty"`
	Confidence string   `json:"confidence"` // high | low
	Suggest    []string `json:"suggest,omitempty"`
	// LogHint preselects the Logs page filters that show the supporting evidence.
	LogHint *LogHint `json:"logHint,omitempty"`
}

// LogHint is a ready-made Logs page query for this verdict.
type LogHint struct {
	Services string `json:"services,omitempty"`
	Level    string `json:"level,omitempty"`
	Grep     string `json:"grep,omitempty"`
}

// Diagnostics is one node's evidence plus the explanation derived from it.
//
// A nil section means UNKNOWN, never "nothing is wrong" — the matching entry in Sources
// says why it is missing, and the UI must render it as unknown rather than as healthy.
type Diagnostics struct {
	Node           string `json:"node"`
	Container      string `json:"container"`
	ContainerState string `json:"containerState,omitempty"`
	FetchedAt      string `json:"fetchedAt"`
	Elapsed        string `json:"elapsed"`

	Sources  []SourceStatus `json:"sources"`
	Degraded bool           `json:"degraded"`

	Tip     *TipInfo                 `json:"tip,omitempty"`
	FSM     *teranode.FSMInfo        `json:"fsm,omitempty"`
	Catchup *teranode.CatchupStatus  `json:"catchup,omitempty"`
	Peers   []teranode.Peer          `json:"peers,omitempty"`
	Heights *teranode.ServiceHeights `json:"heights,omitempty"`
	Invalid []teranode.InvalidBlock  `json:"invalidBlocks,omitempty"`
	Health  *teranode.Health         `json:"health,omitempty"`

	Verdict Verdict `json:"verdict"`
}

// FleetNodeBrief is one node as the fleet snapshot sees it.
type FleetNodeBrief struct {
	Name       string   `json:"name"`
	Height     uint32   `json:"height"`
	Tip        string   `json:"tip"`
	Reachable  bool     `json:"reachable"`
	FSM        string   `json:"fsm"`
	Partitions []string `json:"partitions,omitempty"`
}

// FleetContext is the cross-node view a single node's verdict is judged against.
type FleetContext struct {
	MaxHeight         uint32                    `json:"maxHeight"`
	MajorityTip       string                    `json:"majorityTip"`
	MajorityTipHeight uint32                    `json:"majorityTipHeight"`
	MajorityCount     int                       `json:"majorityCount"`
	Nodes             map[string]FleetNodeBrief `json:"nodes"`
	SnapshotAt        time.Time                 `json:"snapshotAt"`
}

// Report is the whole response.
type Report struct {
	Nodes     []Diagnostics `json:"nodes"`
	Fleet     FleetContext  `json:"fleet"`
	FetchedAt string        `json:"fetchedAt"`
	Cached    bool          `json:"cached"`
	Elapsed   string        `json:"elapsed"`
}
