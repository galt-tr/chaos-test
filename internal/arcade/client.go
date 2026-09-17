// Package arcade reads arcade's HTTP surface: the rich health document on the API port and
// the chain tip of its embedded chaintracks (a separate listener).
package arcade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client talks to one arcade instance.
type Client struct {
	BaseURL        string // API listener, e.g. http://arcade:8080
	ChaintracksURL string // chaintracks listener, e.g. http://arcade:8083 ("" = not configured)
	HTTP           *http.Client
}

// New returns a client with a 10 s timeout.
func New(baseURL, chaintracksURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), ChaintracksURL: strings.TrimRight(chaintracksURL, "/"),
		HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Datahub is one teranode arcade broadcasts to, from GET /health.
type Datahub struct {
	URL     string `json:"url"`
	Source  string `json:"source"`
	Healthy bool   `json:"healthy"`
}

// Health is GET /health on the API listener.
type Health struct {
	Healthy     bool      `json:"healthy"`
	Version     string    `json:"version"`
	Status      string    `json:"status"`
	BlockHeight uint32    `json:"blockHeight"`
	Datahubs    []Datahub `json:"datahub_urls"`
}

// Tip is GET /chaintracks/v2/tip: a block header with display-order hashes.
type Tip struct {
	Height       uint32 `json:"height"`
	Hash         string `json:"hash"`
	PreviousHash string `json:"previousHash"`
	Time         int64  `json:"time"`
}

// Health fetches the health document.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	var h Health
	if err := c.get(ctx, c.BaseURL+"/health", &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Tip fetches the embedded chaintracks' best header.
func (c *Client) Tip(ctx context.Context) (*Tip, error) {
	if c.ChaintracksURL == "" {
		return nil, errors.New("chaintracks URL not in inventory (re-run make gen)")
	}
	var t Tip
	if err := c.get(ctx, c.ChaintracksURL+"/chaintracks/v2/tip", &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *Client) get(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("arcade %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
