package alerts

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-alert-system/app/config"
	"github.com/bsv-blockchain/go-alert-system/app/models"
	"github.com/bsv-blockchain/go-alert-system/app/models/model"
	"github.com/mrz1836/go-datastore"

	"github.com/bsv-blockchain/chaos-test/internal/keys"
)

func genesisKeys(t *testing.T, n int) (privs, pubs []string) {
	t.Helper()
	for i := 0; i < n; i++ {
		k, err := keys.NewSecp256k1Key()
		if err != nil {
			t.Fatal(err)
		}
		privs = append(privs, k.PrivateKeyHex)
		pubs = append(pubs, k.PublicKeyHex)
	}
	return privs, pubs
}

// verifierConfig builds a go-alert-system config with an in-memory datastore whose active
// keys are pubs — what a receiving node has after CreateGenesisAlert.
func verifierConfig(t *testing.T, pubs []string) *config.Config {
	t.Helper()
	ctx := context.Background()
	ds, err := datastore.NewClient(ctx,
		datastore.WithSQLite(&datastore.SQLiteConfig{
			CommonConfig: datastore.CommonConfig{TablePrefix: "alert_system", MaxIdleConnections: 1, MaxOpenConnections: 1},
			DatabasePath: "",
		}),
		datastore.WithAutoMigrate(models.BaseModels...),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close(ctx) })
	cfg := &config.Config{GenesisKeys: pubs}
	cfg.Services.Datastore = ds
	cfg.Services.Log = &libLogger{lg: testLogger()}
	cfg.Services.Node = config.NewNodeMock("t", "t", "http://127.0.0.1:1")
	cfg.Services.HTTPClient = http.DefaultClient
	if err := models.CreateGenesisAlert(ctx, model.WithAllDependencies(cfg)); err != nil {
		t.Fatal(err)
	}
	return cfg
}

const txid = "0e3e2357e806b6cdb1f70b54c3a3a17b6714ee1f0e68bebb44a74b1efd512098"

