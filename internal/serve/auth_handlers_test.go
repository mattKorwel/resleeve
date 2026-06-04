package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/auth"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

// newIdentityServer builds a Server with the full identity stack
// (server_users + devices + pairings) and the local-disk backend, all
// scoped to t.TempDir() so tests are hermetic.
func newIdentityServer(t *testing.T) (*httptest.Server, string, *sqlite.Store) {
	t.Helper()
	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	dsn := "file:" + t.TempDir() + "/id.db?_pragma=journal_mode=WAL&_pragma=foreign_keys=on"
	store, err := sqlite.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	s, err := New(Config{
		Backend:     backend,
		AuthToken:   testToken,
		ServerUsers: store.ServerUsers(),
		Devices:     store.Devices(),
		Pairings:    store.Pairings(),
	})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts, ts.URL, store
}

func TestRegister_LoginRoundTrip(t *testing.T) {
	_, base, _ := newIdentityServer(t)

	// 1) Locally derive crypto material exactly like the CLI does.
	signup, err := auth.Signup("a@b.com", "hunter22-good-pass")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	req := RegisterReq{
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
		Device: DeviceMetadata{Name: "tester"},
	}

	var resp RegisterResp
	post(t, base+"/v2/auth/register", "", req, &resp, 201)
	if resp.DeviceToken == "" || !strings.HasPrefix(resp.DeviceToken, "dev_") {
		t.Errorf("register: bad device token %q", resp.DeviceToken)
	}

	// 2) Duplicate email — must 409.
	postStatus(t, base+"/v2/auth/register", "", req, 409)

	// 3) Login flow: challenge → login.
	var chal LoginChallengeResp
	post(t, base+"/v2/auth/login-challenge", "", LoginChallengeReq{Email: "a@b.com"}, &chal, 200)
	if len(chal.VerifierSalt) == 0 {
		t.Fatal("login-challenge: empty verifier salt")
	}
	verifier := auth.DeriveKey([]byte("hunter22-good-pass"), chal.VerifierSalt, auth.Argon2idParams{
		MemoryKiB:   chal.Params.MemoryKiB,
		TimeIters:   chal.Params.TimeIters,
		Parallelism: chal.Params.Parallelism,
	})
	var login LoginResp
	post(t, base+"/v2/auth/login", "", LoginReq{
		Email: "a@b.com", VerifierHash: verifier, Device: DeviceMetadata{Name: "tester-2"},
	}, &login, 200)
	if login.DeviceToken == "" {
		t.Fatal("login: empty device token")
	}
	if string(login.WrappedKEK.Salt) == "" || len(login.WrappedKEK.CT) == 0 {
		t.Fatal("login: empty wrapped KEK")
	}

	// 4) Wrong password — 401.
	bad := auth.DeriveKey([]byte("not-the-pass"), chal.VerifierSalt, auth.Argon2idParams{
		MemoryKiB: chal.Params.MemoryKiB, TimeIters: chal.Params.TimeIters, Parallelism: chal.Params.Parallelism,
	})
	postStatus(t, base+"/v2/auth/login", "", LoginReq{Email: "a@b.com", VerifierHash: bad}, 401)

	// 5) Unwrap the KEK locally to confirm round-trip integrity.
	wrapped := auth.WrappedKEK{
		Salt: login.WrappedKEK.Salt, Nonce: login.WrappedKEK.Nonce, Ciphertext: login.WrappedKEK.CT,
		Params: auth.Argon2idParams{
			MemoryKiB: login.Params.MemoryKiB, TimeIters: login.Params.TimeIters, Parallelism: login.Params.Parallelism,
		},
	}
	kek, err := wrapped.Unwrap([]byte("hunter22-good-pass"))
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if kek == (auth.KEK{}) {
		t.Error("Unwrapped KEK is zero")
	}

	// 6) The device token issued by login is accepted by sync routes.
	postStatus(t, base+"/v2/sync/push", login.DeviceToken,
		PushReq{Batch: []PushRow{{Key: "sessions/x", Blob: []byte("blob")}}}, 200)

	// 7) Logout revokes the token; subsequent /sync calls 401.
	postStatus(t, base+"/v2/auth/logout", login.DeviceToken, struct{}{}, 200)
	postStatus(t, base+"/v2/sync/push", login.DeviceToken,
		PushReq{Batch: []PushRow{{Key: "sessions/y", Blob: []byte("b")}}}, 401)
}

