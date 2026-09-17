// Package scenario runs YAML-defined scenarios against the stack: ordered steps (mine, spend,
// submit, build/push alerts, partitions) with polled assertions, in auto or step-through mode,
// recording every run as JSON for later comparison.
package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bsv-blockchain/chaos-test/internal/api"
	"github.com/bsv-blockchain/chaos-test/internal/observe"
	"github.com/bsv-blockchain/chaos-test/internal/topology"
)

// Definition is a scenario file.
type Definition struct {
	ID          string            `yaml:"id" json:"id"`
	Name        string            `yaml:"name" json:"name"`
	Description string            `yaml:"description" json:"description"`
	Requires    []string          `yaml:"requires" json:"requires,omitempty"`
	Roles       map[string]string `yaml:"roles" json:"roles"`   // role -> default node
	Params      map[string]any    `yaml:"params" json:"params"` // defaults, overridable per run
	Steps       []Step            `yaml:"steps" json:"steps"`
	Source      string            `yaml:"-" json:"source,omitempty"`
}

// Step is one action with optional assertions evaluated after it.
type Step struct {
	Name    string         `yaml:"name" json:"name"`
	Action  string         `yaml:"action" json:"action"`
	With    map[string]any `yaml:"with" json:"with,omitempty"`
	As      string         `yaml:"as" json:"as,omitempty"` // store the action result under this variable
	Timeout string         `yaml:"timeout" json:"timeout,omitempty"`
	Assert  []Assertion    `yaml:"assert" json:"assert,omitempty"`
	Note    string         `yaml:"note" json:"note,omitempty"`
}

// Assertion is a polled check. Should=true records a failure as a finding without aborting.
type Assertion struct {
	Check   string         `yaml:"check" json:"check"`
	With    map[string]any `yaml:"with" json:"with,omitempty"`
	Timeout string         `yaml:"timeout" json:"timeout,omitempty"`
	Should  bool           `yaml:"should" json:"should,omitempty"`
	Message string         `yaml:"message" json:"message,omitempty"`
}

// AssertResult records one assertion outcome.
type AssertResult struct {
	Check    string `json:"check"`
	Passed   bool   `json:"passed"`
	Should   bool   `json:"should"`
	Detail   string `json:"detail"`
	Duration string `json:"duration"`
}

