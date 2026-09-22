package diag

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/bsv-regtest/teranode"
	"github.com/bsv-blockchain/bsv-regtest/topology"
	"github.com/bsv-blockchain/chaos-test/internal/chaos"
	"github.com/bsv-blockchain/chaos-test/internal/observe"
)

// Budgets. Each probe gets its own deadline so one slow source cannot delay the rest, and
// the whole report is bounded by the same 4s the fleet poller already uses per node.
const (
	probeTimeout  = 2500 * time.Millisecond
	reportTimeout = 4 * time.Second
	invalidCount  = 20
)

// Deps is what the service needs from the rest of the orchestrator.
type Deps struct {
	Inv     *topology.Inventory
	Fleet   *observe.Fleet
	Runtime chaos.Runtime
}

// Service produces diagnostics reports, with a short cache so several viewers cost one
// probe.
type Service struct {
	d      Deps
	health map[string]*teranode.HealthClient

	mu    sync.Mutex
	last  map[string]*Diagnostics // previous sample, for progress comparisons
	cache map[string]*entry
}

type entry struct {
	at   time.Time
	rep  *Diagnostics
	done chan struct{} // closed when an in-flight probe finishes
}

// New builds the service.
func New(d Deps) *Service {
	s := &Service{d: d, health: map[string]*teranode.HealthClient{},
		last: map[string]*Diagnostics{}, cache: map[string]*entry{}}
	for _, n := range d.Inv.Nodes {
		if n.HealthURL != "" {
			s.health[n.Name] = teranode.NewHealthClient(n.HealthURL)
		}
	}
	return s
}

// probeNonTeranode fills the container state and the tip (via the kind-aware fleet lookup)
// for a node without a teranode asset API and returns an informational verdict.
func (s *Service) probeNonTeranode(ctx context.Context, n topology.Node, d *Diagnostics, start time.Time) *Diagnostics {
	c, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	t := time.Now()
	st, err := s.d.Runtime.State(c, n.Container)
	src := SourceStatus{Name: SrcContainer, OK: err == nil, Elapsed: time.Since(t).Round(time.Millisecond).String()}
	if err != nil {
		src.Error, src.Reason = err.Error(), classify(err)
	} else {
		d.ContainerState = st
	}
	d.Sources = append(d.Sources, src)
	t = time.Now()
	h, err := s.d.Fleet.Tip(c, n.Name)
	src = SourceStatus{Name: SrcTip, OK: err == nil, Elapsed: time.Since(t).Round(time.Millisecond).String()}
	if err != nil {
		src.Error, src.Reason = err.Error(), classify(err)
	} else {
		d.Tip = &TipInfo{Height: h.Height, Hash: h.Hash}
	}
	d.Sources = append(d.Sources, src)
	d.Degraded = err != nil
	d.FetchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	d.Elapsed = time.Since(start).Round(time.Millisecond).String()
	d.Verdict = Verdict{
		Class: ClassNotApplicable, Severity: "info", Confidence: "high",
		Headline: fmt.Sprintf("%s is an SV node: teranode diagnostics do not apply", n.Name),
		Details: []string{
			"SV Node has no asset API, FSM, catch-up state or service heights; only its container state and tip (over RPC) are probed here.",
			"See the Fleet page for its legacy peers, mempool and the alert sidecar's sequence and unprocessed count.",
		},
	}
	return d
}

// Report diagnoses one node, or every node when node is empty.
func (s *Service) Report(ctx context.Context, node string, maxAge time.Duration, withLogs bool) (Report, error) {
	start := time.Now()
	snap := s.d.Fleet.Snapshot()
	fc := buildFleetContext(snap)

	var targets []topology.Node
	if node == "" {
		targets = s.d.Inv.Nodes
	} else {
		n, err := s.d.Inv.Node(node)
		if err != nil {
			return Report{}, err
		}
		targets = []topology.Node{*n}
	}

	out := make([]Diagnostics, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(i int, n topology.Node) {
			defer wg.Done()
			out[i] = *s.node(ctx, n, fc, maxAge, withLogs)
		}(i, targets[i])
	}
	wg.Wait()

	return Report{
		Nodes: out, Fleet: fc,
		FetchedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Elapsed:   time.Since(start).Round(time.Millisecond).String(),
	}, nil
}

