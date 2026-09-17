// Package chaos drives the container runtime for fault injection: network partitions
// (connect/disconnect a container from a compose network with its pinned IP), pause,
// stop and start. It speaks the Docker-compatible REST API over the podman socket, and
// falls back to the podman CLI when no socket is available (e.g. running on the host).
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

// New picks the socket API if socketPath (or DOCKER_HOST, or a well-known socket) exists,
// else the podman/docker CLI, else a runtime that reports its absence.
func New(socketPath string) Runtime {
	if socketPath == "" {
		socketPath = strings.TrimPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	}
	if socketPath == "" {
		for _, p := range []string{"/var/run/docker.sock", os.Getenv("XDG_RUNTIME_DIR") + "/podman/podman.sock"} {
			if _, err := os.Stat(p); err == nil {
				socketPath = p
				break
			}
		}
	}
	if socketPath != "" {
		if _, err := os.Stat(socketPath); err == nil {
			return &socketRuntime{path: socketPath, http: &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				}}, Timeout: 60 * time.Second}}
		}
	}
	if _, err := exec.LookPath("podman"); err == nil {
		return &cliRuntime{bin: "podman"}
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return &cliRuntime{bin: "docker"}
	}
	return &noRuntime{}
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
	req, err := http.NewRequestWithContext(ctx, method, "http://d/v1.41"+path, rd)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://d/v1.41"+path, nil)
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

type noRuntime struct{}

// ErrNoRuntime is returned by every operation when no container engine could be found.
// Callers match it with errors.Is to report the cause precisely rather than by string.
var ErrNoRuntime = errors.New("no container runtime available (mount the podman socket or install podman)")

var errNoRuntime = ErrNoRuntime

func (noRuntime) Kind() string                                                 { return "none" }
func (noRuntime) NetworkDisconnect(context.Context, string, string) error      { return errNoRuntime }
func (noRuntime) NetworkConnect(context.Context, string, string, string) error { return errNoRuntime }
func (noRuntime) Pause(context.Context, string) error                          { return errNoRuntime }
func (noRuntime) Unpause(context.Context, string) error                        { return errNoRuntime }
func (noRuntime) Stop(context.Context, string) error                           { return errNoRuntime }
func (noRuntime) Start(context.Context, string) error                          { return errNoRuntime }
func (noRuntime) State(context.Context, string) (string, error)                { return "", errNoRuntime }
func (noRuntime) Networks(context.Context, string) ([]string, error)           { return nil, errNoRuntime }
func (noRuntime) Logs(context.Context, string, LogOptions) (LogResult, error) {
	return LogResult{}, errNoRuntime
}
