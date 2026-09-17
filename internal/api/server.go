// Package api serves the orchestrator's REST + SSE API and the embedded web UI.
package api

import (
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/alerts"
	"github.com/bsv-blockchain/chaos-test/internal/chaos"
	"github.com/bsv-blockchain/chaos-test/internal/observe"
	"github.com/bsv-blockchain/chaos-test/internal/teranode"
	"github.com/bsv-blockchain/chaos-test/internal/topology"
	"github.com/bsv-blockchain/chaos-test/internal/wallet"
)

//go:embed uidist
var uiFS embed.FS

// Scenarios is implemented by the scenario engine (internal/scenario); nil disables it.
type Scenarios interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// Deps wires the server to the rest of the harness.
type Deps struct {
	Inventory *topology.Inventory
	Bus       *observe.Bus
	Fleet     *observe.Fleet
	AlertLog  *alerts.Log
	AlertHost *alerts.Host // nil when the orchestrator is not on alertnet
	Signing   []string     // 3 genesis private keys (hex)
	Genesis   []string     // genesis public keys
	Keys      *Keyring
	Runtime   chaos.Runtime
	Scenarios Scenarios
	Logger    *slog.Logger
	Arcade    string    // arcade base URL (POST /tx), may be empty
	Diag      Diagnoser // nil disables /api/diagnostics
}

// Server is the HTTP API.
type Server struct {
	d     Deps
	mux   *http.ServeMux
	netMu sync.Mutex
}