// node returns a cached sample when it is young enough, otherwise probes. Concurrent
// callers for the same node join the in-flight probe rather than starting a second.
func (s *Service) node(ctx context.Context, n topology.Node, fc FleetContext, maxAge time.Duration, withLogs bool) *Diagnostics {
	s.mu.Lock()
	if e, ok := s.cache[n.Name]; ok {
		if e.done != nil {
			s.mu.Unlock()
			<-e.done
			s.mu.Lock()
			e = s.cache[n.Name]
		}
		if e != nil && e.rep != nil && maxAge > 0 && time.Since(e.at) < maxAge {
			rep := e.rep
			s.mu.Unlock()
			return rep
		}
	}
	e := &entry{done: make(chan struct{})}
	s.cache[n.Name] = e
	prev := s.last[n.Name]
	s.mu.Unlock()

	d := s.probe(ctx, n, fc, prev, withLogs)

	s.mu.Lock()
	e.rep, e.at = d, time.Now()
	close(e.done)
	e.done = nil
	s.last[n.Name] = d
	s.mu.Unlock()
	return d
}

func (s *Service) probe(ctx context.Context, n topology.Node, fc FleetContext, prev *Diagnostics, withLogs bool) *Diagnostics {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()

	d := &Diagnostics{Node: n.Name, Container: n.Container}
	asset := s.d.Fleet.Asset(n.Name)
	if asset == nil {
		// An SV node: no asset API, FSM, catch-up or service heights to inspect. Report what
		// exists (container, tip over RPC) and say so instead of evaluating teranode rules.
		return s.probeNonTeranode(ctx, n, d, start)
	}

	var mu sync.Mutex
	status := map[string]SourceStatus{}
	record := func(name string, t time.Time, err error) {
		st := SourceStatus{Name: name, OK: err == nil, Elapsed: time.Since(t).Round(time.Millisecond).String()}
		if err != nil {
			st.Error = err.Error()
			st.Reason = classify(err)
			st.HTTPStatus = teranode.StatusOf(err)
		}
		mu.Lock()
		status[name] = st
		mu.Unlock()
	}
	run := func(name string, fn func(context.Context) error) func() {
		return func() {
			c, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			t := time.Now()
			record(name, t, fn(c))
		}
	}

	tasks := []func(){
		run(SrcContainer, func(c context.Context) error {
			st, err := s.d.Runtime.State(c, n.Container)
			if err != nil {
				return err
			}
			mu.Lock()
			d.ContainerState = st
			mu.Unlock()
			return nil
		}),
		run(SrcTip, func(c context.Context) error {
			h, err := asset.BestBlockHeader(c)
			if err != nil {
				return err
			}
			mu.Lock()
			d.Tip = &TipInfo{Height: h.Height, Hash: h.Hash}
			mu.Unlock()
			return nil
		}),
		run(SrcFSM, func(c context.Context) error {
			v, err := asset.FSM(c)
			if err != nil {
				return err
			}
			mu.Lock()
			d.FSM = v
			mu.Unlock()
			return nil
		}),
		run(SrcCatchup, func(c context.Context) error {
			v, err := asset.Catchup(c)
			if err != nil {
				return err
			}
			mu.Lock()
			d.Catchup = v
			mu.Unlock()
			return nil
		}),
		run(SrcPeers, func(c context.Context) error {
			v, err := asset.Peers(c)
			if err != nil {
				return err
			}
			mu.Lock()
			d.Peers = v
			mu.Unlock()
			return nil
		}),
		run(SrcHeights, func(c context.Context) error {
			v, err := asset.Heights(c)
			if err != nil {
				return err
			}
			mu.Lock()
			d.Heights = v
			mu.Unlock()
			return nil
		}),
		run(SrcInvalid, func(c context.Context) error {
			v, err := asset.InvalidBlocks(c, invalidCount)
			if err != nil {
				return err
			}
			mu.Lock()
			d.Invalid = v
			mu.Unlock()
			return nil
		}),
		run(SrcHealth, func(c context.Context) error {
			hc := s.health[n.Name]
			if hc == nil {
				return errNotConfigured
			}
			v, err := hc.Get(c)
			if err != nil {
				return err
			}
			mu.Lock()
			d.Health = v
			mu.Unlock()
			return nil
		}),
	}

	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func(fn func()) { defer wg.Done(); fn() }(t)
	}
	wg.Wait()

	// Sources are always reported in full, in a fixed order: a caller must never have to
	// infer that a source is missing from its absence.
	for _, name := range SourceOrder {
		st, ok := status[name]
		if !ok {
			st = SourceStatus{Name: name, Reason: ReasonError, Error: "not probed"}
		}
		d.Sources = append(d.Sources, st)
		if !st.OK {
			d.Degraded = true
		}
	}

	s.resolveNames(d)
	if withLogs {
		s.annotateRejections(ctx, n, d)
	}

	d.FetchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	d.Elapsed = time.Since(start).Round(time.Millisecond).String()
	d.Verdict = Evaluate(Input{Node: n.Name, Diag: d, Fleet: fc, Prev: prev})
	return d
}

