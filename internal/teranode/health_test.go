package teranode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// The golden document is a real capture from the live stack: 11 services, 105 leaf checks,
// 17 distinct resources.
func TestFlattenHealthDedupesGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/health.json")
	if err != nil {
		t.Skip("no golden health document captured")
	}
	var doc rawHealth
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("golden document does not parse: %v", err)
	}
	deps := flattenHealth(&doc)
	if len(deps) == 0 || len(deps) > 40 {
		t.Fatalf("expected the ~105 leaf checks to collapse to a couple of dozen rows, got %d", len(deps))
	}
	var multi *HealthDep
	for i := range deps {
		if len(deps[i].SeenIn) > 1 {
			multi = &deps[i]
			break
		}
	}
	if multi == nil {
		t.Fatal("no resource recorded more than one owning service; the dedupe lost the blast radius")
	}
	for _, d := range deps {
		// "<nil>" is teranode's healthy value and must never surface as an error.
		if d.Error == "<nil>" {
			t.Fatalf("resource %s kept the literal <nil> error", d.Resource)
		}
		if d.Status == 200 && !d.OK {
			t.Fatalf("resource %s is 200 but not OK", d.Resource)
		}
	}
}

// teranode builds this document with fmt.Sprintf and an unescaped message, so a quote in
// any dependency message produces invalid JSON. The probe must still report the status.
func TestMalformedHealthStillReportsStatus(t *testing.T) {
	bad := `{"status":"503", "services":[{"service":"blockchain","status":"503","dependencies":[` +
		`{"resource": "UTXOStore", "status": "503", "error": "boom", "message": "unexpected "quote" here"}]}]}`
	if json.Valid([]byte(bad)) {
		t.Fatal("fixture is supposed to be invalid JSON")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(bad))
	}))
	defer srv.Close()

	h, err := NewHealthClient(srv.URL).Get(context.Background())
	if err != nil {
		t.Fatalf("a malformed body must not fail the probe: %v", err)
	}
	if h.Parsed {
		t.Fatal("Parsed should be false for an unparseable document")
	}
	if h.OverallStatus != http.StatusServiceUnavailable {
		t.Fatalf("status lost: got %d", h.OverallStatus)
	}
	if h.Raw == "" {
		t.Fatal("want the raw body kept for inspection")
	}
}

func TestCatchupNon200IsAnError(t *testing.T) {
	// The exact trap: a 503 carrying a body that reads as a healthy idle node.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"BlockValidation service not available","is_catching_up":false}`))
	}))
	defer srv.Close()

	got, err := NewAssetClient(srv.URL).Catchup(context.Background())
	if err == nil {
		t.Fatal("a 503 must be an error, not a not-catching-up result")
	}
	if got != nil {
		t.Fatal("no status must be returned alongside the error")
	}
	if StatusOf(err) != http.StatusServiceUnavailable {
		t.Fatalf("want the status preserved, got %d", StatusOf(err))
	}
}
