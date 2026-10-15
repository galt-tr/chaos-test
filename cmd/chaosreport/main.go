// Command chaosreport turns scenario runs that dumped transition timing (timing_dump) into
// latency tables: per run, per phase (before / during / after the fault window), the
// submit->SEEN and submit->MINED distributions, plus arcade's merkle endpoint breaker timeline.
//
//	go run ./cmd/chaosreport -runs sim/.data/runs [-json out.json] run-id...
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type mark struct {
	Time  time.Time `json:"time"`
	Label string    `json:"label"`
}

type endpointSample struct {
	URL            string `json:"url"`
	Healthy        bool   `json:"healthy"`
	CatchupPending int    `json:"catchupPending"`
}

type healthSample struct {
	Time      time.Time        `json:"time"`
	Endpoints []endpointSample `json:"merkle_endpoints"`
	Err       string           `json:"err"`
}

type tx struct {
	ID        string
	Submitted time.Time
	Returned  time.Time
	Final     string
	First     map[string]time.Time
}

// Dist is a latency distribution in seconds.
type Dist struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// Phase is one slice of a run by submit time.
type Phase struct {
	Name           string         `json:"name"`
	Txs            int            `json:"txs"`
	Final          map[string]int `json:"final"`
	SubmitToSeen   Dist           `json:"submitToSeen"`
	SubmitToMined  Dist           `json:"submitToMined"`
	WalletCall     Dist           `json:"walletCall"`
	PendingRetry   int            `json:"pendingRetry"`
	BroadcastEarly int            `json:"broadcastBeforeWindowEnd"` // reached SENT/ACCEPTED/SEEN before the fault window closed
}

// EndpointTimeline is one merkle endpoint's breaker as arcade reported it.
type EndpointTimeline struct {
	URL              string    `json:"url"`
	UnhealthyAt      []float64 `json:"unhealthyAt"` // seconds since the fault window opened
	HealthyAt        []float64 `json:"healthyAt"`
	MaxCatchup       int       `json:"maxCatchup"`
	CatchupDrainedAt float64   `json:"catchupDrainedAt"` // last time catch-up went to 0 after being >0; -1 never
}

// Report is one run.
type Report struct {
	Run       string             `json:"run"`
	Scenario  string             `json:"scenario"`
	Status    string             `json:"status"`
	Marks     []mark             `json:"marks"`
	FaultSecs float64            `json:"faultWindowSeconds"`
	Phases    []Phase            `json:"phases"`
	Endpoints []EndpointTimeline `json:"endpoints"`
	Series    []point            `json:"series"`
}

type point struct {
	T     float64 `json:"t"` // submit time, seconds since fault window opened
	Seen  float64 `json:"seen,omitempty"`
	Mined float64 `json:"mined,omitempty"`
	Final string  `json:"final"`
}

