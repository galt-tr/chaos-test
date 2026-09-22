package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/brc29"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/defs"
)

// The BRC-29 key id the harness funds through. These are the toolbox's own faucet constants,
// and the sender is AnyoneKey, which makes the deposit address a deterministic function of the
// wallet's private key alone — so it can be shown before any connection exists, and it stays
// stable across restarts. Changing either constant silently orphans every past top-up.
const (
	derivationPrefixB64 = "SfKxPIJNgdI="
	derivationSuffixB64 = "NaGLC6fMH50="
)

// Deposit is the address an external payer sends to in order to fund this wallet.
type Deposit struct {
	Network             string `json:"network"`
	Address             string `json:"address"`
	LockingScriptHex    string `json:"lockingScriptHex"`
	DerivationPrefixB64 string `json:"derivationPrefixB64"`
	DerivationSuffixB64 string `json:"derivationSuffixB64"`
	SuggestedSatoshis   uint64 `json:"suggestedSatoshis"`
}

// DeriveDeposit builds the BRC-29 receive address for a wallet key.
//
// tstn and ttn are testnet-based, so they use testnet address encoding — which is what regtest
// uses too, and is why the harness's raw signer can pay this address directly.
func DeriveDeposit(privHex string, network defs.BSVNetwork) (Deposit, error) {
	raw, err := hex.DecodeString(privHex)
	if err != nil {
		return Deposit{}, fmt.Errorf("decode private key: %w", err)
	}
	priv, _ := ec.PrivateKeyFromBytes(raw)
	_, anyonePub := sdk.AnyoneKey()
	keyID := brc29.KeyID{DerivationPrefix: derivationPrefixB64, DerivationSuffix: derivationSuffixB64}

	var addr *script.Address
	switch network {
	case defs.NetworkMainnet:
		addr, err = brc29.AddressForSelf(anyonePub, keyID, priv, brc29.WithMainNet())
	default:
		addr, err = brc29.AddressForSelf(anyonePub, keyID, priv, brc29.WithTestNet())
	}
	if err != nil {
		return Deposit{}, fmt.Errorf("derive brc29 address: %w", err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		return Deposit{}, fmt.Errorf("lock deposit address: %w", err)
	}
	return Deposit{
		Network: string(network), Address: addr.AddressString,
		LockingScriptHex:    hex.EncodeToString(lock.Bytes()),
		DerivationPrefixB64: derivationPrefixB64, DerivationSuffixB64: derivationSuffixB64,
		SuggestedSatoshis: 100_000,
	}, nil
}

// anyonePaymentRemittance is the derivation data InternalizeAction needs to reconstruct the
// unlocking key. It must match DeriveDeposit exactly or the wallet cannot spend what it took in.
func anyonePaymentRemittance() (*sdk.Payment, error) {
	prefix, err := base64.StdEncoding.DecodeString(derivationPrefixB64)
	if err != nil {
		return nil, fmt.Errorf("decode prefix: %w", err)
	}
	suffix, err := base64.StdEncoding.DecodeString(derivationSuffixB64)
	if err != nil {
		return nil, fmt.Errorf("decode suffix: %w", err)
	}
	_, anyonePub := sdk.AnyoneKey()
	return &sdk.Payment{DerivationPrefix: prefix, DerivationSuffix: suffix, SenderIdentityKey: anyonePub}, nil
}

// InternalizeRequest credits a mined, proven payment into the wallet.
type InternalizeRequest struct {
	AtomicBeefHex   string `json:"atomicBeefHex"`
	ExpectedAddress string `json:"expectedAddress"`
	OutputIndex     uint32 `json:"outputIndex"`
	Description     string `json:"description"`
}

// InternalizeResult reports what was credited.
type InternalizeResult struct {
	Accepted    bool   `json:"accepted"`
	OutputIndex uint32 `json:"outputIndex"`
	Balance     uint64 `json:"balance"`
	Coins       uint32 `json:"coins"`
}

// Internalize credits an external payment to this wallet.
//
// The bytes must be atomic BEEF with a merkle path the storage server's chain tracker accepts —
// raw transaction hex is rejected outright. That is why the caller has to mine the funding
// transaction and wait for its proof before getting here.
func (w *Wallet) Internalize(ctx context.Context, req InternalizeRequest) (*InternalizeResult, error) {
	h, err := w.Handle()
	if err != nil {
		return nil, err
	}
	atomic, err := hex.DecodeString(req.AtomicBeefHex)
	if err != nil {
		return nil, fmt.Errorf("decode atomic beef: %w", err)
	}
	vout := req.OutputIndex
	if req.ExpectedAddress != "" {
		if vout, err = resolvePaymentOutput(atomic, req.OutputIndex, req.ExpectedAddress); err != nil {
			return nil, err
		}
	}
	remittance, err := anyonePaymentRemittance()
	if err != nil {
		return nil, err
	}
	desc := req.Description
	if desc == "" {
		desc = "bsv-regtest top up"
	}
	if _, err := h.InternalizeAction(ctx, sdk.InternalizeActionArgs{
		Tx: atomic,
		Outputs: []sdk.InternalizeOutput{{
			OutputIndex:       vout,
			Protocol:          sdk.InternalizeProtocolWalletPayment,
			PaymentRemittance: remittance,
		}},
		Description: desc,
		Labels:      []string{labelWallet, "topup"},
	}, w.cfg.Originator); err != nil {
		return nil, fmt.Errorf("internalize: %w", err)
	}
	st, err := w.State(ctx)
	if err != nil {
		return &InternalizeResult{Accepted: true, OutputIndex: vout}, nil
	}
	return &InternalizeResult{Accepted: true, OutputIndex: vout, Balance: st.Balance, Coins: st.Coins}, nil
}

// resolvePaymentOutput finds the output that actually pays the deposit address.
//
// Trusting a caller-supplied vout is the classic way to lose a top-up: the funding transaction
// carries change as well as the payment, and which one lands at index 0 is not something the
// funder controls.
func resolvePaymentOutput(atomic []byte, preferred uint32, expectedAddress string) (uint32, error) {
	addr, err := script.NewAddressFromString(expectedAddress)
	if err != nil {
		return 0, fmt.Errorf("parse expected address: %w", err)
	}
	want, err := p2pkh.Lock(addr)
	if err != nil {
		return 0, fmt.Errorf("lock expected address: %w", err)
	}
	tx, err := parseAtomic(atomic)
	if err != nil {
		// Unparseable here is not fatal: InternalizeAction validates the bytes properly and
		// will produce a better error than a guess would.
		return preferred, nil
	}
	pays := func(i int) bool {
		o := tx.Outputs[i]
		return o != nil && o.LockingScript != nil && o.LockingScript.Equals(want)
	}
	if int(preferred) < len(tx.Outputs) && pays(int(preferred)) {
		return preferred, nil
	}
	for i := range tx.Outputs {
		if pays(i) {
			return uint32(i), nil
		}
	}
	return 0, fmt.Errorf("no output pays the deposit address %s (tx has %d outputs; vout %d is often change)",
		expectedAddress, len(tx.Outputs), preferred)
}

func parseAtomic(b []byte) (*transaction.Transaction, error) {
	if beef, _, txid, err := transaction.ParseBeef(b); err == nil && beef != nil {
		if txid != nil {
			if tx := beef.FindTransaction(txid.String()); tx != nil {
				return tx, nil
			}
		}
	}
	return transaction.NewTransactionFromBytes(b)
}
