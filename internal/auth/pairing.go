package auth

// PairSaltParams is the Argon2id cost used to derive deterministic salts
// for pair-claim verifiers. Cheap on purpose — the derivation is run by
// both the inviter and the accepter on every claim attempt, and the
// salt's only job is to make the verifier hash precomputation per-code
// distinct. The actual secret strength comes from the pair code itself,
// not from this salt.
var PairSaltParams = Argon2idParams{MemoryKiB: 8, TimeIters: 1, Parallelism: 1}

// PairVerifierLabel is the canonical label passed to PairDeterministicSalt
// for pair-claim verifier salts.
const PairVerifierLabel = "pair-verifier"

// PairDeterministicSalt derives a stable 16-byte salt from (codeID, label).
//
// The accepter doesn't know the salt the inviter generated, and we don't
// want a salt-disclosure oracle, so we derive a public-but-bound salt
// from the public code_id. This is NOT security-critical: the salt's job
// is just to make verifier hash precomputation per-code distinct; the
// actual secret is the pair code itself.
//
// Both production callers (cli/pair.go inviter+accepter sides) and tests
// (serve/pair_test.go) call this so the derivation can't drift between
// them.
func PairDeterministicSalt(codeID, label string) []byte {
	h := DeriveKey([]byte(label), []byte(codeID), PairSaltParams)
	if len(h) >= 16 {
		return h[:16]
	}
	return h
}
