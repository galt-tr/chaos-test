package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/defs"
)

func main() {
	var (
		listen     = flag.String("listen", env("LISTEN", ":8700"), "HTTP listen address")
		storageURL = flag.String("storage", env("WALLET_INFRA_URL", ""), "go-wallet-toolbox storage server URL")
		keysPath   = flag.String("keys", env("KEYS", "/config/keys.json"), "keys.json holding walletUserKey")
		network    = flag.String("network", env("BSV_NETWORK", "tstn"), "bsv network")
		originator = flag.String("originator", env("ORIGINATOR", "chaos-wallet.local"), "BRC-100 originator")
	)
	flag.Parse()

	lg := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	priv, err := loadWalletKey(*keysPath)
	if err != nil {
		lg.Error("cannot load wallet identity", "err", err, "path", *keysPath)
		os.Exit(1)
	}
	w, err := NewWallet(Config{
		StorageURL: *storageURL, PrivateKey: priv,
		Network: defs.BSVNetwork(*network), Originator: *originator,
	}, lg)
	if err != nil {
		lg.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go w.Run(ctx)

	srv := &http.Server{Addr: *listen, Handler: routes(w), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	lg.Info("walletd listening", "addr", *listen, "storage", *storageURL,
		"network", *network, "deposit", w.deposit.Address)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		lg.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func routes(w *Wallet) http.Handler {
	m := http.NewServeMux()

	// Always answers, connected or not: when the wallet is down this is how you find out why.
	m.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, 200, w.Health())
	})
	// Derived from the key alone, so it works before any connection exists.
	m.HandleFunc("GET /v1/deposit", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, 200, w.deposit)
	})
	m.HandleFunc("GET /v1/state", func(rw http.ResponseWriter, r *http.Request) {
		st, err := w.State(r.Context())
		if err != nil {
			writeErr(rw, err)
			return
		}
		writeJSON(rw, 200, st)
	})
	m.HandleFunc("GET /v1/outputs", func(rw http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		coins, total, err := w.Outputs(r.Context(), uint32(limit))
		if err != nil {
			writeErr(rw, err)
			return
		}
		writeJSON(rw, 200, map[string]any{"outputs": coins, "total": total})
	})
	m.HandleFunc("GET /v1/actions", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var labels []string
		for _, l := range strings.Split(q.Get("labels"), ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		h, err := w.Actions(r.Context(), labels, uint32(limit), q.Get("include") != "0")
		if err != nil {
			writeErr(rw, err)
			return
		}
		writeJSON(rw, 200, h)
	})
	m.HandleFunc("POST /v1/tx", func(rw http.ResponseWriter, r *http.Request) {
		var req TxRequest
		if err := decode(r, &req); err != nil {
			writeJSON(rw, 400, errBody(err, "bad_request"))
			return
		}
		res, err := w.BuildTx(r.Context(), req)
		if err != nil {
			writeErr(rw, err)
			return
		}
		writeJSON(rw, 200, res)
	})
	m.HandleFunc("POST /v1/internalize", func(rw http.ResponseWriter, r *http.Request) {
		var req InternalizeRequest
		if err := decode(r, &req); err != nil {
			writeJSON(rw, 400, errBody(err, "bad_request"))
			return
		}
		res, err := w.Internalize(r.Context(), req)
		if err != nil {
			writeErr(rw, err)
			return
		}
		writeJSON(rw, 200, res)
	})
	return m
}

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

func errBody(err error, reason string) map[string]any {
	return map[string]any{"error": err.Error(), "reason": reason}
}

// writeErr maps an error to a status plus a machine-readable reason, so the orchestrator can
// branch on a code rather than on message text.
func writeErr(rw http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotConnected):
		writeJSON(rw, 503, errBody(err, "not_connected"))
	case isInsufficientFunds(err):
		writeJSON(rw, 409, errBody(err, "insufficient_funds"))
	default:
		writeJSON(rw, 502, errBody(err, "wallet_error"))
	}
}

// isInsufficientFunds recognises an empty coin pool. This is backpressure, not a failure: the
// caller should wait for change to confirm rather than count it against the send.
func isInsufficientFunds(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not enough funds") || strings.Contains(m, "insufficient") ||
		strings.Contains(m, "no spendable")
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20)).Decode(v)
}

// loadWalletKey pulls walletUserKey out of the harness keys.json. cmd/gen already generates it
// as "the dev wallet identity used by the harness".
func loadWalletKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		WalletUserKey struct {
			PrivateKeyHex string `json:"privateKeyHex"`
			PrivateKey    string `json:"private_key_hex"`
			Hex           string `json:"hex"`
		} `json:"walletUserKey"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	for _, c := range []string{doc.WalletUserKey.PrivateKeyHex, doc.WalletUserKey.PrivateKey, doc.WalletUserKey.Hex} {
		if len(c) == 64 {
			return c, nil
		}
	}
	return "", fmt.Errorf("walletUserKey has no 64-hex private key in %s", path)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
