package auth

import (
	"errors"
	"testing"
)

func TestSignupLoginRoundTrip(t *testing.T) {
	r, err := Signup("matt@example.com", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	if r.RecoveryKey == "" {
		t.Fatal("recovery key not returned")
	}

	kek, err := Login(r.User, "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if kek != r.KEK {
		t.Errorf("login KEK mismatch")
	}

	if _, err := Login(r.User, "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password should produce ErrInvalidCredentials, got %v", err)
	}
}

func TestRecoverRoundTrip(t *testing.T) {
	r, err := Signup("matt@example.com", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	kek, err := Recover(r.User, r.RecoveryKey)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if kek != r.KEK {
		t.Errorf("recovery KEK mismatch")
	}

	// Wrong recovery key (well-formed but doesn't match) → invalid credentials.
	wrong, _ := NewRecoveryKey()
	if _, err := Recover(r.User, wrong); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong recovery key should produce ErrInvalidCredentials, got %v", err)
	}
}

func TestResetPasswordRotatesRecovery(t *testing.T) {
	r, err := Signup("matt@example.com", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	kek, err := Recover(r.User, r.RecoveryKey)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	newRecovery, err := ResetPassword(r.User, kek, "another-strong-password-13!")
	if err != nil {
		t.Fatalf("reset password: %v", err)
	}
	if newRecovery == r.RecoveryKey {
		t.Errorf("recovery key not rotated")
	}

	// Old password rejected.
	if _, err := Login(r.User, "correct-horse-battery-staple"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("old password should be rejected after reset")
	}
	// New password works and returns the same KEK.
	kek2, err := Login(r.User, "another-strong-password-13!")
	if err != nil {
		t.Errorf("new password should work after reset: %v", err)
	}
	if kek2 != kek {
		t.Errorf("KEK changed across password reset; should be stable")
	}
	// Old recovery key rejected.
	if _, err := Recover(r.User, r.RecoveryKey); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("old recovery should be rejected after rotation")
	}
	// New recovery key works.
	if _, err := Recover(r.User, newRecovery); err != nil {
		t.Errorf("new recovery should work: %v", err)
	}
}

func TestRecoveryKeyFormat(t *testing.T) {
	for i := 0; i < 5; i++ {
		rk, err := NewRecoveryKey()
		if err != nil {
			t.Fatalf("new recovery key: %v", err)
		}
		s := string(rk)
		if s[:5] != "RESL-" {
			t.Errorf("recovery key missing RESL- prefix: %q", s)
		}
		buf, err := rk.Bytes()
		if err != nil {
			t.Fatalf("decode recovery key %q: %v", s, err)
		}
		if len(buf) != recoveryKeyBytes {
			t.Errorf("recovery key decoded wrong length: got %d, want %d", len(buf), recoveryKeyBytes)
		}
	}
}

func TestSignupValidation(t *testing.T) {
	cases := []struct{ email, password string }{
		{"", "password-long-enough"},
		{"no-at-sign", "password-long-enough"},
		{"matt@example.com", "short"},
	}
	for _, tc := range cases {
		if _, err := Signup(tc.email, tc.password); err == nil {
			t.Errorf("Signup(%q, %q) should have failed validation", tc.email, tc.password)
		}
	}
}