func main() {
	runsDir := flag.String("runs", "sim/.data/runs", "runs directory")
	jsonOut := flag.String("json", "", "also write the reports as JSON here")
	flag.Parse()
	var reps []Report
	for _, id := range flag.Args() {
		r, err := load(*runsDir, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, id+":", err)
			os.Exit(1)
		}
		reps = append(reps, r)
	}
	for _, r := range reps {
		printReport(r)
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(reps, "", "  ")
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func load(runsDir, id string) (Report, error) {
	rep := Report{Run: id}
	if b, err := os.ReadFile(filepath.Join(runsDir, id+".json")); err == nil {
		var run struct {
			ScenarioID string `json:"scenarioId"`
			Status     string `json:"status"`
		}
		_ = json.Unmarshal(b, &run)
		rep.Scenario, rep.Status = run.ScenarioID, run.Status
	}
	dir := filepath.Join(runsDir, id)
	if b, err := os.ReadFile(filepath.Join(dir, "marks.json")); err == nil {
		_ = json.Unmarshal(b, &rep.Marks)
	}
	txs, err := loadTxs(filepath.Join(dir, "transitions.csv"))
	if err != nil {
		return rep, err
	}
	health, _ := loadHealth(filepath.Join(dir, "health.jsonl"))

	start, end := markTime(rep.Marks, "fault-window-start"), markTime(rep.Marks, "fault-window-end")
	if start.IsZero() && len(txs) > 0 {
		start = txs[0].Submitted
	}
	if end.IsZero() {
		end = start
	}
	rep.FaultSecs = end.Sub(start).Seconds()

	phases := map[string][]*tx{}
	for _, t := range txs {
		p := "during"
		switch {
		case t.Submitted.Before(start):
			p = "before"
		case !t.Submitted.Before(end):
			p = "after"
		}
		phases[p] = append(phases[p], t)
		pt := point{T: t.Submitted.Sub(start).Seconds(), Final: t.Final}
		if s := seenAt(t); !s.IsZero() {
			pt.Seen = s.Sub(t.Submitted).Seconds()
		}
		if m := minedAt(t); !m.IsZero() {
			pt.Mined = m.Sub(t.Submitted).Seconds()
		}
		rep.Series = append(rep.Series, pt)
	}
	phases["all"] = txs
	for _, name := range []string{"before", "during", "after", "all"} {
		rep.Phases = append(rep.Phases, phase(name, phases[name], end))
	}
	rep.Endpoints = endpoints(health, start)
	return rep, nil
}

func phase(name string, txs []*tx, windowEnd time.Time) Phase {
	p := Phase{Name: name, Txs: len(txs), Final: map[string]int{}}
	var seen, mined, call []float64
	for _, t := range txs {
		f := t.Final
		if f == "" {
			f = "UNSEEN"
		}
		p.Final[f]++
		if s := seenAt(t); !s.IsZero() {
			seen = append(seen, s.Sub(t.Submitted).Seconds())
		}
		if m := minedAt(t); !m.IsZero() {
			mined = append(mined, m.Sub(t.Submitted).Seconds())
		}
		if !t.Returned.IsZero() {
			call = append(call, t.Returned.Sub(t.Submitted).Seconds())
		}
		if !t.First["PENDING_RETRY"].IsZero() {
			p.PendingRetry++
		}
		if b := broadcastAt(t); !b.IsZero() && b.Before(windowEnd) {
			p.BroadcastEarly++
		}
	}
	p.SubmitToSeen, p.SubmitToMined, p.WalletCall = dist(seen), dist(mined), dist(call)
	return p
}

// seenAt is the first sighting of any status at or past SEEN_ON_NETWORK: a 1 s poll can miss
// SEEN entirely when the tx is mined quickly.
func seenAt(t *tx) time.Time {
	return earliest(t, "SEEN_ON_NETWORK", "SEEN_MULTIPLE_NODES", "STUMP_PROCESSING", "MINED", "IMMUTABLE")
}

func minedAt(t *tx) time.Time { return earliest(t, "MINED", "IMMUTABLE") }

func broadcastAt(t *tx) time.Time {
	return earliest(t, "SENT_TO_NETWORK", "ACCEPTED_BY_NETWORK", "SEEN_ON_NETWORK", "SEEN_MULTIPLE_NODES", "MINED", "IMMUTABLE")
}

func earliest(t *tx, statuses ...string) time.Time {
	var out time.Time
	for _, s := range statuses {
		if v := t.First[s]; !v.IsZero() && (out.IsZero() || v.Before(out)) {
			out = v
		}
	}
	return out
}

func dist(v []float64) Dist {
	if len(v) == 0 {
		return Dist{}
	}
	sort.Float64s(v)
	q := func(p float64) float64 {
		i := int(math.Ceil(p*float64(len(v)))) - 1
		if i < 0 {
			i = 0
		}
		return round(v[i])
	}
	return Dist{N: len(v), P50: q(0.5), P95: q(0.95), Max: round(v[len(v)-1])}
}

func round(f float64) float64 { return math.Round(f*10) / 10 }

func endpoints(h []healthSample, start time.Time) []EndpointTimeline {
	byURL := map[string]*EndpointTimeline{}
	var order []string
	prev := map[string]endpointSample{}
	for _, s := range h {
		for _, e := range s.Endpoints {
			tl, ok := byURL[e.URL]
			if !ok {
				tl = &EndpointTimeline{URL: e.URL, CatchupDrainedAt: -1}
				byURL[e.URL] = tl
				order = append(order, e.URL)
			}
			at := round(s.Time.Sub(start).Seconds())
			p, had := prev[e.URL]
			if had && p.Healthy && !e.Healthy {
				tl.UnhealthyAt = append(tl.UnhealthyAt, at)
			}
			if had && !p.Healthy && e.Healthy {
				tl.HealthyAt = append(tl.HealthyAt, at)
			}
			if e.CatchupPending > tl.MaxCatchup {
				tl.MaxCatchup = e.CatchupPending
			}
			if had && p.CatchupPending > 0 && e.CatchupPending == 0 {
				tl.CatchupDrainedAt = at
			}
			prev[e.URL] = e
		}
	}
	out := make([]EndpointTimeline, 0, len(order))
	for _, u := range order {
		out = append(out, *byURL[u])
	}
	return out
}

func markTime(ms []mark, label string) time.Time {
	for _, m := range ms {
		if m.Label == label {
			return m.Time
		}
	}
	return time.Time{}
}

func loadTxs(path string) ([]*tx, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: empty", path)
	}
	head := rows[0]
	var out []*tx
	for _, r := range rows[1:] {
		t := &tx{First: map[string]time.Time{}}
		for i, h := range head {
			v := r[i]
			switch h {
			case "txid":
				t.ID = v
			case "submitted":
				t.Submitted = parse(v)
			case "returned":
				t.Returned = parse(v)
			case "final_status":
				t.Final = v
			case "polls":
			default:
				if v != "" {
					t.First[strings.ToUpper(h)] = parse(v)
				}
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Submitted.Before(out[j].Submitted) })
	return out, nil
}

func loadHealth(path string) ([]healthSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []healthSample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var s healthSample
		if json.Unmarshal(sc.Bytes(), &s) == nil {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}

func parse(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func printReport(r Report) {
	fmt.Printf("\n## %s (%s) status=%s fault window=%.0fs\n\n", r.Scenario, r.Run, r.Status, r.FaultSecs)
	fmt.Println("| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	for _, p := range r.Phases {
		fmt.Printf("| %s | %d | %s | %s | %s | %.1f/%.1f | %d | %d |\n", p.Name, p.Txs, finals(p.Final),
			d3(p.SubmitToSeen), d3(p.SubmitToMined), p.WalletCall.P50, p.WalletCall.P95, p.PendingRetry, p.BroadcastEarly)
	}
	fmt.Println()
	for _, e := range r.Endpoints {
		fmt.Printf("- %s: unhealthy at %v, healthy at %v, max catch-up %d, catch-up drained at %.1fs\n",
			e.URL, e.UnhealthyAt, e.HealthyAt, e.MaxCatchup, e.CatchupDrainedAt)
	}
	for _, m := range r.Marks {
		fmt.Printf("- mark %s\n", m.Label)
	}
}

func d3(d Dist) string {
	if d.N == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f/%.1f/%.1f (n=%d)", d.P50, d.P95, d.Max, d.N)
}

func finals(m map[string]int) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}
