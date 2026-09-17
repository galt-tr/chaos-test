package alerts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/bsv-blockchain/go-alert-system/app/config"
	"github.com/bsv-blockchain/go-alert-system/app/models"
	"github.com/bsv-blockchain/go-alert-system/app/models/model"
	asp2p "github.com/bsv-blockchain/go-alert-system/app/p2p"
	"github.com/mrz1836/go-datastore"
)

// GossipConfig configures a full go-alert-system P2P participant used for publishing.
type GossipConfig struct {
	PrivateKeyHex         string        // raw 64-byte Ed25519 key, hex (stable identity)
	ListenIP              string        // default 0.0.0.0
	Port                  string        // e.g. "9909"
	BootstrapPeer         string        // multiaddr of the hub (or any peer already in the network)
	Topic                 string        // e.g. bitcoin_alert_system_regtest
	ProtocolID            string        // default DefaultProtocolID
	GenesisPubKeys        []string      // compressed hex, the network's active keys
	PeerDiscoveryInterval time.Duration // default 15s
	DHTMode               string        // "client" (default) or "server"
	DatabasePath          string        // sqlite file; "" = in-memory
	Logger                *slog.Logger
	// ConnectTimeout bounds how long Start waits for the library to report connected.
	ConnectTimeout time.Duration
}

// Gossip is a go-alert-system p2p.Server we drive as a publisher: it bootstraps into the
// private alert network, joins the gossipsub topic and can publish signed alerts, exactly
// as go-alert-system's hack/publish.go does. It is also a normal participant: it stores
// the alerts it sees and answers peers' sync requests.
type Gossip struct {
	cfg    *config.Config
	srv    *asp2p.Server
	topic  string
	ds     datastore.ClientInterface
	cancel context.CancelFunc
}

