// orchestrator is the simulator backend: it attaches to a running stack (inventory.json),
// observes it, delivers alerts, drives chaos and serves the API + web UI.
//
// Flags/env: -inventory (INVENTORY), -listen (LISTEN, :8600), -keys (KEYS, keys.json for the
// alert genesis keys and identities), -data (DATA dir: alert log, keyring), -kafka (KAFKA_BROKERS,
// empty = no verdict stream), -socket (DOCKER_HOST / podman socket for chaos), -host-mode
// (use host-published URLs; disables alert probing/push and Kafka).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/alerts"
	"github.com/bsv-blockchain/chaos-test/internal/api"
	"github.com/bsv-blockchain/chaos-test/internal/chaos"
	"github.com/bsv-blockchain/chaos-test/internal/diag"
	"github.com/bsv-blockchain/chaos-test/internal/keys"
	"github.com/bsv-blockchain/chaos-test/internal/observe"
	"github.com/bsv-blockchain/chaos-test/internal/scenario"
	"github.com/bsv-blockchain/chaos-test/internal/teranode"
	"github.com/bsv-blockchain/chaos-test/internal/topology"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type keysFile struct {
	Genesis   []keys.Secp256k1Key  `json:"genesis"`
	Publisher keys.Ed25519Identity `json:"publisher"`
}

func main() {
	invPath := flag.String("inventory", env("INVENTORY", "stack/config/inventory.json"), "inventory.json")
	keysPath := flag.String("keys", env("KEYS", "stack/config/keys.json"), "keys.json")
	dataDir := flag.String("data", env("DATA", "sim/.data"), "data directory (alert log, keyring, runs)")
	listen := flag.String("listen", env("LISTEN", ":8600"), "listen address")
	kafka := flag.String("kafka", env("KAFKA_BROKERS", ""), "kafka brokers for verdict topics (empty = off)")
	socket := flag.String("socket", env("PODMAN_SOCKET", ""), "container runtime socket (default: auto)")
	hostMode := flag.Bool("host-mode", env("HOST_MODE", "") == "1", "use host-published URLs (dev on the host)")
	scenariosDir := flag.String("scenarios", env("SCENARIOS", "scenarios"), "directory of scenario YAML files")
	flag.Parse()

	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(lg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, lg, *invPath, *keysPath, *dataDir, *listen, *kafka, *socket, *hostMode, *scenariosDir); err != nil {
		lg.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, lg *slog.Logger, invPath, keysPath, dataDir, listen, kafka, socket string, hostMode bool, scenariosDir string) error {
	inv, err := topology.Load(invPath)
	if err != nil {
		return err
	}
	if hostMode {
		inv.UseHostURLs()
	}
	kb, err := os.ReadFile(keysPath)
	if err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	var kf keysFile
	if err := json.Unmarshal(kb, &kf); err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	if len(kf.Genesis) < 3 {
		return errors.New("keys.json needs at least 3 genesis keys")
	}
	var signing, genesisPubs []string
	for i, g := range kf.Genesis {
		genesisPubs = append(genesisPubs, g.PublicKeyHex)
		if i < 3 {
			signing = append(signing, g.PrivateKeyHex)
		}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	alog, err := alerts.OpenLog(filepath.Join(dataDir, "alerts.json"))
	if err != nil {
		return err
	}
	ring, err := api.OpenKeyring(filepath.Join(dataDir, "keyring.json"), inv.MinerWIF)
	if err != nil {
		return err
	}
	bus := observe.NewBus(5000)

	var host *alerts.Host
	if !hostMode {
		host, err = alerts.NewHost(alerts.HostConfig{PrivateKeyHex: kf.Publisher.PrivateKeyHex, ListenAddrs: []string{"/ip4/0.0.0.0/tcp/9909"},
			ProtocolID: inv.Alert.ProtocolID, Timeout: 45 * time.Second, Logger: lg}, alog)
		if err != nil {
			return fmt.Errorf("alert host: %w", err)
		}
		defer host.Close()
		host.SetDefaultWatermark(0) // never leak alerts to nodes that discover us
		lg.Info("alert host ready", "id", host.ID().String())
	}

	fleet := observe.NewFleet(inv, bus, host, lg)
	go fleet.Run(ctx)

	rt := chaos.New(socket)
	lg.Info("container runtime", "kind", rt.Kind())

	arcadeURL := ""
	if s, ok := inv.Services["arcade"]; ok {
		arcadeURL = s.URL
	}
	dg := diag.New(diag.Deps{Inv: inv, Fleet: fleet, Runtime: rt})
	srv := api.New(api.Deps{Inventory: inv, Bus: bus, Fleet: fleet, AlertLog: alog, AlertHost: host, Signing: signing, Genesis: genesisPubs,
		Keys: ring, Runtime: rt, Logger: lg, Arcade: arcadeURL, Diag: dg})

	eng := scenario.NewEngine(scenario.Deps{Inventory: inv, Bus: bus, Fleet: fleet, API: srv, Logger: lg, RunsDir: filepath.Join(dataDir, "runs")})
	if err := eng.LoadDir(scenariosDir); err != nil {
		lg.Warn("scenarios not loaded", "dir", scenariosDir, "err", err)
	}
	srv.SetScenarios(eng)

	if kafka != "" && !hostMode {
		var topics []teranode.VerdictTopic
		for _, n := range inv.Nodes {
			topics = append(topics, teranode.VerdictTopic{Topic: n.KafkaRejectedTx, Node: n.Name, Kind: "rejected_tx"},
				teranode.VerdictTopic{Topic: n.KafkaInvalidBlocks, Node: n.Name, Kind: "invalid_block"})
		}
		go func() {
			for ctx.Err() == nil {
				if err := teranode.ConsumeVerdicts(ctx, strings.Split(kafka, ","), topics, lg, srv.Verdicts); err != nil {
					lg.Warn("kafka consumer", "err", err)
					time.Sleep(5 * time.Second)
				}
			}
		}()
	}

	httpSrv := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sctx)
	}()
	bus.Publish("log", "", fmt.Sprintf("orchestrator started: %d nodes, runtime %s, alert host %v", len(inv.Nodes), rt.Kind(), host != nil), nil)
	lg.Info("listening", "addr", listen)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
