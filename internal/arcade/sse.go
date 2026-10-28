package arcade

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SSEFrame is one status frame received on GET /events.
type SSEFrame struct {
	ID     string    `json:"id"`
	Event  string    `json:"event"`
	TxID   string    `json:"txid"`
	Status string    `json:"txStatus"`
	At     time.Time `json:"at"`
}

// SSEListener holds one token-scoped subscription to arcade's /events stream and keeps every
// frame it receives. Scenarios use it to assert what a subscriber was actually told, which is a
// different question from what GET /tx answers afterwards: a status that was never pushed is
// invisible to a wallet that only listens.
type SSEListener struct {
	Token  string
	cancel context.CancelFunc

	mu     sync.Mutex
	frames []SSEFrame
	err    error
	open   bool
}

// ListenSSE connects to <eventsURL>/events?callbackToken=<token> and records frames until ctx
// ends or Close is called. It returns once the response headers have arrived, i.e. once the
// subscription is registered, so a submit issued afterwards cannot race it.
func ListenSSE(ctx context.Context, eventsURL, token string) (*SSEListener, error) {
	if eventsURL == "" {
		return nil, fmt.Errorf("arcade events URL not configured")
	}
	ctx, cancel := context.WithCancel(ctx)
	u := strings.TrimRight(eventsURL, "/") + "/events?callbackToken=" + url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	// No client timeout: this is a stream. ctx bounds its lifetime.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	l := &SSEListener{Token: token, cancel: cancel, open: true}
	go l.read(resp.Body)
	return l, nil
}

func (l *SSEListener) read(body io.ReadCloser) {
	defer body.Close()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var id, event string
	var data strings.Builder
	flush := func() {
		if data.Len() > 0 {
			var doc struct {
				TxID   string `json:"txid"`
				Status string `json:"txStatus"`
			}
			if json.Unmarshal([]byte(data.String()), &doc) == nil && doc.TxID != "" {
				l.mu.Lock()
				l.frames = append(l.frames, SSEFrame{ID: id, Event: event, TxID: doc.TxID, Status: doc.Status, At: time.Now()})
				l.mu.Unlock()
			}
		}
		id, event = "", ""
		data.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"): // comment / keepalive
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(line[3:])
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	l.mu.Lock()
	l.err = sc.Err()
	l.open = false
	l.mu.Unlock()
}

// Frames returns the frames received for txid so far ("" = every frame), in arrival order.
func (l *SSEListener) Frames(txid string) []SSEFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []SSEFrame
	for _, f := range l.frames {
		if txid == "" || f.TxID == txid {
			out = append(out, f)
		}
	}
	return out
}

// Statuses lists the statuses pushed for txid, in arrival order.
func (l *SSEListener) Statuses(txid string) []string {
	var out []string
	for _, f := range l.Frames(txid) {
		out = append(out, f.Status)
	}
	return out
}

// Seen reports whether a frame carrying txid at status has arrived.
func (l *SSEListener) Seen(txid, status string) bool {
	for _, f := range l.Frames(txid) {
		if f.Status == status {
			return true
		}
	}
	return false
}

// Open reports whether the stream is still connected, and the read error if it ended.
func (l *SSEListener) Open() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open, l.err
}

// Close ends the subscription.
func (l *SSEListener) Close() { l.cancel() }
