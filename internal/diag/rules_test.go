package diag

import (
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/bsv-regtest/teranode"
)

// allOK builds a Diagnostics whose every source succeeded, as the healthy baseline each
// test then perturbs.
func allOK(node string, height uint32, tip string) *Diagnostics {
	d := &Diagnostics{
		Node: node, Container: "bsv-regtest-" + node, ContainerState: "running",
		Tip:     &TipInfo{Height: height, Hash: tip},
		FSM:     &teranode.FSMInfo{State: "RUNNING", StateValue: 1},
		Catchup: &teranode.CatchupStatus{},
		Peers:   []teranode.Peer{{ClientName: "other", Height: height, IsConnected: true}},
		Heights: &teranode.ServiceHeights{},
		Invalid: []teranode.InvalidBlock{},
		Health:  &teranode.Health{OverallStatus: 200, Parsed: true, Unhealthy: []string{}},
	}
	for _, n := range SourceOrder {
		d.Sources = append(d.Sources, SourceStatus{Name: n, OK: true})
	}
	return d
}

func (d *Diagnostics) fail(name, reason string) *Diagnostics {
	for i := range d.Sources {
		if d.Sources[i].Name == name {
			d.Sources[i].OK = false
			d.Sources[i].Reason = reason
			d.Sources[i].Error = reason
		}
	}
	d.Degraded = true
	return d
}

func fleet(max uint32, tip string, nodes map[string]FleetNodeBrief) FleetContext {
	return FleetContext{MaxHeight: max, MajorityTip: tip, MajorityTipHeight: max, Nodes: nodes}
}

func brief(h uint32, tip string, reachable bool, parts ...string) FleetNodeBrief {
	return FleetNodeBrief{Height: h, Tip: tip, Reachable: reachable, FSM: "RUNNING", Partitions: parts}
}