// verifyAttachment polls the runtime until the container's attachment to network matches
// wantAttached (up to ~5 s).
func (s *Server) verifyAttachment(ctx context.Context, container, network string, wantAttached bool) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		nets, err := s.d.Runtime.Networks(ctx, container)
		if err == nil {
			attached := false
			for _, nw := range nets {
				if nw == network {
					attached = true
				}
			}
			if attached == wantAttached {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("%s attachment to %s is still %v", container, network, !wantAttached)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// New builds the server and its routes.
func New(d Deps) *Server {
	s := &Server{d: d, mux: http.NewServeMux()}
	m := s.mux
	m.HandleFunc("GET /api/inventory", s.getInventory)
	m.HandleFunc("GET /api/state", s.getState)
	m.HandleFunc("GET /api/events", s.sse)
	m.HandleFunc("GET /api/events/recent", s.recentEvents)
	m.HandleFunc("POST /api/mine", s.postMine)
	m.HandleFunc("GET /api/alerts", s.getAlerts)
	m.HandleFunc("POST /api/alerts/build", s.postAlertBuild)
	m.HandleFunc("POST /api/alerts/push", s.postAlertPush)
	m.HandleFunc("POST /api/alerts/rpc", s.postAlertRPC)
	m.HandleFunc("GET /api/keys", s.getKeys)
	m.HandleFunc("POST /api/keys", s.postKey)
	m.HandleFunc("POST /api/tx/spend", s.postSpend)
	m.HandleFunc("POST /api/tx/submit", s.postSubmit)
	m.HandleFunc("GET /api/tx/{txid}", s.getTx)
	m.HandleFunc("GET /api/coinbase", s.getCoinbase)
	m.HandleFunc("POST /api/watch", s.postWatch)
	m.HandleFunc("DELETE /api/watch/{txid}/{vout}", s.deleteWatch)
	m.HandleFunc("POST /api/chaos/partition", s.postPartition)
	m.HandleFunc("POST /api/chaos/{action}", s.postChaos)
	m.HandleFunc("GET /api/logs", s.getLogs)
	m.HandleFunc("GET /api/diagnostics", s.getDiagnostics)
	sub, _ := fs.Sub(uiFS, "uidist")
	fileServer := http.FileServer(http.FS(sub))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// SPA fallback: serve index.html for unknown, non-file paths.
		if r.URL.Path != "/" && !strings.Contains(r.URL.Path, ".") {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
	return s
}

// SetScenarios mounts the scenario engine's routes.
func (s *Server) SetScenarios(sc Scenarios) {
	s.d.Scenarios = sc
	s.mux.Handle("/api/scenarios", sc)
	s.mux.Handle("/api/scenarios/", sc)
	s.mux.Handle("/api/runs", sc)
	s.mux.Handle("/api/runs/", sc)
}

// AlertLatest returns the highest alert sequence in the log.
func (s *Server) AlertLatest() uint32 { return s.d.AlertLog.Latest() }

// NewKey creates a named key in the keyring.
func (s *Server) NewKey(name, note string) (*KeyEntry, error) { return s.d.Keys.New(name, note) }

// Chaos runs a container action (pause, unpause, stop, start) on a node.
// Logs returns a window of a node container's log. Shared by the /api/logs handler and
// the scenario engine's log_count assertion.
func (s *Server) Logs(ctx context.Context, node string, opt chaos.LogOptions) (chaos.LogResult, error) {
	n, err := s.node(node)
	if err != nil {
		return chaos.LogResult{}, err
	}
	return s.d.Runtime.Logs(ctx, n.Container, opt)
}

func (s *Server) Chaos(ctx context.Context, node, action string) error {
	n, err := s.node(node)
	if err != nil {
		return err
	}
	switch action {
	case "pause":
		err = s.d.Runtime.Pause(ctx, n.Container)
	case "unpause":
		err = s.d.Runtime.Unpause(ctx, n.Container)
	case "stop":
		err = s.d.Runtime.Stop(ctx, n.Container)
	case "start":
		err = s.d.Runtime.Start(ctx, n.Container)
	default:
		return fmt.Errorf("unknown chaos action %q", action)
	}
	if err != nil {
		return err
	}
	s.d.Bus.Publish("chaos", n.Name, fmt.Sprintf("%s: %s", n.Name, action), map[string]any{"action": action})
	return nil
}

// Handler returns the root handler with CORS for the Vite dev server.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// ---- helpers ------------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v)
}

func (s *Server) node(name string) (*topology.Node, error) {
	return s.d.Inventory.Node(name)
}

// ---- inventory / state / events -----------------------------------------------------------

func (s *Server) getInventory(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.d.Inventory)
}

func (s *Server) getState(w http.ResponseWriter, _ *http.Request) {
	snap := s.d.Fleet.Snapshot()
	kind := s.d.Runtime.Kind()
	// Advertised so the Logs page can render its unavailable state without first making a
	// request it knows will fail.
	logsInfo := map[string]any{"available": kind != "none", "runtime": kind}
	if kind == "none" {
		logsInfo["reason"] = reasonNoRuntime
	}
	writeJSON(w, 200, map[string]any{
		"snapshot":  snap,
		"alerts":    s.alertSummaries(),
		"runtime":   kind,
		"alertHost": s.d.AlertHost != nil,
		"logs":      logsInfo,
		"diag":      s.d.Diag != nil,
	})
}

func (s *Server) recentEvents(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 200
	}
	writeJSON(w, 200, s.d.Bus.Recent(n))
}

func (s *Server) sse(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		fl.Flush()
	}
	ch, unsub := s.d.Bus.Subscribe(256)
	defer unsub()
	var after uint64
	if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseUint(v, 10, 64)
		for _, e := range s.d.Bus.Since(after) {
			send("event", e)
		}
	}
	send("snapshot", s.d.Fleet.Snapshot())
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			send("event", e)
		case <-tick.C:
			send("snapshot", s.d.Fleet.Snapshot())
		}
	}
}

// ---- mining -------------------------------------------------------------------------------