// TestLoginChallenge_ConstantShapeForUnknownEmail asserts that
// /v2/auth/login-challenge returns an indistinguishable response for
// unknown emails (no 404 enumeration oracle). The follow-up login still
// fails on the synthetic salt — UX identical, no info leak.
func TestLoginChallenge_ConstantShapeForUnknownEmail(t *testing.T) {
	_, base, _ := newIdentityServer(t)

	// Register one real account so we can compare known-vs-unknown.
	signup, err := auth.Signup("real@example.com", "hunter22-good-pass")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	postStatus(t, base+"/v2/auth/register", "", RegisterReq{
		Email:  signup.User.Email,
		Params: Argon2idParams{MemoryKiB: signup.User.Params.MemoryKiB, TimeIters: signup.User.Params.TimeIters, Parallelism: signup.User.Params.Parallelism},
		Password: PasswordEnv{
			VerifierSalt: signup.User.PasswordVerifier.Salt, VerifierHash: signup.User.PasswordVerifier.Hash,
			KEKSalt: signup.User.PasswordKEK.Salt, KEKNonce: signup.User.PasswordKEK.Nonce, KEKCT: signup.User.PasswordKEK.Ciphertext,
		},
		Recovery: PasswordEnv{
			VerifierSalt: signup.User.RecoveryVerifier.Salt, VerifierHash: signup.User.RecoveryVerifier.Hash,
			KEKSalt: signup.User.RecoveryKEK.Salt, KEKNonce: signup.User.RecoveryKEK.Nonce, KEKCT: signup.User.RecoveryKEK.Ciphertext,
		},
		Device: DeviceMetadata{Name: "tester"},
	}, 201)

	// Known email → 200 with the real salt.
	var known LoginChallengeResp
	post(t, base+"/v2/auth/login-challenge", "", LoginChallengeReq{Email: "real@example.com"}, &known, 200)

	// Unknown email → must ALSO be 200 (not 404) and same shape.
	var unknown LoginChallengeResp
	post(t, base+"/v2/auth/login-challenge", "", LoginChallengeReq{Email: "ghost@example.com"}, &unknown, 200)

	if len(unknown.VerifierSalt) != len(known.VerifierSalt) {
		t.Errorf("salt lengths differ: known=%d unknown=%d (leaks existence)", len(known.VerifierSalt), len(unknown.VerifierSalt))
	}
	if unknown.Params.MemoryKiB == 0 || unknown.Params.TimeIters == 0 || unknown.Params.Parallelism == 0 {
		t.Errorf("unknown-email challenge missing Argon2id params: %+v", unknown.Params)
	}
	if string(unknown.VerifierSalt) == string(known.VerifierSalt) {
		t.Errorf("unknown synthetic salt collides with real salt — should be unrelated")
	}

	// Determinism: same unknown email twice → same synthetic salt.
	// Otherwise repeated probes would expose the unknown branch via
	// salt churn (real accounts have stable salts).
	var unknown2 LoginChallengeResp
	post(t, base+"/v2/auth/login-challenge", "", LoginChallengeReq{Email: "ghost@example.com"}, &unknown2, 200)
	if string(unknown.VerifierSalt) != string(unknown2.VerifierSalt) {
		t.Errorf("synthetic salt for same unknown email not stable: %x vs %x", unknown.VerifierSalt, unknown2.VerifierSalt)
	}

	// Different unknown emails → different salts (no fixed-decoy tell).
	var unknown3 LoginChallengeResp
	post(t, base+"/v2/auth/login-challenge", "", LoginChallengeReq{Email: "nobody@example.com"}, &unknown3, 200)
	if string(unknown.VerifierSalt) == string(unknown3.VerifierSalt) {
		t.Errorf("synthetic salt is not per-email: collision across unknown emails")
	}

	// Email case normalization: real account lookup is lower-cased, so
	// the mixed-case form of a KNOWN email must hit the real branch
	// (same salt as `known`), not the synthetic one.
	var knownMixedCase LoginChallengeResp
	post(t, base+"/v2/auth/login-challenge", "", LoginChallengeReq{Email: "Real@Example.COM"}, &knownMixedCase, 200)
	if string(knownMixedCase.VerifierSalt) != string(known.VerifierSalt) {
		t.Errorf("known email case-mismatch routed to synthetic branch (leaks via case probe)")
	}

	// The follow-up login on the synthetic challenge must still fail
	// (verifier mismatch on a non-existent account) — same UX, no leak.
	bogus := auth.DeriveKey([]byte("anything"), unknown.VerifierSalt, auth.Argon2idParams{
		MemoryKiB: unknown.Params.MemoryKiB, TimeIters: unknown.Params.TimeIters, Parallelism: unknown.Params.Parallelism,
	})
	postStatus(t, base+"/v2/auth/login", "",
		LoginReq{Email: "ghost@example.com", VerifierHash: bogus, Device: DeviceMetadata{Name: "x"}}, 401)
}

