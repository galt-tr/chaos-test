package api

import (
	"net/http"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/automine"
)

// autoMineStatus is the cadence as the UI should see it. A nil service reports itself disabled
// rather than being absent, so the header always has something definite to render.
func (s *Server) autoMineStatus() automine.Status {
	if s.d.AutoMine == nil {
		return automine.Status{Now: time.Now().UTC().Format(time.RFC3339)}
	}
	return s.d.AutoMine.Status()
}

// AutoMineStatus is the exported accessor the scenario engine uses.
func (s *Server) AutoMineStatus() automine.Status { return s.autoMineStatus() }

// AutoMineConfigure changes the cadence. Shared by the HTTP route and the scenario action.
func (s *Server) AutoMineConfigure(cfg automine.Config) automine.Status {
	if s.d.AutoMine == nil {
		return s.autoMineStatus()
	}
	if cfg.Node != "" {
		if n, err := s.node(cfg.Node); err == nil {
			cfg.Node = n.Name
		}
	}
	if cfg.Node == "" {
		cfg.Node = s.defaultNode()
	}
	return s.d.AutoMine.Configure(cfg)
}

func (s *Server) postAutoMine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled         *bool  `json:"enabled"`
		IntervalSeconds int    `json:"intervalSeconds"`
		Node            string `json:"node"`
		Blocks          int    `json:"blocks"`
	}
	if err := decode(r, &req); err != nil {
		writeErrReason(w, http.StatusBadRequest, reasonBadRequest, err, nil)
		return
	}
	cur := s.autoMineStatus()
	cfg := automine.Config{
		Enabled:  cur.Enabled,
		Interval: time.Duration(cur.IntervalSeconds) * time.Second,
		Node:     cur.Node,
		Blocks:   cur.Blocks,
	}
	// Absent fields keep their current value, so a caller can flip one knob without having to
	// restate the rest.
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.IntervalSeconds > 0 {
		cfg.Interval = time.Duration(req.IntervalSeconds) * time.Second
	}
	if req.Node != "" {
		cfg.Node = req.Node
	}
	if req.Blocks > 0 {
		cfg.Blocks = req.Blocks
	}
	writeJSON(w, http.StatusOK, s.AutoMineConfigure(cfg))
}
