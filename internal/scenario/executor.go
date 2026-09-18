package scenario

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/alerts"
	"github.com/bsv-blockchain/chaos-test/internal/api"
	"github.com/bsv-blockchain/chaos-test/internal/arcade"
	"github.com/bsv-blockchain/chaos-test/internal/automine"
	"github.com/bsv-blockchain/chaos-test/internal/chaos"
	"github.com/bsv-blockchain/chaos-test/internal/teranode"
	"github.com/bsv-blockchain/chaos-test/internal/walletsvc"
)

type executor struct {
	stepStart time.Time // wall-clock start of the current step (default `since` for log checks)
	e         *Engine
	r         *Run
	def       *Definition
}

// ---- templating -----------------------------------------------------------------------------
//
// Values in `with` may contain ${...} expressions:
//   ${var}            a run variable or param (roles resolve too: ${A} -> teranode1)
//   ${var.field}      a field of a map-valued variable (e.g. ${parent.txid})
//   ${expr + 3}       integer arithmetic on the resolved value (+, -)
//   ${tip(A)}         current height of role/node A;  ${tiphash(A)} its tip hash
//   ${latest}         highest alert sequence in the log

var exprRe = regexp.MustCompile(`\$\{([^}]+)\}`)

func (x *executor) resolve(v any) (any, error) {
	switch t := v.(type) {
	case string:
		if strings.HasPrefix(t, "${") && strings.HasSuffix(t, "}") && strings.Count(t, "${") == 1 {
			return x.eval(strings.TrimSuffix(strings.TrimPrefix(t, "${"), "}"))
		}
		var err error
		out := exprRe.ReplaceAllStringFunc(t, func(m string) string {
			val, e := x.eval(m[2 : len(m)-1])
			if e != nil && err == nil {
				err = e
			}
			return fmt.Sprint(val)
		})
		return out, err
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			rv, err := x.resolve(val)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			rv, err := x.resolve(val)
			if err != nil {
				return nil, err
			}
			out = append(out, rv)
		}
		return out, nil
	default:
		return v, nil
	}
}

func (x *executor) eval(expr string) (any, error) {
	expr = strings.TrimSpace(expr)
	// arithmetic: left (+|-) right
	for _, op := range []string{" + ", " - "} {
		if i := strings.LastIndex(expr, op); i > 0 {
			l, err := x.eval(expr[:i])
			if err != nil {
				return nil, err
			}
			rgt, err := x.eval(expr[i+len(op):])
			if err != nil {
				return nil, err
			}
			li, lok := toInt(l)
			ri, rok := toInt(rgt)
			if !lok || !rok {
				return nil, fmt.Errorf("non-integer arithmetic in %q", expr)
			}
			if op == " + " {
				return li + ri, nil
			}
			return li - ri, nil
		}
	}
	if n, err := strconv.ParseInt(expr, 10, 64); err == nil {
		return n, nil
	}
	// tip()/tiphash() read the node live: the fleet snapshot can lag a block right after mining.
	if strings.HasPrefix(expr, "tip(") && strings.HasSuffix(expr, ")") {
		hdr, err := x.liveTip(strings.TrimSuffix(strings.TrimPrefix(expr, "tip("), ")"))
		if err != nil {
			return nil, err
		}
		return int64(hdr.Height), nil
	}
	if strings.HasPrefix(expr, "tiphash(") && strings.HasSuffix(expr, ")") {
		hdr, err := x.liveTip(strings.TrimSuffix(strings.TrimPrefix(expr, "tiphash("), ")"))
		if err != nil {
			return nil, err
		}
		return hdr.Hash, nil
	}
	if expr == "latest" {
		return int64(x.e.d.API.AlertLatest()), nil
	}
	parts := strings.Split(expr, ".")
	x.r.mu.Lock()
	val, ok := x.r.Vars[parts[0]]
	if !ok {
		val, ok = x.r.Params[parts[0]]
	}
	if !ok {
		if n, isRole := x.r.Roles[parts[0]]; isRole {
			val, ok = n, true
		}
	}
	x.r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown variable %q", parts[0])
	}
	for _, p := range parts[1:] {
		m, isMap := val.(map[string]any)
		if !isMap {
			return nil, fmt.Errorf("%q is not a map, cannot take .%s", parts[0], p)
		}
		val, ok = m[p]
		if !ok {
			return nil, fmt.Errorf("%q has no field %q", parts[0], p)
		}
	}
	return val, nil
}

func toInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int64:
		return t, true
	case uint32:
		return int64(t), true
	case uint64:
		return int64(t), true
	case float64:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// liveTip fetches a node's best header live (asset API on teranodes, RPC on SV nodes).
func (x *executor) liveTip(ref string) (*teranode.BlockHeader, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return x.e.d.Fleet.Tip(ctx, x.node(ref))
}

// node maps a role or node name to a node name.
func (x *executor) node(ref string) string {
	x.r.mu.Lock()
	defer x.r.mu.Unlock()
	if n, ok := x.r.Roles[ref]; ok {
		return n
	}
	return ref
}

func (x *executor) with(w map[string]any) (map[string]any, error) {
	rv, err := x.resolve(w)
	if err != nil {
		return nil, err
	}
	if rv == nil {
		return map[string]any{}, nil
	}
	return rv.(map[string]any), nil
}

func str(m map[string]any, k string) string {
	if v, ok := m[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

func num(m map[string]any, k string, def int64) int64 {
	if v, ok := m[k]; ok {
		if n, ok := toInt(v); ok {
			return n
		}
	}
	return def
}

func boolean(m map[string]any, k string) bool {
	v, ok := m[k]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	}
	return false
}

func nodes(x *executor, m map[string]any, k string) []string {
	var out []string
	switch t := m[k].(type) {
	case []any:
		for _, v := range t {
			out = append(out, x.node(fmt.Sprint(v)))
		}
	case string:
		for _, v := range strings.Split(t, ",") {
			if s := strings.TrimSpace(v); s != "" {
				out = append(out, x.node(s))
			}
		}
	}
	if len(out) == 0 {
		for _, n := range x.e.d.Inventory.Nodes {
			out = append(out, n.Name)
		}
	}
	return out
}

func (x *executor) set(name string, v any) {
	if name == "" {
		return
	}
	x.r.mu.Lock()
	x.r.Vars[name] = v
	x.r.mu.Unlock()
}

func (x *executor) finding(msg string) {
	x.r.mu.Lock()
	x.r.Findings = append(x.r.Findings, msg)
	x.r.mu.Unlock()
	x.e.publish(x.r, "FINDING: "+msg, map[string]any{"finding": true})
}

// ---- steps ------------------------------------------------------------------------------------

func (x *executor) runStep(ctx context.Context, st Step, sinceEvent uint64) StepResult {
	x.stepStart = time.Now()
	res := StepResult{Status: "passed"}
	w, err := x.with(st.With)
	if err != nil {
		res.Status, res.Error = "failed", "resolve: "+err.Error()
		return res
	}
	res.Resolved = w
	timeout := parseDuration(st.Timeout, 5*time.Minute)
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := x.action(sctx, st.Action, w)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res
	}
	res.Output = out
	x.set(st.As, out)
	for _, a := range st.Assert {
		ar := x.assert(ctx, a, sinceEvent)
		res.Assertions = append(res.Assertions, ar)
		if !ar.Passed {
			if a.Should {
				x.finding(fmt.Sprintf("step %q: %s (%s)", st.Name, ar.Check, ar.Detail))
			} else {
				res.Status, res.Error = "failed", fmt.Sprintf("assertion %s: %s", ar.Check, ar.Detail)
				return res
			}
		}
	}
	return res
}

