// gen renders the standalone stack for N teranodes and SV SV nodes: compose.yaml, per-node
// env files and bitcoin.conf files, the alert hub and sidecar configs, keys.json (created
// once, then reused) and inventory.json.
//
//	go run ./cmd/gen -n 3 -sv 2 -out .      (or: make gen N=3 SV=2)
//
// Addressing (all pinned): chaosnet 10.190.0.0/24 (kafka .5, teranodeN .10+N, svnodeJ .20+J,
// arcade .40, merkle .41, wallet .42), alertnet 192.0.0.128/26 (gw .129, hub .130, teranodeN
// .140+N, alert-svnodeJ .150+J, tools .180, orchestrator .181), ctlnet 10.191.0.0/24 (teranodeN
// .10+N, svnodeJ .20+J, hub .30, alert-svnodeJ .30+J, arcade .40, merkle .41, wallet .42, tools
// .50, orchestrator .51). Host ports: teranodes 20000+(N-1)*2000+(port-8000); SV nodes
// 40000+(J-1)*1000 (+332 RPC, +300 sidecar API).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/bsv-blockchain/bsv-regtest/keys"
	"github.com/bsv-blockchain/bsv-regtest/topology"
)

// project names everything the container runtime sees: compose project, containers
// (<project>-<service>), networks (<project>_<net>) and locally built images
// (localhost/<project>/<image>:<tag>). Fixed on purpose: inventory, Makefile and docs stay
// trivially consistent, and nothing needs a second instance on one host.
const project = "bsv-regtest"

// ctr is the container name of a compose service.
func ctr(service string) string { return project + "-" + service }

const (
	alertTopic    = "bitcoin_alert_system_regtest"
	alertProtocol = "/bitcoin/alert-system/1.0.0"
	rpcUser       = "bitcoin"
	rpcPass       = "bitcoin"
	// PK1 from teranode's settings.conf: the default miner_wallet_private_keys entry.
	minerWIF = "L56TgyTpDdvL3W24SMoALYotibToSCySQeo4pThLKxw6EFR6f93Q"
)

// KeysFile mirrors cmd/alertctl's KeysFile.
type KeysFile struct {
	// ArcadeCallbackToken authenticates merkle-service callbacks into arcade.
	ArcadeCallbackToken string `json:"arcadeCallbackToken"`
	// WalletServerKey is go-wallet-toolbox infra's server_private_key (hex).
	WalletServerKey keys.Secp256k1Key `json:"walletServerKey"`
	// WalletUserKey is the dev wallet identity used by the harness (hex).
	WalletUserKey keys.Secp256k1Key    `json:"walletUserKey"`
	AdminAPIKey   string               `json:"adminAPIKey"` // teranode grpc_admin_api_key shared by all nodes (regtest dev value)
	Genesis       []keys.Secp256k1Key  `json:"genesis"`
	Publisher     keys.Ed25519Identity `json:"publisher"`
	Hub           keys.Ed25519Identity `json:"hub"`
	Nodes         map[string]NodeKeys  `json:"nodes"`
	SVNodes       map[string]NodeKeys  `json:"svnodes,omitempty"` // only .Alert is used: the sidecar's identity
	Wallet        map[string]string    `json:"wallet,omitempty"`  // reserved for milestone 3/6
}

// NodeKeys are a teranode's identities.
type NodeKeys struct {
	P2P   keys.Ed25519Identity `json:"p2p"`
	Alert keys.Ed25519Identity `json:"alert"`
}

type nodeView struct {
	topology.Node
	P2PKey, AlertKey                                   string
	StaticPeers                                        string
	HostRPC, HostAsset, HostHealth, HostProp, HostDash int
}

// svView is one SV node plus what its templates need.
type svView struct {
	topology.Node
	AlertKey       string // the sidecar's ed25519 private key
	HostRPC        int
	HostSidecarAPI int
	StrictPolicy   bool // enforce standard-output policy (acceptnonstdoutputs=0)
}

