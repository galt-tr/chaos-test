// alertctl builds, signs, records and delivers BSV alert-system messages against a
// private alert network. It is the standalone tool for the stack (no simulator needed):
//
//	alertctl keygen ed25519 | secp256k1 [-n 5]
//	alertctl build freeze --fund txid:vout:start:stop[:policy] [--fund ...]   (+ --keys, --log)
//	alertctl build info --message "..." | invalidate --hash H --reason R | confiscate --enforce-at H --tx-hex HEX | ban|unban --peer P --reason R
//	alertctl probe --peer /ip4/…/tcp/9908/p2p/…
//	alertctl push --peer /ip4/… --seq N
//	alertctl broadcast --seq N --bootstrap /ip4/… [--port 9909]
//	alertctl serve [--listen /ip4/0.0.0.0/tcp/9909] [--watermark N]
//	alertctl hub --url http://alert-system:3000
//	alertctl log
//
// Defaults come from env: ALERTCTL_KEYS (keys.json), ALERTCTL_LOG (alerts.json),
// ALERTCTL_TOPIC, ALERTCTL_PROTOCOL, ALERTCTL_HUB, ALERTCTL_IDENTITY (ed25519 hex).
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/alerts"
	"github.com/bsv-blockchain/chaos-test/internal/keys"
)

// KeysFile is the shape of keys.json shared by the stack generator and alertctl.
type KeysFile struct {
	Genesis   []keys.Secp256k1Key  `json:"genesis"`   // 5 alert genesis keys; the first 3 sign
	Publisher keys.Ed25519Identity `json:"publisher"` // alertctl / orchestrator libp2p identity
	Hub       keys.Ed25519Identity `json:"hub"`       // go-alert-system node identity
	Nodes     map[string]NodeKeys  `json:"nodes"`     // by teranode name
}

