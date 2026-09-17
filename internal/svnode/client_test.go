package svnode

import "testing"

func TestFrozenAt(t *testing.T) {
	txid := "ab"
	funds := []BlacklistFund{}
	f := BlacklistFund{}
	f.TxOut.TxID, f.TxOut.Vout = "AB", 1
	f.EnforceAtHeight = []struct {
		Start uint64 `json:"start"`
		Stop  uint64 `json:"stop"`
	}{{Start: 100, Stop: 110}}
	funds = append(funds, f)
	g := BlacklistFund{}
	g.TxOut.TxID, g.TxOut.Vout = "cd", 0
	funds = append(funds, g) // no window: frozen everywhere
	cases := []struct {
		id   string
		vout uint32
		h    uint64
		want bool
	}{
		{txid, 1, 99, false}, {txid, 1, 100, true}, {txid, 1, 109, true}, {txid, 1, 110, false},
		{txid, 0, 105, false}, {"cd", 0, 5, true}, {"zz", 0, 105, false},
	}
	for _, c := range cases {
		if got := FrozenAt(funds, c.id, c.vout, c.h); got != c.want {
			t.Errorf("FrozenAt(%s:%d @%d) = %v, want %v", c.id, c.vout, c.h, got, c.want)
		}
	}
	if Satoshis(0.00012345) != 12345 {
		t.Fatalf("Satoshis: %d", Satoshis(0.00012345))
	}
}
