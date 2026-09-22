// Package topology defines inventory.json: the contract between the stack layer (which
// generates it) and the tools/simulator layer (which consumes it).
package topology

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Inventory describes one generated stack.
type Inventory struct {
	Network   string             `json:"network"`   // regtest | teratestnet
	Generated string             `json:"generated"` // RFC3339
	Networks  Networks           `json:"networks"`
	Nodes     []Node             `json:"nodes"`
	Hub       *Hub               `json:"hub,omitempty"`
	Tools     *Endpoint          `json:"tools,omitempty"`
	Services  map[string]Service `json:"services,omitempty"` // arcade, merkle-service, wallet-infra, kafka
	Alert     AlertNetwork       `json:"alert"`
	RPCUser   string             `json:"rpcUser"`
	RPCPass   string             `json:"rpcPass"`
	MinerWIF  string             `json:"minerWIF"` // key the nodes' coinbase pays to (regtest dev key)
}

// Networks lists the compose networks and their subnets.
type Networks struct {
	Chaos string `json:"chaosnet"` // node p2p, datahub access
	Alert string `json:"alertnet"` // alert p2p (public-looking subnet)
	Ctl   string `json:"ctlnet"`   // control plane
}

// Node is one node of the fleet: a teranode (Kind "teranode", or empty in older inventories)
// or an SV Node (Kind "svnode"). SV nodes have no asset API, no embedded alert service and no
// Kafka topics; their alert-network presence is a go-alert-system sidecar (Sidecar), whose
// multiaddr is what AlertAddr/AlertIP/AlertPeerID describe.
type Node struct {
	Kind        string `json:"kind,omitempty"` // teranode | svnode
	Name        string `json:"name"`
	Index       int    `json:"index"`
	ChaosIP     string `json:"chaosIP"`
	AlertIP     string `json:"alertIP"`
	CtlIP       string `json:"ctlIP"`
	PeerID      string `json:"peerID"`      // main p2p identity
	AlertPeerID string `json:"alertPeerID"` // alert p2p identity
	AlertAddr   string `json:"alertAddr"`   // /ip4/<alertIP>/tcp/9908/p2p/<alertPeerID>
	P2PAddr     string `json:"p2pAddr"`     // /ip4/<chaosIP>/tcp/9905/p2p/<peerID>
	// In-network URLs (from ctlnet):
	RPCURL         string `json:"rpcURL"`
	AssetURL       string `json:"assetURL"` // …/api/v1
	PropagationURL string `json:"propagationURL"`
	HealthURL      string `json:"healthURL"`
	// Host-published URLs:
	HostRPCURL   string `json:"hostRPCURL"`
	HostAssetURL string `json:"hostAssetURL"`
	HostBase     int    `json:"hostBase"`
	// Kafka topics this node publishes to:
	KafkaRejectedTx    string `json:"kafkaRejectedTx"`
	KafkaInvalidBlocks string `json:"kafkaInvalidBlocks"`
	Container          string `json:"container"`
	// Legacy (Bitcoin wire protocol) bridging between teranodes and SV nodes:
	LegacyAddr  string   `json:"legacyAddr,omitempty"`  // teranode: <chaosIP>:18444 when its legacy service is on
	LegacyPeers []string `json:"legacyPeers,omitempty"` // svnode: the -connect targets it syncs from
	Sidecar     *Sidecar `json:"sidecar,omitempty"`     // svnode: its go-alert-system sidecar
}

// Sidecar is the go-alert-system process that applies alerts to an SV node over RPC.
type Sidecar struct {
	Name      string `json:"name"`
	Container string `json:"container"`
	CtlIP     string `json:"ctlIP"`
	APIURL    string `json:"apiURL"`     // in-network /health, /alerts
	HostAPI   string `json:"hostAPIURL"` // host-published
}

// IsSV reports whether the node is an SV Node.
func (n *Node) IsSV() bool { return n.Kind == "svnode" }

// Teranodes returns the teranodes in inventory order.
func (inv *Inventory) Teranodes() []Node {
	var out []Node
	for _, n := range inv.Nodes {
		if !n.IsSV() {
			out = append(out, n)
		}
	}
	return out
}

// SVNodes returns the SV nodes in inventory order.
func (inv *Inventory) SVNodes() []Node {
	var out []Node
	for _, n := range inv.Nodes {
		if n.IsSV() {
			out = append(out, n)
		}
	}
	return out
}

