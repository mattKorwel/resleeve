package auth

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// RecoveryKey is the encoded recovery secret shown to a user once at
// signup. Format: "RESL-XXXX-XXXX-XXXX-XXXX-XXXX" — base32 over 12 bytes
// of entropy (96 bits) grouped into 5 four-char chunks.
type RecoveryKey string

const (
	recoveryKeyBytes  = 12
	recoveryKeyPrefix = "RESL-"
)

var b32enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewRecoveryKey generates a fresh recovery key.
func NewRecoveryKey() (RecoveryKey, error) {
	buf := make([]byte, recoveryKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand recovery key: %w", err)
	}
	enc := b32enc.EncodeToString(buf)
	// Group into 4-char chunks separated by dashes.
	var parts []string
	for i := 0; i < len(enc); i += 4 {
		end := i + 4
		if end > len(enc) {
			end = len(enc)
		}
		parts = append(parts, enc[i:end])
	}
	return RecoveryKey(recoveryKeyPrefix + strings.Join(parts, "-")), nil
}

// Bytes returns the decoded 12-byte secret of the recovery key, or an
// error if the format is invalid.
func (r RecoveryKey) Bytes() ([]byte, error) {
	s := strings.ToUpper(strings.TrimSpace(string(r)))
	if !strings.HasPrefix(s, recoveryKeyPrefix) {
		return nil, errors.New("recovery key: missing RESL- prefix")
	}
	s = strings.TrimPrefix(s, recoveryKeyPrefix)
	s = strings.ReplaceAll(s, "-", "")
	buf, err := b32enc.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("recovery key: decode: %w", err)
	}
	if len(buf) != recoveryKeyBytes {
		return nil, fmt.Errorf("recovery key: expected %d bytes, got %d", recoveryKeyBytes, len(buf))
	}
	return buf, nil
}
