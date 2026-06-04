package auth

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// sealKeyLen is the byte length of the daemon-local seal key. Must
// match the AES-256-GCM key length consumed by NewAESGCMSealer.
const sealKeyLen = 32

// LoadOrCreateSealKey reads a 32-byte seal key from path, or, if the
// file does not exist, generates a fresh random key, writes it with
// mode 0600 (creating the parent directory with mode 0700 as needed),
// and returns the key bytes.
//
// This is a deliberate placeholder. Slice 2.5 ships a daemon-local
// random key so client-side encryption is wired end-to-end without
// requiring the full password/Argon2id login flow. The eventual KEK
// derivation (round 5+) plugs in at the same point: same signature,
// same caller — only the bytes' provenance changes.
//
// An existing file with the wrong length is treated as corruption
// (rather than silently regenerating) so an operator notices.
func LoadOrCreateSealKey(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("seal key: path is empty")
	}
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != sealKeyLen {
			return nil, fmt.Errorf("seal key: %s has length %d, want %d", path, len(b), sealKeyLen)
		}
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("seal key: read %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("seal key: mkdir %s: %w", dir, err)
		}
	}

	key := make([]byte, sealKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("seal key: rand: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("seal key: write %s: %w", path, err)
	}
	return key, nil
}