func (x *executor) action(ctx context.Context, name string, w map[string]any) (any, error) {
	a := x.e.d.API
	switch name {
	case "note":
		x.e.publish(x.r, "note: "+str(w, "message"), nil)
		return str(w, "message"), nil
	case "mark":
		// Remember the current event id so later `event`/`no_event` checks can count from here
		// (with: { since: "${name.eventId}" }) instead of from their own step's start.
		var id uint64
		if recent := x.e.d.Bus.Recent(1); len(recent) > 0 {
			id = recent[0].ID
		}
		return map[string]any{"eventId": id, "at": time.Now().UTC().Format(time.RFC3339)}, nil
	case "set":
		for k, v := range w {
			x.set(k, v)
		}
		return w, nil
	case "wait":
		d := time.Duration(num(w, "seconds", 1)) * time.Second
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return d.String(), nil
	case "mine":
		node := x.node(str(w, "node"))
		blocks := int(num(w, "blocks", 1))
		var hashes []string
		var err error
		if addr := str(w, "address"); addr != "" {
			hashes, err = x.e.d.Fleet.RPC(node).GenerateToAddress(ctx, blocks, addr)
		} else {
			hashes, err = x.e.d.Fleet.RPC(node).Generate(ctx, blocks)
		}
		if err != nil {
			return nil, err
		}
		x.e.d.Bus.Publish("mine", node, fmt.Sprintf("%s mined %d block(s) [scenario]", node, len(hashes)), map[string]any{"hashes": hashes})
		last := ""
		if len(hashes) > 0 {
			last = hashes[len(hashes)-1]
		}
		return map[string]any{"node": node, "hashes": hashes, "count": len(hashes), "last": last}, nil
	case "coinbase":
		cb, err := a.Coinbase(ctx, x.node(str(w, "node")), uint32(num(w, "height", 1)))
		if err != nil {
			return nil, err
		}
		return map[string]any{"txid": cb.TxID, "hex": cb.Hex, "height": cb.Height, "block": cb.Hash}, nil
	case "newkey":
		e, err := a.NewKey(str(w, "name"), str(w, "note"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": e.Name, "address": e.Address, "lockingScript": e.LockingScript}, nil
	case "spend":
		req := api.SpendRequest{Node: x.node(str(w, "node")), TxID: str(w, "txid"), Vout: uint32(num(w, "vout", 0)), Key: str(w, "key"),
			To: str(w, "to"), ToScript: str(w, "toScript"), Satoshis: uint64(num(w, "satoshis", 0)), Fee: uint64(num(w, "fee", 0)), Outputs: int(num(w, "outputs", 1))}
		// `outs` spells the outputs out one by one, which is the only way to ask for a genuine
		// zero-satoshi output: the shorthand above splits one amount evenly and reads
		// satoshis 0 as "all minus fee". wallet.Spend still appends the change output.
		if raw, ok := w["outs"].([]any); ok {
			for _, it := range raw {
				om, _ := it.(map[string]any)
				if om == nil {
					continue
				}
				req.Outs = append(req.Outs, api.OutputSpec{Script: str(om, "script"), To: str(om, "to"), Satoshis: uint64(num(om, "satoshis", 0))})
			}
		}
		res, err := a.Spend(ctx, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"txid": res.TxID, "hex": res.Hex, "efHex": res.EFHex, "size": res.Size}, nil
	case "submit":
		target := str(w, "target")
		if target != "arcade" {
			target = x.node(target)
		}
		tx, _ := w["tx"].(map[string]any)
		hexs, ef := str(w, "hex"), str(w, "efHex")
		if tx != nil {
			hexs, ef = str(tx, "hex"), str(tx, "efHex")
		}
		res, err := a.Submit(ctx, target, hexs, ef)
		if err != nil {
			return nil, err
		}
		return map[string]any{"target": res.Target, "accepted": res.Accepted, "status": res.Status, "body": res.Body, "txid": res.TxID}, nil
	case "build_alert":
		req := api.BuildRequest{Type: str(w, "type"), Message: str(w, "message"), BlockHash: str(w, "blockHash"), Reason: str(w, "reason"),
			EnforceAt: uint64(num(w, "enforceAt", 0)), TxHex: str(w, "txHex"), Peer: str(w, "peer"), Note: str(w, "note"), Watch: true}
		if fl, ok := w["funds"].([]any); ok {
			for _, f := range fl {
				fm, _ := f.(map[string]any)
				req.Funds = append(req.Funds, alerts.Fund{TxID: str(fm, "txid"), Vout: uint32(num(fm, "vout", 0)),
					EnforceAtHeightStart: uint64(num(fm, "start", 0)), EnforceAtHeightStop: uint64(num(fm, "stop", 0)),
					PolicyExpiresWithConsensus: boolean(fm, "policyExpires")})
			}
		}
		e, err := a.BuildAlert(req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sequence": e.Sequence, "hash": e.Hash, "type": e.TypeName, "text": e.Text}, nil
	case "push_alert":
		node := x.node(str(w, "node"))
		res, err := a.PushAlert(ctx, node, uint32(num(w, "sequence", 0)))
		if err != nil {
			return nil, err
		}
		return map[string]any{"node": node, "delivered": res.Delivered}, nil
	case "rpc_freeze", "rpc_unfreeze":
		node := x.node(str(w, "node"))
		if name == "rpc_unfreeze" {
			return map[string]any{"ok": true}, x.e.d.Fleet.RPC(node).Unfreeze(ctx, str(w, "txid"), uint32(num(w, "vout", 0)))
		}
		var startP, stopP *uint64
		if _, ok := w["start"]; ok {
			s := uint64(num(w, "start", 0))
			startP = &s
		}
		if _, ok := w["stop"]; ok {
			s := uint64(num(w, "stop", 0))
			stopP = &s
		}
		var err error
		if sv := x.e.d.Fleet.SV(node); sv != nil {
			f := alerts.Fund{TxID: str(w, "txid"), Vout: uint32(num(w, "vout", 0)), PolicyExpiresWithConsensus: boolean(w, "policyExpires")}
			if startP != nil {
				f.EnforceAtHeightStart = *startP
			}
			if stopP != nil {
				f.EnforceAtHeightStop = *stopP
			}
			err = sv.AddToConsensusBlacklist(ctx, []alerts.Fund{f})
		} else {
			err = x.e.d.Fleet.RPC(node).Freeze(ctx, str(w, "txid"), uint32(num(w, "vout", 0)), startP, stopP, boolean(w, "policyExpires"))
		}
		if err == nil {
			x.e.d.Fleet.Watch(str(w, "txid"), uint32(num(w, "vout", 0)), "rpc freeze")
		}
		return map[string]any{"ok": err == nil}, err
	case "partition":
		plane, on := str(w, "plane"), boolean(w, "on")
		for _, node := range nodes(x, w, "nodes") {
			if err := a.Partition(ctx, node, plane, on); err != nil {
				return nil, err
			}
			x.trackPartition(node, plane, on)
		}
		return map[string]any{"nodes": nodes(x, w, "nodes"), "plane": plane, "on": on}, nil
	case "chaos":
		node := x.node(str(w, "node"))
		return map[string]any{"ok": true}, a.Chaos(ctx, node, str(w, "action"))
	case "automine":
		// Configures the orchestrator-wide mining cadence. A scenario that needs a static
		// chain turns it off here rather than fighting it.
		cfg := automine.Config{
			Enabled:  boolean(w, "enabled"),
			Interval: time.Duration(num(w, "intervalSeconds", 0)) * time.Second,
			Node:     x.node(str(w, "node")),
			Blocks:   int(num(w, "blocks", 0)),
		}
		if _, ok := w["enabled"]; !ok {
			cfg.Enabled = true
		}
		st := a.AutoMineConfigure(cfg)
		return map[string]any{"enabled": st.Enabled, "intervalSeconds": st.IntervalSeconds,
			"node": st.Node, "nextMineAt": st.NextMineAt, "blocksMined": st.BlocksMined}, nil

	case "wallet_state":
		st, err := a.WalletState(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"available": st.Available, "connected": st.Connected,
			"balance": st.Balance, "coins": st.Coins, "address": st.Address,
			"acceptRate": st.Health.AcceptRate, "decided": st.Health.Decided}, nil

	case "wallet_topup":
		req := walletsvc.TopUpRequest{
			Node: x.node(str(w, "node")), Satoshis: uint64(num(w, "satoshis", 0)),
			Count: int(num(w, "count", 0)), Fee: uint64(num(w, "fee", 0)),
			WaitSeconds: int(num(w, "waitSeconds", 0)),
		}
		if _, ok := w["mine"]; ok {
			m := boolean(w, "mine")
			req.Mine = &m
		}
		res, err := a.WalletTopUp(ctx, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"txid": res.TxID, "address": res.Address, "satoshis": res.Satoshis,
			"outputIndex": res.OutputIndex, "internalized": res.Internalized,
			"balance": res.Balance, "coins": res.Coins, "fanoutTxid": res.FanoutTxID}, nil

	case "wallet_tx":
		req := walletsvc.TxRequest{
			Shape: str(w, "shape"), Target: str(w, "target"),
			Satoshis: uint64(num(w, "satoshis", 0)), To: str(w, "to"),
			Outputs: int(num(w, "outputs", 0)), Data: str(w, "data"), DataHex: str(w, "dataHex"),
			Script: str(w, "script"), Label: str(w, "label"), Description: str(w, "description"),
			Delayed: boolean(w, "delayed"),
		}
		if req.Target != "" && req.Target != walletsvc.TargetArcade {
			req.Target = x.node(req.Target)
		}
		res, err := a.WalletTx(ctx, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"txid": res.TxID, "shape": res.Shape, "target": res.Target,
			"accepted": res.Accepted, "walletStatus": res.WalletStatus, "status": res.Status,
			"body": res.Body, "noSend": res.NoSend}, nil

	case "wallet_send_start":
		req := walletsvc.SendRequest{
			TPS: num2f(w, "tps", 1), Workers: int(num(w, "workers", 0)),
			Shape: str(w, "shape"), Satoshis: uint64(num(w, "satoshis", 0)),
			Outputs: int(num(w, "outputs", 0)), To: str(w, "to"), Data: str(w, "data"),
			Label: str(w, "label"), DurationSeconds: int(num(w, "durationSeconds", 0)),
			AutoMineSeconds: int(num(w, "autoMineSeconds", 0)),
			MineNode:        x.node(str(w, "mineNode")), MineBlocks: int(num(w, "mineBlocks", 0)),
			StartedBy: "scenario:" + x.r.ScenarioID,
		}
		st, err := a.WalletSendStart(ctx, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"running": st.Running, "tps": st.TPS, "workers": st.Workers,
			"labels": st.Labels, "startedAt": st.StartedAt}, nil

	case "wallet_send_stop":
		st, err := a.WalletSendStop(ctx, time.Duration(num(w, "timeoutSeconds", 20))*time.Second)
		if err != nil {
			return nil, err
		}
		return map[string]any{"running": st.Running, "draining": st.Draining,
			"attempted": st.Attempted, "succeeded": st.Succeeded, "failed": st.Failed,
			"backpressure": st.Backpressure, "canceled": st.Canceled,
			"measuredTps": st.MeasuredTPS, "elapsedSeconds": st.ElapsedSec}, nil

	case "watch":
		x.e.d.Fleet.Watch(str(w, "txid"), uint32(num(w, "vout", 0)), str(w, "label"))
		return map[string]any{"ok": true}, nil
	case "wait_for":
		ar := x.assert(ctx, Assertion{Check: str(w, "check"), With: mapOf(w["with"]), Timeout: str(w, "timeout")}, 0)
		if !ar.Passed {
			return nil, fmt.Errorf("%s: %s", ar.Check, ar.Detail)
		}
		return ar.Detail, nil
	}
	return nil, fmt.Errorf("unknown action %q", name)
}

func mapOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// ---- assertions -------------------------------------------------------------------------------

func (x *executor) assert(ctx context.Context, a Assertion, sinceEvent uint64) AssertResult {
	start := time.Now()
	res := AssertResult{Check: a.Check, Should: a.Should}
	w, err := x.with(a.With)
	if err != nil {
		res.Detail = "resolve: " + err.Error()
		res.Duration = time.Since(start).String()
		return res
	}
	timeout := parseDuration(a.Timeout, 30*time.Second)
	deadline := time.Now().Add(timeout)
	var detail string
	for {
		var ok bool
		ok, detail = x.check(ctx, a.Check, w, sinceEvent, timeout)
		if ok {
			res.Passed = true
			break
		}
		if a.Check == "no_event" || a.Check == "expr" || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(750 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	res.Detail = detail
	if a.Message != "" {
		res.Detail = a.Message + ": " + detail
	}
	res.Duration = time.Since(start).Round(time.Millisecond).String()
	return res
}

func (x *executor) check(ctx context.Context, name string, w map[string]any, sinceEvent uint64, window time.Duration) (bool, string) {
	f := x.e.d.Fleet
	switch name {
	case "same_tip":
		ns := nodes(x, w, "nodes")
		tips := map[string][]string{}
		for _, n := range ns {
			hdr, err := x.liveTip(n)
			if err != nil {
				return false, fmt.Sprintf("%s: %v", n, err)
			}
			tips[hdr.Hash] = append(tips[hdr.Hash], fmt.Sprintf("%s@%d", n, hdr.Height))
		}
		if len(tips) == 1 {
			for h := range tips {
				return true, "all on " + short(h)
			}
		}
		return false, fmt.Sprintf("tips differ: %v", tips)
	case "tip_is", "tip_not", "height":
		node := x.node(str(w, "node"))
		hdr, err := x.liveTip(node)
		if err != nil {
			return false, fmt.Sprintf("%s: %v", node, err)
		}
		switch name {
		case "tip_is":
			want := str(w, "hash")
			return hdr.Hash == want, fmt.Sprintf("%s tip %s@%d (want %s)", node, short(hdr.Hash), hdr.Height, short(want))
		case "tip_not":
			return hdr.Hash != str(w, "hash"), fmt.Sprintf("%s tip %s@%d", node, short(hdr.Hash), hdr.Height)
		default:
			want := num(w, "equals", -1)
			if want >= 0 {
				return int64(hdr.Height) == want, fmt.Sprintf("%s height %d (want %d)", node, hdr.Height, want)
			}
			min := num(w, "atLeast", 0)
			return int64(hdr.Height) >= min, fmt.Sprintf("%s height %d (want ≥ %d)", node, hdr.Height, min)
		}
	case "alert_seq":
		st, _ := f.Node(x.node(str(w, "node")))
		want := num(w, "equals", 0)
		return st.AlertSeq == want, fmt.Sprintf("%s alert seq %d (want %d)", st.Name, st.AlertSeq, want)
	case "utxo_status":
		node := x.node(str(w, "node"))
		txid, vout := str(w, "txid"), uint32(num(w, "vout", 0))
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		v, err := f.Refresh(cctx, node, txid, vout)
		cancel()
		if err != nil {
			return false, fmt.Sprintf("%s: %v", node, err)
		}
		return v.Status == str(w, "status"), fmt.Sprintf("%s sees %s:%d as %s (want %s)", node, short(txid), vout, v.Status, str(w, "status"))
	case "mempool_has", "mempool_lacks":
		node := x.node(str(w, "node"))
		ids, err := f.RPC(node).GetRawMempool(ctx)
		if err != nil {
			return false, err.Error()
		}
		found := false
		for _, id := range ids {
			if id == str(w, "txid") {
				found = true
			}
		}
		if name == "mempool_has" {
			return found, fmt.Sprintf("%s mempool has %s: %v", node, short(str(w, "txid")), found)
		}
		return !found, fmt.Sprintf("%s mempool has %s: %v", node, short(str(w, "txid")), found)
	case "event":
		if v, ok := w["since"]; ok {
			if id, isInt := toInt(v); isInt {
				sinceEvent = uint64(id)
			}
		}
		n := x.countEvents(w, sinceEvent)
		min := num(w, "min", 1)
		return int64(n) >= min, fmt.Sprintf("%d matching event(s) (want ≥ %d)", n, min)
	case "no_event":
		// Wait the whole window, then require fewer than max matching events.
		if v, ok := w["since"]; ok {
			if id, isInt := toInt(v); isInt {
				sinceEvent = uint64(id)
			}
		}
		select {
		case <-time.After(window):
		case <-ctx.Done():
		}
		n := x.countEvents(w, sinceEvent)
		max := num(w, "max", 0)
		return int64(n) <= max, fmt.Sprintf("%d matching event(s) in %s (allowed ≤ %d)", n, window, max)
	case "log_count":
		// Count container log lines of a node matching `pattern` (regexp) and/or `contains`,
		// written since `since` (RFC3339, e.g. "${mark.at}"; default: this step's start).
		// With `max`: wait the whole window, then require count <= max. Otherwise poll until
		// count >= `min` (default 1). Lets scenarios observe behaviour the node never
		// publishes (e.g. upstream main's block re-validation loop).
		node := x.node(str(w, "node"))
		since := x.stepStart
		if v := str(w, "since"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return false, "bad since: " + err.Error()
			}
			since = t
		}
		var re *regexp.Regexp
		if pat := str(w, "pattern"); pat != "" {
			var err error
			if re, err = regexp.Compile(pat); err != nil {
				return false, "bad pattern: " + err.Error()
			}
		}
		truncated := false
		count := func() (int, error) {
			// A generous ceiling: an assertion counting over a long window must not
			// silently undercount. Truncation is surfaced in the failure detail below.
			res, err := x.e.d.API.Logs(ctx, node, chaos.LogOptions{Since: since, MaxBytes: 32 << 20})
			if err != nil {
				return 0, err
			}
			truncated = res.Truncated
			n := 0
			for _, line := range strings.Split(res.Text, "\n") {
				if c := str(w, "contains"); c != "" && !strings.Contains(line, c) {
					continue
				}
				if re != nil && !re.MatchString(line) {
					continue
				}
				n++
			}
			return n, nil
		}
		if _, hasMax := w["max"]; hasMax {
			select {
			case <-time.After(window):
			case <-ctx.Done():
			}
			n, err := count()
			if err != nil {
				return false, err.Error()
			}
			max := num(w, "max", 0)
			return int64(n) <= max, fmt.Sprintf("%d matching log line(s) on %s in %s (allowed ≤ %d)%s", n, node, window, max, truncNote(truncated))
		}
		min := num(w, "min", 1)
		deadline := time.Now().Add(window)
		for {
			n, err := count()
			if err == nil && int64(n) >= min {
				return true, fmt.Sprintf("%d matching log line(s) on %s (want ≥ %d)%s", n, node, min, truncNote(truncated))
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				if err != nil {
					return false, err.Error()
				}
				return false, fmt.Sprintf("%d matching log line(s) on %s in %s (want ≥ %d)%s", n, node, window, min, truncNote(truncated))
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
			}
		}
	case "wallet_balance":
		st, err := x.e.d.API.WalletState(ctx)
		if err != nil {
			return false, err.Error()
		}
		if _, ok := w["atLeast"]; ok {
			want := uint64(num(w, "atLeast", 0))
			if st.Balance < want {
				return false, fmt.Sprintf("balance %d < %d", st.Balance, want)
			}
		}
		if _, ok := w["coinsAtLeast"]; ok {
			want := uint32(num(w, "coinsAtLeast", 0))
			if st.Coins < want {
				return false, fmt.Sprintf("%d spendable coin(s) < %d", st.Coins, want)
			}
		}
		return true, fmt.Sprintf("balance %d over %d coin(s)", st.Balance, st.Coins)

	case "wallet_tx_status":
		txid := str(w, "txid")
		row, err := x.e.d.API.WalletTxStatus(ctx, txid)
		if err != nil {
			return false, err.Error()
		}
		// Wallet and network truth are checked separately on purpose: an action the wallet
		// calls completed can still be one the network refused.
		if want := str(w, "wallet"); want != "" && row.WalletStatus != want {
			return false, fmt.Sprintf("wallet says %q, want %q", row.WalletStatus, want)
		}
		if want := str(w, "arcade"); want != "" && row.ArcadeStatus != want {
			return false, fmt.Sprintf("arcade says %q, want %q", row.ArcadeStatus, want)
		}
		return true, fmt.Sprintf("wallet=%s arcade=%s", row.WalletStatus, row.ArcadeStatus)

	case "arcade_block_status":
		// Arcade's own projection of the reorg stream. Polls, because the status converges
		// asynchronously after a reorg.
		hash := str(w, "hash")
		want := str(w, "status")
		row, err := x.e.d.API.ArcadeBlockStatus(ctx, hash)
		if err != nil {
			if errors.Is(err, arcade.ErrBlockNotFound) {
				return false, fmt.Sprintf("arcade has no processing status for %s", short(hash))
			}
			return false, err.Error()
		}
		detail := fmt.Sprintf("arcade says %s is %q at height %d", short(hash), row.Status, row.BlockHeight)
		if row.OrphanedAt != "" {
			detail += " (orphanedAt " + row.OrphanedAt + ")"
		}
		if row.ReconciledAt != "" {
			detail += " (reconciledAt " + row.ReconciledAt + ")"
		}
		if want == "" {
			return true, detail
		}
		return row.Status == want, detail + fmt.Sprintf(", want %q", want)

	case "arcade_canonical":
		// What arcade's embedded chaintracks believes is the active-chain block at a height.
		// Asserting this next to arcade_block_status is what turns a failure into "the instance
		// contradicts itself" rather than an ambiguous statement about the fleet.
		height := uint32(num(w, "height", 0))
		want := str(w, "hash")
		got, err := x.e.d.API.ArcadeCanonicalHashAt(ctx, height)
		if err != nil {
			return false, err.Error()
		}
		return got == want, fmt.Sprintf("arcade chaintracks has %s at height %d (want %s)",
			short(got), height, short(want))

	case "expr":
		l, r := str(w, "left"), str(w, "right")
		op := str(w, "op")
		if op == "" {
			op = "=="
		}
		switch op {
		case "==":
			return l == r, fmt.Sprintf("%q == %q", l, r)
		case "!=":
			return l != r, fmt.Sprintf("%q != %q", l, r)
		case "contains":
			return strings.Contains(l, r), fmt.Sprintf("%q contains %q", l, r)
		}
		return false, "unknown op " + op
	}
	return false, "unknown check " + name
}

// countEvents counts bus events since `since` matching kind, node, a literal `contains`
// and/or a regular expression `pattern` (Go syntax, e.g. "(?i)consensus[ _-]?frozen").
func (x *executor) countEvents(w map[string]any, since uint64) int {
	kind, node, contains, pattern := str(w, "kind"), str(w, "node"), str(w, "contains"), str(w, "pattern")
	if node != "" {
		node = x.node(node)
	}
	var re *regexp.Regexp
	if pattern != "" {
		if compiled, err := regexp.Compile(pattern); err == nil {
			re = compiled
		}
	}
	n := 0
	for _, e := range x.e.d.Bus.Since(since) {
		if kind != "" && e.Kind != kind {
			continue
		}
		if node != "" && e.Node != node {
			continue
		}
		if contains != "" || re != nil {
			blob := e.Message
			for _, v := range e.Data {
				blob += " " + fmt.Sprint(v)
			}
			if contains != "" && !strings.Contains(blob, contains) {
				continue
			}
			if re != nil && !re.MatchString(blob) {
				continue
			}
		}
		n++
	}
	return n
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

var errNotImplemented = errors.New("not implemented")

// trackPartition records open partitions on the run so cleanup can heal them.
func (x *executor) trackPartition(node, plane string, on bool) {
	x.r.mu.Lock()
	defer x.r.mu.Unlock()
	planes := x.r.OpenPartitions[node]
	var kept []string
	for _, p := range planes {
		if p != plane {
			kept = append(kept, p)
		}
	}
	if on {
		kept = append(kept, plane)
	}
	if len(kept) == 0 {
		delete(x.r.OpenPartitions, node)
	} else {
		x.r.OpenPartitions[node] = kept
	}
}

// truncNote marks a count taken from a log window that hit the read ceiling, so a capped
// read is never mistaken for an exhaustive count.
func truncNote(truncated bool) string {
	if truncated {
		return " (log window truncated; count is a lower bound)"
	}
	return ""
}

// num2f reads a fractional value, so a rate like 0.2 tx/s survives the YAML round trip —
// num() truncates to an integer, which would silently turn 0.5 tx/s into 0.
func num2f(w map[string]any, key string, def float64) float64 {
	v, ok := w[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f
		}
	}
	return def
}
