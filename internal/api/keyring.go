package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/bsv-blockchain/chaos-test/internal/wallet"
)

// KeyEntry is a named key the harness controls (victim coins, change, …).
type KeyEntry struct {
	Name          string `json:"name"`
	PrivateKeyHex string `json:"privateKeyHex"`
	Address       string `json:"address"` // regtest/testnet encoding
	LockingScript string `json:"lockingScript"`
	Note          string `json:"note,omitempty"`
}

// Keyring persists named keys to a JSON file.
type Keyring struct {
	mu   sync.Mutex
	path string
	keys map[string]*KeyEntry
}

// OpenKeyring loads (or creates) the ring at path. minerWIF, when set, is registered as
// "miner" so scenarios can spend coinbase outputs.
func OpenKeyring(path, minerWIF string) (*Keyring, error) {
	k := &Keyring{path: path, keys: map[string]*KeyEntry{}}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var list []*KeyEntry
			if err := json.Unmarshal(b, &list); err != nil {
				return nil, fmt.Errorf("decode keyring %s: %w", path, err)
			}
			for _, e := range list {
				k.keys[e.Name] = e
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if minerWIF != "" {
		mk, err := wallet.KeyFromWIF(minerWIF)
		if err != nil {
			return nil, fmt.Errorf("miner key: %w", err)
		}
		e, err := entryFor("miner", mk, "node coinbase key (miner_wallet_private_keys)")
		if err != nil {
			return nil, err
		}
		k.keys["miner"] = e
	}
	return k, nil
}

func entryFor(name string, key *wallet.Key, note string) (*KeyEntry, error) {
	addr, err := key.Address(true)
	if err != nil {
		return nil, err
	}
	ls, err := key.LockingScript()
	if err != nil {
		return nil, err
	}
	return &KeyEntry{Name: name, PrivateKeyHex: key.Hex(), Address: addr, LockingScript: ls.String(), Note: note}, nil
}

// New creates and stores a fresh key under name (error if it exists).
func (k *Keyring) New(name, note string) (*KeyEntry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.keys[name]; ok {
		return nil, fmt.Errorf("key %q exists", name)
	}
	key, err := wallet.NewKey()
	if err != nil {
		return nil, err
	}
	e, err := entryFor(name, key, note)
	if err != nil {
		return nil, err
	}
	k.keys[name] = e
	return e, k.saveLocked()
}

// Get returns a key by name.
func (k *Keyring) Get(name string) (*KeyEntry, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.keys[name]
	return e, ok
}

// Resolve accepts a key name, a 64-hex private key or a WIF and returns a wallet key.
func (k *Keyring) Resolve(ref string) (*wallet.Key, error) {
	if e, ok := k.Get(ref); ok {
		return wallet.KeyFromHex(e.PrivateKeyHex)
	}
	if len(ref) == 64 {
		return wallet.KeyFromHex(ref)
	}
	if key, err := wallet.KeyFromWIF(ref); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("unknown key %q (name, hex or WIF)", ref)
}

// List returns entries sorted by name, with private keys redacted unless full is set.
func (k *Keyring) List(full bool) []KeyEntry {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]KeyEntry, 0, len(k.keys))
	for _, e := range k.keys {
		cp := *e
		if !full {
			cp.PrivateKeyHex = ""
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (k *Keyring) saveLocked() error {
	if k.path == "" {
		return nil
	}
	var list []*KeyEntry
	for _, e := range k.keys {
		if e.Name != "miner" {
			list = append(list, e)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(k.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(k.path, b, 0o600)
}