type view struct {
	Project        string
	Internal       bool // compose networks internal (egress-free)
	N              int
	Nodes          []nodeView
	SV             int
	SVNodes        []svView
	SVNodeImage    string
	LegacyEnabled  bool // teranodes run the legacy (Bitcoin wire) service so SV nodes can follow
	Hub            topology.Hub
	HubKey         string
	GenesisPubs    []string
	GenesisPipe    string
	TeranodeImage  string
	Topic          string
	Protocol       string
	Generated      string
	DiscoveryEvery string
	AdminAPIKey    string
	ArcadeToken    string
	WalletKey      string
	DatahubURLs    []string // http://<chaosIP>:8090/api/v1 per node
	P2PAddrs       []string // node main-p2p multiaddrs (chaosnet)
	Arcade         topology.Service
	Merkle         topology.Service
	Wallet         topology.Service
}

func main() {
	n := flag.Int("n", 3, "number of teranodes (2..10)")
	sv := flag.Int("sv", 2, "number of SV nodes following the teranodes (0..5), each with an alert sidecar")
	out := flag.String("out", ".", "output directory (the bsv-regtest module root)")
	image := flag.String("teranode-image", "localhost/"+project+"/teranode:main", "default teranode image")
	svImage := flag.String("svnode-image", "docker.io/bitcoinsv/bitcoin-sv:1.2.2", "default SV node image")
	discovery := flag.String("alert-discovery-interval", "15s", "alert p2p peer discovery interval")
	internal := flag.Bool("internal", false, "make the compose networks internal (no egress); host-published ports still work on podman, on docker only from the host itself")
	flag.Parse()
	if *n < 2 || *n > 10 {
		fmt.Fprintln(os.Stderr, "-n must be in 2..10")
		os.Exit(2)
	}
	if *sv < 0 || *sv > 5 {
		fmt.Fprintln(os.Stderr, "-sv must be in 0..5")
		os.Exit(2)
	}
	if err := run(*n, *sv, *out, *image, *svImage, *discovery, *internal); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(n, sv int, out, image, svImage, discovery string, internal bool) error {
	cfgDir := filepath.Join(out, "config")
	dirs := []string{filepath.Join(cfgDir, "teranode"), filepath.Join(cfgDir, "alert-system"), filepath.Join(out, "scripts")}
	if sv > 0 {
		dirs = append(dirs, filepath.Join(cfgDir, "svnode"))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	kf, err := loadOrCreateKeys(filepath.Join(cfgDir, "keys.json"), n, sv)
	if err != nil {
		return err
	}

	v := view{Project: project, Internal: internal, N: n, SV: sv, SVNodeImage: svImage, LegacyEnabled: sv > 0, TeranodeImage: image, Topic: alertTopic, Protocol: alertProtocol,
		Generated: time.Now().UTC().Format(time.RFC3339), DiscoveryEvery: discovery, HubKey: kf.Hub.PrivateKeyHex, AdminAPIKey: kf.AdminAPIKey,
		ArcadeToken: kf.ArcadeCallbackToken, WalletKey: kf.WalletServerKey.PrivateKeyHex}
	v.Arcade = topology.Service{URL: "http://10.191.0.40:8080", HostURL: "http://localhost:18080", Container: ctr("arcade"),
		Endpoint: topology.Endpoint{ChaosIP: "10.190.0.40", CtlIP: "10.191.0.40"},
		// Secondary listeners (ports per the compose template): health, SSE tx events, chaintracks.
		URLs: map[string]topology.URLPair{
			"health":      {URL: "http://10.191.0.40:8081", HostURL: "http://localhost:18081"},
			"events":      {URL: "http://10.191.0.40:8082", HostURL: "http://localhost:18082"},
			"chaintracks": {URL: "http://10.191.0.40:8083", HostURL: "http://localhost:18083"},
		}}
	v.Merkle = topology.Service{URL: "http://10.191.0.41:8080", HostURL: "http://localhost:18090", Container: ctr("merkle-service"),
		Endpoint: topology.Endpoint{ChaosIP: "10.190.0.41", CtlIP: "10.191.0.41"}}
	v.Wallet = topology.Service{URL: "http://10.191.0.42:8100", HostURL: "http://localhost:18100", Container: ctr("wallet-infra"),
		Endpoint: topology.Endpoint{ChaosIP: "10.190.0.42", CtlIP: "10.191.0.42"}}
	for _, g := range kf.Genesis {
		v.GenesisPubs = append(v.GenesisPubs, g.PublicKeyHex)
	}
	v.GenesisPipe = strings.Join(v.GenesisPubs, " | ")
	v.Hub = topology.Hub{
		AlertIP: "192.0.0.130", CtlIP: "10.191.0.30", PeerID: kf.Hub.PeerID, Container: ctr("alert-system"),
		APIURL: "http://10.191.0.30:3000", HostAPI: "http://localhost:3000",
	}
	v.Hub.AlertAddr = fmt.Sprintf("/ip4/%s/tcp/9906/p2p/%s", v.Hub.AlertIP, v.Hub.PeerID)

	inv := topology.Inventory{
		Project: project, Network: "regtest", Generated: v.Generated,
		Networks: topology.Networks{Chaos: "10.190.0.0/24", Alert: "192.0.0.128/26", Ctl: "10.191.0.0/24",
			ChaosName: project + "_chaosnet", AlertName: project + "_alertnet", CtlName: project + "_ctlnet"},
		Hub: &v.Hub, RPCUser: rpcUser, RPCPass: rpcPass, MinerWIF: minerWIF,
		Tools: &topology.Endpoint{AlertIP: "192.0.0.180", CtlIP: "10.191.0.50", Container: ctr("tools")},
		Alert: topology.AlertNetwork{Topic: alertTopic, ProtocolID: alertProtocol, GenesisPubKeys: v.GenesisPubs, Bootstrap: v.Hub.AlertAddr},
		Services: map[string]topology.Service{
			"kafka": {URL: "kafka-shared:9092", Container: ctr("kafka-shared"), Endpoint: topology.Endpoint{ChaosIP: "10.190.0.5"}},
		},
	}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("teranode%d", i)
		nk := kf.Nodes[name]
		base := 20000 + (i-1)*2000
		node := topology.Node{
			Kind: "teranode", Name: name, Index: i, Container: ctr(name),
			ChaosIP: fmt.Sprintf("10.190.0.%d", 10+i), AlertIP: fmt.Sprintf("192.0.0.%d", 140+i), CtlIP: fmt.Sprintf("10.191.0.%d", 10+i),
			PeerID: nk.P2P.PeerID, AlertPeerID: nk.Alert.PeerID, HostBase: base,
			KafkaRejectedTx: "rejectedtx-" + name, KafkaInvalidBlocks: "invalid-blocks-" + name,
		}
		if v.LegacyEnabled {
			node.LegacyAddr = node.ChaosIP + ":18444"
		}
		node.P2PAddr = fmt.Sprintf("/ip4/%s/tcp/9905/p2p/%s", node.ChaosIP, node.PeerID)
		node.AlertAddr = fmt.Sprintf("/ip4/%s/tcp/9908/p2p/%s", node.AlertIP, node.AlertPeerID)
		node.RPCURL = fmt.Sprintf("http://%s:9292", node.CtlIP)
		node.AssetURL = fmt.Sprintf("http://%s:8090/api/v1", node.CtlIP)
		node.PropagationURL = fmt.Sprintf("http://%s:8833", node.CtlIP)
		node.HealthURL = fmt.Sprintf("http://%s:8000/health", node.CtlIP)
		node.HostRPCURL = fmt.Sprintf("http://localhost:%d", base+1292)
		node.HostAssetURL = fmt.Sprintf("http://localhost:%d/api/v1", base+90)
		inv.Nodes = append(inv.Nodes, node)
	}
	// SV nodes follow the teranodes over the legacy (Bitcoin wire) service. Each gets a
	// go-alert-system sidecar on alertnet that applies alerts to it over RPC; the sidecar's
	// multiaddr is the node's AlertAddr so push/probe/partition treat both kinds alike.
	for j := 1; j <= sv; j++ {
		name := fmt.Sprintf("svnode%d", j)
		sk := kf.SVNodes[name]
		base := 40000 + (j-1)*1000
		node := topology.Node{
			Kind: "svnode", Name: name, Index: j, Container: ctr(name),
			ChaosIP: fmt.Sprintf("10.190.0.%d", 20+j), CtlIP: fmt.Sprintf("10.191.0.%d", 20+j),
			AlertIP: fmt.Sprintf("192.0.0.%d", 150+j), AlertPeerID: sk.Alert.PeerID, HostBase: base,
		}
		node.AlertAddr = fmt.Sprintf("/ip4/%s/tcp/9906/p2p/%s", node.AlertIP, node.AlertPeerID)
		node.RPCURL = fmt.Sprintf("http://%s:18332", node.CtlIP)
		node.HostRPCURL = fmt.Sprintf("http://localhost:%d", base+332)
		node.Sidecar = &topology.Sidecar{Name: "alert-" + name, Container: ctr("alert-" + name),
			CtlIP: fmt.Sprintf("10.191.0.%d", 30+j), APIURL: fmt.Sprintf("http://10.191.0.%d:3000", 30+j), HostAPI: fmt.Sprintf("http://localhost:%d", base+300)}
		inv.Nodes = append(inv.Nodes, node)
	}
	for i := range inv.Nodes {
		if !inv.Nodes[i].IsSV() {
			continue
		}
		var peers []string
		for j := range inv.Nodes {
			switch {
			case j == i:
			case inv.Nodes[j].IsSV():
				peers = append(peers, inv.Nodes[j].ChaosIP+":18444")
			case inv.Nodes[j].LegacyAddr != "":
				peers = append(peers, inv.Nodes[j].LegacyAddr)
			}
		}
		inv.Nodes[i].LegacyPeers = peers
	}
	inv.Services["arcade"] = v.Arcade
	inv.Services["merkle-service"] = v.Merkle
	inv.Services["wallet-infra"] = v.Wallet
	teranodes := inv.Teranodes()
	for i := range teranodes {
		v.DatahubURLs = append(v.DatahubURLs, fmt.Sprintf("http://%s:8090/api/v1", teranodes[i].ChaosIP))
		v.P2PAddrs = append(v.P2PAddrs, teranodes[i].P2PAddr)
	}
	for i := range teranodes {
		node := teranodes[i]
		var peers []string
		for j := range teranodes {
			if j != i {
				peers = append(peers, teranodes[j].P2PAddr)
			}
		}
		nk := kf.Nodes[node.Name]
		v.Nodes = append(v.Nodes, nodeView{Node: node, P2PKey: nk.P2P.PrivateKeyHex, AlertKey: nk.Alert.PrivateKeyHex,
			StaticPeers: strings.Join(peers, " | "),
			HostRPC:     node.HostBase + 1292, HostAsset: node.HostBase + 90, HostHealth: node.HostBase, HostProp: node.HostBase + 833, HostDash: node.HostBase + 90})
	}
	for _, node := range inv.SVNodes() {
		v.SVNodes = append(v.SVNodes, svView{Node: node, AlertKey: kf.SVNodes[node.Name].Alert.PrivateKeyHex,
			HostRPC: node.HostBase + 332, HostSidecarAPI: node.HostBase + 300})
	}
	// The LAST SV node enforces standard-output policy, so the fleet always contains one node
	// that rejects a zero-satoshi bare OP_RETURN output while every other node keeps SV Node's
	// permissive post-Genesis default. Requires at least two SV nodes so a permissive one
	// remains for comparison; with -sv 1 the single node stays at the default.
	if len(v.SVNodes) >= 2 {
		v.SVNodes[len(v.SVNodes)-1].StrictPolicy = true
	}

	if err := render(composeTmpl, filepath.Join(out, "compose.yaml"), v); err != nil {
		return err
	}
	if err := render(commonEnvTmpl, filepath.Join(cfgDir, "teranode", "common.env"), v); err != nil {
		return err
	}
	for _, nd := range v.Nodes {
		if err := render(nodeEnvTmpl, filepath.Join(cfgDir, "teranode", nd.Name+".env"), struct {
			nodeView
			view
		}{nd, v}); err != nil {
			return err
		}
	}
	// The hub bootstraps to teranode1 (mutual bootstrap converges) and points its RPC at
	// teranode1, which ignores the SV-style calls; sidecars bootstrap to the hub and point
	// their RPC at their own SV node, retrying failed applies every 30 s.
	if err := writeJSON(filepath.Join(cfgDir, "alert-system", "config.json"),
		alertSystemConfig(v, teranodes[0].AlertAddr, teranodes[0].RPCURL, v.HubKey, "5m")); err != nil {
		return err
	}
	for _, sn := range v.SVNodes {
		if err := writeJSON(filepath.Join(cfgDir, "alert-system", sn.Name+".json"),
			alertSystemConfig(v, v.Hub.AlertAddr, sn.RPCURL, sn.AlertKey, "30s")); err != nil {
			return err
		}
		if err := render(svnodeConfTmpl, filepath.Join(cfgDir, "svnode", sn.Name+".conf"), struct {
			svView
			view
		}{sn, v}); err != nil {
			return err
		}
	}
	if err := render(toolsEnvTmpl, filepath.Join(cfgDir, "tools.env"), v); err != nil {
		return err
	}
	for _, d := range []string{"arcade", "wallet-infra"} {
		if err := os.MkdirAll(filepath.Join(cfgDir, d), 0o755); err != nil {
			return err
		}
	}
	if err := render(arcadeCfgTmpl, filepath.Join(cfgDir, "arcade", "config.yaml"), v); err != nil {
		return err
	}
	if err := render(merkleEnvTmpl, filepath.Join(cfgDir, "merkle-service.env"), v); err != nil {
		return err
	}
	if err := render(walletCfgTmpl, filepath.Join(cfgDir, "wallet-infra", "infra-config.yaml"), v); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(cfgDir, "inventory.json"), inv); err != nil {
		return err
	}
	fmt.Printf("generated %d-teranode, %d-svnode network under %s\n  make up   (or: docker|podman compose -f %s/compose.yaml --profile tools --profile arcade --profile merkle --profile wallet up -d)\n", n, sv, out, out)
	return nil
}

