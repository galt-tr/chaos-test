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

// AssetClient talks to teranode's asset HTTP API (prefix /api/v1). These reads are not
// server-cached the way the JSON-RPC reads are, so observation goes through here.
type AssetClient struct {
	BaseURL string // e.g. http://teranode1:8090/api/v1
	HTTP    *http.Client
}

// NewAssetClient returns a client for the given base URL (including /api/v1).
func NewAssetClient(baseURL string) *AssetClient {
	return &AssetClient{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// FreezeRecord is one per-output freeze window recorded on the PR branch.
type FreezeRecord struct {
	From          uint32 `json:"from"`
	Until         uint32 `json:"until"`
	PolicyExpires bool   `json:"policyExpires"`
}

// TxMeta is the subset of /txmeta/:hash/json the harness inspects.
type TxMeta struct {
	Frozen        bool                    `json:"frozen"`
	FreezeRecords map[uint32]FreezeRecord `json:"freezeRecords"`
	BlockIDs      []uint32                `json:"blockIDs"`
	BlockHeights  []uint32                `json:"blockHeights"`
	IsCoinbase    bool                    `json:"isCoinbase"`
	Conflicting   bool                    `json:"conflicting"`
	Locked        bool                    `json:"locked"`
	UnminedSince  uint32                  `json:"unminedSince"`
	Tx            json.RawMessage         `json:"tx"`
	SpendingDatas json.RawMessage         `json:"spendingDatas"`
}

// TxMeta fetches a transaction's metadata.
func (c *AssetClient) TxMeta(ctx context.Context, txid string) (*TxMeta, error) {
	var m TxMeta
	if err := c.getJSON(ctx, "/txmeta/"+txid+"/json", &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// TxHex fetches a transaction's raw hex.
func (c *AssetClient) TxHex(ctx context.Context, txid string) (string, error) {
	b, status, err := c.get(ctx, "/tx/"+txid+"/hex")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("tx %s: HTTP %d: %s", txid, status, truncate(b))
	}
	return string(bytes.TrimSpace(b)), nil
}

// BlockHeader is the subset of a header JSON document we use.
type BlockHeader struct {
	Hash              string `json:"hash"`
	Height            uint32 `json:"height"`
	PreviousBlockHash string `json:"previousblockhash"`
	Time              int64  `json:"time"`
}

// BestBlockHeader returns the node's current best header.
func (c *AssetClient) BestBlockHeader(ctx context.Context) (*BlockHeader, error) {
	var h BlockHeader
	if err := c.getJSON(ctx, "/bestblockheader/json", &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Block is the subset of /block/… /json we use.
type Block struct {
	Hash             string          `json:"hash"`
	Height           uint32          `json:"height"`
	TransactionCount uint64          `json:"transaction_count"`
	CoinbaseTx       json.RawMessage `json:"coinbase_tx"`
	Header           json.RawMessage `json:"header"`
}

// BlockByHeight fetches a block on the node's main chain by height.
func (c *AssetClient) BlockByHeight(ctx context.Context, height uint32) (*Block, error) {
	var b Block
	if err := c.getJSON(ctx, fmt.Sprintf("/block/height/%d/json", height), &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// SubmitTx posts a raw transaction to this node only (proxied to its propagation
// service). A frozen or otherwise rejected transaction comes back as HTTP 403 with a
// generic body; the reason code is only visible on the node's rejectedtx Kafka topic.
func (c *AssetClient) SubmitTx(ctx context.Context, rawTx []byte) (status int, body string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/tx", bytes.NewReader(rawTx))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(bytes.TrimSpace(b)), nil
}

func (c *AssetClient) getJSON(ctx context.Context, path string, v any) error {
	b, status, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %s", path, status, truncate(b))
	}
	return json.Unmarshal(b, v)
}

func (c *AssetClient) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// UTXOOutput is one row of GET /utxos/:txid/json.
type UTXOOutput struct {
	TxID          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	LockingScript string `json:"lockingScript"`
	Satoshis      uint64 `json:"satoshis"`
	UTXOHash      string `json:"utxoHash"`
	Status        string `json:"status"` // OK, SPENT, FROZEN, …
	SpendingData  *struct {
		TxID string `json:"txId"`
		Vin  uint32 `json:"vin"`
	} `json:"spendingData,omitempty"`
	LockTime uint32 `json:"lockTime"`
}

// UTXOs returns the per-output state of a transaction on this node (the endpoint that
// exposes frozen status; /txmeta does not).
func (c *AssetClient) UTXOs(ctx context.Context, txid string) ([]UTXOOutput, error) {
	var out []UTXOOutput
	if err := c.getJSON(ctx, "/utxos/"+txid+"/json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FSMState returns the node's FSM state name (IDLE, RUNNING, CATCHINGBLOCKS, …).
func (c *AssetClient) FSMState(ctx context.Context) (string, error) {
	var st struct {
		State string `json:"state"`
	}
	if err := c.getJSON(ctx, "/fsm/state", &st); err != nil {
		return "", err
	}
	return st.State, nil
}
