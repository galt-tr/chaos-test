package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"
)

// Every action this wallet creates carries labelWallet, so wallet-side truth can be queried
// back with ListActions. Sustained-send actions add labelSend and a per-run label on top.
const (
	labelWallet = "bsv-regtest-wallet"
	labelSend   = "bsv-regtest-wallet-send"
)

// Transaction shapes the harness can build.
const (
	ShapePayment  = "payment"  // one P2PKH output
	ShapeOpReturn = "opreturn" // one zero-satoshi data output
	ShapeFanout   = "fanout"   // N equal outputs — also the wide-and-shallow coin splitter
	ShapeCustom   = "custom"   // a raw locking script, passed through unvalidated
)

// TxRequest is one transaction to build.
type TxRequest struct {
	Shape       string   `json:"shape"`
	Satoshis    uint64   `json:"satoshis"`
	To          string   `json:"to"`      // address, or a locking-script hex
	Outputs     int      `json:"outputs"` // fanout
	Data        string   `json:"data"`    // opreturn, utf8
	DataHex     string   `json:"dataHex"` // opreturn, hex
	Script      string   `json:"script"`  // custom, locking-script hex
	Labels      []string `json:"labels"`
	Description string   `json:"description"`
	// NoSend builds and signs without broadcasting. The legacy path uses it so the caller can
	// hand the raw hex to an SV node. Note the change it produces is PARKED, not spendable.
	NoSend  bool `json:"noSend"`
	Delayed bool `json:"delayed"`
}

// TxResult is the built transaction.
type TxResult struct {
	TxID     string   `json:"txid"`
	Status   string   `json:"status"`
	Satoshis uint64   `json:"satoshis"`
	RawHex   string   `json:"rawHex,omitempty"`
	EFHex    string   `json:"efHex,omitempty"`
	Labels   []string `json:"labels"`
	NoSend   bool     `json:"noSend"`
}

// BuildTx creates, signs and (unless NoSend) broadcasts one transaction.
//
// Broadcasting is the storage server's job and it is configured to post to arcade, so the
// default path needs nothing from us. The legacy path sets NoSend and lets the caller deliver
// the raw hex wherever it likes.
func (w *Wallet) BuildTx(ctx context.Context, req TxRequest) (*TxResult, error) {
	h, err := w.Handle()
	if err != nil {
		return nil, err
	}
	outs, total, err := w.outputsFor(req)
	if err != nil {
		return nil, err
	}
	labels := append([]string{labelWallet}, req.Labels...)
	desc := req.Description
	if desc == "" {
		desc = "bsv-regtest " + req.Shape
	}

	no, delayed, wantTx := req.NoSend, req.Delayed, true
	res, err := h.CreateAction(ctx, sdk.CreateActionArgs{
		Description: desc,
		Outputs:     outs,
		Labels:      labels,
		Options: &sdk.CreateActionOptions{
			// Delayed broadcast returns before anything touches the network, which makes
			// per-transaction feedback meaningless. Default to synchronous so the returned
			// status is an actual answer.
			AcceptDelayedBroadcast: &delayed,
			NoSend:                 &no,
			ReturnTXIDOnly:         boolPtr(!wantTx),
		},
	}, w.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("create action: %w", err)
	}

	out := &TxResult{TxID: res.Txid.String(), Satoshis: total, Labels: labels, NoSend: req.NoSend}
	out.Status = "sending"
	if req.NoSend {
		out.Status = "nosend"
	}
	if len(res.SendWithResults) > 0 {
		out.Status = string(res.SendWithResults[0].Status)
	}
	// The signed transaction comes back as BEEF; the two serialisations the harness needs are
	// standard raw hex (SV nodes) and extended format (arcade). Build once, serialise twice.
	if len(res.Tx) > 0 {
		if tx, err := parseAtomic(res.Tx); err == nil && tx != nil {
			out.RawHex = tx.Hex()
			if ef, err := tx.EFHex(); err == nil {
				out.EFHex = ef
			}
		}
	}
	return out, nil
}

