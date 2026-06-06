package serve

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
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

// deterministicSaltForTest used to mirror cli/pair.go's deterministicSalt
// by hand — Q10 caught the silent-drift risk and moved the derivation
// to auth.PairDeterministicSalt so both production callers (CLI inviter +
// accepter) and these tests share one source of truth. Thin alias kept
// to minimise diff to existing call sites.
func deterministicSaltForTest(codeID, label string) []byte {
	return auth.PairDeterministicSalt(codeID, label)
}

// --- sec-H2: pair-claim failed-attempt rate-limit ---

// pairClaimFixture spins up a server, registers an inviter, publishes a
// pair code, and returns the artefacts needed to drive /v2/auth/pair/claim
// from a test. ttl controls publish lifetime; pass 5*time.Minute for
// "normal" tests and 1*time.Nanosecond to deliberately TTL-expire. The
// underlying *Server is exposed so sec-H2 tests can introspect the
// in-memory pairAttempts counter.
type pairClaimFixture struct {
	srv       *Server
	base      string
	codeID    string
	rightHash []byte
	wrongHash []byte
	devToken  string
}

func newPairClaimFixture(t *testing.T, email, codeID string, ttl time.Duration) pairClaimFixture {
	t.Helper()
	srv, base := newIdentityServerForPairClaim(t)

	signup, err := auth.Signup(email, "good-pass-here-22")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	var reg RegisterResp
	post(t, base+"/v2/auth/register", "", minimalRegister(signup), &reg, 201)

	pairCode := "RIGHT-CODE-FOR-LOCKOUT-TEST"
	pairParams := auth.Argon2idParams{MemoryKiB: 16 * 1024, TimeIters: 2, Parallelism: 1}
	vsalt := deterministicSaltForTest(codeID, "pair-verifier")
	vhash := auth.DeriveKey([]byte(pairCode), vsalt, pairParams)
	wrapped, err := signup.KEK.Wrap([]byte(pairCode), pairParams)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	var pub PairPublishResp
	post(t, base+"/v2/auth/pair/publish", reg.DeviceToken, PairPublishReq{
		CodeID:   codeID,
		Params:   Argon2idParams{MemoryKiB: pairParams.MemoryKiB, TimeIters: pairParams.TimeIters, Parallelism: pairParams.Parallelism},
		Verifier: VerifierEnv{Salt: vsalt, Hash: vhash},
		Wrapped:  WrappedKEKEnv{Salt: wrapped.Salt, Nonce: wrapped.Nonce, CT: wrapped.Ciphertext},
		TTL:      ttl,
	}, &pub, 201)

	badHash := auth.DeriveKey([]byte("WRONG-CODE-XX"), vsalt, pairParams)
	return pairClaimFixture{
		srv:       srv,
		base:      base,
		codeID:    codeID,
		rightHash: vhash,
		wrongHash: badHash,
		devToken:  reg.DeviceToken,
	}
}

// newIdentityServerForPairClaim mirrors newIdentityServer but also
// returns the *Server so sec-H2 tests can introspect the in-memory
// pairAttempts map. Kept separate from newIdentityServer so we don't
// have to change every existing call site.
func newIdentityServerForPairClaim(t *testing.T) (*Server, string) {
	t.Helper()
	ts, base, _ := newIdentityServer(t)
	// httptest.Server.Config.Handler is the *Server we passed in.
	srv, ok := ts.Config.Handler.(*Server)
	if !ok {
		t.Fatalf("test server handler is not *Server: %T", ts.Config.Handler)
	}
	return srv, base
}

// claimRaw posts to /v2/auth/pair/claim and returns the raw status +
// body so tests can compare them byte-for-byte. Used by the
// "indistinguishable" assertions.
func claimRaw(t *testing.T, base, codeID string, hash []byte) (int, string) {
	t.Helper()
	body, err := json.Marshal(PairClaimReq{CodeID: codeID, VerifierHash: hash})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest("POST", base+"/v2/auth/pair/claim", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody)
}

