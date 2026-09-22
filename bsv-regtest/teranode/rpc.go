// Package teranode holds thin clients for a teranode's JSON-RPC and asset HTTP APIs.
package teranode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// RPCClient talks to teranode's Bitcoin-style JSON-RPC (basic auth).
type RPCClient struct {
	URL, User, Pass string
	HTTP            *http.Client
}

// NewRPCClient returns a client for e.g. http://teranode1:9292.
func NewRPCClient(url, user, pass string) *RPCClient {
	return &RPCClient{URL: url, User: user, Pass: pass, HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Call invokes method with params and decodes the result into out (may be nil).
func (c *RPCClient) Call(ctx context.Context, method string, params []any, out any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": "bsv-regtest", "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: HTTP %d, bad JSON-RPC response: %s", method, resp.StatusCode, truncate(raw))
	}
	if env.Error != nil {
		return env.Error
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Generate mines n blocks (regtest) and returns their hashes.
func (c *RPCClient) Generate(ctx context.Context, n int) ([]string, error) {
	var hashes []string
	err := c.Call(ctx, "generate", []any{n}, &hashes)
	return hashes, err
}

// GenerateToAddress mines n blocks paying the coinbase to address.
func (c *RPCClient) GenerateToAddress(ctx context.Context, n int, address string) ([]string, error) {
	var hashes []string
	err := c.Call(ctx, "generatetoaddress", []any{n, address}, &hashes)
	return hashes, err
}

// ChainInfo is the subset of getblockchaininfo we use (note: cached ~10 s server-side).
type ChainInfo struct {
	Blocks        uint32 `json:"blocks"`
	BestBlockHash string `json:"bestblockhash"`
	Chain         string `json:"chain"`
}

// GetBlockchainInfo calls getblockchaininfo.
func (c *RPCClient) GetBlockchainInfo(ctx context.Context) (*ChainInfo, error) {
	var ci ChainInfo
	err := c.Call(ctx, "getblockchaininfo", nil, &ci)
	return &ci, err
}

// SendRawTransaction submits hex via sendrawtransaction and returns the txid.
func (c *RPCClient) SendRawTransaction(ctx context.Context, txHex string) (string, error) {
	var txid string
	err := c.Call(ctx, "sendrawtransaction", []any{txHex}, &txid)
	return txid, err
}

// GetRawMempool returns the txids in the node's mempool (block assembly view).
func (c *RPCClient) GetRawMempool(ctx context.Context) ([]string, error) {
	var ids []string
	err := c.Call(ctx, "getrawmempool", nil, &ids)
	return ids, err
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

// Version returns the node's version string from the version RPC.
func (c *RPCClient) Version(ctx context.Context) (string, error) {
	var out map[string]any
	if err := c.Call(ctx, "version", nil, &out); err != nil {
		return "", err
	}
	for _, k := range []string{"version", "Version"} {
		if v, ok := out[k]; ok {
			return fmt.Sprint(v), nil
		}
	}
	for k, v := range out {
		if m, ok := v.(map[string]any); ok {
			if vs, ok := m["versionstring"]; ok {
				return fmt.Sprint(vs), nil
			}
			return k + ":" + fmt.Sprint(m["major"]), nil
		}
	}
	return fmt.Sprint(out), nil
}

// ChainTip is one entry of getchaintips (server-cached ~5 min in teranode).
type ChainTip struct {
	Height    uint32 `json:"height"`
	Hash      string `json:"hash"`
	BranchLen uint32 `json:"branchlen"`
	Status    string `json:"status"`
}

// GetChainTips calls getchaintips.
func (c *RPCClient) GetChainTips(ctx context.Context) ([]ChainTip, error) {
	var tips []ChainTip
	err := c.Call(ctx, "getchaintips", nil, &tips)
	return tips, err
}

// InvalidateBlock calls invalidateblock.
func (c *RPCClient) InvalidateBlock(ctx context.Context, hash string) error {
	return c.Call(ctx, "invalidateblock", []any{hash}, nil)
}

// ReconsiderBlock calls reconsiderblock.
func (c *RPCClient) ReconsiderBlock(ctx context.Context, hash string) error {
	return c.Call(ctx, "reconsiderblock", []any{hash}, nil)
}

// Freeze calls the admin freeze RPC (PR #1764 adds the optional window arguments).
func (c *RPCClient) Freeze(ctx context.Context, txid string, vout uint32, start, stop *uint64, policyExpires bool) error {
	params := []any{txid, vout, ""}
	if start != nil || stop != nil {
		var s, e uint64
		if start != nil {
			s = *start
		}
		if stop != nil {
			e = *stop
		}
		params = append(params, s, e, policyExpires)
	}
	return c.Call(ctx, "freeze", params, nil)
}

// Unfreeze calls the admin unfreeze RPC.
func (c *RPCClient) Unfreeze(ctx context.Context, txid string, vout uint32) error {
	return c.Call(ctx, "unfreeze", []any{txid, vout, ""}, nil)
}
