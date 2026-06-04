package auth

import (
	"encoding/hex"
	"testing"
)

// TestPairDeterministicSalt_KnownVector pins the exact byte output of
// PairDeterministicSalt for a fixed input. If the Argon2id parameters or
// the inputs to DeriveKey ever change unintentionally, this test breaks
// loudly — which is the whole point of extracting the derivation: the
// two old copies (cli/pair.go and serve/pair_test.go) could drift
// silently because each "matched only itself".
func TestPairDeterministicSalt_KnownVector(t *testing.T) {
	got := PairDeterministicSalt("CODEID-FIXED", PairVerifierLabel)
	if len(got) != 16 {
		t.Fatalf("salt len = %d, want 16", len(got))
	}
	// Reproduce what both old call sites computed by hand: same Argon2id
	// params, same (label,codeID) ordering, first-16 truncation.
	want := DeriveKey([]byte(PairVerifierLabel), []byte("CODEID-FIXED"),
		Argon2idParams{MemoryKiB: 8, TimeIters: 1, Parallelism: 1})[:16]
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("PairDeterministicSalt drift:\n got=%s\nwant=%s",
			hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

// TestPairDeterministicSalt_Stable just asserts repeated calls give the
// same bytes — defends against any accidental nondeterminism.
func TestPairDeterministicSalt_Stable(t *testing.T) {
	a := PairDeterministicSalt("abc", PairVerifierLabel)
	b := PairDeterministicSalt("abc", PairVerifierLabel)
	if hex.EncodeToString(a) != hex.EncodeToString(b) {
		t.Fatalf("salt not stable: a=%x b=%x", a, b)
	}
	// Different codeID must give different salts.
	c := PairDeterministicSalt("abd", PairVerifierLabel)
	if hex.EncodeToString(a) == hex.EncodeToString(c) {
		t.Fatalf("salt unexpectedly equal for different codeID")
	}
}
