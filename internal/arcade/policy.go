package arcade

import "context"

// Policy is the `policy` object of GET /policy: what arcade advertises to wallets. Only the
// fields the harness reasons about are decoded; the document is ARC-shaped and may grow.
type Policy struct {
	MiningFee struct {
		Satoshis int64 `json:"satoshis"`
		Bytes    int64 `json:"bytes"`
	} `json:"miningFee"`
	MaxTxSizePolicy         int64 `json:"maxtxsizepolicy"`
	MaxScriptSizePolicy     int64 `json:"maxscriptsizepolicy"`
	StandardFormatSupported bool  `json:"standardFormatSupported"`
}

type policyResponse struct {
	Policy    Policy `json:"policy"`
	Timestamp string `json:"timestamp"`
}

// GetPolicy reads GET /policy.
func (c *Client) GetPolicy(ctx context.Context) (*Policy, error) {
	var r policyResponse
	if err := c.get(ctx, c.BaseURL+"/policy", &r); err != nil {
		return nil, err
	}
	return &r.Policy, nil
}
