package serve

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
)

// TestPair_FullFlow covers the inviter→server→accepter pairing
// round-trip end-to-end, mirroring the CLI verbs without the prompts.
// It validates the cryptographic core: a KEK published under a pair
// code can be unwrapped on a separate "device" knowing only the
// (code_id, pair_code) tuple.
func TestPair_FullFlow(t *testing.T) {
	_, base, _ := newIdentityServer(t)

	// --- Inviter side: register + login to seed an account.
	signup, err := auth.Signup("inv@b.com", "good-pass-here-22")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	regReq := RegisterReq{
		Email: signup.User.Email,
		Params: Argon2idParams{
			MemoryKiB:   signup.User.Params.MemoryKiB,
			TimeIters:   signup.User.Params.TimeIters,
			Parallelism: signup.User.Params.Parallelism,
		},
		Password: PasswordEnv{
			VerifierSalt: signup.User.PasswordVerifier.Salt,
			VerifierHash: signup.User.PasswordVerifier.Hash,
			KEKSalt:      signup.User.PasswordKEK.Salt,
			KEKNonce:     signup.User.PasswordKEK.Nonce,
			KEKCT:        signup.User.PasswordKEK.Ciphertext,
		},
		Recovery: PasswordEnv{
			VerifierSalt: signup.User.RecoveryVerifier.Salt,
			VerifierHash: signup.User.RecoveryVerifier.Hash,
			KEKSalt:      signup.User.RecoveryKEK.Salt,
			KEKNonce:     signup.User.RecoveryKEK.Nonce,
			KEKCT:        signup.User.RecoveryKEK.Ciphertext,
		},
	}
	var reg RegisterResp
	post(t, base+"/v2/auth/register", "", regReq, &reg, 201)

	// --- Inviter generates pair code + wraps the KEK.
	rawBytes := mustRand(t, 8)
	pairCode := chunkBase32(rawBytes, 12)
	codeID := hexBytes(mustRand(t, 8))
	pairParams := auth.Argon2idParams{MemoryKiB: 16 * 1024, TimeIters: 2, Parallelism: 1}
	verifierSalt := deterministicSaltForTest(codeID, "pair-verifier")
	verifierHash := auth.DeriveKey([]byte(pairCode), verifierSalt, pairParams)
	wrapped, err := signup.KEK.Wrap([]byte(pairCode), pairParams)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	// --- Inviter publishes the envelope.
	var pub PairPublishResp
	post(t, base+"/v2/auth/pair/publish", reg.DeviceToken, PairPublishReq{
		CodeID: codeID,
		Params: Argon2idParams{MemoryKiB: pairParams.MemoryKiB, TimeIters: pairParams.TimeIters, Parallelism: pairParams.Parallelism},
		Verifier: VerifierEnv{Salt: verifierSalt, Hash: verifierHash},
		Wrapped:  WrappedKEKEnv{Salt: wrapped.Salt, Nonce: wrapped.Nonce, CT: wrapped.Ciphertext},
		TTL:      5 * time.Minute,
	}, &pub, 201)
	if pub.CodeID != codeID {
		t.Errorf("publish code_id: got %q, want %q", pub.CodeID, codeID)
	}
	if pub.ExpiresAt.Before(time.Now()) {
		t.Errorf("publish expires_at in the past: %v", pub.ExpiresAt)
	}

	// --- Accepter side: knows code_id + pair_code only. Derives the
	// same verifier_hash (deterministic salt over code_id) and claims.
	accepterVerifierSalt := deterministicSaltForTest(codeID, "pair-verifier")
	accepterHash := auth.DeriveKey([]byte(pairCode), accepterVerifierSalt, pairParams)
	var claim PairClaimResp
	post(t, base+"/v2/auth/pair/claim", "", PairClaimReq{
		CodeID:       codeID,
		VerifierHash: accepterHash,
	}, &claim, 200)

	if claim.UserID != reg.UserID {
		t.Errorf("claim user_id: got %q, want %q", claim.UserID, reg.UserID)
	}
	if claim.DeviceToken == "" {
		t.Fatal("claim: empty device token")
	}

	// --- Accepter unwraps KEK locally; must match the original.
	rcv := auth.WrappedKEK{
		Salt: claim.Wrapped.Salt, Nonce: claim.Wrapped.Nonce, Ciphertext: claim.Wrapped.CT,
		Params: auth.Argon2idParams{
			MemoryKiB: claim.Params.MemoryKiB, TimeIters: claim.Params.TimeIters, Parallelism: claim.Params.Parallelism,
		},
	}
	recovered, err := rcv.Unwrap([]byte(pairCode))
	if err != nil {
		t.Fatalf("accept Unwrap: %v", err)
	}
	if recovered != signup.KEK {
		t.Error("recovered KEK does not equal inviter's KEK")
	}

	// --- Replay protection: second claim must 410 (already used).
	postStatus(t, base+"/v2/auth/pair/claim", "", PairClaimReq{
		CodeID:       codeID,
		VerifierHash: accepterHash,
	}, 410)

	// --- New device token works for /v2/sync/*.
	postStatus(t, base+"/v2/sync/push", claim.DeviceToken,
		PushReq{Batch: []PushRow{{Key: "sessions/x", Blob: []byte("b")}}}, 200)
}

