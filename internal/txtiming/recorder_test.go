package txtiming

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/arcade"
)

// A tx moving SEEN -> MINED is stamped once per status and stops being polled when terminal.
func TestRecorderStampsFirstSightingAndStopsAtTerminal(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"merkle_endpoints":[{"url":"m1","healthy":true}]}`))
			return
		}
		n := calls.Add(1)
		st := "SEEN_ON_NETWORK"
		if n >= 3 {
			st = "MINED"
		}
		_, _ = w.Write([]byte(`{"txid":"aa","txStatus":"` + st + `"}`))
	}))
	defer srv.Close()

	r := New(arcade.New(srv.URL, ""), srv.URL+"/health")
	now := time.Now()
	r.Note("aa", now, now)
	for i := 0; i < 5; i++ {
		r.pollOnce(context.Background())
	}
	r.sampleHealth(context.Background())
	if got := calls.Load(); got != 3 {
		t.Fatalf("polls after terminal: got %d calls, want 3", got)
	}
	dir := t.TempDir()
	if err := r.Dump(dir); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "transitions.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	if rows[1][col["final_status"]] != "MINED" || rows[1][col["seen_on_network"]] == "" || rows[1][col["mined"]] == "" {
		t.Fatalf("unexpected row %v", rows[1])
	}
	if _, err := os.Stat(filepath.Join(dir, "health.jsonl")); err != nil {
		t.Fatal(err)
	}
}
