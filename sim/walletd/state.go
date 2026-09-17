package main

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"
)

// State is the wallet's current position.
type State struct {
	Connected   bool   `json:"connected"`
	Network     string `json:"network"`
	IdentityKey string `json:"identityKey,omitempty"`
	Address     string `json:"address"`
	Balance     uint64 `json:"balance"`
	// Coins is the spendable output count. It — not balance — is what limits a sustained
	// send: one fat coin means one transaction at a time while change confirms.
	Coins      uint32 `json:"coins"`
	StorageURL string `json:"storageURL"`
}

// State reads balance, coin count and identity.
func (w *Wallet) State(ctx context.Context) (*State, error) {
	h, err := w.Handle()
	if err != nil {
		return nil, err
	}
	st := &State{Connected: true, Network: string(w.cfg.Network),
		Address: w.deposit.Address, StorageURL: w.cfg.StorageURL}
	if st.Balance, err = h.Balance(ctx); err != nil {
		return nil, fmt.Errorf("balance: %w", err)
	}
	// Limit 1 — only TotalOutputs is wanted, and the coin list can be thousands long.
	res, err := h.ListOutputs(ctx, sdk.ListOutputsArgs{
		Basket: basketDefault, Limit: uint32Ptr(1),
	}, w.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("list outputs: %w", err)
	}
	st.Coins = res.TotalOutputs
	if pk, err := h.GetPublicKey(ctx, sdk.GetPublicKeyArgs{IdentityKey: true}, w.cfg.Originator); err == nil && pk != nil && pk.PublicKey != nil {
		st.IdentityKey = pk.PublicKey.ToDERHex()
	}
	return st, nil
}

const basketDefault = "default"

// Coin is one spendable output.
type Coin struct {
	Outpoint  string `json:"outpoint"`
	Satoshis  uint64 `json:"satoshis"`
	Spendable bool   `json:"spendable"`
}

// Outputs lists the wallet's coins, newest basket page first.
func (w *Wallet) Outputs(ctx context.Context, limit uint32) ([]Coin, uint32, error) {
	h, err := w.Handle()
	if err != nil {
		return nil, 0, err
	}
	if limit == 0 || limit > 1000 {
		limit = 100
	}
	res, err := h.ListOutputs(ctx, sdk.ListOutputsArgs{Basket: basketDefault, Limit: &limit}, w.cfg.Originator)
	if err != nil {
		return nil, 0, fmt.Errorf("list outputs: %w", err)
	}
	out := make([]Coin, 0, len(res.Outputs))
	for _, o := range res.Outputs {
		out = append(out, Coin{Outpoint: o.Outpoint.String(), Satoshis: o.Satoshis, Spendable: o.Spendable})
	}
	return out, res.TotalOutputs, nil
}

// Action is one wallet action as the wallet itself sees it.
type Action struct {
	TxID        string   `json:"txid"`
	Status      string   `json:"status"`
	Satoshis    int64    `json:"satoshis"`
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// Health of the wallet's own bookkeeping, bucketed by status.
//
// This is the number that replaces "createAction returned 200". CreateAction succeeds as soon
// as the action is stored, so counting those tells you nothing about the network; counting how
// many actions the wallet later resolved as accepted vs failed does.
type ActionHealth struct {
	Labels     []string          `json:"labels"`
	Total      uint64            `json:"total"`
	Sampled    uint64            `json:"sampled"`
	Buckets    map[string]uint64 `json:"buckets"`
	Accepted   uint64            `json:"accepted"`
	Decided    uint64            `json:"decided"`
	AcceptRate float64           `json:"acceptRate"` // -1 when nothing has been decided yet
	SampledAt  string            `json:"sampledAt"`
	Actions    []Action          `json:"actions,omitempty"`
}

// Actions queries wallet actions by label and buckets them.
func (w *Wallet) Actions(ctx context.Context, labels []string, limit uint32, include bool) (*ActionHealth, error) {
	h, err := w.Handle()
	if err != nil {
		return nil, err
	}
	if len(labels) == 0 {
		labels = []string{labelWallet}
	}
	if limit == 0 || limit > 1000 {
		limit = 1000
	}
	// Probe for the total first: storage orders ascending, so the newest page is at the end
	// and has to be reached by offset rather than by asking for the first page.
	probe, err := h.ListActions(ctx, sdk.ListActionsArgs{
		Labels: labels, LabelQueryMode: "all", Limit: uint32Ptr(1),
	}, w.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("list actions: %w", err)
	}
	out := &ActionHealth{Labels: labels, Total: uint64(probe.TotalActions),
		Buckets: map[string]uint64{}, AcceptRate: -1, SampledAt: time.Now().UTC().Format(time.RFC3339)}
	if probe.TotalActions == 0 {
		return out, nil
	}
	var offset uint32
	if uint32(probe.TotalActions) > limit {
		offset = uint32(probe.TotalActions) - limit
	}
	page, err := h.ListActions(ctx, sdk.ListActionsArgs{
		Labels: labels, LabelQueryMode: "all", Limit: &limit, Offset: &offset,
	}, w.cfg.Originator)
	if err != nil {
		return nil, fmt.Errorf("list actions page: %w", err)
	}
	for _, a := range page.Actions {
		status := string(a.Status)
		out.Buckets[status]++
		out.Sampled++
		if include {
			out.Actions = append(out.Actions, Action{
				TxID: a.Txid.String(), Status: status, Satoshis: a.Satoshis,
				Description: a.Description, Labels: a.Labels,
			})
		}
	}
	// accepted = the wallet believes it is on its way or done; decided excludes everything
	// still in flight, so the rate is over outcomes rather than over hope.
	out.Accepted = out.Buckets["unproven"] + out.Buckets["completed"]
	out.Decided = out.Accepted + out.Buckets["failed"]
	if out.Decided > 0 {
		out.AcceptRate = float64(out.Accepted) / float64(out.Decided)
	}
	return out, nil
}

func uint32Ptr(v uint32) *uint32 { return &v }