// StartGossip starts the participant and blocks until it is connected to the network
// (or ConnectTimeout elapses).
func StartGossip(ctx context.Context, gc GossipConfig) (*Gossip, error) {
	if gc.ListenIP == "" {
		gc.ListenIP = "0.0.0.0"
	}
	if gc.ProtocolID == "" {
		gc.ProtocolID = DefaultProtocolID
	}
	if gc.PeerDiscoveryInterval == 0 {
		gc.PeerDiscoveryInterval = 15 * time.Second
	}
	if gc.DHTMode == "" {
		gc.DHTMode = "client"
	}
	if gc.ConnectTimeout == 0 {
		gc.ConnectTimeout = 2 * time.Minute
	}
	if gc.Logger == nil {
		gc.Logger = slog.Default()
	}
	if gc.Topic == "" || gc.Port == "" || len(gc.GenesisPubKeys) == 0 {
		return nil, errors.New("gossip config needs Topic, Port and GenesisPubKeys")
	}

	ds, err := datastore.NewClient(ctx,
		datastore.WithSQLite(&datastore.SQLiteConfig{
			CommonConfig: datastore.CommonConfig{TablePrefix: "alert_system", MaxIdleConnections: 1, MaxOpenConnections: 1},
			DatabasePath: gc.DatabasePath,
		}),
		datastore.WithAutoMigrate(models.BaseModels...),
	)
	if err != nil {
		return nil, fmt.Errorf("alert datastore: %w", err)
	}

	cfg := &config.Config{
		AlertProcessingInterval: 5 * time.Minute,
		GenesisKeys:             gc.GenesisPubKeys,
		Datastore: config.DatastoreConfig{
			AutoMigrate: true,
			Engine:      datastore.SQLite,
			TablePrefix: "alert_system",
			SQLite:      &datastore.SQLiteConfig{DatabasePath: gc.DatabasePath},
		},
		P2P: config.P2PConfig{
			IP:                    gc.ListenIP,
			Port:                  gc.Port,
			BootstrapPeer:         gc.BootstrapPeer,
			AllowPrivateIPs:       true,
			PrivateKey:            gc.PrivateKeyHex,
			TopicName:             gc.Topic,
			AlertSystemProtocolID: gc.ProtocolID,
			PeerDiscoveryInterval: gc.PeerDiscoveryInterval,
			DHTMode:               gc.DHTMode,
		},
	}
	cfg.Services.Datastore = ds
	cfg.Services.Log = &libLogger{lg: gc.Logger}
	// Received alerts are "executed" against this mock so the publisher never touches a node.
	cfg.Services.Node = config.NewNodeMock("chaos", "chaos", "http://127.0.0.1:1")
	cfg.Services.HTTPClient = http.DefaultClient

	if err := models.CreateGenesisAlert(ctx, model.WithAllDependencies(cfg)); err != nil {
		_ = ds.Close(ctx)
		return nil, fmt.Errorf("create genesis alert: %w", err)
	}

	srv, err := asp2p.NewServer(asp2p.ServerOptions{TopicNames: []string{gc.Topic}, Config: cfg})
	if err != nil {
		_ = ds.Close(ctx)
		return nil, fmt.Errorf("alert p2p server: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	g := &Gossip{cfg: cfg, srv: srv, topic: gc.Topic, ds: ds, cancel: cancel}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(runCtx) }()

	deadline := time.After(gc.ConnectTimeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for !srv.Connected() {
		select {
		case err := <-errCh:
			if err != nil {
				g.Stop(ctx)
				return nil, fmt.Errorf("alert p2p start: %w", err)
			}
		case <-deadline:
			g.Stop(ctx)
			return nil, fmt.Errorf("alert p2p not connected after %s (bootstrap %q)", gc.ConnectTimeout, gc.BootstrapPeer)
		case <-ctx.Done():
			g.Stop(ctx)
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
	// Start returns only after the topics are joined; wait for that too.
	for g.srv.Topics()[g.topic] == nil {
		select {
		case <-deadline:
			g.Stop(ctx)
			return nil, errors.New("alert p2p connected but topic never joined")
		case <-tick.C:
		}
	}
	return g, nil
}

// TopicPeers returns the peers currently subscribed to the alert topic as seen by this
// participant's gossipsub router (the mesh a publish can reach).
func (g *Gossip) TopicPeers() int {
	t := g.srv.Topics()[g.topic]
	if t == nil {
		return 0
	}
	return len(t.ListPeers())
}

// WaitForTopicPeers blocks until at least min peers are subscribed to the topic. A
// publish made before the mesh has formed (gossipsub exchanges subscriptions and builds
// the mesh on a ~1 s heartbeat) reaches nobody and is silently dropped.
func (g *Gossip) WaitForTopicPeers(ctx context.Context, min int) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for g.TopicPeers() < min {
		select {
		case <-ctx.Done():
			return fmt.Errorf("only %d topic peers after waiting: %w", g.TopicPeers(), ctx.Err())
		case <-tick.C:
		}
	}
	return nil
}

// Publish broadcasts wire bytes on the alert topic, first waiting (up to 30 s) for at
// least one subscribed peer so the message actually reaches the mesh.
func (g *Gossip) Publish(ctx context.Context, wire []byte) error {
	t := g.srv.Topics()[g.topic]
	if t == nil {
		return errors.New("topic not joined")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := g.WaitForTopicPeers(waitCtx, 1); err != nil {
		return err
	}
	return t.Publish(ctx, wire)
}

// Latest returns the highest alert sequence this participant has stored.
func (g *Gossip) Latest(ctx context.Context) (uint32, error) {
	a, err := models.GetLatestAlert(ctx, nil, model.WithAllDependencies(g.cfg))
	if err != nil {
		return 0, err
	}
	if a == nil {
		return 0, nil
	}
	return a.SequenceNumber, nil
}

// ActivePeers reports the library's count of peers found in the last discovery round.
func (g *Gossip) ActivePeers() int { return g.srv.ActivePeers() }

// Stop shuts the participant down.
func (g *Gossip) Stop(ctx context.Context) {
	g.cancel()
	_ = g.srv.Stop(ctx)
	_ = g.ds.Close(ctx)
}

// libLogger adapts slog to go-alert-system's LoggerInterface.
type libLogger struct{ lg *slog.Logger }

func (l *libLogger) Debug(args ...interface{})              { l.lg.Debug(fmt.Sprint(args...)) }
func (l *libLogger) Debugf(msg string, args ...interface{}) { l.lg.Debug(fmt.Sprintf(msg, args...)) }
func (l *libLogger) Error(args ...interface{})              { l.lg.Error(fmt.Sprint(args...)) }
func (l *libLogger) ErrorWithStack(msg string, args ...interface{}) {
	l.lg.Error(fmt.Sprintf(msg, args...))
}
func (l *libLogger) Errorf(msg string, args ...interface{}) { l.lg.Error(fmt.Sprintf(msg, args...)) }
func (l *libLogger) Fatal(args ...interface{}) {
	l.lg.Error(fmt.Sprint(args...))
	os.Exit(1)
}
func (l *libLogger) Fatalf(msg string, args ...interface{}) {
	l.lg.Error(fmt.Sprintf(msg, args...))
	os.Exit(1)
}
func (l *libLogger) Info(args ...interface{})              { l.lg.Info(fmt.Sprint(args...)) }
func (l *libLogger) Infof(msg string, args ...interface{}) { l.lg.Info(fmt.Sprintf(msg, args...)) }
func (l *libLogger) LogLevel() string                      { return "debug" }
func (l *libLogger) Panic(args ...interface{})             { panic(fmt.Sprint(args...)) }
func (l *libLogger) Panicf(msg string, args ...interface{}) {
	panic(fmt.Sprintf(msg, args...))
}
func (l *libLogger) Warn(args ...interface{})              { l.lg.Warn(fmt.Sprint(args...)) }
func (l *libLogger) Warnf(msg string, args ...interface{}) { l.lg.Warn(fmt.Sprintf(msg, args...)) }
func (l *libLogger) Printf(format string, v ...interface{}) {
	l.lg.Info(fmt.Sprintf(format, v...))
}
func (l *libLogger) CloseWriter() error { return nil }
