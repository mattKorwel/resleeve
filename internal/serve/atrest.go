package serve

import (
	"context"
	"errors"
	"fmt"

	"github.com/mattkorwel/resleeve/internal/auth"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// Server-at-rest envelope encryption (round-12 Part A, slice 1).
//
// When the operator configures a master key (and a BrainKeys store), the
// server encrypts every blob with a per-brain Data Encryption Key (DEK)
// before backend.Put and decrypts after backend.Get. The DEK is wrapped
// under the master key and persisted in brain_keys (envelope encryption).
// This is ADDITIVE on top of any client-side sealing the daemon still
// does — double-encryption of an already-sealed blob is fine for this
// slice. When no master key is configured, every helper here is a no-op
// and blobs are stored/served as-is (legacy behavior).
//
// Never log DEK or master-key material.

// atRestEnabled reports whether server-at-rest encryption is active: an
// operator master key AND a brain-keys store must both be configured.
func (s *Server) atRestEnabled() bool {
	return len(s.masterKey) == 32 && s.brainKeys != nil
}

// provisionBrainKey generates a fresh DEK, wraps it under the master key,
// and persists it for brainID. Called on brain create (personal + shared
// paths). A no-op (nil) when at-rest is disabled — provisioning then
// stores no key and the sync path stays in legacy passthrough mode.
func (s *Server) provisionBrainKey(ctx context.Context, brainID string) error {
	if !s.atRestEnabled() {
		return nil
	}
	dek, err := auth.GenerateDEK()
	if err != nil {
		return fmt.Errorf("generate dek: %w", err)
	}
	wrapped, err := auth.WrapDEK(s.masterKey, dek)
	if err != nil {
		return fmt.Errorf("wrap dek: %w", err)
	}
	if err := s.brainKeys.PutBrainKey(ctx, brainID, wrapped); err != nil {
		return fmt.Errorf("put brain key: %w", err)
	}
	// Warm the cache so the first push/pull in this process doesn't re-read.
	s.dekCacheMu.Lock()
	s.dekCache[brainID] = dek
	s.dekCacheMu.Unlock()
	return nil
}

// brainDEK returns the unwrapped DEK for brainID, memoized in-process. It
// returns (nil, false, nil) when at-rest is disabled or the brain simply
// has no key provisioned (e.g. created while no master key was set) — the
// caller then falls back to storing/serving the blob as-is. A real error
// (DB failure, or a wrapped DEK that won't unwrap under the current
// master key) is returned so we never silently fall back to plaintext on
// a key mismatch.
func (s *Server) brainDEK(ctx context.Context, brainID string) ([]byte, bool, error) {
	if !s.atRestEnabled() {
		return nil, false, nil
	}
	s.dekCacheMu.RLock()
	dek, ok := s.dekCache[brainID]
	s.dekCacheMu.RUnlock()
	if ok {
		return dek, true, nil
	}
	wrapped, err := s.brainKeys.GetBrainKey(ctx, brainID)
	if err != nil {
		if errors.Is(err, rsql.ErrNotFound) {
			// No key for this brain → legacy passthrough for this brain.
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get brain key: %w", err)
	}
	dek, err = auth.UnwrapDEK(s.masterKey, wrapped)
	if err != nil {
		// A wrapped DEK that won't unwrap means a wrong/rotated master key
		// (or corruption). Surface it — do NOT serve/store plaintext.
		return nil, false, fmt.Errorf("unwrap brain dek: %w", err)
	}
	s.dekCacheMu.Lock()
	s.dekCache[brainID] = dek
	s.dekCacheMu.Unlock()
	return dek, true, nil
}

// encryptForBrain encrypts plaintext with brainID's DEK when at-rest is
// active for that brain; otherwise returns plaintext unchanged.
func (s *Server) encryptForBrain(ctx context.Context, brainID string, plaintext []byte) ([]byte, error) {
	dek, ok, err := s.brainDEK(ctx, brainID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return plaintext, nil
	}
	return auth.EncryptAtRest(dek, plaintext)
}

// decryptForBrain decrypts a stored blob with brainID's DEK when at-rest
// is active for that brain; otherwise returns the blob unchanged.
func (s *Server) decryptForBrain(ctx context.Context, brainID string, blob []byte) ([]byte, error) {
	dek, ok, err := s.brainDEK(ctx, brainID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return blob, nil
	}
	return auth.DecryptAtRest(dek, blob)
}
