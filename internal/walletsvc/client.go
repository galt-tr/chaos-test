package walletsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Error carries a machine-readable reason alongside the message.
type Error struct {
	Reason string
	Msg    string
	Status int
}

func (e *Error) Error() string { return e.Msg }

// ReasonOf extracts the reason from an error, or "" if it carries none.
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// IsInsufficientFunds reports an empty coin pool. This is backpressure — the send should wait,
// not record a failure.
func IsInsufficientFunds(err error) bool {
	if ReasonOf(err) == ReasonInsufficient {
		return true
	}
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not enough funds") || strings.Contains(m, "insufficient") ||
		strings.Contains(m, "no spendable")
}

// Client talks to the walletd sidecar.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a client. It does not dial — walletd may legitimately start later.
func NewClient(base string) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 90 * time.Second}}
}

// WalletdHealth is walletd's own /healthz.
type WalletdHealth struct {
	OK          bool   `json:"ok"`
	Connected   bool   `json:"connected"`
	Attempts    int    `json:"attempts"`
	LastError   string `json:"lastError,omitempty"`
	StorageURL  string `json:"storageURL"`
	Network     string `json:"network"`
	ConnectedAt string `json:"connectedAt,omitempty"`
}

func (c *Client) Health(ctx context.Context) (*WalletdHealth, error) {
	var h WalletdHealth
	return &h, c.do(ctx, http.MethodGet, "/healthz", nil, &h)
}

func (c *Client) Deposit(ctx context.Context) (*Deposit, error) {
	var d Deposit
	return &d, c.do(ctx, http.MethodGet, "/v1/deposit", nil, &d)
}

// WalletdState is walletd's /v1/state.
type WalletdState struct {
	Connected   bool   `json:"connected"`
	Network     string `json:"network"`
	IdentityKey string `json:"identityKey"`
	Address     string `json:"address"`
	Balance     uint64 `json:"balance"`
	Coins       uint32 `json:"coins"`
}

func (c *Client) State(ctx context.Context) (*WalletdState, error) {
	var s WalletdState
	return &s, c.do(ctx, http.MethodGet, "/v1/state", nil, &s)
}

func (c *Client) Outputs(ctx context.Context, limit int) ([]Coin, uint32, error) {
	var out struct {
		Outputs []Coin `json:"outputs"`
		Total   uint32 `json:"total"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/outputs?limit="+strconv.Itoa(limit), nil, &out)
	return out.Outputs, out.Total, err
}

// ActionsResult is walletd's /v1/actions, bucketed wallet truth plus the sampled rows.
type ActionsResult struct {
	Health
	Actions []struct {
		TxID        string   `json:"txid"`
		Status      string   `json:"status"`
		Satoshis    int64    `json:"satoshis"`
		Description string   `json:"description"`
		Labels      []string `json:"labels"`
	} `json:"actions"`
}

func (c *Client) Actions(ctx context.Context, labels []string, limit int, include bool) (*ActionsResult, error) {
	q := url.Values{}
	if len(labels) > 0 {
		q.Set("labels", strings.Join(labels, ","))
	}
	q.Set("limit", strconv.Itoa(limit))
	if !include {
		q.Set("include", "0")
	}
	var r ActionsResult
	return &r, c.do(ctx, http.MethodGet, "/v1/actions?"+q.Encode(), nil, &r)
}

// WalletdTxRequest is walletd's build request.
type WalletdTxRequest struct {
	Shape       string   `json:"shape"`
	Satoshis    uint64   `json:"satoshis"`
	To          string   `json:"to,omitempty"`
	Outputs     int      `json:"outputs,omitempty"`
	Data        string   `json:"data,omitempty"`
	DataHex     string   `json:"dataHex,omitempty"`
	Script      string   `json:"script,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Description string   `json:"description,omitempty"`
	NoSend      bool     `json:"noSend"`
	Delayed     bool     `json:"delayed"`
}

// WalletdTxResult is walletd's build response.
type WalletdTxResult struct {
	TxID     string   `json:"txid"`
	Status   string   `json:"status"`
	Satoshis uint64   `json:"satoshis"`
	RawHex   string   `json:"rawHex"`
	EFHex    string   `json:"efHex"`
	Labels   []string `json:"labels"`
	NoSend   bool     `json:"noSend"`
}

func (c *Client) BuildTx(ctx context.Context, req WalletdTxRequest) (*WalletdTxResult, error) {
	var r WalletdTxResult
	return &r, c.do(ctx, http.MethodPost, "/v1/tx", req, &r)
}

// InternalizeRequest credits a proven payment.
type InternalizeRequest struct {
	AtomicBeefHex   string `json:"atomicBeefHex"`
	ExpectedAddress string `json:"expectedAddress"`
	OutputIndex     uint32 `json:"outputIndex"`
	Description     string `json:"description,omitempty"`
}

// InternalizeResult reports the credit.
type InternalizeResult struct {
	Accepted    bool   `json:"accepted"`
	OutputIndex uint32 `json:"outputIndex"`
	Balance     uint64 `json:"balance"`
	Coins       uint32 `json:"coins"`
}

func (c *Client) Internalize(ctx context.Context, req InternalizeRequest) (*InternalizeResult, error) {
	var r InternalizeResult
	return &r, c.do(ctx, http.MethodPost, "/v1/internalize", req, &r)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if c.BaseURL == "" {
		return &Error{Reason: ReasonDisabled, Msg: "no walletd URL configured"}
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &Error{Reason: ReasonTimeout, Msg: err.Error()}
		}
		// walletd not answering at all: a distinct condition from the wallet being up but
		// unable to reach storage, and the operator needs to tell them apart.
		return &Error{Reason: ReasonUnavailable, Msg: fmt.Sprintf("walletd unreachable: %v", err)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &e)
		reason := mapReason(resp.StatusCode, e.Reason)
		msg := e.Error
		if msg == "" {
			msg = fmt.Sprintf("walletd HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return &Error{Reason: reason, Msg: msg, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func mapReason(status int, reason string) string {
	switch reason {
	case "not_connected":
		return ReasonNotConnected
	case "insufficient_funds":
		return ReasonInsufficient
	case "bad_request":
		return ReasonBadRequest
	}
	if status == http.StatusServiceUnavailable {
		return ReasonNotConnected
	}
	return ReasonUpstream
}
