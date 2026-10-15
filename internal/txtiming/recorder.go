// Package txtiming records when each transaction is first seen in each arcade status, plus
// arcade's view of its merkle-service endpoints, so a run can be turned into latency numbers.
//
// Arcade keeps only the time of a transaction's latest change, so the transitions are
// observed from outside: every non-terminal transaction is polled once per interval and the
// first poll that shows a status stamps it. Resolution is the poll interval (1 s default).
package txtiming

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/arcade"
)

// Statuses are the columns of the CSV, in lifecycle order.
var Statuses = []string{
	"RECEIVED", "SENT_TO_NETWORK", "ACCEPTED_BY_NETWORK", "SEEN_ON_NETWORK", "SEEN_MULTIPLE_NODES",
	"PENDING_RETRY", "STUMP_PROCESSING", "MINED", "IMMUTABLE", "DOUBLE_SPEND_ATTEMPTED", "REJECTED",
}

func terminal(status string) bool {
	switch status {
	case "MINED", "IMMUTABLE", "REJECTED":
		return true
	}
	return false
}

// Tx is one transaction's observed timeline.
type Tx struct {
	TxID      string               `json:"txid"`
	Submitted time.Time            `json:"submitted"` // wallet CreateAction started
	Returned  time.Time            `json:"returned"`  // wallet CreateAction returned (arcade POST /tx done)
	First     map[string]time.Time `json:"first"`     // first poll that showed each status
	Status    string               `json:"status"`
	Polls     int                  `json:"polls"`
}

// Mark is a labelled instant on the run's timeline (a fault injected, a fault healed).
type Mark struct {
	Time  time.Time `json:"time"`
	Label string    `json:"label"`
}

// HealthSample is arcade's merkle_endpoints[] at one instant.
type HealthSample struct {
	Time      time.Time       `json:"time"`
	Endpoints json.RawMessage `json:"merkle_endpoints,omitempty"`
	Err       string          `json:"err,omitempty"`
}

// Recorder polls arcade for every noted transaction until it is terminal.
type Recorder struct {
	arcade    *arcade.Client
	healthURL string
	interval  time.Duration
	workers   int
	http      *http.Client

	mu      sync.Mutex
	txs     map[string]*Tx
	marks   []Mark
	health  []HealthSample
	started time.Time
}

// New returns a recorder; healthURL is arcade's API /health (it carries merkle_endpoints).
func New(a *arcade.Client, healthURL string) *Recorder {
	return &Recorder{arcade: a, healthURL: healthURL, interval: time.Second, workers: 24,
		http: &http.Client{Timeout: 3 * time.Second}, txs: map[string]*Tx{}, started: time.Now().UTC()}
}

// Reset forgets everything; a scenario calls it before its measured section.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.txs, r.marks, r.health, r.started = map[string]*Tx{}, nil, nil, time.Now().UTC()
}

// Note registers a transaction the wallet just sent.
func (r *Recorder) Note(txid string, submitted, returned time.Time) {
	if r == nil || txid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.txs[txid]; !ok {
		r.txs[txid] = &Tx{TxID: txid, Submitted: submitted.UTC(), Returned: returned.UTC(), First: map[string]time.Time{}}
	}
}

// Mark stamps a labelled instant.
func (r *Recorder) Mark(label string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.marks = append(r.marks, Mark{Time: time.Now().UTC(), Label: label})
	r.mu.Unlock()
}

// Run polls until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			go r.sampleHealth(ctx)
			r.pollOnce(ctx)
		}
	}
}

func (r *Recorder) pending() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.txs))
	for id, tx := range r.txs {
		if !terminal(tx.Status) {
			out = append(out, id)
		}
	}
	return out
}

func (r *Recorder) pollOnce(ctx context.Context) {
	ids := r.pending()
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, r.interval*3)
	defer cancel()
	sem := make(chan struct{}, r.workers)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			rec, err := r.arcade.GetTx(ctx, id)
			now := time.Now().UTC()
			r.mu.Lock()
			defer r.mu.Unlock()
			tx := r.txs[id]
			if tx == nil {
				return
			}
			tx.Polls++
			if err != nil || rec == nil || rec.Status == "" {
				return
			}
			tx.Status = rec.Status
			if _, seen := tx.First[rec.Status]; !seen {
				tx.First[rec.Status] = now
			}
		}(id)
	}
	wg.Wait()
}

func (r *Recorder) sampleHealth(ctx context.Context) {
	if r.healthURL == "" {
		return
	}
	s := HealthSample{Time: time.Now().UTC()}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.healthURL, nil)
	resp, err := r.http.Do(req)
	if err == nil {
		var doc struct {
			Merkle json.RawMessage `json:"merkle_endpoints"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if jerr := json.Unmarshal(b, &doc); jerr != nil {
			s.Err = jerr.Error()
		}
		s.Endpoints = doc.Merkle
	} else {
		s.Err = err.Error()
	}
	r.mu.Lock()
	r.health = append(r.health, s)
	r.mu.Unlock()
}

// Summary is the live counters.
type Summary struct {
	Started  time.Time      `json:"started"`
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"byStatus"`
	Marks    []Mark         `json:"marks"`
}

// Summary counts the current status of every noted transaction.
func (r *Recorder) Summary() Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Summary{Started: r.started, Total: len(r.txs), ByStatus: map[string]int{}, Marks: append([]Mark(nil), r.marks...)}
	for _, tx := range r.txs {
		st := tx.Status
		if st == "" {
			st = "UNSEEN"
		}
		s.ByStatus[st]++
	}
	return s
}

// Dump writes transitions.csv, marks.json and health.jsonl under dir.
func (r *Recorder) Dump(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r.mu.Lock()
	txs := make([]*Tx, 0, len(r.txs))
	for _, tx := range r.txs {
		cp := *tx
		cp.First = make(map[string]time.Time, len(tx.First))
		for k, v := range tx.First {
			cp.First[k] = v
		}
		txs = append(txs, &cp)
	}
	marks := append([]Mark(nil), r.marks...)
	health := append([]HealthSample(nil), r.health...)
	r.mu.Unlock()
	sort.Slice(txs, func(i, j int) bool { return txs[i].Submitted.Before(txs[j].Submitted) })

	f, err := os.Create(filepath.Join(dir, "transitions.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	head := append([]string{"txid", "submitted", "returned", "final_status", "polls"}, lower(Statuses)...)
	_ = w.Write(head)
	for _, tx := range txs {
		row := []string{tx.TxID, ts(tx.Submitted), ts(tx.Returned), tx.Status, fmt.Sprint(tx.Polls)}
		for _, st := range Statuses {
			row = append(row, ts(tx.First[st]))
		}
		_ = w.Write(row)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	mb, _ := json.MarshalIndent(marks, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "marks.json"), mb, 0o644); err != nil {
		return err
	}
	hf, err := os.Create(filepath.Join(dir, "health.jsonl"))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(hf)
	for _, h := range health {
		_ = enc.Encode(h)
	}
	return hf.Close()
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func lower(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}
