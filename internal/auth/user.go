package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// User is the in-memory crypto-material carrier returned from Signup.
// The client never persists this; the CLI extracts the verifier/wrap
// fields and sends them to `resleeve serve`, which persists the
// server-side mirror as a ServerUser row (see
// docs/design/round-4/02-cross-machine-sync.md §"Identity"). The
// pre-pivot client-side persistence layer (userStore / migration 0002)
// was removed in Q1 — see docs/REVIEW_QUALITY.md.
type User struct {
	ID        string
	Email     string
	CreatedAt time.Time
	UpdatedAt time.Time

	Params           Argon2idParams
	PasswordVerifier Verifier
	PasswordKEK      WrappedKEK
	RecoveryVerifier Verifier
	RecoveryKEK      WrappedKEK
}

// SignupResult is what's returned to the client after creating a new
// account. RecoveryKey MUST be displayed once and never persisted.
type SignupResult struct {
	User        *User
	RecoveryKey RecoveryKey
	KEK         KEK
}

// Signup creates a new User from email + master password. Generates a
// fresh KEK, wraps it twice (password + recovery key), and returns the
// recovery key for one-time display.
func Signup(email, password string) (*SignupResult, error) {
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}

	params := DefaultArgon2idParams()

	kek, err := NewKEK()
	if err != nil {
		return nil, err
	}

	pv, err := NewVerifier([]byte(password), params)
	if err != nil {
		return nil, err
	}
	pw, err := kek.Wrap([]byte(password), params)
	if err != nil {
		return nil, err
	}

	rk, err := NewRecoveryKey()
	if err != nil {
		return nil, err
	}
	rb, err := rk.Bytes()
	if err != nil {
		return nil, err
	}
	rv, err := NewVerifier(rb, params)
	if err != nil {
		return nil, err
	}
	rw, err := kek.Wrap(rb, params)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	u := &User{
		ID:               newUserID(),
		Email:            strings.ToLower(strings.TrimSpace(email)),
		CreatedAt:        now,
		UpdatedAt:        now,
		Params:           params,
		PasswordVerifier: pv,
		PasswordKEK:      pw,
		RecoveryVerifier: rv,
		RecoveryKEK:      rw,
	}
	return &SignupResult{User: u, RecoveryKey: rk, KEK: kek}, nil
}

func validateEmail(email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("auth: email required")
	}
	if !strings.Contains(email, "@") {
		return errors.New("auth: email must contain @")
	}
	return nil
}

func validatePassword(password string) error {
	if len(password) < 8 {
		return errors.New("auth: password must be at least 8 chars")
	}
	return nil
}

// newUserID returns a 16-byte hex-encoded random ID. v1 simplicity;
// swap to ULIDs when we adopt the dep alongside other ID-using code.
func newUserID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