func loadOrCreateKeys(path string, n, sv int) (*KeysFile, error) {
	kf := &KeysFile{Nodes: map[string]NodeKeys{}}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, kf); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if kf.Nodes == nil {
		kf.Nodes = map[string]NodeKeys{}
	}
	if kf.SVNodes == nil {
		kf.SVNodes = map[string]NodeKeys{}
	}
	changed := false
	for len(kf.Genesis) < 5 {
		k, err := keys.NewSecp256k1Key()
		if err != nil {
			return nil, err
		}
		kf.Genesis = append(kf.Genesis, k)
		changed = true
	}
	var err error
	if kf.AdminAPIKey == "" {
		k, kerr := keys.NewSecp256k1Key()
		if kerr != nil {
			return nil, kerr
		}
		kf.AdminAPIKey = "chaos-" + k.PrivateKeyHex[:40]
		changed = true
	}
	if kf.ArcadeCallbackToken == "" {
		k, kerr := keys.NewSecp256k1Key()
		if kerr != nil {
			return nil, kerr
		}
		kf.ArcadeCallbackToken = k.PrivateKeyHex
		changed = true
	}
	if kf.WalletServerKey.PrivateKeyHex == "" {
		if kf.WalletServerKey, err = keys.NewSecp256k1Key(); err != nil {
			return nil, err
		}
		changed = true
	}
	if kf.WalletUserKey.PrivateKeyHex == "" {
		if kf.WalletUserKey, err = keys.NewSecp256k1Key(); err != nil {
			return nil, err
		}
		changed = true
	}
	if kf.Publisher.PeerID == "" {
		if kf.Publisher, err = keys.NewEd25519Identity(); err != nil {
			return nil, err
		}
		changed = true
	}
	if kf.Hub.PeerID == "" {
		if kf.Hub, err = keys.NewEd25519Identity(); err != nil {
			return nil, err
		}
		changed = true
	}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("teranode%d", i)
		nk, ok := kf.Nodes[name]
		if ok && nk.P2P.PeerID != "" && nk.Alert.PeerID != "" {
			continue
		}
		if nk.P2P, err = keys.NewEd25519Identity(); err != nil {
			return nil, err
		}
		if nk.Alert, err = keys.NewEd25519Identity(); err != nil {
			return nil, err
		}
		kf.Nodes[name] = nk
		changed = true
	}
	for j := 1; j <= sv; j++ {
		name := fmt.Sprintf("svnode%d", j)
		sk, ok := kf.SVNodes[name]
		if ok && sk.Alert.PeerID != "" {
			continue
		}
		if sk.Alert, err = keys.NewEd25519Identity(); err != nil {
			return nil, err
		}
		kf.SVNodes[name] = sk
		changed = true
	}
	if changed {
		if err := writeJSON(path, kf); err != nil {
			return nil, err
		}
	}
	return kf, nil
}

