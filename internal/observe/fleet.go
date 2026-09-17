package observe

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/alerts"
	"github.com/bsv-blockchain/chaos-test/internal/arcade"
	"github.com/bsv-blockchain/chaos-test/internal/svnode"
	"github.com/bsv-blockchain/chaos-test/internal/teranode"
	"github.com/bsv-blockchain/chaos-test/internal/topology"
)

// NodeState is the harness's current view of one teranode.
type NodeState struct {
	Kind           string   `json:"kind"` // teranode | svnode
	Name           string   `json:"name"`
	Index          int      `json:"index"`
	Reachable      bool     `json:"reachable"`
	Height         uint32   `json:"height"`
	Tip            string   `json:"tip"`
	FSM            string   `json:"fsm"`             // teranode only
	Peers          int      `json:"peers,omitempty"` // svnode: getpeerinfo count
	MempoolCount   int      `json:"mempoolCount"`
	Mempool        []string `json:"mempool,omitempty"`
	AlertSeq       int64    `json:"alertSeq"` // -1 = unknown / unreachable
	AlertReachable bool     `json:"alertReachable"`
	// AlertSource is "node" (teranode's embedded alert service) or "sidecar" (a go-alert-system
	// process applying alerts to an SV node over RPC).
	AlertSource      string `json:"alertSource,omitempty"`
	AlertUnprocessed int    `json:"alertUnprocessed"` // sidecar: alerts whose RPC apply failed; -1 unknown
	SidecarURL       string `json:"sidecarURL,omitempty"`
	SidecarContainer string `json:"sidecarContainer,omitempty"`
	Version          string `json:"version,omitempty"`
	Container        string `json:"container"`
	// HostURL is the node's own asset dashboard as a browser on the host can reach it
	// (teranodes only). Static config, so it stays set even while the node is unreachable.
	HostURL    string    `json:"hostURL,omitempty"`
	Partitions []string  `json:"partitions,omitempty"` // planes currently disconnected: p2p, alert
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// HubState is the go-alert-system node's view.
type HubState struct {
	Reachable   bool   `json:"reachable"`
	Sequence    uint32 `json:"sequence"`
	ActivePeers int    `json:"activePeers"`
	Unprocessed int    `json:"unprocessed"`
	Error       string `json:"error,omitempty"`
}

// ServiceState is a non-node service health.
type ServiceState struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	HostURL string `json:"hostURL,omitempty"` // what a browser on the host can open
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

// ArcadeDatahub is one teranode arcade broadcasts to, as arcade reports it.
type ArcadeDatahub struct {
	URL     string `json:"url"`
	Node    string `json:"node,omitempty"` // inventory node name, when the URL's host is a known node IP
	Source  string `json:"source,omitempty"`
	Healthy bool   `json:"healthy"`
}

// ArcadeState is the harness's view of arcade: its health document and the chain tip of
// its embedded chaintracks, which every wallet BEEF verification depends on.
type ArcadeState struct {
	Configured         bool            `json:"configured"`
	Reachable          bool            `json:"reachable"` // GET /health answered
	Healthy            bool            `json:"healthy"`   // .healthy in that document
	Version            string          `json:"version,omitempty"`
	Status             string          `json:"status,omitempty"`
	BlockHeight        uint32          `json:"blockHeight"` // arcade's own notion, from /health
	Height             uint32          `json:"height"`      // chaintracks tip (kept on error)
	Tip                string          `json:"tip"`
	Datahubs           []ArcadeDatahub `json:"datahubs,omitempty"`
	URL                string          `json:"url"` // what the poller uses
	ChaintracksURL     string          `json:"chaintracksURL,omitempty"`
	HostURL            string          `json:"hostURL,omitempty"` // browser links
	HostHealthURL      string          `json:"hostHealthURL,omitempty"`
	HostEventsURL      string          `json:"hostEventsURL,omitempty"`
	HostChaintracksURL string          `json:"hostChaintracksURL,omitempty"`
	Error              string          `json:"error,omitempty"`
	TipError           string          `json:"tipError,omitempty"`
	UpdatedAt          time.Time       `json:"updatedAt"`
}

// OutpointView is one output's state on one node.
type OutpointView struct {
	Status   string `json:"status"` // OK, FROZEN, SPENT, NOT_FOUND, ERROR
	SpentBy  string `json:"spentBy,omitempty"`
	UTXOHash string `json:"utxoHash,omitempty"`
	Satoshis uint64 `json:"satoshis,omitempty"`
	Error    string `json:"error,omitempty"`
}

// OutpointState is a watched output across the fleet.
type OutpointState struct {
	TxID    string                  `json:"txid"`
	Vout    uint32                  `json:"vout"`
	Label   string                  `json:"label,omitempty"`
	PerNode map[string]OutpointView `json:"perNode"`
}

// Snapshot is the whole fleet view.
type Snapshot struct {
	Network   string          `json:"network"`
	Nodes     []NodeState     `json:"nodes"`
	Hub       HubState        `json:"hub"`
	Arcade    ArcadeState     `json:"arcade"`
	Services  []ServiceState  `json:"services"`
	Watched   []OutpointState `json:"watched"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// Fleet polls the stack and maintains the Snapshot, publishing change events on the Bus.
type Fleet struct {
	inv     *topology.Inventory
	bus     *Bus
	lg      *slog.Logger
	alert   *alerts.Host
	rpc     map[string]*teranode.RPCClient
	asset   map[string]*teranode.AssetClient // teranodes only
	sv      map[string]*svnode.Client        // SV nodes only
	sidecar map[string]*alerts.HubClient     // SV nodes' go-alert-system sidecars
	hub     *alerts.HubClient
	arcade  *arcade.Client

	mu       sync.RWMutex
	snap     Snapshot
	watched  map[string]*OutpointState // key txid:vout
	partMu   sync.Mutex
	partSet  map[string]map[string]bool // node -> plane -> partitioned
	interval time.Duration
}

// NewFleet builds the observer. alertHost may be nil (alert sequence probing disabled).
func NewFleet(inv *topology.Inventory, bus *Bus, alertHost *alerts.Host, lg *slog.Logger) *Fleet {
	f := &Fleet{inv: inv, bus: bus, lg: lg, alert: alertHost, rpc: map[string]*teranode.RPCClient{},
		asset: map[string]*teranode.AssetClient{}, sv: map[string]*svnode.Client{}, sidecar: map[string]*alerts.HubClient{},
		watched: map[string]*OutpointState{}, partSet: map[string]map[string]bool{}, interval: time.Second}
	for _, n := range inv.Nodes {
		f.rpc[n.Name] = teranode.NewRPCClient(n.RPCURL, inv.RPCUser, inv.RPCPass)
		st := NodeState{Kind: "teranode", Name: n.Name, Index: n.Index, Container: n.Container, AlertSeq: -1, AlertUnprocessed: -1, AlertSource: "node"}
		if n.IsSV() {
			f.sv[n.Name] = svnode.New(n.RPCURL, inv.RPCUser, inv.RPCPass)
			st.Kind, st.AlertSource = "svnode", "sidecar"
			if n.Sidecar != nil {
				f.sidecar[n.Name] = alerts.NewHubClient(n.Sidecar.APIURL)
				st.SidecarURL, st.SidecarContainer = n.Sidecar.HostAPI, n.Sidecar.Container
			}
		} else {
			f.asset[n.Name] = teranode.NewAssetClient(n.AssetURL)
			// The dashboard is served at the asset origin, and HostAssetURL points at /api/v1
			// beneath it. HostAssetURL rather than AssetURL: UseHostURLs rewrites AssetURL to the
			// host URL in -host-mode but leaves HostAssetURL alone, so only HostAssetURL is
			// browser-correct in both modes — AssetURL is a ctlnet IP when we run in-container.
			st.HostURL = strings.TrimSuffix(n.HostAssetURL, "/api/v1")
		}
		f.snap.Nodes = append(f.snap.Nodes, st)
	}
	if inv.Hub != nil {
		f.hub = alerts.NewHubClient(inv.Hub.APIURL)
	}
	if a, ok := inv.Services["arcade"]; ok && a.URL != "" {
		// In -host-mode UseHostURLs has already made URL the host URL; HostURL is unchanged,
		// so the browser-facing fields below are right in both modes.
		f.arcade = arcade.New(a.URL, a.URLs["chaintracks"].URL)
		f.snap.Arcade = ArcadeState{Configured: true, URL: a.URL, ChaintracksURL: a.URLs["chaintracks"].URL, HostURL: a.HostURL,
			HostHealthURL: a.URLs["health"].HostURL, HostEventsURL: a.URLs["events"].HostURL, HostChaintracksURL: a.URLs["chaintracks"].HostURL}
	}
	f.snap.Network = inv.Network
	return f
}

// RPC returns the RPC client for a node.
func (f *Fleet) RPC(node string) *teranode.RPCClient { return f.rpc[node] }

// Asset returns the asset client for a teranode, nil for SV nodes.
func (f *Fleet) Asset(node string) *teranode.AssetClient { return f.asset[node] }

// SV returns the SV Node client for an SV node, nil for teranodes.
func (f *Fleet) SV(node string) *svnode.Client { return f.sv[node] }

// IsSV reports whether the node is an SV node.
func (f *Fleet) IsSV(node string) bool { return f.sv[node] != nil }

// Tip returns a node's best header live: asset API for teranodes, RPC for SV nodes.
func (f *Fleet) Tip(ctx context.Context, node string) (*teranode.BlockHeader, error) {
	if c := f.sv[node]; c != nil {
		return c.BestBlockHeader(ctx)
	}
	if a := f.asset[node]; a != nil {
		return a.BestBlockHeader(ctx)
	}
	return nil, fmt.Errorf("unknown node %q", node)
}

// Snapshot returns a copy of the current view.
func (f *Fleet) Snapshot() Snapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s := f.snap
	s.Nodes = append([]NodeState(nil), f.snap.Nodes...)
	s.Services = append([]ServiceState(nil), f.snap.Services...)
	s.Arcade.Datahubs = append([]ArcadeDatahub(nil), f.snap.Arcade.Datahubs...)
	s.Watched = nil
	for _, w := range f.watched {
		cp := *w
		cp.PerNode = map[string]OutpointView{}
		for k, v := range w.PerNode {
			cp.PerNode[k] = v
		}
		s.Watched = append(s.Watched, cp)
	}
	sort.Slice(s.Watched, func(i, j int) bool {
		if s.Watched[i].TxID == s.Watched[j].TxID {
			return s.Watched[i].Vout < s.Watched[j].Vout
		}
		return s.Watched[i].TxID < s.Watched[j].TxID
	})
	return s
}

// Node returns the current state of a node.
func (f *Fleet) Node(name string) (NodeState, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, n := range f.snap.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return NodeState{}, false
}

// Watch adds an outpoint to the watched set.
func (f *Fleet) Watch(txid string, vout uint32, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fmt.Sprintf("%s:%d", txid, vout)
	if _, ok := f.watched[k]; !ok {
		f.watched[k] = &OutpointState{TxID: txid, Vout: vout, Label: label, PerNode: map[string]OutpointView{}}
	} else if label != "" {
		f.watched[k].Label = label
	}
}

// Unwatch removes an outpoint.
func (f *Fleet) Unwatch(txid string, vout uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.watched, fmt.Sprintf("%s:%d", txid, vout))
}

// Outpoint returns the current view of a watched outpoint.
func (f *Fleet) Outpoint(txid string, vout uint32) (OutpointState, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	w, ok := f.watched[fmt.Sprintf("%s:%d", txid, vout)]
	if !ok {
		return OutpointState{}, false
	}
	cp := *w
	cp.PerNode = map[string]OutpointView{}
	for k, v := range w.PerNode {
		cp.PerNode[k] = v
	}
	return cp, true
}

// SetPartition records (for display) that a node's plane is disconnected.
func (f *Fleet) SetPartition(node, plane string, on bool) {
	f.partMu.Lock()
	if f.partSet[node] == nil {
		f.partSet[node] = map[string]bool{}
	}
	f.partSet[node][plane] = on
	f.partMu.Unlock()
}

func (f *Fleet) partitions(node string) []string {
	f.partMu.Lock()
	defer f.partMu.Unlock()
	var out []string
	for plane, on := range f.partSet[node] {
		if on {
			out = append(out, plane)
		}
	}
	sort.Strings(out)
	return out
}

// Run polls until ctx is done. Each concern has its own loop so a slow probe (for
// example an alert dial to a partitioned node) never delays chain or UTXO observation.
func (f *Fleet) Run(ctx context.Context) {
	loop := func(every time.Duration, fn func(context.Context)) {
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				fn(ctx)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	loop(f.interval, func(c context.Context) { f.pollNodes(c, false) })
	loop(3*time.Second, f.pollAlertSeqs)
	loop(f.interval, f.pollWatched)
	loop(2*time.Second, f.pollHub)
	loop(2*time.Second, f.pollArcade)
	loop(5*time.Second, f.pollServices)
	<-ctx.Done()
}

// pollAlertSeqs probes every node's alert sequence in parallel.
func (f *Fleet) pollAlertSeqs(ctx context.Context) {
	if f.alert == nil {
		return
	}
	var wg sync.WaitGroup
	for i := range f.inv.Nodes {
		node := f.inv.Nodes[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			seq, err := f.alert.Probe(actx, node.AlertAddr)
			f.mu.Lock()
			defer f.mu.Unlock()
			for j := range f.snap.Nodes {
				st := &f.snap.Nodes[j]
				if st.Name != node.Name {
					continue
				}
				if err != nil {
					st.AlertReachable = false
					return
				}
				st.AlertReachable = true
				if int64(seq) != st.AlertSeq {
					f.bus.Publish("alert_seq", node.Name, fmt.Sprintf("%s alert sequence → %d", node.Name, seq),
						map[string]any{"sequence": seq, "prev": st.AlertSeq})
				}
				st.AlertSeq = int64(seq)
			}
		}()
	}
	wg.Wait()
}

// Refresh fetches one outpoint's state on one node right now (bypassing the poll cadence)
// and records it in the watched set.
func (f *Fleet) Refresh(ctx context.Context, node, txid string, vout uint32) (OutpointView, error) {
	f.Watch(txid, vout, "")
	if f.sv[node] != nil {
		return f.refreshSV(ctx, node, txid, vout)
	}
	a := f.asset[node]
	if a == nil {
		return OutpointView{Status: "ERROR", Error: "unknown node"}, fmt.Errorf("unknown node %q", node)
	}
	outs, err := a.UTXOs(ctx, txid)
	if err != nil {
		return OutpointView{Status: "ERROR", Error: err.Error()}, err
	}
	v := OutpointView{Status: "NOT_FOUND"}
	for _, o := range outs {
		if o.Vout == vout {
			v = OutpointView{Status: o.Status, UTXOHash: o.UTXOHash, Satoshis: o.Satoshis}
			if o.SpendingData != nil && o.SpendingData.TxID != "" && o.Status != "FROZEN" {
				v.SpentBy = o.SpendingData.TxID
			}
		}
	}
	f.record(node, txid, vout, v)
	return v, nil
}

// refreshSV derives an outpoint's state on an SV node from gettxout (unspent → OK, and
// FROZEN when the consensus blacklist covers it at the next height), getrawtransaction
// (known but spent → SPENT) and otherwise NOT_FOUND.
func (f *Fleet) refreshSV(ctx context.Context, node, txid string, vout uint32) (OutpointView, error) {
	c := f.sv[node]
	out, err := c.TxOut(ctx, txid, vout)
	if err != nil {
		return OutpointView{Status: "ERROR", Error: err.Error()}, err
	}
	v := OutpointView{Status: "NOT_FOUND"}
	if out != nil {
		v = OutpointView{Status: "OK", Satoshis: svnode.Satoshis(out.Value)}
		if frozen, ferr := f.SVFrozen(ctx, node, txid, vout); ferr == nil && frozen {
			v.Status = "FROZEN"
		}
	} else if _, rerr := c.RawTransaction(ctx, txid); rerr == nil {
		v.Status = "SPENT"
	}
	f.record(node, txid, vout, v)
	return v, nil
}

// SVFrozen reports whether an SV node's consensus blacklist covers the outpoint at the
// height its next block would have.
func (f *Fleet) SVFrozen(ctx context.Context, node, txid string, vout uint32) (bool, error) {
	c := f.sv[node]
	if c == nil {
		return false, fmt.Errorf("%s is not an SV node", node)
	}
	funds, err := c.QueryBlacklist(ctx)
	if err != nil {
		return false, err
	}
	st, _ := f.Node(node)
	return svnode.FrozenAt(funds, txid, vout, uint64(st.Height)+1), nil
}

// record stores an outpoint view and publishes a utxo event when the status changed.
func (f *Fleet) record(node, txid string, vout uint32, v OutpointView) {
	f.mu.Lock()
	if cur, ok := f.watched[fmt.Sprintf("%s:%d", txid, vout)]; ok {
		if old, had := cur.PerNode[node]; !had || old.Status != v.Status {
			f.bus.Publish("utxo", node, fmt.Sprintf("%s sees %s:%d as %s", node, short(txid), vout, v.Status),
				map[string]any{"txid": txid, "vout": vout, "status": v.Status, "prev": old.Status})
		}
		cur.PerNode[node] = v
	}
	f.mu.Unlock()
}

func (f *Fleet) pollNodes(ctx context.Context, probeAlerts bool) {
	var wg sync.WaitGroup
	for i := range f.inv.Nodes {
		node := f.inv.Nodes[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.pollNode(ctx, node, probeAlerts)
		}()
	}
	wg.Wait()
}

func (f *Fleet) pollNode(ctx context.Context, node topology.Node, probeAlerts bool) {
	if f.sv[node.Name] != nil {
		f.pollSVNode(ctx, node)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	prev, _ := f.Node(node.Name)
	st := prev
	st.Name, st.Index, st.Container = node.Name, node.Index, node.Container
	st.UpdatedAt = time.Now().UTC()
	st.Partitions = f.partitions(node.Name)

	hdr, err := f.asset[node.Name].BestBlockHeader(cctx)
	if err != nil {
		st.Reachable = false
		st.Error = err.Error()
	} else {
		st.Reachable = true
		st.Error = ""
		if hdr.Hash != prev.Tip {
			f.bus.Publish("tip", node.Name, fmt.Sprintf("%s tip → %d %s", node.Name, hdr.Height, short(hdr.Hash)),
				map[string]any{"height": hdr.Height, "hash": hdr.Hash, "prev": prev.Tip})
		}
		st.Height, st.Tip = hdr.Height, hdr.Hash
		if fsm, err := f.asset[node.Name].FSMState(cctx); err == nil {
			st.FSM = fsm
		}
		if mp, err := f.rpc[node.Name].GetRawMempool(cctx); err == nil {
			var clean []string
			for _, id := range mp {
				if id != "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" {
					clean = append(clean, id)
				}
			}
			st.MempoolCount = len(clean)
			if len(clean) > 20 {
				clean = clean[:20]
			}
			st.Mempool = clean
		}
		if st.Version == "" {
			if v, err := f.rpc[node.Name].Version(cctx); err == nil {
				st.Version = v
			}
		}
	}
	_ = probeAlerts
	f.mu.Lock()
	for i := range f.snap.Nodes {
		if f.snap.Nodes[i].Name == node.Name {
			// keep the alert fields maintained by pollAlertSeqs
			st.AlertSeq, st.AlertReachable = f.snap.Nodes[i].AlertSeq, f.snap.Nodes[i].AlertReachable
			f.snap.Nodes[i] = st
		}
	}
	f.snap.UpdatedAt = time.Now().UTC()
	f.mu.Unlock()
}

// pollSVNode observes an SV node over RPC (tip, mempool, peers, version) and its alert
// sidecar's health (unprocessed alerts = RPC applies that failed).
func (f *Fleet) pollSVNode(ctx context.Context, node topology.Node) {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c := f.sv[node.Name]
	prev, _ := f.Node(node.Name)
	st := prev
	st.Name, st.Index, st.Container = node.Name, node.Index, node.Container
	st.UpdatedAt = time.Now().UTC()
	st.Partitions = f.partitions(node.Name)

	hdr, err := c.BestBlockHeader(cctx)
	if err != nil {
		st.Reachable = false
		st.Error = err.Error()
	} else {
		st.Reachable = true
		st.Error = ""
		if hdr.Hash != prev.Tip {
			f.bus.Publish("tip", node.Name, fmt.Sprintf("%s tip → %d %s", node.Name, hdr.Height, short(hdr.Hash)),
				map[string]any{"height": hdr.Height, "hash": hdr.Hash, "prev": prev.Tip})
		}
		st.Height, st.Tip = hdr.Height, hdr.Hash
		if mp, err := c.GetRawMempool(cctx); err == nil {
			st.MempoolCount = len(mp)
			if len(mp) > 20 {
				mp = mp[:20]
			}
			st.Mempool = mp
		}
		if peers, err := c.PeerInfo(cctx); err == nil {
			st.Peers = len(peers)
		}
		if st.Version == "" {
			if v, err := c.SubVersion(cctx); err == nil {
				st.Version = v
			}
		}
	}
	if sc := f.sidecar[node.Name]; sc != nil {
		if h, err := sc.Health(cctx); err == nil {
			st.AlertUnprocessed = h.UnprocessedAlert
		}
	}
	f.mu.Lock()
	for i := range f.snap.Nodes {
		if f.snap.Nodes[i].Name == node.Name {
			st.AlertSeq, st.AlertReachable = f.snap.Nodes[i].AlertSeq, f.snap.Nodes[i].AlertReachable
			f.snap.Nodes[i] = st
		}
	}
	f.snap.UpdatedAt = time.Now().UTC()
	f.mu.Unlock()
}

func (f *Fleet) pollHub(ctx context.Context) {
	if f.hub == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	h, err := f.hub.Health(cctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.snap.Hub = HubState{Reachable: false, Error: err.Error(), Sequence: f.snap.Hub.Sequence}
		return
	}
	if h.Sequence != f.snap.Hub.Sequence {
		f.bus.Publish("alert_seq", "hub", fmt.Sprintf("hub alert sequence → %d", h.Sequence), map[string]any{"sequence": h.Sequence})
	}
	f.snap.Hub = HubState{Reachable: true, Sequence: h.Sequence, ActivePeers: h.ActivePeers, Unprocessed: h.UnprocessedAlert}
}

// pollArcade refreshes arcade's health document and its chaintracks tip, publishing a
// `tip` event (node "arcade") when the tip moves, so the feed shows arcade alongside the nodes.
func (f *Fleet) pollArcade(ctx context.Context) {
	if f.arcade == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var (
		h    *arcade.Health
		herr error
		t    *arcade.Tip
		terr error
		wg   sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); h, herr = f.arcade.Health(cctx) }()
	go func() { defer wg.Done(); t, terr = f.arcade.Tip(cctx) }()
	wg.Wait()

	f.mu.Lock()
	defer f.mu.Unlock()
	a := &f.snap.Arcade
	a.UpdatedAt = time.Now().UTC()
	if herr != nil {
		a.Reachable, a.Healthy, a.Error = false, false, herr.Error()
	} else {
		a.Reachable, a.Error = true, ""
		a.Healthy, a.Version, a.Status, a.BlockHeight = h.Healthy, h.Version, h.Status, h.BlockHeight
		hubs := make([]ArcadeDatahub, 0, len(h.Datahubs))
		for _, d := range h.Datahubs {
			hubs = append(hubs, ArcadeDatahub{URL: d.URL, Node: f.nodeByHost(d.URL), Source: d.Source, Healthy: d.Healthy})
		}
		a.Datahubs = hubs
	}
	if terr != nil {
		a.TipError = terr.Error()
	} else {
		a.TipError = ""
		if t.Hash != a.Tip {
			f.bus.Publish("tip", "arcade", fmt.Sprintf("arcade tip → %d %s", t.Height, short(t.Hash)),
				map[string]any{"height": t.Height, "hash": t.Hash, "prev": a.Tip})
		}
		a.Height, a.Tip = t.Height, t.Hash
	}
}

// nodeByHost names the inventory node whose IP, name or container a URL points at.
func (f *Fleet) nodeByHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	for _, n := range f.inv.Nodes {
		if host == n.ChaosIP || host == n.CtlIP || host == n.Name || host == n.Container {
			return n.Name
		}
	}
	return ""
}

func (f *Fleet) pollServices(ctx context.Context) {
	var out []ServiceState
	check := func(name, url, hostURL, path string) {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		ok, detail := httpOK(cctx, url+path)
		out = append(out, ServiceState{Name: name, URL: url, HostURL: hostURL, Healthy: ok, Detail: detail})
	}
	for name, svc := range f.inv.Services {
		switch name {
		case "arcade", "merkle-service":
			check(name, svc.URL, svc.HostURL, "/health")
		case "wallet-infra":
			// JSON-RPC only; any HTTP answer means the process is up.
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, detail := httpOK(cctx, svc.URL+"/")
			cancel()
			out = append(out, ServiceState{Name: name, URL: svc.URL, HostURL: svc.HostURL, Healthy: detail != "unreachable", Detail: detail})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	f.mu.Lock()
	f.snap.Services = out
	f.mu.Unlock()
}

func (f *Fleet) pollWatched(ctx context.Context) {
	f.mu.RLock()
	keys := make([]*OutpointState, 0, len(f.watched))
	for _, w := range f.watched {
		keys = append(keys, w)
	}
	f.mu.RUnlock()
	var wg sync.WaitGroup
	for _, w := range keys {
		for _, node := range f.inv.Nodes {
			wg.Add(1)
			go func(w *OutpointState, node string) {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
				defer cancel()
				_, _ = f.Refresh(cctx, node, w.TxID, w.Vout)
			}(w, node.Name)
		}
	}
	wg.Wait()
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
