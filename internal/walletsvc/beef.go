package walletsvc

import (
	"encoding/hex"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

// BuildAtomicBEEF wraps a mined transaction and its merkle proof into the atomic BEEF that
// InternalizeAction requires.
//
// The wallet will not accept raw transaction hex: it parses the bytes as atomic BEEF and then
// validates every merkle root against its chain tracker. That is why funding has to be mined
// and proven before it can be credited — this function is the last step of that dance, and it
// is pure so the awkward part is testable without a chain.
func BuildAtomicBEEF(rawHex, bumpHex string) ([]byte, string, error) {
	if rawHex == "" {
		return nil, "", fmt.Errorf("no raw transaction")
	}
	if bumpHex == "" {
		return nil, "", fmt.Errorf("no merkle path: the funding transaction is not mined yet")
	}
	tx, err := transaction.NewTransactionFromHex(rawHex)
	if err != nil {
		return nil, "", fmt.Errorf("parse funding tx: %w", err)
	}
	bump, err := transaction.NewMerklePathFromHex(bumpHex)
	if err != nil {
		return nil, "", fmt.Errorf("parse merkle path: %w", err)
	}
	tx.MerklePath = bump

	beef, err := transaction.NewBeefFromTransaction(tx)
	if err != nil {
		return nil, "", fmt.Errorf("build beef: %w", err)
	}
	b, err := beef.AtomicBytes(tx.TxID())
	if err != nil {
		return nil, "", fmt.Errorf("atomic beef: %w", err)
	}
	return b, hex.EncodeToString(b), nil
}
