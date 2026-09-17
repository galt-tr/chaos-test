package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/arcade"
	"github.com/bsv-blockchain/chaos-test/internal/walletsvc"
)

// ---- plumbing the send controller needs ---------------------------------------------------

// SendOne builds and broadcasts one transaction. It satisfies walletsvc.TxSender.
func (s *Server) SendOne(ctx context.Context, req walletsvc.TxRequest) (*walletsvc.TxResult, error) {
	return s.WalletTx(ctx, req)
}

// Mine satisfies walletsvc.Miner.
func (s *Server) Mine(ctx context.Context, node string, blocks int) ([]string, error) {
	n, err := s.node(node)
	if err != nil {
		return nil, err
	}
	return s.d.Fleet.RPC(n.Name).Generate(ctx, blocks)
}

// TipHeight satisfies walletsvc.Miner. The send's auto-miner uses it to skip its turn when
// something else already produced a block.
func (s *Server) TipHeight(ctx context.Context, node string) (uint32, error) {
	n, err := s.node(node)
	if err != nil {
		return 0, err
	}
	hdr, err := s.d.Fleet.Tip(ctx, n.Name)
	if err != nil {
		return 0, err
	}
	return hdr.Height, nil
}

// ---- exported capabilities (HTTP + scenario engine share these) ---------------------------

func (s *Server) walletOK() error {
	if s.d.Wallet == nil || !s.d.Wallet.Enabled() {
		return &walletsvc.Error{Reason: walletsvc.ReasonDisabled, Msg: "the wallet feature is not enabled"}
	}
	return nil
}

// WalletState returns wallet state, funding position and recent transactions.
func (s *Server) WalletState(ctx context.Context) (walletsvc.State, error) {
	if err := s.walletOK(); err != nil {
		return walletsvc.State{Reason: walletsvc.ReasonDisabled, Error: err.Error()}, nil
	}
	return s.d.Wallet.State(ctx), nil
}

// WalletDeposit returns the BRC-29 address that funds this wallet.
func (s *Server) WalletDeposit(ctx context.Context) (*walletsvc.Deposit, error) {
	if err := s.walletOK(); err != nil {
		return nil, err
	}
	return s.d.Wallet.Client().Deposit(ctx)
}

// WalletTx builds one transaction and broadcasts it.
//
// Arcade is the default and needs nothing from us: the wallet's storage server is configured to
// post there itself. A node target is the legacy path — the transaction is built with NoSend so
// we can hand the node standard raw hex over sendrawtransaction, which is what it expects
// (arcade wants extended format instead).
func (s *Server) WalletTx(ctx context.Context, req walletsvc.TxRequest) (*walletsvc.TxResult, error) {
	if err := s.walletOK(); err != nil {
		return nil, err
	}
	target := strings.TrimSpace(req.Target)
	if target == "" {
		target = walletsvc.TargetArcade
	}
	legacy := target != walletsvc.TargetArcade
	if legacy {
		n, err := s.node(target)
		if err != nil {
			return nil, &walletsvc.Error{Reason: walletsvc.ReasonUnknownNode, Msg: err.Error()}
		}
		target = n.Name
	}

	labels := []string{}
	if req.Label != "" {
		labels = append(labels, req.Label)
	}
	built, err := s.d.Wallet.Client().BuildTx(ctx, walletsvc.WalletdTxRequest{
		Shape: req.Shape, Satoshis: req.Satoshis, To: req.To, Outputs: req.Outputs,
		Data: req.Data, DataHex: req.DataHex, Script: req.Script,
		Labels: labels, Description: req.Description,
		NoSend: legacy, Delayed: req.Delayed,
	})
	if err != nil {
		return nil, err
	}

	out := &walletsvc.TxResult{
		TxID: built.TxID, Shape: req.Shape, Target: target,
		WalletStatus: built.Status, Satoshis: built.Satoshis, NoSend: legacy,
		RawHex: built.RawHex,
	}
	if !legacy {
		// The storage server broadcast it to arcade already; the wallet status is the answer.
		out.Accepted = built.Status != "failed"
		s.d.Wallet.Tracker().Note(out.TxID, req.Shape, target, built.Satoshis, built.Status)
		return out, nil
	}

	// Legacy: deliver the raw hex ourselves. Note the change this leaves behind is parked
	// rather than spendable, which is why the sustained send refuses this path.
	res, serr := s.Submit(ctx, target, built.RawHex, "")
	if serr != nil {
		out.Body = serr.Error()
		s.d.Wallet.Tracker().Note(out.TxID, req.Shape, target, built.Satoshis, built.Status)
		return out, nil
	}
	out.Accepted, out.Status, out.Body = res.Accepted, res.Status, res.Body
	if res.TxID != "" {
		out.TxID = res.TxID
	}
	s.d.Wallet.Tracker().Note(out.TxID, req.Shape, target, built.Satoshis, built.Status)
	return out, nil
}

