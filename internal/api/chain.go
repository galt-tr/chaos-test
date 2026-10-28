package api

import (
	"context"
	"fmt"
	"time"

	"github.com/bsv-blockchain/bsv-regtest/wallet"
)

// ChainRequest builds a chain of Depth transactions, each spending output 0 of the one before,
// from one funded outpoint, and (when Target is set) submits them in order as fast as the
// target answers, without waiting for any of them to be accepted. This is the shape a wallet
// produces when it pays out of its own unconfirmed change, and the shape that exercises a
// broadcaster's ordering: every hop but the first is a child of a transaction the network may
// not have yet.
type ChainRequest struct {
	Node   string     `json:"node"`   // node to fetch the funding transaction from (default: first)
	TxID   string     `json:"txid"`   // funding outpoint
	Vout   uint32     `json:"vout"`   //
	Key    string     `json:"key"`    // key name | hex | WIF unlocking the funding output; every hop pays back to it
	Depth  int        `json:"depth"`  // number of transactions to build (default 10)
	Fee    uint64     `json:"fee"`    // absolute fee per hop (default 500)
	Target string     `json:"target"` // "arcade" or a node name; "" builds without submitting
	Opts   SubmitOpts `json:"opts"`   // arcade subscription headers
}

// ChainHop is one built transaction of the chain, in the shape `submit` takes as `tx:`.
type ChainHop struct {
	TxID  string `json:"txid"`
	Hex   string `json:"hex"`
	EFHex string `json:"efHex"`
}

// ChainResult is the built (and possibly submitted) chain.
type ChainResult struct {
	TxIDs     []string   `json:"txids"`
	First     string     `json:"first"`
	Last      string     `json:"last"`
	Hops      []ChainHop `json:"hops"`
	Submitted int        `json:"submitted"` // submits attempted
	Accepted  int        `json:"accepted"`  // 2xx answers
	Rejected  []string   `json:"rejected"`  // "<txid>: <status> <body>" for every non-2xx
	ElapsedMs int64      `json:"elapsedMs"` // wall-clock for the submits
}

// Chain builds and optionally submits a dependent chain (shared with the scenario engine).
func (s *Server) Chain(ctx context.Context, req ChainRequest) (*ChainResult, error) {
	if req.Depth <= 0 {
		req.Depth = 10
	}
	if req.Fee == 0 {
		req.Fee = 500
	}
	if req.Node == "" {
		req.Node = s.defaultNode()
	}
	n, err := s.node(req.Node)
	if err != nil {
		return nil, err
	}
	key, err := s.d.Keys.Resolve(req.Key)
	if err != nil {
		return nil, err
	}
	srcHex, err := s.txHex(ctx, n, req.TxID)
	if err != nil {
		return nil, err
	}
	src, err := wallet.ParseTx(srcHex)
	if err != nil {
		return nil, fmt.Errorf("parse funding tx: %w", err)
	}
	vout := req.Vout
	if int(vout) >= len(src.Outputs) {
		return nil, fmt.Errorf("vout %d out of range", vout)
	}
	res := &ChainResult{}
	for i := 0; i < req.Depth; i++ {
		value := src.Outputs[vout].Satoshis
		if value <= req.Fee {
			return nil, fmt.Errorf("hop %d: output of %d sat cannot pay a %d sat fee", i, value, req.Fee)
		}
		// Exactly one output, worth everything minus the fee, so wallet.Spend adds no change and
		// the next hop always spends vout 0.
		tx, err := wallet.Spend([]wallet.Input{{SourceTx: src, Vout: vout, Key: key}},
			[]wallet.Output{{Key: key, Satoshis: value - req.Fee}}, req.Fee)
		if err != nil {
			return nil, fmt.Errorf("hop %d: %w", i, err)
		}
		ef, err := tx.EFHex()
		if err != nil {
			return nil, fmt.Errorf("hop %d: ef: %w", i, err)
		}
		hop := ChainHop{TxID: tx.TxID().String(), Hex: tx.Hex(), EFHex: ef}
		res.Hops = append(res.Hops, hop)
		res.TxIDs = append(res.TxIDs, hop.TxID)
		src, vout = tx, 0
	}
	res.First, res.Last = res.TxIDs[0], res.TxIDs[len(res.TxIDs)-1]
	if req.Target == "" {
		return res, nil
	}
	start := time.Now()
	for _, hop := range res.Hops {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Submitted++
		sr, err := s.Submit(ctx, req.Target, hop.Hex, hop.EFHex, req.Opts)
		if err != nil {
			res.Rejected = append(res.Rejected, fmt.Sprintf("%s: %v", hop.TxID, err))
			continue
		}
		if sr.Accepted {
			res.Accepted++
		} else {
			res.Rejected = append(res.Rejected, fmt.Sprintf("%s: %d %s", hop.TxID, sr.Status, short(sr.Body)))
		}
	}
	res.ElapsedMs = time.Since(start).Milliseconds()
	return res, nil
}
