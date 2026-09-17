package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/chaos"
	"github.com/bsv-blockchain/chaos-test/internal/diag"
	"github.com/bsv-blockchain/chaos-test/internal/logs"
)

// Error reasons the UI branches on. Branching on a code rather than on message text keeps
// the page's behaviour stable when an underlying error string changes.
const (
	reasonBadRequest       = "bad_request"
	reasonUnknownNode      = "unknown_node"
	reasonNoRuntime        = "no_runtime"
	reasonContainerMissing = "container_missing"
	reasonRuntimeError     = "runtime_error"
	reasonTimeout          = "timeout"
)

// logFetchTimeout bounds a single log read, comfortably inside the runtime client's own
// 60s ceiling so a hung socket surfaces as a 504 rather than a dangling request.
const logFetchTimeout = 20 * time.Second

func writeErrReason(w http.ResponseWriter, code int, reason string, err error, extra map[string]any) {
	body := map[string]any{"error": err.Error(), "reason": reason}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, code, body)
}

// getLogs serves a window of a node's container log.
//
// Filtering happens here rather than in the browser because the line cap has to count
// MATCHED lines: teranode logs are ~75% DEBUG, so a tail of 500 raw lines yields only a
// couple of dozen at INFO+, which would make the page look empty on a busy node.
func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n, err := s.node(q.Get("node"))
	if err != nil {
		writeErrReason(w, http.StatusNotFound, reasonUnknownNode, err, nil)
		return
	}
	if s.d.Runtime.Kind() == "none" {
		writeErrReason(w, http.StatusServiceUnavailable, reasonNoRuntime, chaos.ErrNoRuntime,
			map[string]any{"runtime": "none"})
		return
	}

	filter, err := logs.ParseFilter(q.Get("level"), q.Get("service"), q.Get("grep"),
		q.Get("regex") == "1", q.Get("case") == "1")
	if err != nil {
		writeErrReason(w, http.StatusBadRequest, reasonBadRequest, err, nil)
		return
	}

	req := logs.Request{Filter: filter, Raw: q.Get("raw") == "1", Limit: clampInt(q.Get("limit"), logs.DefaultLimit, logs.MaxLines)}
	opt := chaos.LogOptions{Timestamps: true}

	if cs := q.Get("cursor"); cs != "" {
		cur, err := logs.DecodeCursor(cs)
		if err != nil {
			writeErrReason(w, http.StatusBadRequest, reasonBadRequest, err, nil)
			return
		}
		req.Cursor, req.HasCursor = cur, true
		opt.Since = cur.Since()
	} else if since := q.Get("since"); since != "" {
		d, err := parseSince(since)
		if err != nil {
			writeErrReason(w, http.StatusBadRequest, reasonBadRequest, err, nil)
			return
		}
		opt.Since = d
	} else {
		// Seeding by tail rather than by time keeps the first fetch bounded however long
		// the container has been running.
		opt.Tail = clampInt(q.Get("tail"), logs.DefaultTail, logs.MaxLines)
	}

	ctx, cancel := context.WithTimeout(r.Context(), logFetchTimeout)
	defer cancel()
	start := time.Now()
	res, err := s.d.Runtime.Logs(ctx, n.Container, opt)
	if err != nil {
		switch {
		case errors.Is(err, chaos.ErrNoRuntime):
			writeErrReason(w, http.StatusServiceUnavailable, reasonNoRuntime, err, map[string]any{"runtime": s.d.Runtime.Kind()})
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			writeErrReason(w, http.StatusGatewayTimeout, reasonTimeout,
				fmt.Errorf("log fetch timed out after %s", logFetchTimeout), nil)
		case strings.Contains(err.Error(), "no such container"), strings.Contains(err.Error(), "404"):
			writeErrReason(w, http.StatusBadGateway, reasonContainerMissing, err, map[string]any{"container": n.Container})
		default:
			writeErrReason(w, http.StatusBadGateway, reasonRuntimeError, err, map[string]any{"runtime": s.d.Runtime.Kind()})
		}
		return
	}

	page := logs.Build(res.Text, req)
	page.Node, page.Container, page.Runtime = n.Name, n.Container, s.d.Runtime.Kind()
	page.Truncated = res.Truncated
	page.FetchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	page.Elapsed = time.Since(start).Round(time.Millisecond).String()
	writeJSON(w, http.StatusOK, page)
}

// getDiagnostics explains why each node is in its current state.
func (s *Server) getDiagnostics(w http.ResponseWriter, r *http.Request) {
	if s.d.Diag == nil {
		writeErrReason(w, http.StatusServiceUnavailable, reasonRuntimeError,
			errors.New("diagnostics are not enabled"), nil)
		return
	}
	q := r.URL.Query()
	maxAge := 2 * time.Second
	if q.Get("refresh") == "1" {
		maxAge = 0
	} else if v := q.Get("maxAge"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeErrReason(w, http.StatusBadRequest, reasonBadRequest, err, nil)
			return
		}
		maxAge = d
	}
	// The log cross-reference costs one extra container read, so it is on for a single
	// node (where someone is looking at the detail) and off for a whole-fleet sweep.
	withLogs := q.Get("logs") != "0" && q.Get("node") != ""

	rep, err := s.d.Diag.Report(r.Context(), q.Get("node"), maxAge, withLogs)
	if err != nil {
		writeErrReason(w, http.StatusNotFound, reasonUnknownNode, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// Diagnoser is implemented by internal/diag; nil disables the endpoint.
type Diagnoser interface {
	Report(ctx context.Context, node string, maxAge time.Duration, withLogs bool) (diag.Report, error)
}

func clampInt(s string, def, max int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// parseSince accepts an RFC3339 instant or a relative duration like "10m".
func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("since must be RFC3339 or a duration like 10m: %q", s)
	}
	if d < 0 {
		d = -d
	}
	// A very wide window is capped: the read ceiling would truncate it anyway, and the
	// honest answer is a bounded window rather than a silently clipped one.
	if d > time.Hour {
		d = time.Hour
	}
	return time.Now().Add(-d), nil
}
