package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bsv-blockchain/bsv-regtest/teranode"
	"github.com/bsv-blockchain/bsv-regtest/wallet"
)

// cmdTopup funds the BRC-100 wallet behind walletd from a regtest coinbase.
//
// The wallet only credits atomic BEEF whose merkle root its chain tracker can verify, so the
// funding transaction has to be mined and proven before it can be internalized: pay the deposit
// address with the miner key, broadcast through arcade, mine until arcade returns the BUMP, wrap
// the transaction and proof into atomic BEEF and hand it to walletd.
func cmdTopup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("topup", flag.ContinueOnError)
	asset := assetFlag(fs)
	node, user, pass := rpcFlags(fs)
	walletd := fs.String("walletd", env("WALLETD_URL", "http://localhost:18700"), "walletd base URL")
	arcadeURL := fs.String("arcade", env("ARCADE_URL", "http://localhost:18080"), "arcade base URL")
	key := fs.String("key", "", "WIF or hex key unlocking the coinbases (default: minerWIF from $INVENTORY)")
	sats := fs.Uint64("sats", 100_000, "satoshis to deposit")
	fee := fs.Uint64("fee", 500, "absolute fee in satoshis")
	noMine := fs.Bool("no-mine", false, "do not mine; wait for someone else to confirm the funding transaction")
	wait := fs.Duration("wait", 2*time.Minute, "how long to wait for the merkle proof")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *key == "" {
		wif, err := minerWIF(env("INVENTORY", "/config/inventory.json"))
		if err != nil {
			return fmt.Errorf("--key not given and %w", err)
		}
		*key = wif
	}
	k, err := loadKey(*key)
	if err != nil {
		return err
	}
	ac := teranode.NewAssetClient(*asset)
	rpc := teranode.NewRPCClient(*node, *user, *pass)
	wd := strings.TrimRight(*walletd, "/")

	// 1. where to pay
	var dep struct {
		Address          string `json:"address"`
		LockingScriptHex string `json:"lockingScriptHex"`
	}
	if err := getJSON(ctx, wd+"/v1/deposit", &dep); err != nil {
		return fmt.Errorf("walletd deposit: %w", err)
	}

	// 2. a mature, unspent coinbase the key controls
	cbTxID, cbHeight, err := spendableCoinbase(ctx, ac, *sats+*fee)
	if err != nil {
		return err
	}

	// 3. the funding transaction (change goes back to the miner key)
	src, err := ac.TxHex(ctx, cbTxID)
	if err != nil {
		return err
	}
	srcTx, err := wallet.ParseTx(src)
	if err != nil {
		return fmt.Errorf("parse coinbase: %w", err)
	}
	tx, err := wallet.Spend([]wallet.Input{{SourceTx: srcTx, Vout: 0, Key: k}},
		[]wallet.Output{{Script: dep.LockingScriptHex, Satoshis: *sats}}, *fee)
	if err != nil {
		return err
	}
	ef, err := tx.EFHex()
	if err != nil {
		return fmt.Errorf("extended format: %w", err)
	}
	txid := tx.TxID().String()

	// 4. broadcast through arcade, which is what proves it later
	status, body, err := submitArcade(ctx, *arcadeURL, ef)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("arcade rejected the funding transaction: HTTP %d %s", status, body)
	}

	// 5. mine until arcade has the proof. Arcade answers the submit before the transaction has
	// reached a node, so the first block is only sealed once the node knows the transaction;
	// on an idle regtest nothing else produces blocks, so mining is repeated while waiting.
	deadline := time.Now().Add(*wait)
	if !*noMine {
		for time.Now().Before(deadline) {
			if _, err := ac.TxMeta(ctx, txid); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
	}
	var rec struct {
		TxStatus    string  `json:"txStatus"`
		BlockHeight *uint32 `json:"blockHeight"`
		MerklePath  string  `json:"merklePath"`
	}
	lastMine := time.Time{}
	for {
		if !*noMine && time.Since(lastMine) > 10*time.Second {
			if _, err := rpc.Generate(ctx, 1); err != nil {
				return fmt.Errorf("mine: %w", err)
			}
			lastMine = time.Now()
		}
		if err := getJSON(ctx, strings.TrimRight(*arcadeURL, "/")+"/tx/"+txid, &rec); err == nil && rec.MerklePath != "" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no merkle proof for %s after %s (arcade status %q); mine a block and retry with the same wallet", txid, *wait, rec.TxStatus)
		}
		time.Sleep(time.Second)
	}

	// 6. atomic BEEF = transaction + BUMP
	_, beefHex, err := wallet.BuildAtomicBEEF(tx.Hex(), rec.MerklePath)
	if err != nil {
		return err
	}

	// 7. credit it
	var res struct {
		Accepted    bool   `json:"accepted"`
		OutputIndex uint32 `json:"outputIndex"`
		Balance     uint64 `json:"balance"`
		Coins       uint32 `json:"coins"`
	}
	req := map[string]any{"atomicBeefHex": beefHex, "expectedAddress": dep.Address, "description": "stackctl topup"}
	if err := postJSON(ctx, wd+"/v1/internalize", req, &res); err != nil {
		return fmt.Errorf("walletd internalize: %w", err)
	}
	out := map[string]any{"deposit": dep.Address, "coinbase": cbTxID, "coinbaseHeight": cbHeight, "txid": txid,
		"satoshis": *sats, "accepted": res.Accepted, "balance": res.Balance, "coins": res.Coins}
	if rec.BlockHeight != nil {
		out["blockHeight"] = *rec.BlockHeight
	}
	return printJSON(out)
}

