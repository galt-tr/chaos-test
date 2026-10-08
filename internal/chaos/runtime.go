// Package chaos drives the container runtime for fault injection: network partitions
// (connect/disconnect a container from a compose network with its pinned IP), pause,
// stop and start. It speaks the Docker Engine API over the engine's unix socket (docker, or
// podman, which serves the same API) and falls back to the docker/podman CLI when no socket
// is available (e.g. running on the host).
package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runtime is what the harness needs from the container engine.
type Runtime interface {
	NetworkDisconnect(ctx context.Context, network, container string) error
	NetworkConnect(ctx context.Context, network, container, ip string) error
	Pause(ctx context.Context, container string) error
	Unpause(ctx context.Context, container string) error
	Stop(ctx context.Context, container string) error
	Start(ctx context.Context, container string) error
	State(ctx context.Context, container string) (string, error)
	// Networks lists the networks a container is currently attached to.
	Networks(ctx context.Context, container string) ([]string, error)
	// Logs returns a window of the container's stdout+stderr.
	Logs(ctx context.Context, container string, opt LogOptions) (LogResult, error)
	Kind() string
}

// LogOptions selects a window of a container's log. Since and Tail are alternatives: the
// runtime applies them in an order that is not contractually specified, so callers pick one.
type LogOptions struct {
	Since      time.Time // zero = from the start of the log
	Tail       int       // >0 = last N lines; ignored when Since is set
	Timestamps bool      // prefix each line with the runtime's own clock
	MaxBytes   int       // read ceiling; 0 = DefaultMaxLogBytes
}

// LogResult is the window, plus what did not fit in it.
type LogResult struct {
	Text      string
	Bytes     int
	Truncated bool // MaxBytes stopped the read before the end of the window
}

// DefaultMaxLogBytes bounds a single log read. Teranode writes ~30 lines/s, so an
// unbounded window on a long-running container is tens of megabytes.
const DefaultMaxLogBytes = 8 << 20

// apiBase is the unversioned API root; the daemon then applies its own current version.
// Docker Engine 29.0-29.2 refuse requests pinned below v1.44 (29.3 lowered the floor to
// v1.40) and podman accepts any version prefix or none, so leaving the version out is the
// one form every engine answers.
const apiBase = "http://d"

// New picks the engine socket when socketPath (or DOCKER_HOST, or a well-known docker/podman
// socket) is a unix socket, else the docker/podman CLI, else a runtime that reports its
// absence and why (see Reason).
func New(socketPath string) Runtime {
	path, reason := firstSocket(socketCandidates(socketPath, os.Getenv))
	if path != "" {
		return newSocketRuntime(path)
	}
	for _, bin := range cliOrder(os.Getenv("RUNTIME")) {
		if _, err := exec.LookPath(bin); err == nil {
			return &cliRuntime{bin: bin}
		}
	}
	return &noRuntime{reason: reason}
}

// socketCandidates lists the socket paths to try, most specific first: the explicit path,
// DOCKER_HOST (unix:// only), then the well-known docker and podman sockets, rootless and
// root. Empty entries (an unset XDG_RUNTIME_DIR) and duplicates are dropped.
func socketCandidates(explicit string, getenv func(string) string) []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, q := range out {
			if q == p {
				return
			}
		}
		out = append(out, p)
	}
	add(explicit)
	if h := getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		add(strings.TrimPrefix(h, "unix://"))
	}
	add("/var/run/docker.sock")
	if x := getenv("XDG_RUNTIME_DIR"); x != "" {
		add(x + "/docker.sock")
		add(x + "/podman/podman.sock")
	}
	add("/run/podman/podman.sock")
	return out
}

// firstSocket returns the first candidate that is a unix socket. When none is, reason says
// why. A candidate that exists but is not a socket is the interesting case (docker creates
// a directory at a bind-mount source that is missing on the host), so it is reported over
// plain absences.
func firstSocket(candidates []string) (path, reason string) {
	var absent []string
	for _, p := range candidates {
		fi, err := os.Stat(p)
		switch {
		case err != nil:
			absent = append(absent, p)
		case fi.Mode()&os.ModeSocket != 0:
			return p, ""
		case reason != "": // the first finding is the one worth reporting
		case fi.IsDir():
			reason = p + " is a directory, not a socket: the engine socket was not bind-mounted from the host (set CONTAINER_SOCKET to the host's docker/podman socket and recreate the orchestrator)"
		default:
			reason = p + " is not a unix socket"
		}
	}
	if reason == "" {
		reason = "no engine socket at " + strings.Join(absent, ", ")
	}
	return "", reason
}

// cliOrder prefers the engine the user asked for (RUNTIME=docker|podman, the Makefiles'
// variable); otherwise podman first, matching the Makefiles' own detection.
func cliOrder(runtime string) []string {
	if runtime == "docker" {
		return []string{"docker", "podman"}
	}
	return []string{"podman", "docker"}
}