func TestInSync(t *testing.T) {
	d := allOK("teranode1", 116, "aaa")
	v := Evaluate(Input{Node: "teranode1", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode1": brief(116, "aaa", true)})})
	if v.Class != ClassInSync || v.Severity != "ok" {
		t.Fatalf("got %s/%s: %s", v.Class, v.Severity, v.Headline)
	}
}

// The single most important rule in the table: a failed probe must never be rounded up to
// a green badge inferred from a height that happens to match.
func TestMissingFSMYieldsUnknownNotInSync(t *testing.T) {
	d := allOK("teranode1", 116, "aaa").fail(SrcFSM, "timeout")
	d.FSM = nil
	v := Evaluate(Input{Node: "teranode1", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode1": brief(116, "aaa", true)})})
	if v.Class == ClassInSync {
		t.Fatal("reported in_sync while the FSM probe had failed")
	}
	if v.Class != ClassUnknown {
		t.Fatalf("want unknown, got %s: %s", v.Class, v.Headline)
	}
	if v.Confidence != "low" {
		t.Fatalf("want low confidence, got %s", v.Confidence)
	}
	if !containsStr(v.Missing, SrcFSM) {
		t.Fatalf("missing sources not recorded: %v", v.Missing)
	}
}

func TestContainerDownBeatsEverything(t *testing.T) {
	d := allOK("teranode2", 100, "bbb")
	d.ContainerState = "exited"
	v := Evaluate(Input{Node: "teranode2", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode2": brief(100, "bbb", true)})})
	if v.Class != ClassContainerDown {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
}

// A partition is the cause of the lag it produces, so it must outrank "behind".
func TestPartitionedBeatsBehind(t *testing.T) {
	d := allOK("teranode3", 100, "ccc")
	v := Evaluate(Input{Node: "teranode3", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode3": brief(100, "ccc", true, "p2p")})})
	if v.Class != ClassPartitioned {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
	if !strings.Contains(strings.Join(v.Details, " "), "16 block") {
		t.Fatalf("expected the lag reported as detail, got %v", v.Details)
	}
}

// Only the FSM endpoint reveals IDLE; /health returns 200 in every state.
func TestFSMIdle(t *testing.T) {
	d := allOK("teranode1", 116, "aaa")
	d.FSM = &teranode.FSMInfo{State: "IDLE", LegalEvents: []string{"RUN"}}
	v := Evaluate(Input{Node: "teranode1", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode1": brief(116, "aaa", true)})})
	if v.Class != ClassFSMIdle {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
	if len(v.Suggest) == 0 || !strings.Contains(v.Suggest[0], "RUN") {
		t.Fatalf("legal transitions not suggested: %v", v.Suggest)
	}
}

func TestCatchingUp(t *testing.T) {
	d := allOK("teranode2", 100, "bbb")
	d.FSM = &teranode.FSMInfo{State: "CATCHINGBLOCKS"}
	d.Catchup = &teranode.CatchupStatus{IsCatchingUp: true, PeerNode: "teranode1",
		TotalBlocks: 5, BlocksFetched: 5, BlocksValidated: 2, TargetBlockHeight: 116, DurationMs: 3000}
	v := Evaluate(Input{Node: "teranode2", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode2": brief(100, "bbb", true)})})
	if v.Class != ClassCatchingUp {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
	if !strings.Contains(v.Headline, "teranode1") || !strings.Contains(v.Headline, "2 of 5") {
		t.Fatalf("headline lacks the useful facts: %s", v.Headline)
	}
}

// A stall must not hide behind a reassuring progress bar.
func TestCatchupStalledBeatsCatchingUp(t *testing.T) {
	mk := func(validated int64) *Diagnostics {
		d := allOK("teranode2", 100, "bbb")
		d.FSM = &teranode.FSMInfo{State: "CATCHINGBLOCKS"}
		d.Catchup = &teranode.CatchupStatus{IsCatchingUp: true, PeerNode: "teranode1",
			TotalBlocks: 5, BlocksValidated: validated, DurationMs: 300_000}
		return d
	}
	v := Evaluate(Input{Node: "teranode2", Diag: mk(2), Prev: mk(2),
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode2": brief(100, "bbb", true)})})
	if v.Class != ClassCatchupStalled {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
	// Progress since the previous sample means it is merely slow, not wedged.
	v = Evaluate(Input{Node: "teranode2", Diag: mk(3), Prev: mk(2),
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode2": brief(100, "bbb", true)})})
	if v.Class != ClassCatchingUp {
		t.Fatalf("progress should not read as stalled, got %s", v.Class)
	}
}

// The failed attempt explains the lag, so it outranks merely restating the lag.
func TestCatchupFailedBeatsBehind(t *testing.T) {
	d := allOK("teranode2", 100, "bbb")
	d.Catchup = &teranode.CatchupStatus{PreviousAttempt: &teranode.PreviousAttempt{
		PeerNode: "teranode1", ErrorType: "validation_failure",
		ErrorMessage: "UTXO_CONSENSUS_FROZEN (78): utxo is frozen",
		AttemptTime:  time.Now().Add(-30 * time.Second).Unix(),
	}}
	v := Evaluate(Input{Node: "teranode2", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode2": brief(100, "bbb", true)})})
	if v.Class != ClassCatchupFailed {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
	if !strings.Contains(strings.Join(v.Details, " "), "UTXO_CONSENSUS_FROZEN") {
		t.Fatalf("root cause not surfaced: %v", v.Details)
	}
	// A stale failure is history, not the current explanation.
	d.Catchup.PreviousAttempt.AttemptTime = time.Now().Add(-2 * time.Hour).Unix()
	if v := Evaluate(Input{Node: "teranode2", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode2": brief(100, "bbb", true)})}); v.Class != ClassBehind {
		t.Fatalf("a two-hour-old failure should not explain the present, got %s", v.Class)
	}
}

// Equal height on a different chain is a fork, and calling it "behind" would mislead.
func TestMinorityTipBeatsBehind(t *testing.T) {
	d := allOK("teranode1", 116, "aaa")
	d.Invalid = []teranode.InvalidBlock{
		{Height: 117, Hash: "z1", Miner: "/teranode2/", MinerNode: "teranode2", RejectRootCause: "utxo is frozen"},
	}
	v := Evaluate(Input{Node: "teranode1", Diag: d,
		Fleet: fleet(117, "bbb", map[string]FleetNodeBrief{"teranode1": brief(116, "aaa", true)})})
	if v.Class != ClassMinorityTip {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
	if !strings.Contains(v.Headline, "teranode2") {
		t.Fatalf("headline should name who mined the rejected block: %s", v.Headline)
	}
}

// The live fleet is converged at 116 while teranode1 still holds rejections from an older
// fork. That history must NOT make a healthy node read as broken.
func TestOldRejectionHistoryDoesNotBreakInSync(t *testing.T) {
	d := allOK("teranode1", 116, "aaa")
	d.Invalid = []teranode.InvalidBlock{
		{Height: 106, Hash: "x", Miner: "/teranode2/", MinerNode: "teranode2"},
		{Height: 107, Hash: "y", Miner: "/teranode2/", MinerNode: "teranode2"},
	}
	v := Evaluate(Input{Node: "teranode1", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode1": brief(116, "aaa", true)})})
	if v.Class != ClassInSync {
		t.Fatalf("rejection history must not override a converged tip, got %s: %s", v.Class, v.Headline)
	}
}

func TestNoPeers(t *testing.T) {
	d := allOK("teranode3", 116, "aaa")
	d.Peers = []teranode.Peer{}
	v := Evaluate(Input{Node: "teranode3", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode3": brief(116, "aaa", true)})})
	if v.Class != ClassNoPeers {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
}

func TestUnreachable(t *testing.T) {
	d := allOK("teranode2", 0, "").fail(SrcTip, "unreachable")
	d.Tip = nil
	v := Evaluate(Input{Node: "teranode2", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode2": brief(0, "", false)})})
	if v.Class != ClassUnreachable {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
}

func TestUnhealthyDependency(t *testing.T) {
	d := allOK("teranode1", 116, "aaa")
	d.Health = &teranode.Health{OverallStatus: 503, Parsed: true,
		Unhealthy: []string{"UTXOStore"},
		Deps:      []teranode.HealthDep{{Resource: "UTXOStore", Status: 503, Error: "boom"}}}
	v := Evaluate(Input{Node: "teranode1", Diag: d,
		Fleet: fleet(116, "aaa", map[string]FleetNodeBrief{"teranode1": brief(116, "aaa", true)})})
	if v.Class != ClassUnhealthy {
		t.Fatalf("got %s: %s", v.Class, v.Headline)
	}
}
