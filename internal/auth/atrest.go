package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// Server-at-rest envelope encryption (round-12 Part A, slice 1).
//
// The server encrypts every blob it stores with a per-brain Data
// Encryption Key (DEK). Each DEK is itself wrapped (encrypted) with the
// operator's 32-byte master key and persisted in the brain_keys table.
// This is classic envelope encryption: rotating the master key re-wraps
// the DEKs (cheap) without re-encrypting the data; a stolen disk snapshot
// yields only ciphertext + wrapped DEKs, both useless without the master
// key.
//
// All three operations share one AEAD shape (AES-256-GCM, random 12-byte
// nonce, output = nonce||ciphertext+tag). This is deliberately distinct
// from the version-prefixed Sealer envelope in envelope.go: the at-rest
// helpers are pure functions over a caller-supplied key (no retained
// cipher state, no Wipeable), so they suit the per-request DEK lifecycle
// the sync handlers use. We never log key or plaintext material here.

// dekLen is the DEK / master-key length: AES-256 (32 bytes).
const dekLen = 32

// atRestNonceLen is the AES-GCM nonce length (the standard 12 bytes).
const atRestNonceLen = 12

// GenerateDEK returns a fresh random 32-byte Data Encryption Key.
func GenerateDEK() ([]byte, error) {
	dek := make([]byte, dekLen)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("atrest: rand dek: %w", err)
	}
	return dek, nil
}

// WrapDEK encrypts dek under masterKey (AES-256-GCM) and returns the
// wrapped form (nonce||ciphertext+tag) ready to persist. masterKey must
// be 32 bytes and dek must be 32 bytes.
func WrapDEK(masterKey, dek []byte) ([]byte, error) {
	if len(masterKey) != dekLen {
		return nil, fmt.Errorf("atrest: master key must be %d bytes, got %d", dekLen, len(masterKey))
	}
	if len(dek) != dekLen {
		return nil, fmt.Errorf("atrest: dek must be %d bytes, got %d", dekLen, len(dek))
	}
	return aeadSeal(masterKey, dek)
}

// UnwrapDEK reverses WrapDEK: decrypts wrapped under masterKey and
// returns the raw 32-byte DEK. A wrong/rotated master key, or any
// tampering of the wrapped bytes, fails the GCM tag check and returns an
// error — never silent plaintext. The recovered length is asserted to be
// 32 bytes as a defense against a malformed row.
func UnwrapDEK(masterKey, wrapped []byte) ([]byte, error) {
	if len(masterKey) != dekLen {
		return nil, fmt.Errorf("atrest: master key must be %d bytes, got %d", dekLen, len(masterKey))
	}
	dek, err := aeadOpen(masterKey, wrapped)
	if err != nil {
		return nil, fmt.Errorf("atrest: unwrap dek: %w", err)
	}
	if len(dek) != dekLen {
		return nil, fmt.Errorf("atrest: unwrapped dek wrong length %d", len(dek))
	}
	return dek, nil
}

// EncryptAtRest encrypts plaintext with the brain's DEK (AES-256-GCM) and
// returns the at-rest blob (nonce||ciphertext+tag). dek must be 32 bytes.
func EncryptAtRest(dek, plaintext []byte) ([]byte, error) {
	if len(dek) != dekLen {
		return nil, fmt.Errorf("atrest: dek must be %d bytes, got %d", dekLen, len(dek))
	}
	return aeadSeal(dek, plaintext)
}

// DecryptAtRest reverses EncryptAtRest. A wrong DEK or tampered blob fails
// the GCM tag check (error, never silent plaintext).
func DecryptAtRest(dek, blob []byte) ([]byte, error) {
	if len(dek) != dekLen {
		return nil, fmt.Errorf("atrest: dek must be %d bytes, got %d", dekLen, len(dek))
	}
	plain, err := aeadOpen(dek, blob)
	if err != nil {
		return nil, fmt.Errorf("atrest: decrypt: %w", err)
	}
	return plain, nil
}

// aeadSeal is the shared AES-256-GCM seal: fresh random nonce, output
// nonce||ciphertext+tag. key must be 32 bytes (callers validate length).
func aeadSeal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newAtRestGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, atRestNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("atrest: rand nonce: %w", err)
	}
	// Pre-allocate nonce + plaintext + tag; gcm.Seal appends onto nonce.
	out := make([]byte, 0, len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, nil), nil
}

// aeadOpen is the shared AES-256-GCM open over a nonce||ciphertext+tag
// blob. Returns an error on length underrun or tag failure.
func aeadOpen(key, blob []byte) ([]byte, error) {
	gcm, err := newAtRestGCM(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < atRestNonceLen+gcm.Overhead() {
		return nil, errors.New("atrest: blob too short")
	}
	nonce := blob[:atRestNonceLen]
	body := blob[atRestNonceLen:]
	return gcm.Open(nil, nonce, body, nil)
}

func newAtRestGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("atrest: aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("atrest: aes-gcm: %w", err)
	}
	return gcm, nil
}
