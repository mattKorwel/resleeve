package auth

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func newTestSealer(t *testing.T) (*AESGCMSealer, []byte) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	s, err := NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	return s, key
}

func TestAESGCMSealer_RoundTrip(t *testing.T) {
	s, _ := newTestSealer(t)
	plaintext := []byte(`{"event":"hello","seq":42}`)
	ct, err := s.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if ct[0] != envelopeVersion {
		t.Errorf("version byte: got 0x%02x, want 0x%02x", ct[0], envelopeVersion)
	}
	got, err := s.Open(ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestAESGCMSealer_EmptyPlaintext(t *testing.T) {
	s, _ := newTestSealer(t)
	ct, err := s.Seal(nil)
	if err != nil {
		t.Fatalf("Seal(nil): %v", err)
	}
	got, err := s.Open(ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty round-trip: got %d bytes, want 0", len(got))
	}
}

func TestAESGCMSealer_TamperedCiphertextFails(t *testing.T) {
	s, _ := newTestSealer(t)
	ct, err := s.Seal([]byte("hello world"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip a bit somewhere in the body (past the version+nonce prefix).
	ct[len(ct)-1] ^= 0x01
	if _, err := s.Open(ct); err == nil {
		t.Fatal("expected error on tampered ciphertext")
	}
}

func TestAESGCMSealer_WrongKeyFails(t *testing.T) {
	s1, _ := newTestSealer(t)
	s2, _ := newTestSealer(t)
	ct, err := s1.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := s2.Open(ct); err == nil {
		t.Fatal("expected error opening with wrong key")
	}
}

func TestAESGCMSealer_NonceUniqueness(t *testing.T) {
	s, _ := newTestSealer(t)
	plaintext := []byte("same input")
	ct1, err := s.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal 1: %v", err)
	}
	ct2, err := s.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("two Seal calls produced identical ciphertext — nonce not randomized")
	}
}

func TestAESGCMSealer_BadKeyLength(t *testing.T) {
	if _, err := NewAESGCMSealer(make([]byte, 16)); err == nil {
		t.Fatal("expected error on 16-byte key")
	}
	if _, err := NewAESGCMSealer(nil); err == nil {
		t.Fatal("expected error on nil key")
	}
}

func TestAESGCMSealer_UnsupportedVersionFails(t *testing.T) {
	s, _ := newTestSealer(t)
	ct, err := s.Seal([]byte("hi"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ct[0] = 0xFF
	if _, err := s.Open(ct); err == nil {
		t.Fatal("expected error on unsupported version byte")
	}
}

func TestAESGCMSealer_ShortCiphertextFails(t *testing.T) {
	s, _ := newTestSealer(t)
	if _, err := s.Open([]byte{0x01, 0x02}); err == nil {
		t.Fatal("expected error on too-short ciphertext")
	}
}

// TestAESGCMSealer_WipeZerosKeyAndDisablesSeal proves sec-H4: Wipe
// scrubs the sealer's owned key copy AND makes subsequent Seal/Open fail
// closed instead of operating with stale material.
func TestAESGCMSealer_WipeZerosKeyAndDisablesSeal(t *testing.T) {
	key := bytes.Repeat([]byte{0xAB}, 32)
	s, err := NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	// Seal works before wipe.
	if _, err := s.Seal([]byte("pre")); err != nil {
		t.Fatalf("Seal before wipe: %v", err)
	}
	// The sealer must hold a NON-zero copy of the key right now.
	if allZero(s.key) {
		t.Fatal("sealer key copy is zero before wipe — nothing to scrub")
	}
	s.Wipe()
	if !allZero(s.key) && s.key != nil {
		t.Errorf("sealer key not zeroed after Wipe: %x", s.key)
	}
	// Seal/Open must now fail closed rather than silently work or panic.
	if _, err := s.Seal([]byte("post")); err == nil {
		t.Error("Seal after Wipe should fail closed")
	}
	if _, err := s.Open(bytes.Repeat([]byte{0}, 64)); err == nil {
		t.Error("Open after Wipe should fail closed")
	}
	// Idempotent: second Wipe is a no-op, not a panic.
	s.Wipe()
}

// TestAESGCMSealer_CopiesKeyOnConstruct proves the sealer doesn't alias
// the caller's buffer — wiping the caller's KEK after construction must
// not break the sealer (sec-H4: handleSealUnlock wipes req.KEK right
// after NewAESGCMSealer).
func TestAESGCMSealer_CopiesKeyOnConstruct(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	s, err := NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	Wipe(key) // caller scrubs its copy
	ct, err := s.Seal([]byte("still works"))
	if err != nil {
		t.Fatalf("Seal after caller wiped its key: %v", err)
	}
	got, err := s.Open(ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(got) != "still works" {
		t.Errorf("round-trip: got %q", got)
	}
}

func TestWipe_ZerosSlice(t *testing.T) {
	b := bytes.Repeat([]byte{0xFF}, 16)
	Wipe(b)
	if !allZero(b) {
		t.Errorf("Wipe left non-zero bytes: %x", b)
	}
	Wipe(nil) // must not panic
}

func TestKEK_Wipe(t *testing.T) {
	k, err := NewKEK()
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	if allZero(k[:]) {
		t.Fatal("fresh KEK is all-zero — astronomically unlikely; rand broken")
	}
	k.Wipe()
	if !allZero(k[:]) {
		t.Errorf("KEK not zeroed after Wipe: %x", k)
	}
	(*KEK)(nil).Wipe() // nil receiver must not panic
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