func newSocketRuntime(path string) *socketRuntime {
	return &socketRuntime{path: path, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}, Timeout: 60 * time.Second}}
}

// ---- Docker-compatible API over a unix socket -------------------------------------------

type socketRuntime struct {
	path string
	http *http.Client
}

func (r *socketRuntime) Kind() string { return "socket:" + r.path }

func (r *socketRuntime) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return b, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (r *socketRuntime) NetworkDisconnect(ctx context.Context, network, container string) error {
	_, err := r.do(ctx, http.MethodPost, "/networks/"+network+"/disconnect", map[string]any{"Container": container, "Force": true})
	return err
}

func (r *socketRuntime) NetworkConnect(ctx context.Context, network, container, ip string) error {
	body := map[string]any{"Container": container}
	if ip != "" {
		body["EndpointConfig"] = map[string]any{"IPAMConfig": map[string]any{"IPv4Address": ip}}
	}
	_, err := r.do(ctx, http.MethodPost, "/networks/"+network+"/connect", body)
	return err
}

func (r *socketRuntime) Pause(ctx context.Context, c string) error {
	_, err := r.do(ctx, http.MethodPost, "/containers/"+c+"/pause", nil)
	return err
}

func (r *socketRuntime) Unpause(ctx context.Context, c string) error {
	_, err := r.do(ctx, http.MethodPost, "/containers/"+c+"/unpause", nil)
	return err
}

func (r *socketRuntime) Stop(ctx context.Context, c string) error {
	_, err := r.do(ctx, http.MethodPost, "/containers/"+c+"/stop?t=10", nil)
	return err
}

func (r *socketRuntime) Start(ctx context.Context, c string) error {
	_, err := r.do(ctx, http.MethodPost, "/containers/"+c+"/start", nil)
	return err
}

func (r *socketRuntime) State(ctx context.Context, c string) (string, error) {
	b, err := r.do(ctx, http.MethodGet, "/containers/"+c+"/json", nil)
	if err != nil {
		return "", err
	}
	var doc struct {
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", err
	}
	return doc.State.Status, nil
}

func (r *socketRuntime) Logs(ctx context.Context, c string, opt LogOptions) (LogResult, error) {
	b, truncated, err := r.getLimited(ctx, "/containers/"+c+"/logs"+logQuery(opt), maxBytes(opt))
	if err != nil {
		return LogResult{}, err
	}
	text := demuxLogStream(b)
	if truncated {
		// The read stopped mid-stream, so the final line is very likely a fragment.
		// Dropping it is better than handing the parser half a record.
		if i := strings.LastIndexByte(text, '\n'); i >= 0 {
			text = text[:i+1]
		}
	}
	return LogResult{Text: text, Bytes: len(b), Truncated: truncated}, nil
}

// logQuery builds the log query string. Exactly one of tail/since is ever emitted.
func logQuery(opt LogOptions) string {
	q := "?stdout=true&stderr=true"
	if opt.Timestamps {
		q += "&timestamps=true"
	}
	switch {
	case !opt.Since.IsZero():
		q += fmt.Sprintf("&since=%d.%09d", opt.Since.Unix(), opt.Since.Nanosecond())
	case opt.Tail > 0:
		q += fmt.Sprintf("&tail=%d", opt.Tail)
	}
	return q
}

func maxBytes(opt LogOptions) int {
	if opt.MaxBytes > 0 {
		return opt.MaxBytes
	}
	return DefaultMaxLogBytes
}

// getLimited reads at most max bytes, reporting whether the body was longer. The runtime
// streams oldest-first, so hitting the ceiling loses the NEWEST lines — which is why the
// caller must not advance its cursor past what it actually parsed.
func (r *socketRuntime) getLimited(ctx context.Context, path string, max int) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode >= 300 {
		return b, false, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if len(b) > max {
		return b[:max], true, nil
	}
	return b, false, nil
}

// demuxLogStream strips the 8-byte frame headers of Docker's multiplexed log stream
// (stream type, 3 zero bytes, big-endian payload length). A body that does not start with
// a valid header (TTY containers) is returned unchanged.
func demuxLogStream(b []byte) string {
	var out bytes.Buffer
	for len(b) > 0 {
		if len(b) < 8 || b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
			if out.Len() == 0 {
				return string(b)
			}
			out.Write(b)
			break
		}
		n := int(b[4])<<24 | int(b[5])<<16 | int(b[6])<<8 | int(b[7])
		b = b[8:]
		if n > len(b) {
			n = len(b)
		}
		out.Write(b[:n])
		b = b[n:]
	}
	return out.String()
}