func TestRegister_InvalidPayload(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	postStatus(t, base+"/v2/auth/register", "", RegisterReq{}, 400)
	postStatus(t, base+"/v2/auth/register", "", RegisterReq{Email: "not-an-email"}, 400)
}

// buildRegisterReq returns a fully-formed RegisterReq with the given
// Argon2id params overriding auth.Signup's defaults. Lets the sec-H1
// tests probe each axis of the floor/ceiling without rebuilding all the
// envelope plumbing inline.
func buildRegisterReq(t *testing.T, email string, override Argon2idParams) RegisterReq {
	t.Helper()
	signup, err := auth.Signup(email, "hunter22-good-pass")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	return RegisterReq{
		Email:  signup.User.Email,
		Params: override,
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
		Device: DeviceMetadata{Name: "tester"},
	}
}

// TestRegister_RejectsWeakParams asserts the sec-H1 floor: a malicious
// client cannot register with sub-floor Argon2id params on any axis
// (memory, time, parallelism). Each below-floor request must 400 BEFORE
// the verifier is persisted.
func TestRegister_RejectsWeakParams(t *testing.T) {
	_, base, _ := newIdentityServer(t)

	cases := []struct {
		name   string
		params Argon2idParams
	}{
		{"memory_below_floor", Argon2idParams{MemoryKiB: argon2MinMemoryKiB - 1, TimeIters: argon2MinTimeIters, Parallelism: argon2MinParallelism}},
		{"memory_tiny", Argon2idParams{MemoryKiB: 8, TimeIters: argon2MinTimeIters, Parallelism: argon2MinParallelism}},
		{"time_below_floor", Argon2idParams{MemoryKiB: argon2MinMemoryKiB, TimeIters: argon2MinTimeIters - 1, Parallelism: argon2MinParallelism}},
		{"parallelism_zero", Argon2idParams{MemoryKiB: argon2MinMemoryKiB, TimeIters: argon2MinTimeIters, Parallelism: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			postStatus(t, base+"/v2/auth/register", "", buildRegisterReq(t, tc.name+"@example.com", tc.params), 400)
		})
	}
}

// TestRegister_RejectsDoSParams asserts the sec-H1 ceiling: a malicious
// client cannot register with absurd Argon2id params that would punish
// every legitimate login thereafter (the server itself doesn't run
// Argon2 for register, but the params are stored and consumed by the
// client on every login).
func TestRegister_RejectsDoSParams(t *testing.T) {
	_, base, _ := newIdentityServer(t)

	cases := []struct {
		name   string
		params Argon2idParams
	}{
		{"memory_above_ceiling", Argon2idParams{MemoryKiB: argon2MaxMemoryKiB + 1, TimeIters: argon2MinTimeIters, Parallelism: argon2MinParallelism}},
		{"time_above_ceiling", Argon2idParams{MemoryKiB: argon2MinMemoryKiB, TimeIters: argon2MaxTimeIters + 1, Parallelism: argon2MinParallelism}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			postStatus(t, base+"/v2/auth/register", "", buildRegisterReq(t, tc.name+"@example.com", tc.params), 400)
		})
	}
}

// TestRegister_AcceptsRoundTwoTenBaseline asserts the sec-H1 floor is
// not over-eager: exactly-at-floor params (the round-2/10 baseline) must
// be accepted. This is the canonical "honest client" config.
func TestRegister_AcceptsRoundTwoTenBaseline(t *testing.T) {
	_, base, _ := newIdentityServer(t)

	baseline := Argon2idParams{
		MemoryKiB:   argon2MinMemoryKiB,
		TimeIters:   argon2MinTimeIters,
		Parallelism: argon2MinParallelism,
	}
	var resp RegisterResp
	post(t, base+"/v2/auth/register", "", buildRegisterReq(t, "baseline@example.com", baseline), &resp, 201)
	if resp.DeviceToken == "" {
		t.Fatal("baseline register: empty device token")
	}
}