func TestFreezeRoundTripAndSignatures(t *testing.T) {
	privs, pubs := genesisKeys(t, 5)
	payload, err := FundsPayload([]Fund{
		{TxID: txid, Vout: 0, EnforceAtHeightStart: 110, EnforceAtHeightStop: 120},
		{TxID: txid, Vout: 1, EnforceAtHeightStart: 110, EnforceAtHeightStop: 120, PolicyExpiresWithConsensus: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 2*57 {
		t.Fatalf("freeze payload is %d bytes, want 114", len(payload))
	}
	a, err := Build(1, TypeFreezeUTXO, payload, time.Unix(1_700_000_000, 0), privs[:3])
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Wire) != len(a.Unsigned)+3*65 {
		t.Fatalf("wire has %d bytes, unsigned %d", len(a.Wire), len(a.Unsigned))
	}

	cfg := verifierConfig(t, pubs)
	m, err := models.NewAlertFromBytes(a.Wire, model.WithAllDependencies(cfg))
	if err != nil {
		t.Fatalf("receiver could not parse alert: %v", err)
	}
	if m.SequenceNumber != 1 || m.GetAlertType() != models.AlertTypeFreezeUtxo {
		t.Fatalf("parsed seq=%d type=%d", m.SequenceNumber, m.GetAlertType())
	}
	m.SerializeData()
	valid, err := m.AreSignaturesValid(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("receiver rejected signatures made with genesis keys")
	}

	if _, err := Describe(a.Wire); err != nil {
		t.Fatal(err)
	}
	// The parsed fund must carry the txid in display order: the node hex-encodes the 32
	// bytes verbatim into the RPC txId (MessageString double-hexes it, so check the struct).
	fz, ok := m.ProcessAlertMessage().(*models.AlertMessageFreezeUtxo)
	if !ok {
		t.Fatalf("unexpected message type %T", m.ProcessAlertMessage())
	}
	if err := fz.Read(m.GetRawMessage()); err != nil {
		t.Fatal(err)
	}
	if len(fz.Funds) != 2 || fz.Funds[0].TxOut.TxId != txid || fz.Funds[1].TxOut.Vout != 1 ||
		fz.Funds[0].EnforceAtHeight[0].Start != 110 || fz.Funds[0].EnforceAtHeight[0].Stop != 120 ||
		fz.Funds[0].PolicyExpiresWithConsensus || !fz.Funds[1].PolicyExpiresWithConsensus {
		t.Fatalf("parsed funds do not match input: %+v", fz.Funds)
	}

	// A signer outside the genesis set must be rejected.
	rogue, _ := genesisKeys(t, 3)
	bad, err := Build(1, TypeFreezeUTXO, payload, time.Now(), rogue)
	if err != nil {
		t.Fatal(err)
	}
	bm, err := models.NewAlertFromBytes(bad.Wire, model.WithAllDependencies(cfg))
	if err != nil {
		t.Fatal(err)
	}
	bm.SerializeData()
	valid, err = bm.AreSignaturesValid(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("receiver accepted signatures from non-genesis keys")
	}
}

func TestOtherPayloadsParse(t *testing.T) {
	privs, _ := genesisKeys(t, 3)
	cases := []struct {
		name    string
		typ     Type
		payload func() ([]byte, error)
		want    string
	}{
		{"informational", TypeInformational, func() ([]byte, error) { return InformationalPayload("hello fleet"), nil }, "hello fleet"},
		{"invalidate", TypeInvalidateBlock, func() ([]byte, error) { return InvalidateBlockPayload(txid, "testing") }, txid},
		{"ban", TypeBanPeer, func() ([]byte, error) { return PeerPayload("192.0.2.1/32", "misbehaving") }, "192.0.2.1/32"},
		{"unban", TypeUnbanPeer, func() ([]byte, error) { return PeerPayload("192.0.2.1/32", "forgiven") }, "192.0.2.1/32"},
		{"confiscate", TypeConfiscateUTXO, func() ([]byte, error) { return ConfiscatePayload(150, minimalTx()) }, "150"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := c.payload()
			if err != nil {
				t.Fatal(err)
			}
			a, err := Build(7, c.typ, p, time.Now(), privs)
			if err != nil {
				t.Fatal(err)
			}
			text, err := Describe(a.Wire)
			if err != nil {
				t.Fatalf("receiver could not parse %s payload: %v", c.name, err)
			}
			if !strings.Contains(text, c.want) {
				t.Fatalf("%s description %q does not contain %q", c.name, text, c.want)
			}
		})
	}
}

func TestSetKeysPayload(t *testing.T) {
	_, pubs := genesisKeys(t, 5)
	p, err := SetKeysPayload(pubs)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 5*33 {
		t.Fatalf("set keys payload is %d bytes", len(p))
	}
	if _, err := SetKeysPayload(pubs[:4]); err == nil {
		t.Fatal("expected error for 4 keys")
	}
}

func TestLogPersistence(t *testing.T) {
	privs, _ := genesisKeys(t, 3)
	path := t.TempDir() + "/alerts.json"
	l, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Build(1, TypeInformational, InformationalPayload("one"), time.Now(), privs)
	if _, err := l.Append(a, "first"); err != nil {
		t.Fatal(err)
	}
	b, _ := Build(2, TypeInformational, InformationalPayload("two"), time.Now(), privs)
	if _, err := l.Append(b, ""); err != nil {
		t.Fatal(err)
	}
	other, _ := Build(2, TypeInformational, InformationalPayload("dup"), time.Now(), privs)
	if _, err := l.Append(other, ""); err == nil {
		t.Fatal("expected conflict on duplicate sequence")
	}
	again, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Latest() != 2 {
		t.Fatalf("latest after reload = %d", again.Latest())
	}
	w, ok := again.Wire(1)
	if !ok || hex.EncodeToString(w) != hex.EncodeToString(a.Wire) {
		t.Fatal("wire bytes did not survive reload")
	}
}

// minimalTx is a syntactically valid 1-in/1-out transaction (coinbase-shaped input).
func minimalTx() []byte {
	h := "01000000" + // version
		"01" + strings.Repeat("00", 32) + "ffffffff" + "00" + "ffffffff" + // input
		"01" + "0100000000000000" + "01" + "51" + // output: 1 sat, OP_TRUE
		"00000000" // locktime
	b, _ := hex.DecodeString(h)
	return b
}

// TestFundsRoundTrip checks that Funds decodes exactly what FundsPayload encoded, through
// a fully built (signed) alert, and yields nil for alert types without funds.
func TestFundsRoundTrip(t *testing.T) {
	privs, _ := genesisKeys(t, 3)
	want := []Fund{
		{TxID: txid, Vout: 0, EnforceAtHeightStart: 828, EnforceAtHeightStop: 838},
		{TxID: strings.Repeat("ab", 32), Vout: 7, EnforceAtHeightStart: 1, EnforceAtHeightStop: 2, PolicyExpiresWithConsensus: true},
	}
	payload, err := FundsPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(1, TypeFreezeUTXO, payload, time.Unix(1_700_000_000, 0), privs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Funds(a.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d funds, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fund %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	info, err := Build(2, TypeInformational, InformationalPayload("hello"), time.Unix(1_700_000_000, 0), privs)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := Funds(info.Wire); err != nil || f != nil {
		t.Fatalf("informational alert: funds=%v err=%v, want nil, nil", f, err)
	}
}