func (r *socketRuntime) Networks(ctx context.Context, c string) ([]string, error) {
	b, err := r.do(ctx, http.MethodGet, "/containers/"+c+"/json", nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		NetworkSettings struct {
			Networks map[string]any `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.NetworkSettings.Networks))
	for n := range doc.NetworkSettings.Networks {
		out = append(out, n)
	}
	return out, nil
}

// ---- CLI fallback -----------------------------------------------------------------------

type cliRuntime struct{ bin string }

func (r *cliRuntime) Kind() string { return "cli:" + r.bin }

func (r *cliRuntime) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", r.bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *cliRuntime) NetworkDisconnect(ctx context.Context, network, container string) error {
	_, err := r.run(ctx, "network", "disconnect", "-f", network, container)
	return err
}

func (r *cliRuntime) NetworkConnect(ctx context.Context, network, container, ip string) error {
	args := []string{"network", "connect"}
	if ip != "" {
		args = append(args, "--ip", ip)
	}
	_, err := r.run(ctx, append(args, network, container)...)
	return err
}

func (r *cliRuntime) Pause(ctx context.Context, c string) error {
	_, err := r.run(ctx, "pause", c)
	return err
}
func (r *cliRuntime) Unpause(ctx context.Context, c string) error {
	_, err := r.run(ctx, "unpause", c)
	return err
}
func (r *cliRuntime) Stop(ctx context.Context, c string) error {
	_, err := r.run(ctx, "stop", "-t", "10", c)
	return err
}
func (r *cliRuntime) Start(ctx context.Context, c string) error {
	_, err := r.run(ctx, "start", c)
	return err
}
func (r *cliRuntime) State(ctx context.Context, c string) (string, error) {
	return r.run(ctx, "inspect", "-f", "{{.State.Status}}", c)
}

func (r *cliRuntime) Logs(ctx context.Context, c string, opt LogOptions) (LogResult, error) {
	args := []string{"logs"}
	if opt.Timestamps {
		args = append(args, "--timestamps")
	}
	switch {
	case !opt.Since.IsZero():
		args = append(args, "--since", opt.Since.UTC().Format(time.RFC3339Nano))
	case opt.Tail > 0:
		args = append(args, "--tail", strconv.Itoa(opt.Tail))
	}
	// Deliberately not r.run: that merges stderr into stdout, which would splice the
	// runtime's own diagnostics into the container's log as phantom lines.
	cmd := exec.CommandContext(ctx, r.bin, append(args, c)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return LogResult{}, fmt.Errorf("%s logs %s: %w: %s", r.bin, c, err, strings.TrimSpace(stderr.String()))
	}
	text := stdout.String()
	max := maxBytes(opt)
	truncated := len(text) > max
	if truncated {
		text = text[len(text)-max:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return LogResult{Text: text, Bytes: stdout.Len(), Truncated: truncated}, nil
}

func (r *cliRuntime) Networks(ctx context.Context, c string) ([]string, error) {
	out, err := r.run(ctx, "inspect", "-f", "{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}", c)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// ---- no runtime -------------------------------------------------------------------------

// ErrNoRuntime is returned by every operation when no container engine could be found.
// Callers match it with errors.Is to report the cause precisely rather than by string.
var ErrNoRuntime = errors.New("no container runtime available (bind-mount the engine's API socket at /var/run/docker.sock, or run where docker or podman is installed)")

// noRuntime stands in when neither a socket nor a CLI was found; reason says what was
// checked, for the startup log and the UI.
type noRuntime struct{ reason string }

func (r noRuntime) err() error {
	if r.reason == "" {
		return ErrNoRuntime
	}
	return fmt.Errorf("%w: %s", ErrNoRuntime, r.reason)
}

// Kind is exactly "none": the API branches on that value.
func (noRuntime) Kind() string                                                   { return "none" }
func (r noRuntime) Reason() string                                               { return r.reason }
func (r noRuntime) NetworkDisconnect(context.Context, string, string) error      { return r.err() }
func (r noRuntime) NetworkConnect(context.Context, string, string, string) error { return r.err() }
func (r noRuntime) Pause(context.Context, string) error                          { return r.err() }
func (r noRuntime) Unpause(context.Context, string) error                        { return r.err() }
func (r noRuntime) Stop(context.Context, string) error                           { return r.err() }
func (r noRuntime) Start(context.Context, string) error                          { return r.err() }
func (r noRuntime) State(context.Context, string) (string, error)                { return "", r.err() }
func (r noRuntime) Networks(context.Context, string) ([]string, error)           { return nil, r.err() }
func (r noRuntime) Logs(context.Context, string, LogOptions) (LogResult, error) {
	return LogResult{}, r.err()
}

// Reason says why rt found no engine; "" for a working runtime.
func Reason(rt Runtime) string {
	if r, ok := rt.(interface{ Reason() string }); ok {
		return r.Reason()
	}
	return ""
}
