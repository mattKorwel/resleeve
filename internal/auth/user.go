package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// User is a resleeve account. All cryptographic material lives here so
// it can be persisted as one storage record. See
// docs/design/round-2/10-auth-subsystem.md.
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

// ErrInvalidCredentials is returned when a password or recovery key doesn't match.
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

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

// Login verifies password against the user's PasswordVerifier and, on
// success, returns the unwrapped KEK.
func Login(u *User, password string) (KEK, error) {
	if !u.PasswordVerifier.Verify([]byte(password), u.Params) {
		return KEK{}, ErrInvalidCredentials
	}
	return u.PasswordKEK.Unwrap([]byte(password))
}

// Recover verifies recovery key and returns the unwrapped KEK. Caller
// must follow up with ResetPassword to re-key both wraps.
func Recover(u *User, rk RecoveryKey) (KEK, error) {
	rb, err := rk.Bytes()
	if err != nil {
		return KEK{}, fmt.Errorf("recover: %w", err)
	}
	if !u.RecoveryVerifier.Verify(rb, u.Params) {
		return KEK{}, ErrInvalidCredentials
	}
	return u.RecoveryKEK.Unwrap(rb)
}

// ResetPassword takes a recovered KEK and a new password, re-wraps the
// KEK, and rotates the recovery key. Returns the new recovery key
// (to be shown once); mutates u in place with the updated material.
func ResetPassword(u *User, kek KEK, newPassword string) (RecoveryKey, error) {
	if err := validatePassword(newPassword); err != nil {
		return "", err
	}

	pv, err := NewVerifier([]byte(newPassword), u.Params)
	if err != nil {
		return "", err
	}
	pw, err := kek.Wrap([]byte(newPassword), u.Params)
	if err != nil {
		return "", err
	}

	newRecovery, err := NewRecoveryKey()
	if err != nil {
		return "", err
	}
	rb, err := newRecovery.Bytes()
	if err != nil {
		return "", err
	}
	rv, err := NewVerifier(rb, u.Params)
	if err != nil {
		return "", err
	}
	rw, err := kek.Wrap(rb, u.Params)
	if err != nil {
		return "", err
	}

	u.PasswordVerifier = pv
	u.PasswordKEK = pw
	u.RecoveryVerifier = rv
	u.RecoveryKEK = rw
	u.UpdatedAt = time.Now().UTC()
	return newRecovery, nil
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
