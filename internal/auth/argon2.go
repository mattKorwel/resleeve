package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// Argon2idParams controls the cost of Argon2id derivations.
type Argon2idParams struct {
	MemoryKiB   uint32
	TimeIters   uint32
	Parallelism uint8
}

// DefaultArgon2idParams returns OWASP-leaning defaults: 64 MiB memory,
// 3 iterations, 1 parallel lane. ~200–300 ms on modern hardware — fine
// for interactive login.
func DefaultArgon2idParams() Argon2idParams {
	return Argon2idParams{
		MemoryKiB:   64 * 1024,
		TimeIters:   3,
		Parallelism: 1,
	}
}

const (
	saltLen = 16 // 128 bits
	keyLen  = 32 // 256 bits — AES-256-GCM key
)

// NewSalt returns a fresh cryptographically random salt.
func NewSalt() ([]byte, error) {
	b := make([]byte, saltLen)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("rand salt: %w", err)
	}
	return b, nil
}

// DeriveKey runs Argon2id over (input, salt) with the given params and
// returns 32 bytes of derived key material.
func DeriveKey(input, salt []byte, p Argon2idParams) []byte {
	return argon2.IDKey(input, salt, p.TimeIters, p.MemoryKiB, p.Parallelism, keyLen)
}

// Verifier is a stored proof that the holder of a secret knows it.
// Verifier hash and the secret's KEK-wrap key are derived from DIFFERENT
// salts so the verifier is safe to store server-side — knowing it does
// not let an attacker unwrap the KEK.
type Verifier struct {
	Salt []byte
	Hash []byte
}

// NewVerifier derives a fresh verifier for the given secret.
func NewVerifier(secret []byte, p Argon2idParams) (Verifier, error) {
	salt, err := NewSalt()
	if err != nil {
		return Verifier{}, err
	}
	return Verifier{Salt: salt, Hash: DeriveKey(secret, salt, p)}, nil
}

// Verify reports whether the given secret matches v's stored hash.
// Constant-time comparison to defeat timing attacks.
func (v Verifier) Verify(secret []byte, p Argon2idParams) bool {
	computed := DeriveKey(secret, v.Salt, p)
	return subtle.ConstantTimeCompare(computed, v.Hash) == 1
}
