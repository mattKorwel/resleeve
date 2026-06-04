package auth

import (
	"testing"
)

func TestSignupProducesMaterial(t *testing.T) {
	r, err := Signup("matt@example.com", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	if r.RecoveryKey == "" {
		t.Fatal("recovery key not returned")
	}
	if r.User == nil {
		t.Fatal("user not returned")
	}
	if r.User.Email != "matt@example.com" {
		t.Errorf("email: got %q, want matt@example.com", r.User.Email)
	}
	// All four wrap-material components must be populated; the CLI
	// streams these to `resleeve serve` in RegisterReq.
	if len(r.User.PasswordVerifier.Hash) == 0 || len(r.User.PasswordVerifier.Salt) == 0 {
		t.Error("password verifier not populated")
	}
	if len(r.User.PasswordKEK.Ciphertext) == 0 || len(r.User.PasswordKEK.Salt) == 0 {
		t.Error("password KEK wrap not populated")
	}
	if len(r.User.RecoveryVerifier.Hash) == 0 || len(r.User.RecoveryVerifier.Salt) == 0 {
		t.Error("recovery verifier not populated")
	}
	if len(r.User.RecoveryKEK.Ciphertext) == 0 || len(r.User.RecoveryKEK.Salt) == 0 {
		t.Error("recovery KEK wrap not populated")
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