func (s *Server) postMine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node    string `json:"node"`
		Blocks  int    `json:"blocks"`
		Address string `json:"address"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.Blocks <= 0 {
		req.Blocks = 1
	}
	n, err := s.node(req.Node)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	var hashes []string
	if req.Address != "" {
		hashes, err = s.d.Fleet.RPC(n.Name).GenerateToAddress(ctx, req.Blocks, req.Address)
	} else {
		hashes, err = s.d.Fleet.RPC(n.Name).Generate(ctx, req.Blocks)
	}
	if err != nil {
		s.d.Bus.Publish("error", n.Name, "mine failed: "+err.Error(), nil)
		writeErr(w, 502, err)
		return
	}
	s.d.Bus.Publish("mine", n.Name, fmt.Sprintf("%s mined %d block(s)", n.Name, len(hashes)), map[string]any{"hashes": hashes, "address": req.Address})
	writeJSON(w, 200, map[string]any{"node": n.Name, "hashes": hashes})
}

// ---- alerts -------------------------------------------------------------------------------

// AlertSummary is what the UI lists.
type AlertSummary struct {
	Sequence  uint32    `json:"sequence"`
	Type      string    `json:"type"`
	Hash      string    `json:"hash"`
	Text      string    `json:"text"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// Funds are the outpoints of a freeze/unfreeze alert, decoded from the wire bytes so the
	// UI can link them (the library's free text double-hex-encodes the txid).
	Funds []alerts.Fund `json:"funds,omitempty"`
}

func (s *Server) alertSummaries() []AlertSummary {
	out := []AlertSummary{}
	for _, e := range s.d.AlertLog.Entries() {
		funds, _ := alerts.Funds(e.Wire)
		out = append(out, AlertSummary{Sequence: e.Sequence, Type: e.TypeName, Hash: e.Hash, Text: e.Text, Note: e.Note, CreatedAt: e.CreatedAt, Funds: funds})
	}
	return out
}

func (s *Server) getAlerts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.alertSummaries())
}

// BuildRequest describes an alert to build and sign.
type BuildRequest struct {
	Type      string        `json:"type"` // freeze unfreeze informational invalidateblock confiscate ban unban setkeys
	Sequence  uint32        `json:"sequence"`
	Funds     []alerts.Fund `json:"funds"`
	Message   string        `json:"message"`
	BlockHash string        `json:"blockHash"`
	Reason    string        `json:"reason"`
	EnforceAt uint64        `json:"enforceAt"`
	TxHex     string        `json:"txHex"`
	Peer      string        `json:"peer"`
	PubKeys   []string      `json:"pubKeys"`
	Note      string        `json:"note"`
	Watch     bool          `json:"watch"` // also watch the funds' outpoints
}

// BuildAlert builds, signs and logs an alert (shared with the scenario engine).
func (s *Server) BuildAlert(req BuildRequest) (*alerts.Entry, error) {
	typ, err := alerts.ParseType(req.Type)
	if err != nil {
		return nil, err
	}
	var payload []byte
	switch typ {
	case alerts.TypeFreezeUTXO, alerts.TypeUnfreezeUTXO:
		payload, err = alerts.FundsPayload(req.Funds)
	case alerts.TypeInformational:
		if req.Message == "" {
			err = errors.New("message required")
		}
		payload = alerts.InformationalPayload(req.Message)
	case alerts.TypeInvalidateBlock:
		payload, err = alerts.InvalidateBlockPayload(req.BlockHash, req.Reason)
	case alerts.TypeConfiscateUTXO:
		var raw []byte
		raw, err = hex.DecodeString(req.TxHex)
		if err == nil {
			payload, err = alerts.ConfiscatePayload(req.EnforceAt, raw)
		}
	case alerts.TypeBanPeer, alerts.TypeUnbanPeer:
		payload, err = alerts.PeerPayload(req.Peer, req.Reason)
	case alerts.TypeSetKeys:
		payload, err = alerts.SetKeysPayload(req.PubKeys)
	}
	if err != nil {
		return nil, err
	}
	seq := req.Sequence
	if seq == 0 {
		seq = s.d.AlertLog.Latest() + 1
	}
	a, err := alerts.Build(seq, typ, payload, time.Now(), s.d.Signing)
	if err != nil {
		return nil, err
	}
	e, err := s.d.AlertLog.Append(a, req.Note)
	if err != nil {
		return nil, err
	}
	if req.Watch {
		for _, f := range req.Funds {
			s.d.Fleet.Watch(f.TxID, f.Vout, fmt.Sprintf("alert #%d", seq))
		}
	}
	s.d.Bus.Publish("alert", "", fmt.Sprintf("built alert #%d (%s): %s", e.Sequence, e.TypeName, e.Text), map[string]any{"sequence": e.Sequence, "type": e.TypeName})
	return e, nil
}