func TestPair_WrongCodeRejected(t *testing.T) {
	_, base, _ := newIdentityServer(t)

	// Register inviter.
	signup, _ := auth.Signup("inv@b.com", "good-pass-here-22")
	regReq := minimalRegister(signup)
	var reg RegisterResp
	post(t, base+"/v2/auth/register", "", regReq, &reg, 201)

	// Publish.
	pairCode := "RIGHT-CODE"
	codeID := "code-zzz"
	pairParams := auth.Argon2idParams{MemoryKiB: 16 * 1024, TimeIters: 2, Parallelism: 1}
	vsalt := deterministicSaltForTest(codeID, "pair-verifier")
	vhash := auth.DeriveKey([]byte(pairCode), vsalt, pairParams)
	wrapped, _ := signup.KEK.Wrap([]byte(pairCode), pairParams)
	var pub PairPublishResp
	post(t, base+"/v2/auth/pair/publish", reg.DeviceToken, PairPublishReq{
		CodeID: codeID,
		Params: Argon2idParams{MemoryKiB: pairParams.MemoryKiB, TimeIters: pairParams.TimeIters, Parallelism: pairParams.Parallelism},
		Verifier: VerifierEnv{Salt: vsalt, Hash: vhash},
		Wrapped:  WrappedKEKEnv{Salt: wrapped.Salt, Nonce: wrapped.Nonce, CT: wrapped.Ciphertext},
	}, &pub, 201)

	// Wrong pair code: 401 from verifier check.
	badHash := auth.DeriveKey([]byte("WRONG-CODE"), vsalt, pairParams)
	postStatus(t, base+"/v2/auth/pair/claim", "", PairClaimReq{
		CodeID:       codeID,
		VerifierHash: badHash,
	}, 401)
}

// --- helpers ---

func minimalRegister(signup *auth.SignupResult) RegisterReq {
	return RegisterReq{
		Email: signup.User.Email,
		Params: Argon2idParams{
			MemoryKiB:   signup.User.Params.MemoryKiB,
			TimeIters:   signup.User.Params.TimeIters,
			Parallelism: signup.User.Params.Parallelism,
		},
		Password: PasswordEnv{
			VerifierSalt: signup.User.PasswordVerifier.Salt,
			VerifierHash: signup.User.PasswordVerifier.Hash,
			KEKSalt:      signup.User.PasswordKEK.Salt,
			KEKNonce:     signup.User.PasswordKEK.Nonce,
			KEKCT:        signup.User.PasswordKEK.Ciphertext,
		},
		Recovery: PasswordEnv{
			VerifierSalt: signup.User.RecoveryVerifier.Salt,
			VerifierHash: signup.User.RecoveryVerifier.Hash,
			KEKSalt:      signup.User.RecoveryKEK.Salt,
			KEKNonce:     signup.User.RecoveryKEK.Nonce,
			KEKCT:        signup.User.RecoveryKEK.Ciphertext,
		},
	}
}

func mustRand(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func hexBytes(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, by := range b {
		out[2*i] = hex[by>>4]
		out[2*i+1] = hex[by&0x0f]
	}
	return string(out)
}

func chunkBase32(b []byte, width int) string {
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	if len(enc) > width {
		enc = enc[:width]
	}
	var parts []string
	for i := 0; i < len(enc); i += 4 {
		end := i + 4
		if end > len(enc) {
			end = len(enc)
		}
		parts = append(parts, enc[i:end])
	}
	return strings.Join(parts, "-")
}

// deterministicSaltForTest mirrors cli/pair.go's deterministicSalt
// (kept in the test package so we don't import the cli package from
// internal/serve, which would create a circular import).
func deterministicSaltForTest(codeID, label string) []byte {
	h := auth.DeriveKey([]byte(label), []byte(codeID), auth.Argon2idParams{MemoryKiB: 8, TimeIters: 1, Parallelism: 1})
	if len(h) >= 16 {
		return h[:16]
	}
	return h
}
