// Package wallet holds the harness's transaction builders. The raw signer here spends
// specific outpoints with keys the harness controls (miner key, victim keys), which is
// what the freeze scenarios need: exact control over which coin is spent, where, and when.
package wallet

import (
	"encoding/hex"
	"errors"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
)

// Key is a secp256k1 private key with its P2PKH locking script.
type Key struct {
	Priv *ec.PrivateKey
}

// KeyFromWIF loads a WIF private key (regtest/testnet or mainnet encoding).
func KeyFromWIF(wif string) (*Key, error) {
	priv, err := ec.PrivateKeyFromWif(wif)
	if err != nil {
		return nil, fmt.Errorf("decode WIF: %w", err)
	}
	return &Key{Priv: priv}, nil
}

// KeyFromHex loads a 32-byte hex private key.
func KeyFromHex(h string) (*Key, error) {
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes of hex: %v", err)
	}
	priv, _ := ec.PrivateKeyFromBytes(raw)
	return &Key{Priv: priv}, nil
}

// NewKey generates a fresh key.
func NewKey() (*Key, error) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	return &Key{Priv: priv}, nil
}

// Hex returns the 32-byte private key as hex.
func (k *Key) Hex() string { return hex.EncodeToString(k.Priv.Serialize()) }

// Address returns the P2PKH address (testnet/regtest encoding when testnet is true).
func (k *Key) Address(testnet bool) (string, error) {
	addr, err := script.NewAddressFromPublicKey(k.Priv.PubKey(), !testnet)
	if err != nil {
		return "", err
	}
	return addr.AddressString, nil
}

// LockingScript returns the P2PKH locking script for the key.
func (k *Key) LockingScript() (*script.Script, error) {
	addr, err := script.NewAddressFromPublicKey(k.Priv.PubKey(), true)
	if err != nil {
		return nil, err
	}
	return p2pkh.Lock(addr)
}

// Input names an outpoint to spend along with the key that can unlock it and the
// source transaction's output (needed for the BIP-143 style signature and fees).
type Input struct {
	SourceTx *transaction.Transaction
	Vout     uint32
	Key      *Key
}

// Output is a destination for a spend.
type Output struct {
	Key      *Key   // pay to this key's P2PKH script …
	Script   string // … or to this locking script (hex), if Key is nil
	Satoshis uint64
}

// Spend builds and signs a transaction spending inputs to outputs. The remaining value
// (inputs minus outputs minus fee) is added as change to the first input's key when it
// is above dust; fee is an absolute number of satoshis.
func Spend(inputs []Input, outputs []Output, fee uint64) (*transaction.Transaction, error) {
	if len(inputs) == 0 {
		return nil, errors.New("spend needs at least one input")
	}
	tx := transaction.NewTransaction()
	var in uint64
	for i, inp := range inputs {
		if inp.SourceTx == nil || inp.Key == nil {
			return nil, fmt.Errorf("input %d: source tx and key required", i)
		}
		if int(inp.Vout) >= len(inp.SourceTx.Outputs) {
			return nil, fmt.Errorf("input %d: vout %d out of range", i, inp.Vout)
		}
		unlocker, err := p2pkh.Unlock(inp.Key.Priv, nil)
		if err != nil {
			return nil, fmt.Errorf("input %d: unlocker: %w", i, err)
		}
		if err := tx.AddInputFrom(inp.SourceTx.TxID().String(), inp.Vout,
			inp.SourceTx.Outputs[inp.Vout].LockingScript.String(),
			inp.SourceTx.Outputs[inp.Vout].Satoshis, unlocker); err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		in += inp.SourceTx.Outputs[inp.Vout].Satoshis
	}
	var out uint64
	for i, o := range outputs {
		var ls *script.Script
		var err error
		switch {
		case o.Key != nil:
			ls, err = o.Key.LockingScript()
		case o.Script != "":
			ls, err = script.NewFromHex(o.Script)
		default:
			err = errors.New("output needs a key or a script")
		}
		if err != nil {
			return nil, fmt.Errorf("output %d: %w", i, err)
		}
		tx.AddOutput(&transaction.TransactionOutput{LockingScript: ls, Satoshis: o.Satoshis})
		out += o.Satoshis
	}
	if out+fee > in {
		return nil, fmt.Errorf("outputs (%d) + fee (%d) exceed inputs (%d)", out, fee, in)
	}
	if change := in - out - fee; change > 546 {
		ls, err := inputs[0].Key.LockingScript()
		if err != nil {
			return nil, err
		}
		tx.AddOutput(&transaction.TransactionOutput{LockingScript: ls, Satoshis: change})
	}
	if err := tx.Sign(); err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return tx, nil
}

// ParseTx decodes a transaction from hex.
func ParseTx(h string) (*transaction.Transaction, error) {
	return transaction.NewTransactionFromHex(h)
}