func (w *Wallet) outputsFor(req TxRequest) ([]sdk.CreateActionOutput, uint64, error) {
	switch req.Shape {
	case ShapePayment, "":
		lock, err := w.lockFor(req.To)
		if err != nil {
			return nil, 0, err
		}
		if req.Satoshis == 0 {
			return nil, 0, fmt.Errorf("payment needs satoshis > 0")
		}
		return []sdk.CreateActionOutput{{
			LockingScript: lock, Satoshis: req.Satoshis, OutputDescription: "payment",
		}}, req.Satoshis, nil

	case ShapeOpReturn:
		var payload []byte
		var err error
		switch {
		case req.DataHex != "":
			if payload, err = hex.DecodeString(strings.TrimSpace(req.DataHex)); err != nil {
				return nil, 0, fmt.Errorf("decode dataHex: %w", err)
			}
		case req.Data != "":
			payload = []byte(req.Data)
		default:
			return nil, 0, fmt.Errorf("opreturn needs data or dataHex")
		}
		o, err := transaction.CreateOpReturnOutput([][]byte{payload})
		if err != nil {
			return nil, 0, fmt.Errorf("build op_return: %w", err)
		}
		// A data output carries no value; the fee comes out of the input.
		// The description has a 5-character minimum in the toolbox's validation.
		return []sdk.CreateActionOutput{{
			LockingScript: o.LockingScript.Bytes(), Satoshis: 0, OutputDescription: "op_return data",
		}}, 0, nil

	case ShapeFanout:
		n := req.Outputs
		if n < 2 || n > 1000 {
			return nil, 0, fmt.Errorf("fanout needs between 2 and 1000 outputs, got %d", n)
		}
		if req.Satoshis == 0 {
			return nil, 0, fmt.Errorf("fanout needs satoshis > 0 per output")
		}
		lock, err := w.lockFor(req.To)
		if err != nil {
			return nil, 0, err
		}
		outs := make([]sdk.CreateActionOutput, 0, n)
		for i := 0; i < n; i++ {
			outs = append(outs, sdk.CreateActionOutput{
				LockingScript: lock, Satoshis: req.Satoshis,
				OutputDescription: fmt.Sprintf("fanout %d/%d", i+1, n),
			})
		}
		return outs, req.Satoshis * uint64(n), nil

	case ShapeCustom:
		b, err := hex.DecodeString(strings.TrimSpace(req.Script))
		if err != nil {
			return nil, 0, fmt.Errorf("decode locking script: %w", err)
		}
		if len(b) == 0 {
			return nil, 0, fmt.Errorf("custom shape needs a locking script")
		}
		// Deliberately unvalidated: feeding a node a script it may refuse is the point.
		return []sdk.CreateActionOutput{{
			LockingScript: b, Satoshis: req.Satoshis, OutputDescription: "custom script",
		}}, req.Satoshis, nil
	}
	return nil, 0, fmt.Errorf("unknown shape %q (want payment, opreturn, fanout or custom)", req.Shape)
}

// lockFor turns a destination into a locking script. An empty destination pays this wallet's
// own deposit address, which is what makes fanout a self-funding coin splitter.
func (w *Wallet) lockFor(to string) ([]byte, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		b, err := hex.DecodeString(w.deposit.LockingScriptHex)
		if err != nil {
			return nil, fmt.Errorf("own deposit script: %w", err)
		}
		return b, nil
	}
	// A long hex string is a locking script; anything else is an address.
	if b, err := hex.DecodeString(to); err == nil && len(b) >= 20 {
		return b, nil
	}
	addr, err := script.NewAddressFromString(to)
	if err != nil {
		return nil, fmt.Errorf("parse destination %q: %w", to, err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		return nil, fmt.Errorf("lock destination: %w", err)
	}
	return lock.Bytes(), nil
}

func boolPtr(b bool) *bool { return &b }
