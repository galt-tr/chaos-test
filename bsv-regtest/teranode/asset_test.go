package teranode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The envelope as teranode v0.16.0 returns it for /settings?category=none.
func TestBuildInfoReadsSettingsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "bitcoin" || p != "bitcoin" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Authentication required","success":false}`))
			return
		}
		if r.URL.Path != "/settings" || r.URL.Query().Get("category") != "none" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"settings":[],"total":660,"filtered":0,"version":"v0.16.0","commit":"4edb60a40"}`))
	}))
	defer srv.Close()

	c := NewAssetClient(srv.URL)
	if _, _, err := c.BuildInfo(context.Background()); err == nil {
		t.Fatal("without credentials the 401 must be an error")
	}
	c.User, c.Pass = "bitcoin", "bitcoin"
	v, commit, err := c.BuildInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "v0.16.0" || commit != "4edb60a40" {
		t.Fatalf("got %q %q", v, commit)
	}
}

func TestBuildInfoWithoutVersionIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"settings":[],"total":660,"filtered":0}`))
	}))
	defer srv.Close()

	if _, _, err := NewAssetClient(srv.URL).BuildInfo(context.Background()); err == nil {
		t.Fatal("an envelope without a version must be an error, not an empty version")
	}
}