// TestPairClaim_LocksAfterFiveFailedAttempts asserts the sec-H2 core
// guarantee: five consecutive wrong-verifier attempts hard-delete the
// code, and the sixth attempt sees a "not found" response identical to
// what an expired-and-swept code would return.
func TestPairClaim_LocksAfterFiveFailedAttempts(t *testing.T) {
	fx := newPairClaimFixture(t, "lock@b.com", "code-lock-5", 5*time.Minute)

	// Attempts 1..4 — should return 401 verifier-failed, code still live.
	for i := 1; i <= pairClaimMaxAttempts-1; i++ {
		status, body := claimRaw(t, fx.base, fx.codeID, fx.wrongHash)
		if status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d (body %q), want 401", i, status, body)
		}
	}
	// Attempt 5 — pushes counter to threshold, triggers lockout, must
	// return the "code not found or expired" shape.
	status, body := claimRaw(t, fx.base, fx.codeID, fx.wrongHash)
	if status != http.StatusNotFound {
		t.Fatalf("attempt 5 (lockout): status %d (body %q), want 404", status, body)
	}
	// Attempt 6 — code already deleted; Get returns ErrNotFound; the
	// response is the same as the lockout response.
	status6, body6 := claimRaw(t, fx.base, fx.codeID, fx.wrongHash)
	if status6 != http.StatusNotFound {
		t.Fatalf("attempt 6: status %d (body %q), want 404", status6, body6)
	}
	// A subsequent attempt with the *correct* verifier hash also fails
	// the same way — the lockout is total, not just per-bad-hash.
	statusR, bodyR := claimRaw(t, fx.base, fx.codeID, fx.rightHash)
	if statusR != http.StatusNotFound {
		t.Fatalf("post-lockout right-hash claim: status %d (body %q), want 404", statusR, bodyR)
	}
}

// TestPairClaim_CorrectClaimBeforeLockoutSucceeds is the sec-H2
// counterpart to the lockout test: a legitimate accepter who fat-fingers
// the code a few times (below the threshold) and then types it correctly
// must still succeed. Lockout protects against brute force; it must not
// punish a human typo within the budget.
func TestPairClaim_CorrectClaimBeforeLockoutSucceeds(t *testing.T) {
	fx := newPairClaimFixture(t, "typo@b.com", "code-typo", 5*time.Minute)

	// Two wrong attempts (well under pairClaimMaxAttempts) — code stays live.
	for i := 0; i < 2; i++ {
		status, body := claimRaw(t, fx.base, fx.codeID, fx.wrongHash)
		if status != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d: status %d (%q), want 401", i, status, body)
		}
	}
	// Correct claim now succeeds despite the prior failures.
	status, body := claimRaw(t, fx.base, fx.codeID, fx.rightHash)
	if status != http.StatusOK {
		t.Fatalf("correct claim before lockout: status %d (%q), want 200", status, body)
	}
	// Counter cleared on success.
	if got := fixtureCounterFor(fx, fx.codeID); got != 0 {
		t.Errorf("counter after successful claim: got %d, want 0", got)
	}
}

