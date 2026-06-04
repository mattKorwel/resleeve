package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattkorwel/resleeve/internal/agent"
)

// keychain is the abstraction for storing the per-device bearer token
// returned by `resleeve serve` /v2/auth/login. The token gates ALL
// /v2/sync/* traffic for this device — losing it == reauth required.
//
// Round-1 implementation: a 0600 file under ~/.resleeve/devices/.
// Round 5+ should swap to the OS keychain (macOS Security framework,
// Linux libsecret, Windows DPAPI). The interface is intentionally
// minimal so the swap is single-package.
//
// Known limitations of the file backend (called out in the
// punch-list/learnings):
//   - readable by any process running as the same user
//   - survives logout unless explicitly purged
//   - does not benefit from secure enclave / kernel keyring isolation
//
// These match the same threat model as ~/.resleeve/seal.key and the
// agent endpoint secret today, so file-based is a coherent v1 default.
type keychain interface {
	// Put stores token under (host, account). Overwrites prior values.
	Put(host, account, token string) error
	// Get returns the token for (host, account), or "" if absent.
	Get(host, account string) (string, error)
	// Delete removes the (host, account) entry. Idempotent.
	Delete(host, account string) error
}

// defaultKeychain returns the keychain to use for this build. v1 is
// always file-based; the platform-specific keychain wiring lives behind
// build tags in a follow-up.
func defaultKeychain() (keychain, error) {
	dir, err := agent.DataDir()
	if err != nil {
		return nil, err
	}
	return &fileKeychain{root: filepath.Join(dir, "devices")}, nil
}

// fileKeychain persists tokens under <root>/<host>/<account>.json with
// mode 0600. Files are sharded by host so a single `serve` deployment
// with multiple accounts (rare) doesn't conflict.
type fileKeychain struct{ root string }

type devEntry struct {
	Token  string `json:"token"`
	UserID string `json:"user_id,omitempty"`
}

func (f *fileKeychain) path(host, account string) string {
	return filepath.Join(f.root, safeName(host), safeName(account)+".json")
}

func (f *fileKeychain) Put(host, account, token string) error {
	p := f.path(host, account)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("keychain mkdir: %w", err)
	}
	body, err := json.Marshal(devEntry{Token: token})
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("keychain write: %w", err)
	}
	return os.Rename(tmp, p)
}

func (f *fileKeychain) Get(host, account string) (string, error) {
	body, err := os.ReadFile(f.path(host, account))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	var e devEntry
	if err := json.Unmarshal(body, &e); err != nil {
		return "", fmt.Errorf("keychain decode: %w", err)
	}
	return e.Token, nil
}

func (f *fileKeychain) Delete(host, account string) error {
	if err := os.Remove(f.path(host, account)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// safeName makes a string filesystem-safe by replacing path separators
// and other reserved chars with '_'. We don't need reversibility — the
// (host, account) pair is also stored inside the JSON.
func safeName(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "?", "_", "#", "_", " ", "_")
	return r.Replace(s)
}