// spendableCoinbase scans the chain from block 1 for a mature coinbase whose first output is
// still unspent and worth at least min satoshis.
func spendableCoinbase(ctx context.Context, ac *teranode.AssetClient, min uint64) (string, uint32, error) {
	tip, err := ac.BestBlockHeader(ctx)
	if err != nil {
		return "", 0, err
	}
	if tip.Height < 101 {
		return "", 0, fmt.Errorf("chain is at height %d; coinbases mature after 100 blocks (scripts/mine.sh 1 101)", tip.Height)
	}
	for h := uint32(1); h+100 <= tip.Height; h++ {
		b, err := ac.BlockByHeight(ctx, h)
		if err != nil {
			return "", 0, err
		}
		var cb struct {
			TxID string `json:"txid"`
		}
		if err := json.Unmarshal(b.CoinbaseTx, &cb); err != nil || cb.TxID == "" {
			continue
		}
		outs, err := ac.UTXOs(ctx, cb.TxID)
		if err != nil || len(outs) == 0 {
			continue
		}
		if outs[0].Status == "OK" && outs[0].Satoshis >= min {
			return cb.TxID, h, nil
		}
	}
	return "", 0, errors.New("no mature unspent coinbase found; mine more blocks (scripts/mine.sh 1 101)")
}

// minerWIF reads the miner key from the inventory (the same key every generated coinbase pays).
func minerWIF(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read inventory %s: %w", path, err)
	}
	var inv struct {
		MinerWIF string `json:"minerWIF"`
	}
	if err := json.Unmarshal(raw, &inv); err != nil || inv.MinerWIF == "" {
		return "", fmt.Errorf("inventory %s carries no minerWIF", path)
	}
	return inv.MinerWIF, nil
}

// submitArcade posts a transaction (Extended Format hex preferred) to arcade's POST /tx.
func submitArcade(ctx context.Context, base, txHex string) (int, any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/tx", strings.NewReader(strings.TrimSpace(txHex)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		doc = string(body)
	}
	return resp.StatusCode, doc, nil
}

func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: HTTP %d %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, v)
}

func postJSON(ctx context.Context, url string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: HTTP %d %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}
