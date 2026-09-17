package arcade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Bodies captured from a live arcade (ghcr.io/bsv-blockchain/arcade, 2026-09-17).
const healthBody = `{"healthy":true,"version":"latest","status":"ok","blockHeight":108,"datahub_urls":[{"url":"http://10.190.0.11:8090/api/v1","source":"configured","healthy":true},{"url":"http://10.190.0.12:8090/api/v1","source":"configured","healthy":false}]}`
const tipBody = `{"version":536870912,"previousHash":"29ca4b4c0000000000000000000000000000000000000000000000000000d0fc","merkleRoot":"cf68","time":1789648261,"bits":545259519,"nonce":0,"height":108,"hash":"4d40bb0a98c2000000000000000000000000000000000000000000000000087f"}`

func TestClient(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(healthBody))
	}))
	defer api.Close()
	ct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chaintracks/v2/tip" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(tipBody))
	}))
	defer ct.Close()

	c := New(api.URL+"/", ct.URL)
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !h.Healthy || h.Version != "latest" || h.BlockHeight != 108 || len(h.Datahubs) != 2 || h.Datahubs[1].Healthy {
		t.Fatalf("health parsed wrong: %+v", h)
	}
	tip, err := c.Tip(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tip.Height != 108 || tip.Hash[:12] != "4d40bb0a98c2" || tip.PreviousHash[:8] != "29ca4b4c" {
		t.Fatalf("tip parsed wrong: %+v", tip)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	if _, err := New(bad.URL, bad.URL).Health(context.Background()); err == nil {
		t.Fatal("HTTP 500 must be an error")
	}
	if _, err := New(api.URL, "").Tip(context.Background()); err == nil {
		t.Fatal("missing chaintracks URL must be an error")
	}
}
