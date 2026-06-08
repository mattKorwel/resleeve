package auth

import (
	"bytes"
	"testing"
)

func mustMasterKey(t *testing.T) []byte {
	t.Helper()
	k, err := GenerateDEK() // 32 random bytes; reuse the generator for the master key
	if err != nil {
		t.Fatalf("GenerateDEK (master): %v", err)
	}
	return k
}

func TestGenerateDEK_LengthAndRandomness(t *testing.T) {
	a, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	if len(a) != 32 {
		t.Fatalf("DEK length = %d, want 32", len(a))
	}
	b, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two DEKs were identical; rand is broken")
	}
}

func TestWrapUnwrapDEK_RoundTrip(t *testing.T) {
	master := mustMasterKey(t)
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	wrapped, err := WrapDEK(master, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if bytes.Equal(wrapped, dek) {
		t.Fatal("wrapped DEK equals plaintext DEK")
	}
	got, err := UnwrapDEK(master, wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("unwrapped DEK != original")
	}
}

func TestEncryptDecryptAtRest_RoundTrip(t *testing.T) {
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	cases := [][]byte{
		nil,
		{},
		[]byte("hello world"),
		bytes.Repeat([]byte("x"), 4096),
	}
	for _, pt := range cases {
		blob, err := EncryptAtRest(dek, pt)
		if err != nil {
			t.Fatalf("EncryptAtRest: %v", err)
		}
		if len(pt) > 0 && bytes.Contains(blob, pt) {
			t.Fatal("ciphertext contains plaintext")
		}
		got, err := DecryptAtRest(dek, blob)
		if err != nil {
			t.Fatalf("DecryptAtRest: %v", err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("round-trip mismatch: got %q want %q", got, pt)
		}
	}
}

func TestEncryptAtRest_NonceIsRandom(t *testing.T) {
	dek, _ := GenerateDEK()
	a, _ := EncryptAtRest(dek, []byte("same plaintext"))
	b, _ := EncryptAtRest(dek, []byte("same plaintext"))
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext were byte-identical; nonce not random")
	}
}

func TestUnwrapDEK_WrongMasterKeyFails(t *testing.T) {
	master := mustMasterKey(t)
	other := mustMasterKey(t)
	dek, _ := GenerateDEK()
	wrapped, err := WrapDEK(master, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if _, err := UnwrapDEK(other, wrapped); err == nil {
		t.Fatal("UnwrapDEK with wrong master key succeeded; must fail (no silent plaintext)")
	}
}

func TestUnwrapDEK_TamperFails(t *testing.T) {
	master := mustMasterKey(t)
	dek, _ := GenerateDEK()
	wrapped, _ := WrapDEK(master, dek)
	// Flip a bit in the ciphertext body.
	tampered := append([]byte(nil), wrapped...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := UnwrapDEK(master, tampered); err == nil {
		t.Fatal("UnwrapDEK on tampered blob succeeded; GCM tag must reject")
	}
}

func TestDecryptAtRest_WrongKeyAndTamperFail(t *testing.T) {
	dek, _ := GenerateDEK()
	wrong, _ := GenerateDEK()
	blob, _ := EncryptAtRest(dek, []byte("secret payload"))

	if _, err := DecryptAtRest(wrong, blob); err == nil {
		t.Fatal("DecryptAtRest with wrong DEK succeeded; must fail")
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := DecryptAtRest(dek, tampered); err == nil {
		t.Fatal("DecryptAtRest on tampered blob succeeded; GCM tag must reject")
	}
	// Truncated blob (shorter than nonce+tag) must error, not panic.
	if _, err := DecryptAtRest(dek, blob[:3]); err == nil {
		t.Fatal("DecryptAtRest on truncated blob succeeded; must error")
	}
}

func TestWrapDEK_RejectsBadLengths(t *testing.T) {
	good := bytes.Repeat([]byte{1}, 32)
	if _, err := WrapDEK(good[:31], good); err == nil {
		t.Fatal("WrapDEK accepted a 31-byte master key")
	}
	if _, err := WrapDEK(good, good[:16]); err == nil {
		t.Fatal("WrapDEK accepted a 16-byte dek")
	}
	if _, err := EncryptAtRest(good[:8], []byte("x")); err == nil {
		t.Fatal("EncryptAtRest accepted an 8-byte dek")
	}
}