// WalletTxStatus returns wallet truth and network truth for one transaction, freshly fetched.
func (s *Server) WalletTxStatus(ctx context.Context, txid string) (*walletsvc.TxRow, error) {
	if err := s.walletOK(); err != nil {
		return nil, err
	}
	row, known := s.d.Wallet.Tracker().Row(txid)
	if !known {
		row = walletsvc.TxRow{TxID: txid}
	}
	if s.d.ArcadeClient != nil {
		rec, err := s.d.ArcadeClient.GetTx(ctx, txid)
		switch {
		case errors.Is(err, arcade.ErrTxNotFound):
			row.ArcadeStatus = arcade.StatusNotFound
		case err == nil && rec != nil:
			row.ArcadeStatus, row.BlockHeight = rec.Status, rec.BlockHeight
			row.ExtraInfo, row.CompetingTxs = rec.ExtraInfo, rec.CompetingTxs
			row.Terminal = arcade.IsSettled(rec.Status)
		}
		row.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if !known && row.ArcadeStatus == "" {
		return nil, &walletsvc.Error{Reason: walletsvc.ReasonTxNotFound,
			Msg: "neither the wallet nor arcade has a record of " + txid}
	}
	return &row, nil
}

// WalletSendStart begins the sustained send.
func (s *Server) WalletSendStart(ctx context.Context, req walletsvc.SendRequest) (walletsvc.SendStatus, error) {
	if err := s.walletOK(); err != nil {
		return walletsvc.SendStatus{}, err
	}
	if req.MineNode == "" {
		req.MineNode = s.d.Wallet.DefaultMineNode()
	}
	if req.MineNode != "" && req.AutoMineSeconds >= 0 {
		n, err := s.node(req.MineNode)
		if err != nil {
			return walletsvc.SendStatus{}, &walletsvc.Error{Reason: walletsvc.ReasonUnknownNode, Msg: err.Error()}
		}
		req.MineNode = n.Name
	}
	return s.d.Wallet.Send().Start(s.sendCtx(), req)
}

// WalletSendStop halts the send, draining in-flight work.
func (s *Server) WalletSendStop(_ context.Context, wait time.Duration) (walletsvc.SendStatus, error) {
	if err := s.walletOK(); err != nil {
		return walletsvc.SendStatus{}, err
	}
	if wait <= 0 {
		wait = 15 * time.Second
	}
	return s.d.Wallet.Send().Stop(wait), nil
}

// WalletSendStatus is a snapshot of the send.
func (s *Server) WalletSendStatus() walletsvc.SendStatus {
	if s.d.Wallet == nil || s.d.Wallet.Send() == nil {
		return walletsvc.SendStatus{}
	}
	return s.d.Wallet.Send().Status()
}

// sendCtx is the lifetime of a send: the orchestrator's, not one HTTP request's. A send has to
// outlive the call that started it and the browser tab that made the call.
func (s *Server) sendCtx() context.Context {
	if s.d.BaseCtx != nil {
		return s.d.BaseCtx
	}
	return context.Background()
}

// ---- top up -------------------------------------------------------------------------------

// WalletTopUp funds the wallet from a regtest coinbase.
//
// It has to mine. InternalizeAction only accepts atomic BEEF whose merkle root the wallet's
// chain tracker can verify, so the funding transaction must be in a block and proven before it
// can be credited — raw hex is rejected outright. Every stage is recorded in Steps so a failure
// says where it broke rather than just that it broke.
func (s *Server) WalletTopUp(ctx context.Context, req walletsvc.TopUpRequest) (*walletsvc.TopUpResult, error) {
	if err := s.walletOK(); err != nil {
		return nil, err
	}
	if s.d.ArcadeClient == nil {
		return nil, &walletsvc.Error{Reason: walletsvc.ReasonArcade,
			Msg: "arcade is required to prove the funding transaction before it can be internalized"}
	}
	mine := req.Mine == nil || *req.Mine
	if req.Satoshis == 0 {
		req.Satoshis = 100_000
	}
	if req.Fee == 0 {
		req.Fee = 500
	}
	if req.WaitSeconds <= 0 {
		req.WaitSeconds = 120
	} else if req.WaitSeconds > 600 {
		req.WaitSeconds = 600
	}
	node := req.Node
	if node == "" {
		node = s.d.Wallet.DefaultMineNode()
	}
	n, err := s.node(node)
	if err != nil {
		return nil, &walletsvc.Error{Reason: walletsvc.ReasonUnknownNode, Msg: err.Error()}
	}
	if s.d.Fleet.IsSV(n.Name) {
		return nil, &walletsvc.Error{Reason: walletsvc.ReasonBadRequest,
			Msg: "top up needs a teranode to source a coinbase from; SV nodes follow the teranodes"}
	}
	out := &walletsvc.TopUpResult{}
	step := func(name string, start time.Time, err error, detail string) {
		st := walletsvc.Step{Name: name, OK: err == nil, Detail: detail,
			Elapsed: time.Since(start).Round(time.Millisecond).String()}
		if err != nil {
			st.Detail = err.Error()
		}
		out.Steps = append(out.Steps, st)
		s.d.Bus.Publish("wallet", n.Name, "top up: "+name+ok(err), map[string]any{"step": name, "ok": err == nil})
	}

	// 1. where to pay
	t0 := time.Now()
	dep, err := s.d.Wallet.Client().Deposit(ctx)
	step("read deposit address", t0, err, "")
	if err != nil {
		return out, err
	}
	out.Address, out.Satoshis = dep.Address, req.Satoshis

	// 2. a mature, unspent coinbase
	t0 = time.Now()
	cb, height, err := s.findSpendableCoinbase(ctx, n.Name, mine)
	step("find a spendable coinbase", t0, err, fmt.Sprintf("height %d", height))
	if err != nil {
		return out, err
	}
	out.CoinbaseTxID, out.CoinbaseHeight = cb, height

	// 3. build the funding transaction with the raw signer
	t0 = time.Now()
	spend, err := s.Spend(ctx, SpendRequest{
		Node: n.Name, TxID: cb, Vout: 0, Key: "miner",
		ToScript: dep.LockingScriptHex, Satoshis: req.Satoshis, Outputs: 1, Fee: req.Fee,
	})
	step("build the funding transaction", t0, err, "")
	if err != nil {
		return out, err
	}
	out.TxID = spend.TxID

	// 4. broadcast it through arcade
	t0 = time.Now()
	sub, err := s.Submit(ctx, walletsvc.TargetArcade, spend.Hex, spend.EFHex)
	if err == nil && !sub.Accepted {
		err = fmt.Errorf("arcade rejected the funding transaction: HTTP %d %s", sub.Status, sub.Body)
	}
	step("broadcast the funding transaction", t0, err, "")
	if err != nil {
		return out, &walletsvc.Error{Reason: walletsvc.ReasonFundingReject, Msg: err.Error()}
	}

	// 5. let it reach a mempool before sealing a block.
	//
	// Arcade answers the submit as soon as it has accepted it, well before the transaction has
	// propagated to a node. Mining immediately produces an empty block and the funding sits
	// unconfirmed — which is exactly what happened the first time this ran.
	if mine {
		t0 = time.Now()
		err = s.waitForMempool(ctx, n.Name, out.TxID, 30*time.Second)
		step("wait for it to reach a mempool", t0, err, "")
	}

	// 6-9. mine until it confirms, wait for the proof, then credit it. Mining is interleaved
	// with the wait because on regtest nothing else produces blocks, so a transaction that
	// missed one block would otherwise never confirm.
	miner := func() {}
	if mine {
		miner = func() { _, _ = s.d.Fleet.RPC(n.Name).Generate(ctx, 1) }
	}
	if err := s.internalizeFunding(ctx, out, spend.Hex, dep.Address, req.WaitSeconds, step, miner); err != nil {
		return out, err
	}

	// optional: split into several coins, because a send is limited by spendable outputs
	if req.Count > 1 {
		t0 = time.Now()
		per := req.Satoshis / uint64(req.Count+1)
		fan, ferr := s.WalletTx(ctx, walletsvc.TxRequest{
			Shape: walletsvc.ShapeFanout, Outputs: req.Count, Satoshis: per,
			Label: "topup-split", Description: "split the top up into spendable coins",
		})
		step(fmt.Sprintf("split into %d coins", req.Count), t0, ferr, "")
		if ferr == nil && fan != nil {
			out.FanoutTxID = fan.TxID
		}
	}

	if st, err := s.d.Wallet.Client().State(ctx); err == nil {
		out.Balance, out.Coins = st.Balance, st.Coins
	}
	return out, nil
}

// WalletInternalize finishes a top up whose proof arrived late. It is the documented recovery
// from a proof timeout, which is why the timeout returns the txid.
func (s *Server) WalletInternalize(ctx context.Context, txid string, vout uint32, wait int) (*walletsvc.TopUpResult, error) {
	if err := s.walletOK(); err != nil {
		return nil, err
	}
	if wait <= 0 {
		wait = 120
	}
	dep, err := s.d.Wallet.Client().Deposit(ctx)
	if err != nil {
		return nil, err
	}
	src, err := s.node(s.defaultNode())
	if err != nil {
		return nil, &walletsvc.Error{Reason: walletsvc.ReasonUnknownNode, Msg: err.Error()}
	}
	raw, err := s.txHex(ctx, src, txid)
	if err != nil {
		return nil, &walletsvc.Error{Reason: walletsvc.ReasonTxNotFound, Msg: err.Error()}
	}
	out := &walletsvc.TopUpResult{TxID: txid, Address: dep.Address, OutputIndex: vout}
	step := func(name string, start time.Time, err error, detail string) {
		st := walletsvc.Step{Name: name, OK: err == nil, Detail: detail,
			Elapsed: time.Since(start).Round(time.Millisecond).String()}
		if err != nil {
			st.Detail = err.Error()
		}
		out.Steps = append(out.Steps, st)
	}
	// The recovery path deliberately does not mine: it exists to finish a top up whose proof
	// arrived late, and silently advancing the chain would be a surprise.
	if err := s.internalizeFunding(ctx, out, raw, dep.Address, wait, step, nil); err != nil {
		return out, err
	}
	if st, err := s.d.Wallet.Client().State(ctx); err == nil {
		out.Balance, out.Coins = st.Balance, st.Coins
	}
	return out, nil
}

// internalizeFunding waits for arcade to publish a merkle proof, then credits the payment.
func (s *Server) internalizeFunding(ctx context.Context, out *walletsvc.TopUpResult, rawHex, address string,
	waitSeconds int, step func(string, time.Time, error, string), mine func()) error {

	t0 := time.Now()
	bump, height, err := s.waitForProof(ctx, out.TxID, time.Duration(waitSeconds)*time.Second, mine)
	step("wait for the merkle proof", t0, err, fmt.Sprintf("block %d", height))
	if err != nil {
		// The txid is carried back so the caller can retry just the credit once the proof
		// lands, instead of funding all over again.
		return &walletsvc.Error{Reason: walletsvc.ReasonProofTimeout,
			Msg: fmt.Sprintf("no merkle proof for %s within %ds; retry the internalize step once it is mined", out.TxID, waitSeconds)}
	}

	t0 = time.Now()
	_, beefHex, err := walletsvc.BuildAtomicBEEF(rawHex, bump)
	step("assemble atomic BEEF", t0, err, "")
	if err != nil {
		return &walletsvc.Error{Reason: walletsvc.ReasonInternalize, Msg: err.Error()}
	}

	t0 = time.Now()
	res, err := s.d.Wallet.Client().Internalize(ctx, walletsvc.InternalizeRequest{
		AtomicBeefHex: beefHex, ExpectedAddress: address, OutputIndex: out.OutputIndex,
		Description: "chaos-test top up",
	})
	step("credit it to the wallet", t0, err, "")
	if err != nil {
		return &walletsvc.Error{Reason: walletsvc.ReasonInternalize, Msg: err.Error()}
	}
	out.Internalized, out.OutputIndex = res.Accepted, res.OutputIndex
	out.Balance, out.Coins = res.Balance, res.Coins
	return nil
}

// waitForProof polls arcade until it publishes a merkle path, mining periodically while it
// waits.
//
// The mining is not incidental. On regtest no one else produces blocks, so a transaction that
// arrived a moment after the last block would sit unconfirmed forever and the proof would never
// exist. Re-mining every few polls also covers the case where the first block was sealed before
// the transaction had propagated.
func (s *Server) waitForProof(ctx context.Context, txid string, within time.Duration, mine func()) (string, uint64, error) {
	deadline := time.Now().Add(within)
	for i := 0; ; i++ {
		rec, err := s.d.ArcadeClient.GetTx(ctx, txid)
		if err == nil && rec.MerklePath != "" {
			return rec.MerklePath, rec.BlockHeight, nil
		}
		if time.Now().After(deadline) {
			return "", 0, fmt.Errorf("timed out after %s", within)
		}
		// Every ~10s: nudge the chain forward.
		if mine != nil && i > 0 && i%5 == 0 {
			mine()
		}
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitForMempool waits until a node has the transaction in its mempool.
func (s *Server) waitForMempool(ctx context.Context, node, txid string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		mp, err := s.d.Fleet.RPC(node).GetRawMempool(ctx)
		if err == nil {
			for _, id := range mp {
				if id == txid {
					return nil
				}
			}
		}
		// Arcade seeing it on the network is just as good: it means a node accepted it.
		if rec, err := s.d.ArcadeClient.GetTx(ctx, txid); err == nil {
			switch rec.Status {
			case arcade.StatusSeenOnNetwork, arcade.StatusSeenMultipleNodes,
				arcade.StatusAcceptedByNetwork, arcade.StatusMined, arcade.StatusImmutable:
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s never appeared in %s's mempool within %s", txid, node, within)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// findSpendableCoinbase walks back from maturity looking for a coinbase nobody has spent.
//
// Walking matters: a second top up cannot reuse the first one's coinbase, and silently
// re-spending it would produce a confusing double-spend rather than a clear error.
func (s *Server) findSpendableCoinbase(ctx context.Context, node string, mine bool) (string, uint32, error) {
	hdr, err := s.d.Fleet.Tip(ctx, node)
	if err != nil {
		return "", 0, err
	}
	tip := hdr.Height
	if tip < 101 {
		if !mine {
			return "", 0, &walletsvc.Error{Reason: walletsvc.ReasonImmatureChain,
				Msg: fmt.Sprintf("chain is at height %d; coinbase maturity needs 101 blocks", tip)}
		}
		if _, err := s.d.Fleet.RPC(node).Generate(ctx, int(101-tip)+1); err != nil {
			return "", 0, err
		}
		if hdr, err = s.d.Fleet.Tip(ctx, node); err != nil {
			return "", 0, err
		}
		tip = hdr.Height
	}
	for h := int(tip) - 100; h > 0 && h > int(tip)-125; h-- {
		cb, err := s.Coinbase(ctx, node, uint32(h))
		if err != nil {
			continue
		}
		view, err := s.d.Fleet.Refresh(ctx, node, cb.TxID, 0)
		if err != nil || view.Status == "OK" {
			return cb.TxID, uint32(h), nil
		}
	}
	return "", 0, &walletsvc.Error{Reason: walletsvc.ReasonNoCoinbase,
		Msg: "no unspent mature coinbase in the last 25 eligible blocks — mine more blocks and retry"}
}

func ok(err error) string {
	if err == nil {
		return " ok"
	}
	return " failed"
}

// ---- HTTP ---------------------------------------------------------------------------------

func (s *Server) walletRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/wallet/state", func(w http.ResponseWriter, r *http.Request) {
		st, _ := s.WalletState(r.Context())
		writeJSON(w, 200, st)
	})
	m.HandleFunc("GET /api/wallet/deposit", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.WalletDeposit(r.Context())
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSON(w, 200, d)
	})
	m.HandleFunc("GET /api/wallet/send", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, s.WalletSendStatus())
	})
	m.HandleFunc("POST /api/wallet/send/start", func(w http.ResponseWriter, r *http.Request) {
		var req walletsvc.SendRequest
		if err := decode(r, &req); err != nil {
			writeErrReason(w, 400, walletsvc.ReasonBadRequest, err, nil)
			return
		}
		if req.StartedBy == "" {
			req.StartedBy = "ui"
		}
		st, err := s.WalletSendStart(r.Context(), req)
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSON(w, 200, st)
	})
	m.HandleFunc("POST /api/wallet/send/stop", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.WalletSendStop(r.Context(), 15*time.Second)
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSON(w, 200, st)
	})
	m.HandleFunc("POST /api/wallet/tx", func(w http.ResponseWriter, r *http.Request) {
		var req walletsvc.TxRequest
		if err := decode(r, &req); err != nil {
			writeErrReason(w, 400, walletsvc.ReasonBadRequest, err, nil)
			return
		}
		res, err := s.WalletTx(r.Context(), req)
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSON(w, 200, res)
	})
	m.HandleFunc("GET /api/wallet/tx/{txid}", func(w http.ResponseWriter, r *http.Request) {
		row, err := s.WalletTxStatus(r.Context(), r.PathValue("txid"))
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSON(w, 200, row)
	})
	m.HandleFunc("POST /api/wallet/topup", func(w http.ResponseWriter, r *http.Request) {
		var req walletsvc.TopUpRequest
		if err := decode(r, &req); err != nil {
			writeErrReason(w, 400, walletsvc.ReasonBadRequest, err, nil)
			return
		}
		res, err := s.WalletTopUp(r.Context(), req)
		if err != nil {
			writeWalletErrBody(w, err, res)
			return
		}
		writeJSON(w, 200, res)
	})
	m.HandleFunc("POST /api/wallet/topup/internalize", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TxID        string `json:"txid"`
			OutputIndex uint32 `json:"outputIndex"`
			WaitSeconds int    `json:"waitSeconds"`
		}
		if err := decode(r, &req); err != nil {
			writeErrReason(w, 400, walletsvc.ReasonBadRequest, err, nil)
			return
		}
		res, err := s.WalletInternalize(r.Context(), req.TxID, req.OutputIndex, req.WaitSeconds)
		if err != nil {
			writeWalletErrBody(w, err, res)
			return
		}
		writeJSON(w, 200, res)
	})
}

