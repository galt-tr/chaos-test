// Package keys generates and encodes the identities used by the stack: libp2p Ed25519
// identities (teranode P2P, teranode alert P2P, alert hub, publisher) and secp256k1 alert
// genesis keys. Encodings match what each consumer expects:
//   - teranode p2p_private_key / alert_p2p_private_key and go-alert-system p2p.private_key:
//     hex of the raw 64-byte Ed25519 private key (seed || public key);
//   - alert_genesis_keys: 33-byte compressed public keys, hex.
package keys

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Ed25519Identity is a libp2p identity in the encodings the stack needs.
type Ed25519Identity struct {
	PrivateKeyHex string `json:"private_key_hex"` // raw 64 bytes, hex
	PeerID        string `json:"peer_id"`
}

// NewEd25519Identity generates a fresh Ed25519 libp2p identity.
func NewEd25519Identity() (Ed25519Identity, error) {
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return Ed25519Identity{}, fmt.Errorf("generate ed25519 key: %w", err)
	}
	return ed25519IdentityFromPriv(priv)
}

// Ed25519IdentityFromHex rebuilds the identity (and peer id) from the raw 64-byte hex form.
func Ed25519IdentityFromHex(privHex string) (Ed25519Identity, error) {
	raw, err := hex.DecodeString(privHex)
	if err != nil {
		return Ed25519Identity{}, fmt.Errorf("decode ed25519 key hex: %w", err)
	}
	priv, err := crypto.UnmarshalEd25519PrivateKey(raw)
	if err != nil {
		return Ed25519Identity{}, fmt.Errorf("unmarshal ed25519 key: %w", err)
	}
	return ed25519IdentityFromPriv(priv)
}

func ed25519IdentityFromPriv(priv crypto.PrivKey) (Ed25519Identity, error) {
	raw, err := priv.Raw()
	if err != nil {
		return Ed25519Identity{}, fmt.Errorf("raw ed25519 key: %w", err)
	}
	id, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return Ed25519Identity{}, fmt.Errorf("peer id: %w", err)
	}
	return Ed25519Identity{PrivateKeyHex: hex.EncodeToString(raw), PeerID: id.String()}, nil
}

// Secp256k1Key is an alert-system signing key pair.
type Secp256k1Key struct {
	PrivateKeyHex string `json:"private_key_hex"` // 32 bytes, hex
	PublicKeyHex  string `json:"public_key_hex"`  // 33-byte compressed, hex
}

// NewSecp256k1Key generates a fresh secp256k1 key pair.
func NewSecp256k1Key() (Secp256k1Key, error) {
	priv, err := ec.NewPrivateKey()
	if err != nil {
		return Secp256k1Key{}, fmt.Errorf("generate secp256k1 key: %w", err)
	}
	return Secp256k1Key{
		PrivateKeyHex: hex.EncodeToString(priv.Serialize()),
		PublicKeyHex:  hex.EncodeToString(priv.PubKey().Compressed()),
	}, nil
}
