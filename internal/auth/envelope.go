package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// envelopeVersion is the leading byte of every sealed blob. Lets us
// roll the cipher (or add an algorithm marker) without re-encrypting
// existing data: a future Open dispatches on this prefix.
const envelopeVersion byte = 0x01

// Sealer is the abstraction the sync client uses to wrap and unwrap
// blobs at the push/pull boundary. The slice-2.5 implementation is
// AESGCMSealer backed by a daemon-local random key; the eventual
// password+Argon2id KEK flow (round 5+) will swap a different Sealer
// in behind the same interface.
type Sealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

// Wipeable is an optional interface a Sealer may implement to support
// best-effort zeroization of its key material (sec-H4). Callers that
// hold a Sealer they want to revoke (e.g. the daemon on `resleeve
// logout` / idle-lock) should type-assert to Wipeable and call Wipe
// before dropping the reference, so the raw key bytes don't linger in
// the heap until the GC happens to reuse the page.
//
// Wipe MUST be idempotent and safe to call concurrently with a nil
// receiver path (the caller guards the reference under its own mutex).
// After Wipe, Seal/Open behavior is undefined — the contract is "wiped
// sealers are discarded immediately."
type Wipeable interface {
	Wipe()
}

// AESGCMSealer is a Sealer backed by AES-256-GCM with a random 12-byte
// nonce per Seal call. Wire format:
//
//	<version:1byte=0x01><nonce:12bytes><ciphertext+tag>
//
// The AEAD tag (16 bytes, appended to ciphertext by gcm.Seal) detects
// any tampering of the version/nonce/ciphertext during Open.
//
// sec-H4: the sealer retains its own copy of the raw 32-byte key so
// Wipe() can zero it on lock/logout. The cipher.AEAD itself holds an
// expanded AES key schedule we cannot reach from pure Go (no exported
// zeroize on crypto/aes), so wiping our copy is best-effort: it shrinks
// the window in which the raw key is recoverable from process memory but
// does not (and cannot, without CGO/mlock) guarantee the expanded
// schedule or swapped-out pages are scrubbed. Documented as such.
type AESGCMSealer struct {
	aead cipher.AEAD
	key  []byte // owned copy of the raw AES-256 key; zeroed by Wipe
}

// Compile-time assertion that AESGCMSealer implements Sealer + Wipeable.
// Lives here (next to the concrete type) rather than in a downstream
// package per Q6: the assertions are load-bearing for the contracts, not
// a keep-import-alive crutch.
var (
	_ Sealer   = (*AESGCMSealer)(nil)
	_ Wipeable = (*AESGCMSealer)(nil)
)

// NewAESGCMSealer constructs an AESGCMSealer from a 32-byte key
// (AES-256). Returns an error if the key length is wrong. The sealer
// copies the key so the caller may zero its own buffer immediately
// after construction (sec-H4); the copy is scrubbed by Wipe.
func NewAESGCMSealer(key []byte) (*AESGCMSealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("envelope: key must be 32 bytes, got %d", len(key))
	}
	owned := make([]byte, len(key))
	copy(owned, key)
	block, err := aes.NewCipher(owned)
	if err != nil {
		Wipe(owned)
		return nil, fmt.Errorf("envelope: aes new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		Wipe(owned)
		return nil, fmt.Errorf("envelope: aes-gcm: %w", err)
	}
	return &AESGCMSealer{aead: aead, key: owned}, nil
}

// Wipe zeros the sealer's owned copy of the raw key (sec-H4). Best-effort:
// the expanded AES key schedule inside the cipher.AEAD is unreachable in
// pure Go and may persist until GC; swapped-out pages and core dumps are
// out of reach without CGO/mlock. Idempotent — a second call is a no-op.
// After Wipe, the AEAD is dropped so subsequent Seal/Open on this value
// would panic on a nil dereference; callers are expected to discard the
// reference (the daemon nils it under sealerMu).
func (s *AESGCMSealer) Wipe() {
	if s == nil {
		return
	}
	Wipe(s.key)
	s.key = nil
	s.aead = nil
}

// Seal encrypts plaintext with a fresh random nonce and returns the
// version-prefixed envelope. Safe to call concurrently — the AEAD is
// stateless and crypto/rand is goroutine-safe.
func (s *AESGCMSealer) Seal(plaintext []byte) ([]byte, error) {
	if s.aead == nil {
		// Wiped sealer (sec-H4): fail closed rather than panic on the nil
		// AEAD. Callers discard wiped sealers immediately, so this path is
		// a defensive guard against a use-after-wipe race, not a hot path.
		return nil, errors.New("envelope: sealer has been wiped")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("envelope: rand nonce: %w", err)
	}
	// Pre-allocate: 1 version + nonce + plaintext + tag.
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+s.aead.Overhead())
	out = append(out, envelopeVersion)
	out = append(out, nonce...)
	out = s.aead.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Open verifies and decrypts a previously-sealed envelope. Returns an
// error on version mismatch, length underrun, or AEAD tag failure
// (wrong key, mutated bytes, or truncation).
func (s *AESGCMSealer) Open(ciphertext []byte) ([]byte, error) {
	if s.aead == nil {
		return nil, errors.New("envelope: sealer has been wiped")
	}
	nonceSize := s.aead.NonceSize()
	if len(ciphertext) < 1+nonceSize+s.aead.Overhead() {
		return nil, errors.New("envelope: ciphertext too short")
	}
	if ciphertext[0] != envelopeVersion {
		return nil, fmt.Errorf("envelope: unsupported version 0x%02x", ciphertext[0])
	}
	nonce := ciphertext[1 : 1+nonceSize]
	body := ciphertext[1+nonceSize:]
	plain, err := s.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("envelope: open: %w", err)
	}
	return plain, nil
}
