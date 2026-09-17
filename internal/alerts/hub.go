package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HubClient reads a go-alert-system node's HTTP API (the network's canonical alert store).
type HubClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewHubClient returns a client for e.g. http://alert-system:3000.
func NewHubClient(baseURL string) *HubClient {
	return &HubClient{BaseURL: baseURL, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// HubHealth is GET /health.
type HubHealth struct {
	Sequence         uint32 `json:"sequence"`
	Synced           bool   `json:"synced"`
	ActivePeers      int    `json:"active_peers"`
	UnprocessedAlert int    `json:"unprocessed_alerts"`
}

// Health returns the hub's health document.
func (c *HubClient) Health(ctx context.Context) (*HubHealth, error) {
	var h HubHealth
	if err := c.get(ctx, "/health", &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// HubAlert is one row of GET /alerts.
type HubAlert struct {
	Sequence  uint32 `json:"sequence_number"`
	Hash      string `json:"hash"`
	Raw       string `json:"raw"`
	Processed bool   `json:"processed"`
}

// Alerts lists the hub's stored alerts.
func (c *HubClient) Alerts(ctx context.Context) ([]HubAlert, error) {
	var out struct {
		Alerts         []HubAlert `json:"alerts"`
		LatestSequence uint32     `json:"latest_sequence"`
	}
	if err := c.get(ctx, "/alerts", &out); err != nil {
		return nil, err
	}
	return out.Alerts, nil
}

func (c *HubClient) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("hub %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