// StepResult records one step.
type StepResult struct {
	Index      int            `json:"index"`
	Name       string         `json:"name"`
	Action     string         `json:"action"`
	Status     string         `json:"status"` // pending running passed failed skipped
	StartedAt  *time.Time     `json:"startedAt,omitempty"`
	FinishedAt *time.Time     `json:"finishedAt,omitempty"`
	Output     any            `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
	Assertions []AssertResult `json:"assertions,omitempty"`
	Resolved   map[string]any `json:"resolved,omitempty"`
}

// Run is one execution of a scenario.
type Run struct {
	ID          string            `json:"id"`
	ScenarioID  string            `json:"scenarioId"`
	Mode        string            `json:"mode"` // auto | step
	Status      string            `json:"status"`
	Roles       map[string]string `json:"roles"`
	Params      map[string]any    `json:"params"`
	Vars        map[string]any    `json:"vars"`
	Steps       []StepResult      `json:"steps"`
	Findings    []string          `json:"findings"`
	StartedAt   time.Time         `json:"startedAt"`
	FinishedAt  *time.Time        `json:"finishedAt,omitempty"`
	CurrentStep int               `json:"currentStep"`
	// Partitions the run switched on and has not yet healed (node -> planes); healed on exit.
	OpenPartitions map[string][]string `json:"openPartitions,omitempty"`
	FirstEvent     uint64              `json:"firstEvent"`
	LastEvent      uint64              `json:"lastEvent"`
	Error          string              `json:"error,omitempty"`

	mu     sync.Mutex
	next   chan struct{}
	cancel context.CancelFunc
}

// Deps wires the engine.
type Deps struct {
	Inventory *topology.Inventory
	Bus       *observe.Bus
	Fleet     *observe.Fleet
	API       *api.Server
	Logger    *slog.Logger
	RunsDir   string
}

// Engine holds definitions and runs.
type Engine struct {
	d    Deps
	mu   sync.RWMutex
	defs map[string]*Definition
	runs map[string]*Run
	seq  int
	dir  string // scenario directory, re-read on every list/start so edits apply live
}

// NewEngine creates an engine.
func NewEngine(d Deps) *Engine {
	e := &Engine{d: d, defs: map[string]*Definition{}, runs: map[string]*Run{}}
	_ = os.MkdirAll(d.RunsDir, 0o755)
	e.loadRuns()
	return e
}

// LoadDir loads every *.yaml in dir and remembers dir for live reloads.
func (e *Engine) LoadDir(dir string) error {
	e.mu.Lock()
	e.dir = dir
	e.mu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.defs = map[string]*Definition{}
	e.mu.Unlock()
	for _, ent := range entries {
		if ent.IsDir() || !(strings.HasSuffix(ent.Name(), ".yaml") || strings.HasSuffix(ent.Name(), ".yml")) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			return err
		}
		var def Definition
		if err := yaml.Unmarshal(b, &def); err != nil {
			return fmt.Errorf("%s: %w", ent.Name(), err)
		}
		if def.ID == "" {
			def.ID = strings.TrimSuffix(strings.TrimSuffix(ent.Name(), ".yaml"), ".yml")
		}
		def.Source = ent.Name()
		e.mu.Lock()
		e.defs[def.ID] = &def
		e.mu.Unlock()
		e.d.Logger.Debug("scenario loaded", "id", def.ID, "steps", len(def.Steps))
	}
	return nil
}

func (e *Engine) loadRuns() {
	entries, _ := os.ReadDir(e.d.RunsDir)
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(e.d.RunsDir, ent.Name()))
		if err != nil {
			continue
		}
		var r Run
		if json.Unmarshal(b, &r) == nil && r.ID != "" {
			if r.Status == "running" || r.Status == "paused" {
				r.Status = "aborted"
				r.Error = "orchestrator restarted"
			}
			e.runs[r.ID] = &r
		}
	}
}

func (e *Engine) save(r *Run) {
	r.mu.Lock()
	b, err := json.MarshalIndent(r, "", "  ")
	r.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(e.d.RunsDir, r.ID+".json"), b, 0o644)
}

// reload re-reads the scenario directory (ignoring errors, keeping the last good set).
func (e *Engine) reload() {
	e.mu.RLock()
	dir := e.dir
	e.mu.RUnlock()
	if dir == "" {
		return
	}
	if err := e.LoadDir(dir); err != nil {
		e.d.Logger.Warn("scenario reload failed", "err", err)
	}
}

// Start begins a run.
func (e *Engine) Start(scenarioID, mode string, roles map[string]string, params map[string]any) (*Run, error) {
	e.reload()
	e.mu.Lock()
	def, ok := e.defs[scenarioID]
	if !ok {
		e.mu.Unlock()
		return nil, fmt.Errorf("unknown scenario %q", scenarioID)
	}
	for _, r := range e.runs {
		if r.Status == "running" || r.Status == "paused" {
			e.mu.Unlock()
			return nil, fmt.Errorf("run %s is still %s; abort it first", r.ID, r.Status)
		}
	}
	e.seq++
	id := fmt.Sprintf("%s-%s-%d", time.Now().UTC().Format("20060102-150405"), scenarioID, e.seq)
	if mode != "step" {
		mode = "auto"
	}
	r := &Run{ID: id, ScenarioID: scenarioID, Mode: mode, Status: "running", Roles: map[string]string{}, Params: map[string]any{},
		Vars: map[string]any{}, Findings: []string{}, OpenPartitions: map[string][]string{}, StartedAt: time.Now().UTC(), next: make(chan struct{}, 1)}
	if recent := e.d.Bus.Recent(1); len(recent) > 0 {
		r.FirstEvent = recent[0].ID
	}
	for k, v := range def.Roles {
		r.Roles[k] = v
	}
	for k, v := range roles {
		if v != "" {
			r.Roles[k] = v
		}
	}
	for k, v := range def.Params {
		r.Params[k] = v
	}
	for k, v := range params {
		r.Params[k] = v
	}
	for i, st := range def.Steps {
		r.Steps = append(r.Steps, StepResult{Index: i, Name: st.Name, Action: st.Action, Status: "pending"})
	}
	e.runs[id] = r
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go e.execute(ctx, def, r)
	return r, nil
}

// Next releases a paused step-mode run to its next step.
func (e *Engine) Next(id string) error {
	r, ok := e.run(id)
	if !ok {
		return fmt.Errorf("unknown run %q", id)
	}
	select {
	case r.next <- struct{}{}:
	default:
	}
	return nil
}

// Abort cancels a run.
func (e *Engine) Abort(id string) error {
	r, ok := e.run(id)
	if !ok {
		return fmt.Errorf("unknown run %q", id)
	}
	if r.cancel != nil {
		r.cancel()
	}
	select {
	case r.next <- struct{}{}:
	default:
	}
	return nil
}

func (e *Engine) run(id string) (*Run, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	r, ok := e.runs[id]
	return r, ok
}

func (e *Engine) publish(r *Run, msg string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["run"] = r.ID
	ev := e.d.Bus.Publish("scenario", "", msg, data)
	r.mu.Lock()
	r.LastEvent = ev.ID
	r.mu.Unlock()
}

func (e *Engine) execute(ctx context.Context, def *Definition, r *Run) {
	e.publish(r, fmt.Sprintf("run %s started (%s, %s mode)", r.ID, def.Name, r.Mode), map[string]any{"status": "running"})
	ex := &executor{e: e, r: r, def: def}
	finalStatus := "passed"
	for i := range def.Steps {
		st := def.Steps[i]
		if r.Mode == "step" && i > 0 {
			r.mu.Lock()
			r.Status = "paused"
			r.CurrentStep = i
			r.mu.Unlock()
			e.save(r)
			e.publish(r, fmt.Sprintf("paused before step %d: %s", i+1, st.Name), map[string]any{"status": "paused", "step": i})
			select {
			case <-r.next:
			case <-ctx.Done():
			}
			r.mu.Lock()
			r.Status = "running"
			r.mu.Unlock()
		}
		if ctx.Err() != nil {
			finalStatus = "aborted"
			break
		}
		r.mu.Lock()
		r.CurrentStep = i
		now := time.Now().UTC()
		r.Steps[i].Status = "running"
		r.Steps[i].StartedAt = &now
		r.mu.Unlock()
		e.publish(r, fmt.Sprintf("step %d/%d: %s", i+1, len(def.Steps), st.Name), map[string]any{"step": i, "action": st.Action})
		stepStart := e.d.Bus.Recent(1)
		var sinceID uint64
		if len(stepStart) > 0 {
			sinceID = stepStart[0].ID
		}
		res := ex.runStep(ctx, st, sinceID)
		r.mu.Lock()
		fin := time.Now().UTC()
		res.Index, res.Name, res.Action, res.StartedAt, res.FinishedAt = i, st.Name, st.Action, &now, &fin
		r.Steps[i] = res
		r.mu.Unlock()
		e.save(r)
		if res.Status == "failed" {
			e.publish(r, fmt.Sprintf("step %d failed: %s", i+1, res.Error), map[string]any{"step": i, "status": "failed"})
			finalStatus = "failed"
			break
		}
		e.publish(r, fmt.Sprintf("step %d passed: %s", i+1, st.Name), map[string]any{"step": i, "status": "passed"})
	}
	if ctx.Err() != nil && finalStatus == "passed" {
		finalStatus = "aborted"
	}
	e.cleanup(r)
	r.mu.Lock()
	r.Status = finalStatus
	fin := time.Now().UTC()
	r.FinishedAt = &fin
	for i := range r.Steps {
		if r.Steps[i].Status == "pending" {
			r.Steps[i].Status = "skipped"
		}
	}
	r.mu.Unlock()
	e.save(r)
	e.publish(r, fmt.Sprintf("run %s %s (%d findings)", r.ID, finalStatus, len(r.Findings)), map[string]any{"status": finalStatus})
}

// ---- HTTP ---------------------------------------------------------------------------------

func (e *Engine) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(req.URL.Path, "/api/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case parts[0] == "scenarios" && len(parts) == 1 && req.Method == http.MethodGet:
		e.reload()
		e.mu.RLock()
		list := make([]*Definition, 0, len(e.defs))
		for _, d := range e.defs {
			list = append(list, d)
		}
		e.mu.RUnlock()
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		writeJSON(w, 200, list)
	case parts[0] == "scenarios" && len(parts) == 2 && req.Method == http.MethodGet:
		e.mu.RLock()
		d, ok := e.defs[parts[1]]
		e.mu.RUnlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "unknown scenario"})
			return
		}
		writeJSON(w, 200, d)
	case parts[0] == "scenarios" && len(parts) == 3 && parts[2] == "run" && req.Method == http.MethodPost:
		var body struct {
			Mode   string            `json:"mode"`
			Roles  map[string]string `json:"roles"`
			Params map[string]any    `json:"params"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r, err := e.Start(parts[1], body.Mode, body.Roles, body.Params)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, e.snapshot(r))
	case parts[0] == "runs" && len(parts) == 1 && req.Method == http.MethodGet:
		e.mu.RLock()
		list := make([]*Run, 0, len(e.runs))
		for _, r := range e.runs {
			list = append(list, r)
		}
		e.mu.RUnlock()
		sort.Slice(list, func(i, j int) bool { return list[i].StartedAt.After(list[j].StartedAt) })
		out := make([]any, 0, len(list))
		for _, r := range list {
			out = append(out, e.snapshot(r))
		}
		writeJSON(w, 200, out)
	case parts[0] == "runs" && len(parts) == 2 && req.Method == http.MethodGet:
		r, ok := e.run(parts[1])
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "unknown run"})
			return
		}
		writeJSON(w, 200, e.snapshot(r))
	case parts[0] == "runs" && len(parts) == 3 && req.Method == http.MethodPost:
		var err error
		switch parts[2] {
		case "next":
			err = e.Next(parts[1])
		case "abort":
			err = e.Abort(parts[1])
		default:
			err = errors.New("unknown run action")
		}
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

func (e *Engine) snapshot(r *Run) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}

// cleanup heals every partition the run left open so a failed or aborted run does not leave
// the stack in a chaos state. Set param "cleanup: false" to keep partitions for inspection.
func (e *Engine) cleanup(r *Run) {
	r.mu.Lock()
	keep := false
	if v, ok := r.Params["cleanup"]; ok {
		if b, isBool := v.(bool); isBool && !b {
			keep = true
		}
	}
	open := map[string][]string{}
	for n, planes := range r.OpenPartitions {
		open[n] = append([]string(nil), planes...)
	}
	r.mu.Unlock()
	if keep || len(open) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for node, planes := range open {
		for _, plane := range planes {
			if err := e.d.API.Partition(ctx, node, plane, false); err != nil {
				e.publish(r, fmt.Sprintf("cleanup: could not heal %s %s plane: %v", node, plane, err), nil)
			} else {
				e.publish(r, fmt.Sprintf("cleanup: healed %s %s plane", node, plane), nil)
			}
		}
	}
	r.mu.Lock()
	r.OpenPartitions = map[string][]string{}
	r.mu.Unlock()
}
