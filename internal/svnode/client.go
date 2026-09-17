// Package svnode is a thin JSON-RPC client for Bitcoin SV Node (bitcoind), covering what the
// harness needs to observe an SV node next to teranodes and to apply freezes to it the way
// go-alert-system does (addToConsensusBlacklist).
package svnode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/chaos-test/internal/alerts"
	"github.com/bsv-blockchain/chaos-test/internal/teranode"
)

// Client wraps the generic Bitcoin-style RPC client (same wire format as teranode's).
type Client struct {
	*teranode.RPCClient
}

// New returns a client for e.g. http://svnode1:18332.
func New(url, user, pass string) *Client {
	return &Client{RPCClient: teranode.NewRPCClient(url, user, pass)}
}

// BestBlockHeader returns the best header (getbestblockhash + getblockheader).
func (c *Client) BestBlockHeader(ctx context.Context) (*teranode.BlockHeader, error) {
	var hash string
	if err := c.Call(ctx, "getbestblockhash", nil, &hash); err != nil {
		return nil, err
	}
	return c.HeaderByHash(ctx, hash)
}

// HeaderByHash returns a verbose block header. The JSON field names match teranode's
// (hash, height, previousblockhash, time), so the same struct serves both node kinds.
func (c *Client) HeaderByHash(ctx context.Context, hash string) (*teranode.BlockHeader, error) {
	var h teranode.BlockHeader
	if err := c.Call(ctx, "getblockheader", []any{hash, true}, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// BlockHash returns the main-chain block hash at a height.
func (c *Client) BlockHash(ctx context.Context, height uint32) (string, error) {
	var hash string
	err := c.Call(ctx, "getblockhash", []any{height}, &hash)
	return hash, err
}

// Block is getblock with verbosity 1.
type Block struct {
	Hash   string   `json:"hash"`
	Height uint32   `json:"height"`
	Time   int64    `json:"time"`
	Tx     []string `json:"tx"`
}

// Block fetches a block's txids by hash.
func (c *Client) Block(ctx context.Context, hash string) (*Block, error) {
	var b Block
	if err := c.Call(ctx, "getblock", []any{hash, 1}, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// RawTx is the verbose getrawtransaction document (needs txindex=1 for mined txs).
type RawTx struct {
	TxID          string `json:"txid"`
	Hex           string `json:"hex"`
	BlockHash     string `json:"blockhash"`
	Confirmations int64  `json:"confirmations"`
	Vin           []struct {
		Coinbase string `json:"coinbase"`
	} `json:"vin"`
	Vout []struct {
		Value        float64 `json:"value"`
		N            uint32  `json:"n"`
		ScriptPubKey struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	} `json:"vout"`
}

// RawTransaction fetches a transaction; an unknown txid is an error.
func (c *Client) RawTransaction(ctx context.Context, txid string) (*RawTx, error) {
	var t RawTx
	if err := c.Call(ctx, "getrawtransaction", []any{txid, 1}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// TxOut is one gettxout result.
type TxOut struct {
	Value         float64 `json:"value"`
	Confirmations int64   `json:"confirmations"`
	Coinbase      bool    `json:"coinbase"`
	ScriptPubKey  struct {
		Hex string `json:"hex"`
	} `json:"scriptPubKey"`
}

// TxOut returns the output if it is unspent (mempool spends count as spent), nil otherwise.
func (c *Client) TxOut(ctx context.Context, txid string, vout uint32) (*TxOut, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "gettxout", []any{txid, vout, true}, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var o TxOut
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// Satoshis converts a BSV amount to satoshis.
func Satoshis(v float64) uint64 { return uint64(v*1e8 + 0.5) }

// Peer is one getpeerinfo row.
type Peer struct {
	Addr           string `json:"addr"`
	Inbound        bool   `json:"inbound"`
	SubVer         string `json:"subver"`
	StartingHeight int64  `json:"startingheight"`
	SyncedHeaders  int64  `json:"synced_headers"`
	SyncedBlocks   int64  `json:"synced_blocks"`
}

// PeerInfo lists the node's peers.
func (c *Client) PeerInfo(ctx context.Context) ([]Peer, error) {
	var peers []Peer
	err := c.Call(ctx, "getpeerinfo", nil, &peers)
	return peers, err
}

// SubVersion returns getnetworkinfo.subversion, e.g. "/Bitcoin SV:1.2.2/".
func (c *Client) SubVersion(ctx context.Context) (string, error) {
	var ni struct {
		SubVersion string `json:"subversion"`
	}
	if err := c.Call(ctx, "getnetworkinfo", nil, &ni); err != nil {
		return "", err
	}
	return strings.Trim(ni.SubVersion, "/"), nil
}

// BlacklistFund is one entry of queryBlacklist.
type BlacklistFund struct {
	TxOut struct {
		TxID string `json:"txId"`
		Vout uint32 `json:"vout"`
	} `json:"txOut"`
	EnforceAtHeight []struct {
		Start uint64 `json:"start"`
		Stop  uint64 `json:"stop"`
	} `json:"enforceAtHeight"`
	PolicyExpiresWithConsensus bool     `json:"policyExpiresWithConsensus"`
	Blacklist                  []string `json:"blacklist"`
}

// QueryBlacklist lists the node's frozen outputs (policy and consensus blacklists).
func (c *Client) QueryBlacklist(ctx context.Context) ([]BlacklistFund, error) {
	var out struct {
		Funds []BlacklistFund `json:"funds"`
	}
	if err := c.Call(ctx, "queryBlacklist", nil, &out); err != nil {
		return nil, err
	}
	return out.Funds, nil
}

// FrozenAt reports whether an outpoint is on the consensus blacklist at height h: a fund with
// no window is frozen everywhere, otherwise some window must contain h ([start, stop), stop 0
// or missing meaning open-ended).
func FrozenAt(funds []BlacklistFund, txid string, vout uint32, h uint64) bool {
	for _, f := range funds {
		if !strings.EqualFold(f.TxOut.TxID, txid) || f.TxOut.Vout != vout {
			continue
		}
		if len(f.EnforceAtHeight) == 0 {
			return true
		}
		for _, w := range f.EnforceAtHeight {
			if h >= w.Start && (w.Stop == 0 || h < w.Stop) {
				return true
			}
		}
	}
	return false
}

// ClearBlacklists removes every blacklist entry and returns how many were removed.
func (c *Client) ClearBlacklists(ctx context.Context) (int, error) {
	var out struct {
		NumRemovedEntries int `json:"numRemovedEntries"`
	}
	err := c.Call(ctx, "clearBlacklists", []any{map[string]any{"removeAllEntries": true}}, &out)
	return out.NumRemovedEntries, err
}

// AddToConsensusBlacklist freezes funds the way go-alert-system does for a freeze alert.
// A fund with start == stop == 0 is sent without a window (frozen at every height).
func (c *Client) AddToConsensusBlacklist(ctx context.Context, funds []alerts.Fund) error {
	list := make([]map[string]any, 0, len(funds))
	for _, f := range funds {
		entry := map[string]any{
			"txOut":                      map[string]any{"txId": f.TxID, "vout": f.Vout},
			"policyExpiresWithConsensus": f.PolicyExpiresWithConsensus,
		}
		if f.EnforceAtHeightStart != 0 || f.EnforceAtHeightStop != 0 {
			w := map[string]any{"start": f.EnforceAtHeightStart}
			if f.EnforceAtHeightStop != 0 {
				w["stop"] = f.EnforceAtHeightStop
			}
			entry["enforceAtHeight"] = []map[string]any{w}
		}
		list = append(list, entry)
	}
	var out struct {
		NotProcessed []json.RawMessage `json:"notProcessed"`
	}
	if err := c.Call(ctx, "addToConsensusBlacklist", []any{map[string]any{"funds": list}}, &out); err != nil {
		return err
	}
	if len(out.NotProcessed) > 0 {
		return fmt.Errorf("addToConsensusBlacklist: %d fund(s) not processed: %s", len(out.NotProcessed), out.NotProcessed[0])
	}
	return nil
}