var errNotConfigured = errors.New("no health URL configured for this node")

// classify turns a probe error into a stable reason code the UI can branch on.
func classify(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errNotConfigured):
		return ReasonNotConfigured
	case errors.Is(err, chaos.ErrNoRuntime):
		return ReasonNoRuntime
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case teranode.StatusOf(err) != 0:
		return ReasonHTTPStatus
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ReasonTimeout
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return ReasonUnreachable
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"):
		return ReasonUnreachable
	case strings.Contains(msg, "cannot unmarshal"), strings.Contains(msg, "invalid character"):
		return ReasonDecode
	}
	return ReasonError
}

// resolveNames maps teranode's own identifiers onto inventory node names, so the UI can
// say "teranode2" instead of a peer id or a "/teranode2/" miner tag.
func (s *Service) resolveNames(d *Diagnostics) {
	byPeerID := map[string]string{}
	for _, n := range s.d.Inv.Nodes {
		byPeerID[n.PeerID] = n.Name
	}
	name := func(peerID, clientName string) string {
		if n, ok := byPeerID[peerID]; ok {
			return n
		}
		for _, n := range s.d.Inv.Nodes {
			if n.Name == clientName {
				return n.Name
			}
		}
		return ""
	}
	for i := range d.Peers {
		d.Peers[i].Node = name(d.Peers[i].ID, d.Peers[i].ClientName)
	}
	if d.Catchup != nil {
		d.Catchup.PeerNode = name(d.Catchup.PeerID, "")
		if pa := d.Catchup.PreviousAttempt; pa != nil {
			pa.PeerNode = name(pa.PeerID, "")
		}
	}
	for i := range d.Invalid {
		tag := strings.Trim(d.Invalid[i].Miner, "/")
		for _, n := range s.d.Inv.Nodes {
			if n.Name == tag {
				d.Invalid[i].MinerNode = n.Name
			}
		}
	}
}

func buildFleetContext(snap observe.Snapshot) FleetContext {
	fc := FleetContext{Nodes: map[string]FleetNodeBrief{}, SnapshotAt: snap.UpdatedAt}
	tips := map[string]int{}
	for _, n := range snap.Nodes {
		fc.Nodes[n.Name] = FleetNodeBrief{
			Name: n.Name, Height: n.Height, Tip: n.Tip,
			Reachable: n.Reachable, FSM: n.FSM, Partitions: n.Partitions,
		}
		if !n.Reachable {
			continue
		}
		if n.Height > fc.MaxHeight {
			fc.MaxHeight = n.Height
		}
		if n.Tip != "" {
			tips[n.Tip]++
		}
	}
	// The majority tip is the one most reachable nodes hold at the highest height; ties
	// break on the hash so the verdict is stable across polls.
	for _, n := range snap.Nodes {
		if !n.Reachable || n.Tip == "" || n.Height != fc.MaxHeight {
			continue
		}
		c := tips[n.Tip]
		if c > fc.MajorityCount || (c == fc.MajorityCount && n.Tip < fc.MajorityTip) {
			fc.MajorityTip, fc.MajorityCount, fc.MajorityTipHeight = n.Tip, c, n.Height
		}
	}
	return fc
}
