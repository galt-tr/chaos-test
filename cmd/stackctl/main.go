// stackctl is the stack-layer operator tool: mine, query and submit transactions against
// individual teranodes, and build raw spends with keys the harness controls.
//
//	stackctl rpc --node URL <method> [json-params]
//	stackctl mine --node URL --blocks N [--address ADDR]
//	stackctl tip --asset URL
//	stackctl txmeta --asset URL --txid TXID
//	stackctl coinbase --asset URL --height H            (prints the coinbase txid and hex)
//	stackctl newkey                                      (fresh victim key: hex + regtest address)
//	stackctl spend --asset URL --txid TXID --vout N --key WIF|HEX --to ADDRKEYHEX|--to-script HEX --sats N [--fee N]
//	stackctl submit --asset URL --hex RAWTX | --rpc URL --hex RAWTX
//
// Defaults: NODE_RPC (http://…:9292), NODE_ASSET (http://…:8090/api/v1), RPC_USER/RPC_PASS.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bsv-blockchain/chaos-test/internal/teranode"
	"github.com/bsv-blockchain/chaos-test/internal/wallet"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var err error
	switch os.Args[1] {
	case "rpc":
		err = cmdRPC(ctx, os.Args[2:])
	case "mine":
		err = cmdMine(ctx, os.Args[2:])
	case "tip":
		err = cmdTip(ctx, os.Args[2:])
	case "txmeta":
		err = cmdTxMeta(ctx, os.Args[2:])
	case "coinbase":
		err = cmdCoinbase(ctx, os.Args[2:])
	case "newkey":
		err = cmdNewKey()
	case "spend":
		err = cmdSpend(ctx, os.Args[2:])
	case "submit":
		err = cmdSubmit(ctx, os.Args[2:])
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "stackctl rpc|mine|tip|txmeta|coinbase|newkey|spend|submit [flags]  (see source header)")
}

func rpcFlags(fs *flag.FlagSet) (*string, *string, *string) {
	return fs.String("node", env("NODE_RPC", "http://localhost:21292"), "teranode RPC URL"),
		fs.String("user", env("RPC_USER", "bitcoin"), "rpc user"),
		fs.String("pass", env("RPC_PASS", "bitcoin"), "rpc password")
}

func assetFlag(fs *flag.FlagSet) *string {
	return fs.String("asset", env("NODE_ASSET", "http://localhost:20090/api/v1"), "teranode asset API base URL (with /api/v1)")
}

func cmdRPC(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rpc", flag.ContinueOnError)
	node, user, pass := rpcFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("rpc needs a method")
	}
	var params []any
	if fs.NArg() > 1 {
		if err := json.Unmarshal([]byte(fs.Arg(1)), &params); err != nil {
			return fmt.Errorf("params must be a JSON array: %w", err)
		}
	}
	var out json.RawMessage
	if err := teranode.NewRPCClient(*node, *user, *pass).Call(ctx, fs.Arg(0), params, &out); err != nil {
		return err
	}
	return printJSON(out)
}

func cmdMine(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mine", flag.ContinueOnError)
	node, user, pass := rpcFlags(fs)
	n := fs.Int("blocks", 1, "blocks to mine")
	addr := fs.String("address", "", "pay coinbase to this address (default: node miner key)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := teranode.NewRPCClient(*node, *user, *pass)
	var hashes []string
	var err error
	if *addr != "" {
		hashes, err = c.GenerateToAddress(ctx, *n, *addr)
	} else {
		hashes, err = c.Generate(ctx, *n)
	}
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"mined": len(hashes), "hashes": hashes})
}

func cmdTip(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tip", flag.ContinueOnError)
	asset := assetFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	h, err := teranode.NewAssetClient(*asset).BestBlockHeader(ctx)
	if err != nil {
		return err
	}
	return printJSON(h)
}