// Hub is the go-alert-system node.
type Hub struct {
	AlertIP   string `json:"alertIP"`
	CtlIP     string `json:"ctlIP"`
	PeerID    string `json:"peerID"`
	AlertAddr string `json:"alertAddr"`
	APIURL    string `json:"apiURL"`     // in-network
	HostAPI   string `json:"hostAPIURL"` // host-published
	Container string `json:"container"`
}

// Endpoint is a generic service placement.
type Endpoint struct {
	AlertIP   string `json:"alertIP,omitempty"`
	CtlIP     string `json:"ctlIP,omitempty"`
	ChaosIP   string `json:"chaosIP,omitempty"`
	Container string `json:"container,omitempty"`
}

// Service is a non-node service the tools may talk to.
type Service struct {
	URL       string `json:"url"`
	HostURL   string `json:"hostURL,omitempty"`
	Container string `json:"container,omitempty"`
	// URLs holds a service's secondary listeners by name, e.g. arcade's health (8081),
	// events (8082, SSE) and chaintracks (8083), each as an in-network/host pair.
	URLs map[string]URLPair `json:"urls,omitempty"`
	Endpoint
}

// URLPair is one in-network / host-published URL pair for a secondary listener.
type URLPair struct {
	URL     string `json:"url"`
	HostURL string `json:"hostURL,omitempty"`
}

// ChaosURL returns the service URL reachable from chaosnet (same port as URL).
func (s Service) ChaosURL() string {
	if s.ChaosIP == "" || s.URL == "" {
		return s.URL
	}
	// URL is http://<ctlIP>:<port>[/path]; swap the host.
	rest := s.URL[len("http://"):]
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		return "http://" + s.ChaosIP + rest[i:]
	}
	return "http://" + s.ChaosIP
}

// AlertNetwork holds the shared alert-network parameters.
type AlertNetwork struct {
	Topic          string   `json:"topic"`
	ProtocolID     string   `json:"protocolID"`
	GenesisPubKeys []string `json:"genesisPubKeys"`
	Bootstrap      string   `json:"bootstrap"` // hub alert addr
}

// Load reads an inventory file.
func Load(path string) (*Inventory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read inventory: %w", err)
	}
	var inv Inventory
	if err := json.Unmarshal(b, &inv); err != nil {
		return nil, fmt.Errorf("decode inventory %s: %w", path, err)
	}
	return &inv, nil
}

// Node returns the node with the given name, or the teranode with the given 1-based index
// (SV nodes number from 1 too and are addressed by name only).
func (inv *Inventory) Node(nameOrIndex string) (*Node, error) {
	for i := range inv.Nodes {
		n := &inv.Nodes[i]
		if n.Name == nameOrIndex || (!n.IsSV() && fmt.Sprint(n.Index) == nameOrIndex) {
			return n, nil
		}
	}
	return nil, fmt.Errorf("no node %q in inventory", nameOrIndex)
}

// UseHostURLs rewrites every in-network URL to its host-published counterpart, for a
// process that runs on the host instead of inside the compose networks (-host-mode).
func (inv *Inventory) UseHostURLs() {
	for i := range inv.Nodes {
		n := &inv.Nodes[i]
		if n.HostRPCURL != "" {
			n.RPCURL = n.HostRPCURL
		}
		if n.HostAssetURL != "" {
			n.AssetURL = n.HostAssetURL
		}
		// The health port is published at hostBase (see cmd/gen). Without this the
		// health probe would dial the unreachable ctlnet address in -host-mode.
		if n.HostBase != 0 {
			n.HealthURL = fmt.Sprintf("http://localhost:%d/health", n.HostBase)
		}
		if n.Sidecar != nil && n.Sidecar.HostAPI != "" {
			n.Sidecar.APIURL = n.Sidecar.HostAPI
		}
	}
	if inv.Hub != nil && inv.Hub.HostAPI != "" {
		inv.Hub.APIURL = inv.Hub.HostAPI
	}
	for k, s := range inv.Services {
		if s.HostURL != "" {
			s.URL = s.HostURL
		}
		for name, p := range s.URLs {
			if p.HostURL != "" {
				p.URL = p.HostURL
				s.URLs[name] = p
			}
		}
		inv.Services[k] = s
	}
}
