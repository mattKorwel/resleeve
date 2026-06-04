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

// AESGCMSealer is a Sealer backed by AES-256-GCM with a random 12-byte
// nonce per Seal call. Wire format:
//
//	<version:1byte=0x01><nonce:12bytes><ciphertext+tag>
//
// The AEAD tag (16 bytes, appended to ciphertext by gcm.Seal) detects
// any tampering of the version/nonce/ciphertext during Open.
type AESGCMSealer struct {
	aead cipher.AEAD
}

// Compile-time assertion that AESGCMSealer implements Sealer. Lives
// here (next to the concrete type) rather than in a downstream package
// per Q6: the assertion is load-bearing for the Sealer contract, not a
// keep-import-alive crutch.
var _ Sealer = (*AESGCMSealer)(nil)

// NewAESGCMSealer constructs an AESGCMSealer from a 32-byte key
// (AES-256). Returns an error if the key length is wrong.
func NewAESGCMSealer(key []byte) (*AESGCMSealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("envelope: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("envelope: aes new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("envelope: aes-gcm: %w", err)
	}
	return &AESGCMSealer{aead: aead}, nil
}

// Seal encrypts plaintext with a fresh random nonce and returns the
// version-prefixed envelope. Safe to call concurrently — the AEAD is
// stateless and crypto/rand is goroutine-safe.
func (s *AESGCMSealer) Seal(plaintext []byte) ([]byte, error) {
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