// NodeKeys are a teranode's identities.
type NodeKeys struct {
	P2P   keys.Ed25519Identity `json:"p2p"`
	Alert keys.Ed25519Identity `json:"alert"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(lg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "build":
		err = cmdBuild(os.Args[2:])
	case "probe":
		err = cmdProbe(ctx, os.Args[2:])
	case "push":
		err = cmdPush(ctx, os.Args[2:])
	case "broadcast":
		err = cmdBroadcast(ctx, os.Args[2:])
	case "serve":
		err = cmdServe(ctx, os.Args[2:])
	case "hub":
		err = cmdHub(ctx, os.Args[2:])
	case "log":
		err = cmdLog(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `alertctl <command> [flags]

  keygen ed25519|secp256k1 [-n N]      print fresh keys as JSON
  build <type> [flags]                 build+sign an alert and append it to the log
     types: freeze unfreeze info invalidate confiscate ban unban setkeys
  probe --peer ADDR                    ask a peer for its latest alert sequence
  push --peer ADDR --seq N             make a peer sync alerts up to N from the log
  broadcast --seq N --bootstrap ADDR   join the alert network and gossip alert N
  serve                                stay online answering peers' sync requests
  hub --url URL                        show a go-alert-system node's health and alerts
  log                                  print the alert log
`)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func commonFlags(fs *flag.FlagSet) (keysPath, logPath, topic, proto, identity *string, timeout *time.Duration) {
	keysPath = fs.String("keys", env("ALERTCTL_KEYS", "/data/keys.json"), "keys.json path")
	logPath = fs.String("log", env("ALERTCTL_LOG", "/data/alerts.json"), "alert log path")
	topic = fs.String("topic", env("ALERTCTL_TOPIC", "bitcoin_alert_system_regtest"), "gossipsub topic")
	proto = fs.String("protocol", env("ALERTCTL_PROTOCOL", alerts.DefaultProtocolID), "sync protocol id")
	identity = fs.String("identity", env("ALERTCTL_IDENTITY", ""), "ed25519 private key hex (default: publisher key from keys.json)")
	timeout = fs.Duration("timeout", 45*time.Second, "per-operation timeout")
	return
}

func loadKeys(path string) (*KeysFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read keys: %w", err)
	}
	var k KeysFile
	if err := json.Unmarshal(data, &k); err != nil {
		return nil, fmt.Errorf("decode keys %s: %w", path, err)
	}
	return &k, nil
}

func (k *KeysFile) signing() ([]string, error) {
	if len(k.Genesis) < 3 {
		return nil, fmt.Errorf("keys.json has %d genesis keys, need at least 3", len(k.Genesis))
	}
	out := make([]string, 0, 3)
	for _, g := range k.Genesis[:3] {
		out = append(out, g.PrivateKeyHex)
	}
	return out, nil
}

func (k *KeysFile) genesisPubs() []string {
	out := make([]string, 0, len(k.Genesis))
	for _, g := range k.Genesis {
		out = append(out, g.PublicKeyHex)
	}
	return out
}

func newHost(keysPath, identity, proto string, timeout time.Duration, log *alerts.Log, listen []string) (*alerts.Host, error) {
	if identity == "" {
		if k, err := loadKeys(keysPath); err == nil {
			identity = k.Publisher.PrivateKeyHex
		}
	}
	return alerts.NewHost(alerts.HostConfig{PrivateKeyHex: identity, ListenAddrs: listen, ProtocolID: proto, Timeout: timeout}, log)
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	n := fs.Int("n", 1, "how many keys")
	kind := "ed25519"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		kind, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		kind = fs.Arg(0)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	switch kind {
	case "ed25519":
		var out []keys.Ed25519Identity
		for i := 0; i < *n; i++ {
			id, err := keys.NewEd25519Identity()
			if err != nil {
				return err
			}
			out = append(out, id)
		}
		return enc.Encode(out)
	case "secp256k1":
		var out []keys.Secp256k1Key
		for i := 0; i < *n; i++ {
			k, err := keys.NewSecp256k1Key()
			if err != nil {
				return err
			}
			out = append(out, k)
		}
		return enc.Encode(out)
	default:
		return fmt.Errorf("unknown key kind %q (ed25519|secp256k1)", kind)
	}
}

type fundList []alerts.Fund

func (f *fundList) String() string { return fmt.Sprint(*f) }

// Set parses txid:vout:start:stop[:policyExpires].
func (f *fundList) Set(v string) error {
	parts := strings.Split(v, ":")
	if len(parts) < 4 || len(parts) > 5 {
		return fmt.Errorf("fund %q: want txid:vout:start:stop[:policy]", v)
	}
	vout, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return fmt.Errorf("fund %q: bad vout: %w", v, err)
	}
	start, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return fmt.Errorf("fund %q: bad start: %w", v, err)
	}
	stop, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return fmt.Errorf("fund %q: bad stop: %w", v, err)
	}
	policy := false
	if len(parts) == 5 {
		policy, err = strconv.ParseBool(parts[4])
		if err != nil {
			return fmt.Errorf("fund %q: bad policy flag: %w", v, err)
		}
	}
	*f = append(*f, alerts.Fund{TxID: parts[0], Vout: uint32(vout), EnforceAtHeightStart: start, EnforceAtHeightStop: stop, PolicyExpiresWithConsensus: policy})
	return nil
}

func cmdBuild(args []string) error {
	if len(args) == 0 {
		return errors.New("build needs a type: freeze unfreeze info invalidate confiscate ban unban setkeys")
	}
	kind := args[0]
	fs := flag.NewFlagSet("build "+kind, flag.ContinueOnError)
	keysPath, logPath, _, _, _, _ := commonFlags(fs)
	seq := fs.Uint("seq", 0, "sequence number (default: log latest + 1)")
	note := fs.String("note", "", "free-text note stored in the log")
	var funds fundList
	fs.Var(&funds, "fund", "txid:vout:start:stop[:policy] (repeatable)")
	message := fs.String("message", "", "informational message")
	hash := fs.String("hash", "", "block hash (display hex)")
	reason := fs.String("reason", "", "reason text")
	enforceAt := fs.Uint64("enforce-at", 0, "confiscation enforce-at height")
	txHex := fs.String("tx-hex", "", "confiscation raw transaction hex")
	peer := fs.String("peer", "", "peer/subnet to ban or unban, e.g. 192.0.2.1/32")
	pubkeys := fs.String("pubkeys", "", "comma-separated 5 compressed pubkeys for setkeys")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	k, err := loadKeys(*keysPath)
	if err != nil {
		return err
	}
	signing, err := k.signing()
	if err != nil {
		return err
	}
	log, err := alerts.OpenLog(*logPath)
	if err != nil {
		return err
	}
	if *seq == 0 {
		*seq = uint(log.Latest()) + 1
	}

	var typ alerts.Type
	var payload []byte
	switch kind {
	case "freeze", "unfreeze":
		typ = alerts.TypeFreezeUTXO
		if kind == "unfreeze" {
			typ = alerts.TypeUnfreezeUTXO
		}
		payload, err = alerts.FundsPayload(funds)
	case "info":
		typ, payload = alerts.TypeInformational, alerts.InformationalPayload(*message)
		if *message == "" {
			err = errors.New("--message required")
		}
	case "invalidate":
		typ = alerts.TypeInvalidateBlock
		payload, err = alerts.InvalidateBlockPayload(*hash, *reason)
	case "confiscate":
		typ = alerts.TypeConfiscateUTXO
		var raw []byte
		raw, err = hex.DecodeString(*txHex)
		if err == nil {
			payload, err = alerts.ConfiscatePayload(*enforceAt, raw)
		}
	case "ban", "unban":
		typ = alerts.TypeBanPeer
		if kind == "unban" {
			typ = alerts.TypeUnbanPeer
		}
		payload, err = alerts.PeerPayload(*peer, *reason)
	case "setkeys":
		typ = alerts.TypeSetKeys
		payload, err = alerts.SetKeysPayload(strings.Split(*pubkeys, ","))
	default:
		return fmt.Errorf("unknown alert type %q", kind)
	}
	if err != nil {
		return err
	}
	a, err := alerts.Build(uint32(*seq), typ, payload, time.Now(), signing)
	if err != nil {
		return err
	}
	e, err := log.Append(a, *note)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"sequence": e.Sequence, "type": e.TypeName, "hash": e.Hash, "text": e.Text, "wireHex": hex.EncodeToString(e.Wire)})
}

func cmdProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	keysPath, logPath, _, proto, identity, timeout := commonFlags(fs)
	peer := fs.String("peer", "", "peer multiaddr with /p2p/<id>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *peer == "" {
		return errors.New("--peer required")
	}
	log, err := alerts.OpenLog(*logPath)
	if err != nil {
		return err
	}
	h, err := newHost(*keysPath, *identity, *proto, *timeout, log, nil)
	if err != nil {
		return err
	}
	defer h.Close()
	seq, err := h.Probe(ctx, *peer)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"peer": *peer, "latestSequence": seq})
}

func cmdPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	keysPath, logPath, _, proto, identity, timeout := commonFlags(fs)
	peer := fs.String("peer", "", "peer multiaddr with /p2p/<id>")
	seq := fs.Uint("seq", 0, "deliver alerts up to this sequence (default: log latest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *peer == "" {
		return errors.New("--peer required")
	}
	log, err := alerts.OpenLog(*logPath)
	if err != nil {
		return err
	}
	if *seq == 0 {
		*seq = uint(log.Latest())
	}
	h, err := newHost(*keysPath, *identity, *proto, *timeout, log, nil)
	if err != nil {
		return err
	}
	defer h.Close()
	before, err := h.Probe(ctx, *peer)
	if err != nil {
		return err
	}
	res, err := h.Push(ctx, *peer, uint32(*seq))
	if err != nil {
		return err
	}
	after, aerr := h.Probe(ctx, *peer)
	return printJSON(map[string]any{"peer": *peer, "before": before, "delivered": res.Delivered, "after": after, "afterProbeError": errString(aerr)})
}

func cmdBroadcast(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("broadcast", flag.ContinueOnError)
	keysPath, logPath, topic, proto, identity, timeout := commonFlags(fs)
	seq := fs.Uint("seq", 0, "alert sequence to publish (default: log latest)")
	bootstrap := fs.String("bootstrap", env("ALERTCTL_BOOTSTRAP", ""), "bootstrap peer multiaddr (the hub)")
	port := fs.String("port", env("ALERTCTL_GOSSIP_PORT", "9909"), "listen port for the gossip participant")
	dht := fs.String("dht", "client", "dht mode: client|server")
	wait := fs.Duration("wait", 5*time.Second, "time to keep the participant alive after publishing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	k, err := loadKeys(*keysPath)
	if err != nil {
		return err
	}
	if *identity == "" {
		*identity = k.Publisher.PrivateKeyHex
	}
	log, err := alerts.OpenLog(*logPath)
	if err != nil {
		return err
	}
	if *seq == 0 {
		*seq = uint(log.Latest())
	}
	wire, ok := log.Wire(uint32(*seq))
	if !ok {
		return fmt.Errorf("alert %d not in log", *seq)
	}
	slog.Info("joining alert network", "bootstrap", *bootstrap, "topic", *topic)
	g, err := alerts.StartGossip(ctx, alerts.GossipConfig{
		PrivateKeyHex: *identity, Port: *port, BootstrapPeer: *bootstrap, Topic: *topic, ProtocolID: *proto,
		GenesisPubKeys: k.genesisPubs(), DHTMode: *dht, ConnectTimeout: *timeout,
	})
	if err != nil {
		return err
	}
	defer g.Stop(context.Background())
	if err := g.Publish(ctx, wire); err != nil {
		return err
	}
	slog.Info("published", "seq", *seq, "topicPeers", g.TopicPeers(), "activePeers", g.ActivePeers())
	select {
	case <-time.After(*wait):
	case <-ctx.Done():
	}
	latest, _ := g.Latest(ctx)
	return printJSON(map[string]any{"published": *seq, "topicPeers": g.TopicPeers(), "activePeers": g.ActivePeers(), "participantLatest": latest})
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	keysPath, logPath, _, proto, identity, timeout := commonFlags(fs)
	listen := fs.String("listen", env("ALERTCTL_LISTEN", "/ip4/0.0.0.0/tcp/9909"), "listen multiaddr")
	watermark := fs.Uint("watermark", 0, "highest sequence served to peers (default: log latest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log, err := alerts.OpenLog(*logPath)
	if err != nil {
		return err
	}
	if *watermark == 0 {
		*watermark = uint(log.Latest())
	}
	h, err := newHost(*keysPath, *identity, *proto, *timeout, log, []string{*listen})
	if err != nil {
		return err
	}
	defer h.Close()
	h.SetDefaultWatermark(uint32(*watermark))
	for _, a := range h.Addrs() {
		slog.Info("serving alert sync", "addr", a, "watermark", *watermark)
	}
	<-ctx.Done()
	return nil
}

func cmdHub(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	url := fs.String("url", env("ALERTCTL_HUB", "http://alert-system:3000"), "go-alert-system base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := alerts.NewHubClient(*url)
	h, err := c.Health(ctx)
	if err != nil {
		return err
	}
	list, lerr := c.Alerts(ctx)
	return printJSON(map[string]any{"health": h, "alerts": list, "alertsError": errString(lerr)})
}

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	_, logPath, _, _, _, _ := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	log, err := alerts.OpenLog(*logPath)
	if err != nil {
		return err
	}
	type row struct {
		Sequence uint32 `json:"sequence"`
		Type     string `json:"type"`
		Hash     string `json:"hash"`
		Text     string `json:"text"`
		Note     string `json:"note,omitempty"`
	}
	var rows []row
	for _, e := range log.Entries() {
		rows = append(rows, row{e.Sequence, e.TypeName, e.Hash, e.Text, e.Note})
	}
	return printJSON(rows)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
