package walletsvc

import "github.com/bsv-blockchain/bsv-regtest/wallet"

// BuildAtomicBEEF wraps a mined transaction and its merkle proof into the atomic BEEF that
// InternalizeAction requires. The implementation lives in bsv-regtest's wallet package so
// `stackctl topup` and the simulator's top up build the same bytes.
func BuildAtomicBEEF(rawHex, bumpHex string) ([]byte, string, error) {
	return wallet.BuildAtomicBEEF(rawHex, bumpHex)
}