func (s *Server) postAlertBuild(w http.ResponseWriter, r *http.Request) {
	var req BuildRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	e, err := s.BuildAlert(req)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, e)
}

// PushAlert delivers alerts up to seq to a node over the sync stream.
func (s *Server) PushAlert(ctx context.Context, node string, seq uint32) (*alerts.PushResult, error) {
	if s.d.AlertHost == nil {
		return nil, errors.New("alert host disabled (orchestrator is not on alertnet)")
	}
	n, err := s.node(node)
	if err != nil {
		return nil, err
	}
	if seq == 0 {
		seq = s.d.AlertLog.Latest()
	}
	res, err := s.d.AlertHost.Push(ctx, n.AlertAddr, seq)
	if err != nil {
		s.d.Bus.Publish("error", n.Name, fmt.Sprintf("push alert #%d to %s failed: %v", seq, n.Name, err), nil)
		return nil, err
	}
	s.d.Bus.Publish("alert", n.Name, fmt.Sprintf("pushed alerts up to #%d to %s (delivered %v)", seq, n.Name, res.Delivered),
		map[string]any{"sequence": seq, "delivered": res.Delivered})
	return res, nil
}

func (s *Server) postAlertPush(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node     string `json:"node"`
		Sequence uint32 `json:"sequence"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res, err := s.PushAlert(ctx, req.Node, req.Sequence)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, res)
}

// postAlertRPC applies a freeze/unfreeze through the node's admin RPC instead of the alert
// network — the comparison mechanism.
func (s *Server) postAlertRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node          string  `json:"node"`
		Action        string  `json:"action"` // freeze | unfreeze
		TxID          string  `json:"txid"`
		Vout          uint32  `json:"vout"`
		Start         *uint64 `json:"start"`
		Stop          *uint64 `json:"stop"`
		PolicyExpires bool    `json:"policyExpires"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	n, err := s.node(req.Node)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	switch req.Action {
	case "freeze":
		err = s.d.Fleet.RPC(n.Name).Freeze(ctx, req.TxID, req.Vout, req.Start, req.Stop, req.PolicyExpires)
	case "unfreeze":
		err = s.d.Fleet.RPC(n.Name).Unfreeze(ctx, req.TxID, req.Vout)
	default:
		err = fmt.Errorf("unknown action %q", req.Action)
	}
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	s.d.Fleet.Watch(req.TxID, req.Vout, "rpc "+req.Action)
	s.d.Bus.Publish("alert", n.Name, fmt.Sprintf("%s via RPC on %s: %s:%d", req.Action, n.Name, req.TxID, req.Vout), nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- keys / transactions -----------------------------------------------------------------

func (s *Server) getKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.d.Keys.List(r.URL.Query().Get("full") == "1"))
}

func (s *Server) postKey(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Note string }
	if err := decode(r, &req); err != nil || req.Name == "" {
		writeErr(w, 400, errors.New("name required"))
		return
	}
	e, err := s.d.Keys.New(req.Name, req.Note)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, e)
}

