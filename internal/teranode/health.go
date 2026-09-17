package teranode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// HealthClient reads a teranode's aggregate health document (its own port, not /api/v1).
type HealthClient struct {
	URL  string
	HTTP *http.Client
}

// NewHealthClient returns a client for a node's health URL.
func NewHealthClient(url string) *HealthClient {
	return &HealthClient{URL: url, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// HealthDep is one dependency check, flattened out of the nested document.
type HealthDep struct {
	Resource string   `json:"resource"`
	Status   int      `json:"status"`
	Error    string   `json:"error,omitempty"`
	Message  string   `json:"message,omitempty"`
	SeenIn   []string `json:"seenIn,omitempty"` // services reporting it — the blast radius
	OK       bool     `json:"ok"`
}

// Health is a node's health document, flattened and de-duplicated.
type Health struct {
	OverallStatus int         `json:"overallStatus"`
	Parsed        bool        `json:"parsed"` // false when teranode emitted invalid JSON
	Raw           string      `json:"raw,omitempty"`
	Services      int         `json:"services"`
	Checks        int         `json:"checks"`    // distinct resource/status/error combinations
	Deps          []HealthDep `json:"deps"`      // healthy and unhealthy, unhealthy first
	Unhealthy     []string    `json:"unhealthy"` // resource names failing
}

// rawHealth mirrors the document's shape. Nodes either carry a leaf check or nest further;
// the recursive BlockchainClient subtree is why one document holds ~105 leaf checks for
// only ~17 distinct resources.
type rawHealth struct {
	Status       string      `json:"status"`
	Service      string      `json:"service,omitempty"`
	Resource     string      `json:"resource,omitempty"`
	Error        string      `json:"error,omitempty"`
	Message      string      `json:"message,omitempty"`
	Services     []rawHealth `json:"services,omitempty"`
	Dependencies []rawHealth `json:"dependencies,omitempty"`
}

// Get fetches and flattens the health document.
//
// The HTTP status is kept independently of the body because teranode builds this document
// with fmt.Sprintf and an unescaped message (util/health/health.go), so any dependency
// message containing a quote or backslash yields invalid JSON. A parse failure must still
// report healthy/unhealthy rather than losing the answer entirely.
func (c *HealthClient) Get(ctx context.Context) (*Health, error) {
	if c.URL == "" {
		return nil, fmt.Errorf("no health URL configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	h := &Health{OverallStatus: resp.StatusCode, Deps: []HealthDep{}, Unhealthy: []string{}}
	var doc rawHealth
	if err := json.Unmarshal(b, &doc); err != nil {
		h.Raw = truncate(b)
		return h, nil
	}
	h.Parsed = true
	if s, err := strconv.Atoi(doc.Status); err == nil && s != 0 {
		h.OverallStatus = s
	}
	h.Services = len(doc.Services)
	h.Deps = flattenHealth(&doc)
	h.Checks = len(h.Deps)
	for _, d := range h.Deps {
		if !d.OK {
			h.Unhealthy = append(h.Unhealthy, d.Resource)
		}
	}
	return h, nil
}

// flattenHealth walks the nested document into one row per distinct
// resource/status/error, recording every service a row was reached through. A resource
// healthy under one service and failing under another correctly stays two rows.
func flattenHealth(doc *rawHealth) []HealthDep {
	type key struct{ resource, status, err string }
	idx := map[key]int{}
	var out []HealthDep

	var walk func(n *rawHealth, service string)
	walk = func(n *rawHealth, service string) {
		if n.Service != "" {
			service = n.Service
		}
		if n.Resource != "" {
			k := key{n.Resource, n.Status, n.Error}
			i, seen := idx[k]
			if !seen {
				status, _ := strconv.Atoi(n.Status)
				// teranode writes the error with %v, so "<nil>" is the healthy value.
				errText := n.Error
				if errText == "<nil>" {
					errText = ""
				}
				out = append(out, HealthDep{
					Resource: n.Resource,
					Status:   status,
					Error:    errText,
					Message:  n.Message,
					OK:       status == http.StatusOK && errText == "",
				})
				i = len(out) - 1
				idx[k] = i
			}
			if service != "" && !containsStr(out[i].SeenIn, service) {
				out[i].SeenIn = append(out[i].SeenIn, service)
			}
		}
		for j := range n.Services {
			walk(&n.Services[j], service)
		}
		for j := range n.Dependencies {
			walk(&n.Dependencies[j], service)
		}
	}
	walk(doc, "")

	// Failures first, then alphabetically, so the interesting rows are never below a fold.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OK != out[j].OK {
			return !out[i].OK
		}
		return out[i].Resource < out[j].Resource
	})
	return out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