// alertSystemConfig renders a go-alert-system config: for the hub (bootstrap = teranode1,
// RPC = teranode1) or for an SV node's sidecar (bootstrap = hub, RPC = that SV node).
func alertSystemConfig(v view, bootstrap, rpcURL, privateKey, processingInterval string) map[string]any {
	return map[string]any{
		"alert_processing_interval": processingInterval,
		"alert_webhook_url":         "",
		"bitcoin_config_path":       "",
		"datastore": map[string]any{
			"auto_migrate": true, "debug": false, "engine": "sqlite", "password": "",
			"sqlite":       map[string]any{"database_path": "/data/alert.db", "shared": false},
			"table_prefix": "alert_system",
		},
		"disable_rpc_verification": true,
		"environment":              "local",
		"genesis_keys":             v.GenesisPubs,
		"log_output_file":          "",
		"log_level":                "debug",
		"p2p": map[string]any{
			"alert_system_protocol_id":   alertProtocol,
			"bootstrap_peer":             bootstrap,
			"ip":                         "0.0.0.0",
			"port":                       "9906",
			"peer_discovery_interval":    v.DiscoveryEvery,
			"allow_private_ip_addresses": false, // alertnet is public-looking; keep alert traffic off ctlnet
			"private_key":                privateKey,
			"topic_name":                 alertTopic,
		},
		"request_logging": true,
		"rpc_connections": []map[string]string{{"host": rpcURL, "user": rpcUser, "password": rpcPass}},
		"web_server":      map[string]string{"idle_timeout": "60s", "port": "3000", "read_timeout": "15s", "write_timeout": "15s"},
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func render(tmpl, dst string, data any) error {
	t, err := template.New(filepath.Base(dst)).Funcs(template.FuncMap{"join": strings.Join}).Parse(tmpl)
	if err != nil {
		return fmt.Errorf("template %s: %w", dst, err)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	return t.Execute(f, data)
}