// TestPairClaim_LockoutIsIndistinguishableFromExpiry asserts that the
// status code AND error body are byte-identical between (a) a code
// locked out via 5 failed attempts and (b) a code that ran past its TTL
// and was lazily deleted on the next claim. This is the sec-H2 oracle
// guarantee: an attacker who exhausts their budget cannot tell their
// brute force tripped the lockout vs the legitimate inviter's TTL
// expiring naturally.
func TestPairClaim_LockoutIsIndistinguishableFromExpiry(t *testing.T) {
	// (a) lockout path — burn through pairClaimMaxAttempts attempts on a
	// long-TTL code and capture the response at the lockout transition.
	lockFx := newPairClaimFixture(t, "lock@b.com", "code-lock", 5*time.Minute)
	for i := 1; i < pairClaimMaxAttempts; i++ {
		claimRaw(t, lockFx.base, lockFx.codeID, lockFx.wrongHash)
	}
	lockStatus, lockBody := claimRaw(t, lockFx.base, lockFx.codeID, lockFx.wrongHash)

	// (b) TTL-expired path — publish a code with a 1ns TTL, sleep enough
	// for the wall clock to move past it, then claim. The handler should
	// detect expiry, lazy-sweep the row, and emit the same response.
	expFx := newPairClaimFixture(t, "exp@b.com", "code-exp", 1*time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	expStatus, expBody := claimRaw(t, expFx.base, expFx.codeID, expFx.rightHash)

	if lockStatus != expStatus {
		t.Errorf("status: lockout %d, ttl-expiry %d (must match for sec-H2)", lockStatus, expStatus)
	}
	if lockBody != expBody {
		t.Errorf("body: lockout %q, ttl-expiry %q (must match for sec-H2)", lockBody, expBody)
	}
}

// TestPairClaim_SuccessfulClaimDoesNotPersistAttemptCount checks that a
// successful claim wipes its in-memory counter so a downstream operator
// can't observe leftover state. We can't introspect the map from a
// blackbox HTTP test, but we can assert the behavioural consequence:
// after a successful claim of code A, an attacker who somehow republishes
// the same code_id later (via a fresh PairPublish — possible because
// Claim only sets claimed_at, and the row could be re-created by a new
// publish if we Sweep it) does NOT inherit any counter state from the
// previous successful claim. Concretely: after success on code A, we
// publish a new code under the same code_id (after deleting the old row
// via sweep), and verify that 4 wrong attempts on the new code do NOT
// trip the lockout (which would only happen if leftover counter from
// the previous successful claim were still around).
func TestPairClaim_SuccessfulClaimDoesNotPersistAttemptCount(t *testing.T) {
	// Use the lockout-aware fixture so we have a *Server handle for
	// direct map introspection.
	fx := newPairClaimFixture(t, "succ@b.com", "code-success-state", 5*time.Minute)

	// Burn 4 wrong attempts so the in-memory counter sits at 4 — just
	// short of the lockout threshold.
	for i := 0; i < pairClaimMaxAttempts-1; i++ {
		status, body := claimRaw(t, fx.base, fx.codeID, fx.wrongHash)
		if status != http.StatusUnauthorized {
			t.Fatalf("burn %d: got %d (%q), want 401", i, status, body)
		}
	}
	// Sanity: per-code counter should sit at pairClaimMaxAttempts-1.
	if got := fixtureCounterFor(fx, fx.codeID); got != pairClaimMaxAttempts-1 {
		t.Fatalf("pre-success counter for %q: got %d, want %d", fx.codeID, got, pairClaimMaxAttempts-1)
	}

	// Successful claim with the right hash. This must clear the counter
	// so a subsequent operator (or attacker who later regains code_id
	// visibility) cannot probe with the leftover N=4 state.
	if status, body := claimRaw(t, fx.base, fx.codeID, fx.rightHash); status != http.StatusOK {
		t.Fatalf("success claim: got %d (%q), want 200", status, body)
	}

	// Post-success: NO entry must remain in the in-memory counter map
	// — sec-H2's "no leftover state" guarantee. We assert both the
	// per-code lookup AND the global map size to catch both per-row
	// and map-wide leaks.
	if got := fixtureCounterFor(fx, fx.codeID); got != 0 {
		t.Errorf("post-success counter for %q: got %d, want 0", fx.codeID, got)
	}
	if got := fixtureMapSize(fx); got != 0 {
		t.Errorf("post-success pairAttempts map size: %d, want 0 (leftover state lets an attacker probe)", got)
	}
}

// fixtureCounterFor returns the per-code attempt counter under the
// server's mutex. Zero when the entry has been cleared or never written.
func fixtureCounterFor(fx pairClaimFixture, codeID string) int {
	fx.srv.pairAttemptsMu.Lock()
	defer fx.srv.pairAttemptsMu.Unlock()
	return fx.srv.pairAttempts[codeID]
}

// fixtureMapSize returns the total number of entries in the server's
// pairAttempts map. Used to assert "no leftover state anywhere".
func fixtureMapSize(fx pairClaimFixture) int {
	fx.srv.pairAttemptsMu.Lock()
	defer fx.srv.pairAttemptsMu.Unlock()
	return len(fx.srv.pairAttempts)
}