// SpendRequest builds a signed spend of one outpoint.
type SpendRequest struct {
	Node     string `json:"node"` // node to fetch the source tx from (default first)
	TxID     string `json:"txid"`
	Vout     uint32 `json:"vout"`
	Key      string `json:"key"`      // key name | hex | WIF that unlocks the source
	To       string `json:"to"`       // destination key name | hex (default: same key)
	ToScript string `json:"toScript"` // or a locking script hex
	Satoshis uint64 `json:"satoshis"` // 0 = all minus fee
	Fee      uint64 `json:"fee"`
	Outputs  int    `json:"outputs"` // split the amount over this many outputs to `To` (default 1)
}

// SpendResult is the built transaction.
type SpendResult struct {
	TxID  string `json:"txid"`
	Hex   string `json:"hex"`
	EFHex string `json:"efHex"`
	Size  int    `json:"size"`
}

// Spend builds a transaction (shared with the scenario engine).
func (s *Server) Spend(ctx context.Context, req SpendRequest) (*SpendResult, error) {
	if req.Node == "" {
		req.Node = s.d.Inventory.Nodes[0].Name
	}
	n, err := s.node(req.Node)
	if err != nil {
		return nil, err
	}
	key, err := s.d.Keys.Resolve(req.Key)
	if err != nil {
		return nil, err
	}
	srcHex, err := s.d.Fleet.Asset(n.Name).TxHex(ctx, req.TxID)
	if err != nil {
		return nil, err
	}
	src, err := wallet.ParseTx(srcHex)
	if err != nil {
		return nil, fmt.Errorf("parse source tx: %w", err)
	}
	if int(req.Vout) >= len(src.Outputs) {
		return nil, fmt.Errorf("vout %d out of range", req.Vout)
	}
	if req.Fee == 0 {
		req.Fee = 500
	}
	total := req.Satoshis
	if total == 0 {
		if src.Outputs[req.Vout].Satoshis <= req.Fee {
			return nil, errors.New("output too small for fee")
		}
		total = src.Outputs[req.Vout].Satoshis - req.Fee
	}
	if req.Outputs <= 0 {
		req.Outputs = 1
	}
	var toKey *wallet.Key
	if req.ToScript == "" {
		if req.To == "" {
			toKey = key
		} else if toKey, err = s.d.Keys.Resolve(req.To); err != nil {
			return nil, err
		}
	}
	var outs []wallet.Output
	each := total / uint64(req.Outputs)
	for i := 0; i < req.Outputs; i++ {
		outs = append(outs, wallet.Output{Key: toKey, Script: req.ToScript, Satoshis: each})
	}
	tx, err := wallet.Spend([]wallet.Input{{SourceTx: src, Vout: req.Vout, Key: key}}, outs, req.Fee)
	if err != nil {
		return nil, err
	}
	ef, err := tx.EFHex()
	if err != nil {
		return nil, err
	}
	return &SpendResult{TxID: tx.TxID().String(), Hex: tx.Hex(), EFHex: ef, Size: tx.Size()}, nil
}

func (s *Server) postSpend(w http.ResponseWriter, r *http.Request) {
	var req SpendRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := s.Spend(ctx, req)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.d.Bus.Publish("tx", "", fmt.Sprintf("built tx %s spending %s:%d", res.TxID, req.TxID, req.Vout), map[string]any{"txid": res.TxID})
	writeJSON(w, 200, res)
}

// SubmitResult reports a submission.
type SubmitResult struct {
	Target   string `json:"target"`
	TxID     string `json:"txid,omitempty"`
	Accepted bool   `json:"accepted"`
	Status   int    `json:"status"`
	Body     string `json:"body,omitempty"`
}