func cmdTxMeta(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("txmeta", flag.ContinueOnError)
	asset := assetFlag(fs)
	txid := fs.String("txid", "", "transaction id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := teranode.NewAssetClient(*asset).TxMeta(ctx, *txid)
	if err != nil {
		return err
	}
	m.Tx, m.SpendingDatas = nil, nil
	return printJSON(m)
}

func cmdCoinbase(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("coinbase", flag.ContinueOnError)
	asset := assetFlag(fs)
	height := fs.Uint("height", 1, "block height")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := teranode.NewAssetClient(*asset).BlockByHeight(ctx, uint32(*height))
	if err != nil {
		return err
	}
	var cb struct {
		TxID string `json:"txid"`
		Hex  string `json:"hex"`
	}
	if err := json.Unmarshal(b.CoinbaseTx, &cb); err != nil {
		return fmt.Errorf("decode coinbase: %w", err)
	}
	return printJSON(map[string]any{"height": b.Height, "txid": cb.TxID, "hex": cb.Hex})
}

func cmdNewKey() error {
	k, err := wallet.NewKey()
	if err != nil {
		return err
	}
	addr, err := k.Address(true)
	if err != nil {
		return err
	}
	ls, _ := k.LockingScript()
	return printJSON(map[string]any{"privateKeyHex": k.Hex(), "address": addr, "lockingScript": ls.String()})
}

func loadKey(s string) (*wallet.Key, error) {
	if len(s) == 64 {
		return wallet.KeyFromHex(s)
	}
	return wallet.KeyFromWIF(s)
}

func cmdSpend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("spend", flag.ContinueOnError)
	asset := assetFlag(fs)
	txid := fs.String("txid", "", "source txid")
	vout := fs.Uint("vout", 0, "source output index")
	key := fs.String("key", "", "WIF or 32-byte hex private key unlocking the source output")
	toKey := fs.String("to", "", "destination private key hex (pays to its P2PKH)")
	toScript := fs.String("to-script", "", "destination locking script hex")
	sats := fs.Uint64("sats", 0, "satoshis to send (default: input minus fee)")
	fee := fs.Uint64("fee", 500, "absolute fee in satoshis")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *txid == "" || *key == "" {
		return errors.New("--txid and --key required")
	}
	k, err := loadKey(*key)
	if err != nil {
		return err
	}
	src, err := teranode.NewAssetClient(*asset).TxHex(ctx, *txid)
	if err != nil {
		return err
	}
	srcTx, err := wallet.ParseTx(src)
	if err != nil {
		return fmt.Errorf("parse source tx: %w", err)
	}
	if int(*vout) >= len(srcTx.Outputs) {
		return fmt.Errorf("vout %d out of range (%d outputs)", *vout, len(srcTx.Outputs))
	}
	amount := *sats
	if amount == 0 {
		amount = srcTx.Outputs[*vout].Satoshis - *fee
	}
	out := wallet.Output{Satoshis: amount}
	switch {
	case *toKey != "":
		if out.Key, err = wallet.KeyFromHex(*toKey); err != nil {
			return err
		}
	case *toScript != "":
		out.Script = *toScript
	default:
		out.Key = k // pay back to self
	}
	tx, err := wallet.Spend([]wallet.Input{{SourceTx: srcTx, Vout: uint32(*vout), Key: k}}, []wallet.Output{out}, *fee)
	if err != nil {
		return err
	}
	ef, err := tx.EFHex()
	if err != nil {
		return fmt.Errorf("extended format: %w", err)
	}
	return printJSON(map[string]any{"txid": tx.TxID().String(), "hex": tx.Hex(), "efHex": ef, "size": tx.Size()})
}

func cmdSubmit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	asset := assetFlag(fs)
	rpcURL := fs.String("rpc", "", "submit via sendrawtransaction on this RPC URL instead of the asset API")
	arcadeURL := fs.String("arcade", "", "submit via arcade POST /tx on this base URL (send the efHex from 'spend')")
	user := fs.String("user", env("RPC_USER", "bitcoin"), "rpc user")
	pass := fs.String("pass", env("RPC_PASS", "bitcoin"), "rpc password")
	h := fs.String("hex", "", "raw transaction hex")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *h == "" {
		return errors.New("--hex required")
	}
	if *rpcURL != "" {
		id, err := teranode.NewRPCClient(*rpcURL, *user, *pass).SendRawTransaction(ctx, *h)
		return printJSON(map[string]any{"via": "rpc", "txid": id, "error": errString(err)})
	}
	if *arcadeURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(*arcadeURL, "/")+"/tx", strings.NewReader(strings.TrimSpace(*h)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var doc any
		_ = json.Unmarshal(body, &doc)
		return printJSON(map[string]any{"via": "arcade", "status": resp.StatusCode, "body": doc, "accepted": resp.StatusCode/100 == 2})
	}
	raw, err := hex.DecodeString(strings.TrimSpace(*h))
	if err != nil {
		return err
	}
	status, body, err := teranode.NewAssetClient(*asset).SubmitTx(ctx, raw)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"via": "asset", "status": status, "body": body, "accepted": status/100 == 2})
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
