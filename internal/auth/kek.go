package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KEK is the key-encryption key: a 32-byte secret that wraps the user's
// per-data-domain encryption keys. Lives in memory after login; never
// persisted in plaintext.
type KEK [32]byte

// NewKEK returns a fresh random KEK.
func NewKEK() (KEK, error) {
	var k KEK
	if _, err := rand.Read(k[:]); err != nil {
		return KEK{}, fmt.Errorf("rand kek: %w", err)
	}
	return k, nil
}

// Wipe zeros a KEK in place (sec-H4). Best-effort RAM scrub: pure Go
// cannot mlock the page against swap nor scrub copies the compiler may
// have made by value, but zeroing the canonical array shortens the
// window in which a memory-reading attacker can recover it. Callers
// passing the KEK by slice (kek[:]) and then wiping the source array
// also clear that view, since the slice aliases the array's backing.
func (k *KEK) Wipe() {
	if k == nil {
		return
	}
	Wipe(k[:])
}

// Wipe overwrites b with zeros. Best-effort key zeroization (sec-H4):
// see KEK.Wipe / AESGCMSealer.Wipe for the threat-model caveats. The
// loop is written so the compiler cannot trivially optimize it away as
// a dead store (the slice escapes via the exported function boundary).
func Wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// WrappedKEK is a KEK encrypted with a key derived from a user-held
// secret (password or recovery key). Safe to persist; useless without
// the originating secret.
type WrappedKEK struct {
	Salt       []byte // Argon2id salt used to derive the wrapping key
	Nonce      []byte // AES-GCM nonce
	Ciphertext []byte
	Params     Argon2idParams
}

const nonceLen = 12 // AES-GCM standard

// Wrap encrypts the KEK with a key derived from `secret` and returns
// a WrappedKEK ready to persist.
func (k KEK) Wrap(secret []byte, p Argon2idParams) (WrappedKEK, error) {
	salt, err := NewSalt()
	if err != nil {
		return WrappedKEK{}, err
	}
	derived := DeriveKey(secret, salt, p)
	ct, nonce, err := aesGCMSeal(derived, k[:])
	if err != nil {
		return WrappedKEK{}, err
	}
	return WrappedKEK{Salt: salt, Nonce: nonce, Ciphertext: ct, Params: p}, nil
}

// Unwrap decrypts w using the given secret.
func (w WrappedKEK) Unwrap(secret []byte) (KEK, error) {
	derived := DeriveKey(secret, w.Salt, w.Params)
	plain, err := aesGCMOpen(derived, w.Nonce, w.Ciphertext)
	if err != nil {
		return KEK{}, fmt.Errorf("unwrap kek: %w", err)
	}
	if len(plain) != len(KEK{}) {
		return KEK{}, errors.New("unwrap kek: wrong plaintext length")
	}
	var k KEK
	copy(k[:], plain)
	return k, nil
}

func aesGCMSeal(key, plaintext []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("aes-gcm: %w", err)
	}
	nonce = make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("rand nonce: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

func aesGCMOpen(key, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aes-gcm: %w", err)
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}