// walletStatusFor maps a reason to an HTTP status.
func walletStatusFor(reason string) int {
	switch reason {
	case walletsvc.ReasonBadRequest:
		return http.StatusBadRequest
	case walletsvc.ReasonLegacyInSend:
		return http.StatusBadRequest
	case walletsvc.ReasonUnknownNode, walletsvc.ReasonTxNotFound:
		return http.StatusNotFound
	case walletsvc.ReasonInsufficient, walletsvc.ReasonNoCoinbase, walletsvc.ReasonImmatureChain,
		walletsvc.ReasonAlreadyRunning, walletsvc.ReasonDraining:
		return http.StatusConflict
	case walletsvc.ReasonDisabled, walletsvc.ReasonUnavailable, walletsvc.ReasonNotConnected, walletsvc.ReasonArcade:
		return http.StatusServiceUnavailable
	case walletsvc.ReasonProofTimeout, walletsvc.ReasonTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func writeWalletErr(w http.ResponseWriter, err error) { writeWalletErrBody(w, err, nil) }

// writeWalletErrBody returns the error plus whatever partial result exists, so a top up that
// failed halfway still shows which steps ran.
func writeWalletErrBody(w http.ResponseWriter, err error, partial any) {
	reason := walletsvc.ReasonOf(err)
	if reason == "" {
		reason = walletsvc.ReasonUpstream
	}
	extra := map[string]any{}
	if partial != nil {
		extra["result"] = partial
	}
	writeErrReason(w, walletStatusFor(reason), reason, err, extra)
}