// Submit sends a transaction to one node (asset POST /tx) or to arcade (target "arcade").
func (s *Server) Submit(ctx context.Context, target, txHex, efHex string) (*SubmitResult, error) {
	if target == "arcade" {
		if s.d.Arcade == "" {
			return nil, errors.New("arcade URL not configured")
		}
		body := efHex
		if body == "" {
			body = txHex
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.d.Arcade, "/")+"/tx", strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		res := &SubmitResult{Target: "arcade", Accepted: resp.StatusCode/100 == 2, Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
		var doc struct {
			TxID string `json:"txid"`
		}
		_ = json.Unmarshal(b, &doc)
		res.TxID = doc.TxID
		if res.TxID == "" {
			res.TxID = txidOf(txHex)
		}
		s.d.Bus.Publish("tx", "", fmt.Sprintf("submitted to arcade: %d %s", resp.StatusCode, short(res.Body)), map[string]any{"status": resp.StatusCode, "txid": res.TxID})
		return res, nil
	}
	n, err := s.node(target)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(txHex))
	if err != nil {
		return nil, fmt.Errorf("tx hex: %w", err)
	}
	status, body, err := s.d.Fleet.Asset(n.Name).SubmitTx(ctx, raw)
	if err != nil {
		return nil, err
	}
	res := &SubmitResult{Target: n.Name, TxID: txidOf(txHex), Accepted: status/100 == 2, Status: status, Body: body}
	s.d.Bus.Publish("tx", n.Name, fmt.Sprintf("submitted to %s: %d %s", n.Name, status, short(body)), map[string]any{"status": status, "body": body, "txid": res.TxID})
	return res, nil
}

// txidOf returns the txid of a raw transaction hex, or "" when it does not parse.
func txidOf(txHex string) string {
	tx, err := wallet.ParseTx(strings.TrimSpace(txHex))
	if err != nil || tx == nil {
		return ""
	}
	return tx.TxID().String()
}