func TestSync_LegacyBearerStillWorks(t *testing.T) {
	// Migration overlap: register a user, but the existing legacy bearer
	// must keep authenticating /v2/sync/* calls until operators rotate
	// devices to per-device tokens.
	_, base, _ := newIdentityServer(t)
	postStatus(t, base+"/v2/sync/push", testToken,
		PushReq{Batch: []PushRow{{Key: "sessions/legacy", Blob: []byte("b")}}}, 200)
}

// TestPairPublish_RejectsLegacyBearerWithEmptyUID guards sec-H3: the
// legacy single-bearer fallback returns a synthetic device with
// UserID="". Before the fix, that synthetic device could mint a
// pairing_codes row under an empty user. After the fix, pair/publish
// must reject with 401 — only a real per-device token can publish.
func TestPairPublish_RejectsLegacyBearerWithEmptyUID(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	// Body shape is irrelevant — the guard fires before decode.
	postStatus(t, base+"/v2/auth/pair/publish", testToken, PairPublishReq{
		CodeID: "anything",
	}, 401)
}

// TestSyncPush_StillAcceptsLegacyBearer is the regression companion:
// the sec-H3 guard must NOT bleed into content-blind sync endpoints.
// Legacy bearer + /v2/sync/push remains 200 until operators rotate
// devices.
func TestSyncPush_StillAcceptsLegacyBearer(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	postStatus(t, base+"/v2/sync/push", testToken,
		PushReq{Batch: []PushRow{{Key: "sessions/regression", Blob: []byte("b")}}}, 200)
}

// TestRequireUserDevice_RejectsEmptyUID is a direct unit test on the
// helper — verifies that an empty UserID device is rejected (writes
// 401) and a real-user device is accepted (returns true, no write).
func TestRequireUserDevice_RejectsEmptyUID(t *testing.T) {
	s, _, _ := newIdentityServer(t)
	_ = s // server is only constructed for parity with other tests; we use a fresh Server below.

	srv := &Server{}
	r := httptest.NewRequest("POST", "/v2/auth/pair/publish", nil)

	// 1) Legacy synthetic device (UserID=="") → rejected, 401 written.
	rec := httptest.NewRecorder()
	if ok := srv.requireUserDevice(rec, r, &rsql.Device{ID: "legacy", UserID: ""}); ok {
		t.Fatal("requireUserDevice: legacy-synthetic device returned true; want false")
	}
	if rec.Code != 401 {
		t.Errorf("requireUserDevice: legacy-synthetic status = %d, want 401", rec.Code)
	}

	// 2) Real device (UserID set) → accepted, nothing written.
	rec2 := httptest.NewRecorder()
	if ok := srv.requireUserDevice(rec2, r, &rsql.Device{ID: "dev_xxx", UserID: "u_real"}); !ok {
		t.Fatal("requireUserDevice: real device returned false; want true")
	}
	if rec2.Code != 200 {
		// httptest.NewRecorder defaults to 200 if WriteHeader was not called.
		t.Errorf("requireUserDevice: real-device write occurred (status=%d); want no write", rec2.Code)
	}

	// 3) Nil device → rejected as well (defense-in-depth).
	rec3 := httptest.NewRecorder()
	if ok := srv.requireUserDevice(rec3, r, nil); ok {
		t.Fatal("requireUserDevice: nil device returned true; want false")
	}
	if rec3.Code != 401 {
		t.Errorf("requireUserDevice: nil-device status = %d, want 401", rec3.Code)
	}
}

// --- helpers ---

func post(t *testing.T, url, bearer string, body any, out any, wantStatus int) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := newHTTPReq(t, url, bearer, b)
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		errBody := readAll(t, resp.Body)
		t.Fatalf("%s: got %d, want %d (%s)", url, resp.StatusCode, wantStatus, errBody)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
}

func postStatus(t *testing.T, url, bearer string, body any, wantStatus int) {
	t.Helper()
	post(t, url, bearer, body, nil, wantStatus)
}
