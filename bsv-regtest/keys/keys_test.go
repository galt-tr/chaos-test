package keys

import "testing"

func TestEd25519RoundTrip(t *testing.T) {
	id, err := NewEd25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if len(id.PrivateKeyHex) != 128 {
		t.Fatalf("expected 64-byte raw key, got %d hex chars", len(id.PrivateKeyHex))
	}
	again, err := Ed25519IdentityFromHex(id.PrivateKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if again.PeerID != id.PeerID {
		t.Fatalf("peer id mismatch: %s vs %s", again.PeerID, id.PeerID)
	}
}

func TestSecp256k1(t *testing.T) {
	k, err := NewSecp256k1Key()
	if err != nil {
		t.Fatal(err)
	}
	if len(k.PrivateKeyHex) != 64 || len(k.PublicKeyHex) != 66 {
		t.Fatalf("unexpected lengths priv=%d pub=%d", len(k.PrivateKeyHex), len(k.PublicKeyHex))
	}
}