func (s *Server) postSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string `json:"target"`
		Hex    string `json:"hex"`
		EFHex  string `json:"efHex"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := s.Submit(ctx, req.Target, req.Hex, req.EFHex)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) getTx(w http.ResponseWriter, r *http.Request) {
	txid := r.PathValue("txid")
	nodeName := r.URL.Query().Get("node")
	if nodeName == "" {
		nodeName = s.d.Inventory.Nodes[0].Name
	}
	n, err := s.node(nodeName)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out := map[string]any{"node": n.Name, "txid": txid}
	if meta, err := s.d.Fleet.Asset(n.Name).TxMeta(ctx, txid); err == nil {
		meta.Tx, meta.SpendingDatas = nil, nil
		out["txmeta"] = meta
	} else {
		out["txmetaError"] = err.Error()
	}
	if utxos, err := s.d.Fleet.Asset(n.Name).UTXOs(ctx, txid); err == nil {
		out["utxos"] = utxos
	} else {
		out["utxosError"] = err.Error()
	}
	if h, err := s.d.Fleet.Asset(n.Name).TxHex(ctx, txid); err == nil {
		out["hex"] = h
	}
	writeJSON(w, 200, out)
}

func (s *Server) getCoinbase(w http.ResponseWriter, r *http.Request) {
	nodeName := r.URL.Query().Get("node")
	if nodeName == "" {
		nodeName = s.d.Inventory.Nodes[0].Name
	}
	n, err := s.node(nodeName)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	h, _ := strconv.Atoi(r.URL.Query().Get("height"))
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	b, err := s.d.Fleet.Asset(n.Name).BlockByHeight(ctx, uint32(h))
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	var cb struct {
		TxID string `json:"txid"`
		Hex  string `json:"hex"`
	}
	_ = json.Unmarshal(b.CoinbaseTx, &cb)
	writeJSON(w, 200, map[string]any{"height": b.Height, "hash": b.Hash, "txid": cb.TxID, "hex": cb.Hex})
}

func (s *Server) postWatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TxID  string `json:"txid"`
		Vout  uint32 `json:"vout"`
		Label string `json:"label"`
	}
	if err := decode(r, &req); err != nil || len(req.TxID) != 64 {
		writeErr(w, 400, errors.New("txid (64 hex) and vout required"))
		return
	}
	s.d.Fleet.Watch(req.TxID, req.Vout, req.Label)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) deleteWatch(w http.ResponseWriter, r *http.Request) {
	v, _ := strconv.Atoi(r.PathValue("vout"))
	s.d.Fleet.Unwatch(r.PathValue("txid"), uint32(v))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- chaos --------------------------------------------------------------------------------

// Partition connects/disconnects a node's plane (shared with the scenario engine).
func (s *Server) Partition(ctx context.Context, node, plane string, on bool) error {
	n, err := s.node(node)
	if err != nil {
		return err
	}
	var network, ip string
	switch plane {
	case "p2p":
		network, ip = "chaos_chaosnet", n.ChaosIP
	case "alert":
		network, ip = "chaos_alertnet", n.AlertIP
	default:
		return fmt.Errorf("plane must be p2p or alert, got %q", plane)
	}
	// Network operations are serialised and verified: concurrent teardowns have been seen to
	// leave one container still attached, which silently defeats a partition.
	s.netMu.Lock()
	defer s.netMu.Unlock()
	for attempt := 1; attempt <= 3; attempt++ {
		if on {
			err = s.d.Runtime.NetworkDisconnect(ctx, network, n.Container)
		} else {
			err = s.d.Runtime.NetworkConnect(ctx, network, n.Container, ip)
		}
		if err != nil && !(!on && strings.Contains(err.Error(), "already")) {
			s.d.Bus.Publish("error", n.Name, fmt.Sprintf("partition %s %s on=%v failed (attempt %d): %v", n.Name, plane, on, attempt, err), nil)
			continue
		}
		if verr := s.verifyAttachment(ctx, n.Container, network, !on); verr == nil {
			err = nil
			break
		} else {
			err = verr
			s.d.Bus.Publish("error", n.Name, fmt.Sprintf("partition %s %s on=%v not effective (attempt %d): %v", n.Name, plane, on, attempt, verr), nil)
		}
	}
	if err != nil {
		return err
	}
	s.d.Fleet.SetPartition(n.Name, plane, on)
	verb := "reconnected to"
	if on {
		verb = "partitioned from"
	}
	s.d.Bus.Publish("chaos", n.Name, fmt.Sprintf("%s %s the %s plane", n.Name, verb, plane), map[string]any{"plane": plane, "on": on})
	return nil
}

func (s *Server) postPartition(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node  string `json:"node"`
		Plane string `json:"plane"`
		On    bool   `json:"on"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.Partition(ctx, req.Node, req.Plane, req.On); err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) postChaos(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	var req struct {
		Node string `json:"node"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	n, err := s.node(req.Node)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	switch action {
	case "pause":
		err = s.d.Runtime.Pause(ctx, n.Container)
	case "unpause":
		err = s.d.Runtime.Unpause(ctx, n.Container)
	case "stop":
		err = s.d.Runtime.Stop(ctx, n.Container)
	case "start":
		err = s.d.Runtime.Start(ctx, n.Container)
	default:
		writeErr(w, 404, fmt.Errorf("unknown chaos action %q", action))
		return
	}
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	s.d.Bus.Publish("chaos", n.Name, fmt.Sprintf("%s: %s", n.Name, action), map[string]any{"action": action})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// Verdicts is the Kafka consumer hook: publish verdict events on the bus.
func (s *Server) Verdicts(v teranode.Verdict) {
	data := map[string]any{"hash": v.Hash, "reason": v.Reason, "peerID": v.PeerID, "peerURL": v.PeerURL}
	if v.Kind == "rejected_tx" {
		data["txid"] = v.Hash // the hash of a rejected-tx verdict is a txid; say so for consumers
	}
	s.d.Bus.Publish(v.Kind, v.Node, fmt.Sprintf("%s %s %s: %s", v.Node, strings.ReplaceAll(v.Kind, "_", " "), short(v.Hash), v.Reason), data)
}

func short(h string) string {
	if len(h) > 16 {
		return h[:16] + "…"
	}
	return h
}
