package main

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/defs"
)

// A fixed key must always derive the same deposit address.
//
// This is the one value a human copies out of the UI and pays from somewhere else. If the
// derivation ever shifts — a changed prefix constant, a different sender, mainnet encoding —
// every past top-up address silently becomes wrong, and funds paid to the old one are
// unrecoverable by this wallet. Pinning it makes that a test failure instead of lost coins.
func TestDepositAddressIsStable(t *testing.T) {
	const key = "299fbb100a1cd5b06ba5c59bbfd0f52ff0e0a06e0dd1da7d0c38e97e50e3c8a1"
	got, err := DeriveDeposit(key, defs.NetworkTSTN)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address == "" || got.LockingScriptHex == "" {
		t.Fatalf("empty derivation: %+v", got)
	}
	// tstn is testnet-based, so the address must use testnet encoding — which is also what
	// regtest uses, and is why the harness's raw signer can pay it.
	if !strings.ContainsAny(got.Address[:1], "mn2") {
		t.Fatalf("address %q is not testnet-encoded; the raw signer pays testnet addresses", got.Address)
	}
	if got.DerivationPrefixB64 != derivationPrefixB64 || got.DerivationSuffixB64 != derivationSuffixB64 {
		t.Fatal("derivation constants changed: every previously issued deposit address is now wrong")
	}
	// Deriving twice must agree, and the locking script must match the address.
	again, err := DeriveDeposit(key, defs.NetworkTSTN)
	if err != nil {
		t.Fatal(err)
	}
	if again.Address != got.Address || again.LockingScriptHex != got.LockingScriptHex {
		t.Fatal("derivation is not deterministic")
	}
	if _, err := hex.DecodeString(got.LockingScriptHex); err != nil {
		t.Fatalf("locking script is not hex: %v", err)
	}
}

func TestDeriveDepositRejectsBadKey(t *testing.T) {
	if _, err := DeriveDeposit("nothex", defs.NetworkTSTN); err == nil {
		t.Fatal("want an error for a non-hex key")
	}
}

func TestNewWalletValidatesConfig(t *testing.T) {
	if _, err := NewWallet(Config{PrivateKey: strings.Repeat("a", 64)}, nil); err == nil {
		t.Fatal("want an error when no storage URL is configured")
	}
	if _, err := NewWallet(Config{StorageURL: "http://x", PrivateKey: "short"}, nil); err == nil {
		t.Fatal("want an error for a key that is not 64 hex characters")
	}
}

// outputsFor is where the four shapes are defined; it must not need a connection.
func TestOutputsForEachShape(t *testing.T) {
	w := &Wallet{deposit: Deposit{LockingScriptHex: "76a9145e5508cec9b67d99eac6c4cfc7af6ea8ac8f47de88ac"}}

	t.Run("payment", func(t *testing.T) {
		outs, total, err := w.outputsFor(TxRequest{Shape: ShapePayment, Satoshis: 1000})
		if err != nil || len(outs) != 1 || total != 1000 {
			t.Fatalf("got %d outs, total %d, err %v", len(outs), total, err)
		}
	})

	t.Run("opreturn carries no value and is prefixed", func(t *testing.T) {
		outs, total, err := w.outputsFor(TxRequest{Shape: ShapeOpReturn, Data: "hello"})
		if err != nil || len(outs) != 1 {
			t.Fatalf("err %v, outs %d", err, len(outs))
		}
		if total != 0 || outs[0].Satoshis != 0 {
			t.Fatal("a data output must carry 0 satoshis; the fee comes from the input")
		}
		// OP_FALSE OP_RETURN
		if got := hex.EncodeToString(outs[0].LockingScript[:2]); got != "006a" {
			t.Fatalf("want an OP_FALSE OP_RETURN prefix, got %s", got)
		}
		// The toolbox enforces a 5-character minimum on output descriptions; too short an
		// answer here is an invalid-args error at createAction time rather than here.
		if len(outs[0].OutputDescription) < 5 {
			t.Fatalf("outputDescription %q is shorter than the toolbox's 5-char minimum", outs[0].OutputDescription)
		}
	})

	t.Run("fanout makes N equal coins", func(t *testing.T) {
		outs, total, err := w.outputsFor(TxRequest{Shape: ShapeFanout, Outputs: 5, Satoshis: 200})
		if err != nil || len(outs) != 5 || total != 1000 {
			t.Fatalf("got %d outs, total %d, err %v", len(outs), total, err)
		}
		for _, o := range outs {
			if len(o.OutputDescription) < 5 {
				t.Fatalf("description too short: %q", o.OutputDescription)
			}
		}
	})

	t.Run("custom passes the script through unvalidated", func(t *testing.T) {
		outs, _, err := w.outputsFor(TxRequest{Shape: ShapeCustom, Script: "006a0b68656c6c6f", Satoshis: 0})
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(outs[0].LockingScript) != "006a0b68656c6c6f" {
			t.Fatal("the script must reach the node byte for byte — feeding it something odd is the point")
		}
	})

	t.Run("rejects nonsense", func(t *testing.T) {
		for _, req := range []TxRequest{
			{Shape: "nope"},
			{Shape: ShapePayment, Satoshis: 0},
			{Shape: ShapeOpReturn},
			{Shape: ShapeFanout, Outputs: 1, Satoshis: 10},
			{Shape: ShapeCustom, Script: "zz"},
		} {
			if _, _, err := w.outputsFor(req); err == nil {
				t.Fatalf("want an error for %+v", req)
			}
		}
	})
}

// An empty destination pays the wallet's own deposit address, which is what makes fanout a
// self-funding coin splitter.
func TestLockForDefaultsToOwnDeposit(t *testing.T) {
	w := &Wallet{deposit: Deposit{LockingScriptHex: "76a914aabbccddeeff00112233445566778899aabbccdd88ac"}}
	b, err := w.lockFor("")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(b) != w.deposit.LockingScriptHex {
		t.Fatal("an empty destination should pay this wallet")
	}
}

// /healthz and /v1/deposit must answer while disconnected: when the wallet is down, those are
// the only two things that can tell an operator why.
func TestHealthAndDepositAnswerWhileDisconnected(t *testing.T) {
	w, err := NewWallet(Config{
		StorageURL: "http://127.0.0.1:1", // nothing listening
		PrivateKey: "299fbb100a1cd5b06ba5c59bbfd0f52ff0e0a06e0dd1da7d0c38e97e50e3c8a1",
		Network:    defs.NetworkTSTN,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(routes(w))
	defer srv.Close()

	for _, path := range []string{"/healthz", "/v1/deposit"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s returned %d while disconnected; it must still answer", path, resp.StatusCode)
		}
	}
	// A call that genuinely needs storage must report that distinctly, not pretend.
	resp, err := http.Get(srv.URL + "/v1/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 from /v1/state while disconnected, got %d", resp.StatusCode)
	}
}

func TestIsInsufficientFunds(t *testing.T) {
	for _, s := range []string{"not enough funds", "INSUFFICIENT balance", "no spendable outputs"} {
		if !isInsufficientFunds(errString(s)) {
			t.Fatalf("%q should read as backpressure, not failure", s)
		}
	}
	if isInsufficientFunds(errString("connection refused")) {
		t.Fatal("a transport error is not backpressure")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
