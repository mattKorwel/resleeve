package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/memory"
	"github.com/mattkorwel/resleeve/internal/serve"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

// --- helpers ---

// newMultiTenantServer stands up a real multi-tenant serve.Server (master
// key wired, brain partitioning on) fronted by httptest, returning the
// base URL plus the local backend so a test can inspect raw stored blobs.
func newMultiTenantServer(t *testing.T) (string, *local.Backend) {
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
	master, err := auth.GenerateDEK()
	if err != nil {
		t.Fatalf("gen master: %v", err)
	}
	srv, err := serve.New(serve.Config{
		Backend:     backend,
		AuthToken:   "legacy-test-token",
		ServerUsers: store.ServerUsers(),
		Devices:     store.Devices(),
		Pairings:    store.Pairings(),
		ServeMeta:   store.ServeMeta(),
		Brains:      store.Brains(),
		BrainKeys:   store.BrainKeys(),
		Memberships: store.Memberships(),
		MasterKey:   master,
	})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts.URL, backend
}

// newSingleTenantServer stands up a single-tenant serve.Server (no brain
// partitioning, no master key — the daemon zero-knowledge seals).
func newSingleTenantServer(t *testing.T) (string, *local.Backend, string) {
	t.Helper()
	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	srv, err := serve.New(serve.Config{Backend: backend, AuthToken: testSyncToken, SingleTenant: true})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts.URL, backend, testSyncToken
}

// registerDevice registers a fresh user against a multi-tenant server and
// returns its device token + personal brain id. Mirrors the CLI register
// flow (auth.Signup → POST /v2/auth/register).
func registerDevice(t *testing.T, base, email string) (token, brainID string) {
	t.Helper()
	signup, err := auth.Signup(email, "hunter22-good-password")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	req := serve.RegisterReq{
		Email: signup.User.Email,
		Params: serve.Argon2idParams{
			MemoryKiB:   signup.User.Params.MemoryKiB,
			TimeIters:   signup.User.Params.TimeIters,
			Parallelism: signup.User.Params.Parallelism,
		},
		Password: serve.PasswordEnv{
			VerifierSalt: signup.User.PasswordVerifier.Salt,
			VerifierHash: signup.User.PasswordVerifier.Hash,
			KEKSalt:      signup.User.PasswordKEK.Salt,
			KEKNonce:     signup.User.PasswordKEK.Nonce,
			KEKCT:        signup.User.PasswordKEK.Ciphertext,
		},
		Recovery: serve.PasswordEnv{
			VerifierSalt: signup.User.RecoveryVerifier.Salt,
			VerifierHash: signup.User.RecoveryVerifier.Hash,
			KEKSalt:      signup.User.RecoveryKEK.Salt,
			KEKNonce:     signup.User.RecoveryKEK.Nonce,
			KEKCT:        signup.User.RecoveryKEK.Ciphertext,
		},
		Device: serve.DeviceMetadata{Name: "tester"},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal register: %v", err)
	}
	resp, err := http.Post(base+"/v2/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status %d", resp.StatusCode)
	}
	var out serve.RegisterResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if out.DeviceToken == "" || out.BrainID == "" {
		t.Fatalf("register returned empty token/brain: %+v", out)
	}
	return out.DeviceToken, out.BrainID
}

// --- the seal-decision boundary tests ---

// TestSealDecision_MultiTenantServerSidePushesPlaintext: against a
// multi-tenant server-side server, the daemon's whoami handshake flips
// shouldSeal=false, so the blob landing in the upstream backend is
// PLAINTEXT (the server's at-rest DEK encrypts it on disk), and the round
// trip recovers the original.
func TestSealDecision_MultiTenantServerSidePushesPlaintext(t *testing.T) {
	ctx := context.Background()
	base, backend := newMultiTenantServer(t)
	token, brainID := registerDevice(t, base, "owner@example.com")

	store := newSyncTestStore(t)
	sealer := newTestSealer(t)
	sc := NewSyncClientWithSealer(store, base, token, sealer)
	sc.SetActiveBrain(brainID)

	// Run the handshake synchronously so the decision is settled.
	if err := sc.sampleSealDecision(ctx); err != nil {
		t.Fatalf("sampleSealDecision: %v", err)
	}
	if sc.getShouldSeal() {
		t.Fatal("shouldSeal = true for multi-tenant server-side; want false (plaintext)")
	}

	scope := &memory.Scope{Path: "proj/api", Kind: memory.ScopeKindProject, Description: "secret-scope-content"}
	if err := sc.EnqueueScope(ctx, scope); err != nil {
		t.Fatalf("EnqueueScope: %v", err)
	}
	if err := sc.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}

	// The OUTBOX blob (what the daemon enqueued for push) must be plaintext
	// JSON, NOT a sealed envelope. We assert via the backend's at-rest
	// ciphertext below; here, assert the daemon did not seal by confirming
	// the server could decrypt it as JSON on pull (a sealed blob would be
	// opaque to the server and pull would return garbage / fail ingest).

	// The raw stored blob is the server's AT-REST ciphertext (brain
	// prefix), so it must NOT contain the plaintext marker.
	storedKey := brainID + "/memory/proj:api"
	raw, err := backend.Get(ctx, storedKey)
	if err != nil {
		t.Fatalf("backend.Get(%s): %v", storedKey, err)
	}
	if bytes.Contains(raw, []byte("secret-scope-content")) {
		t.Fatal("stored blob leaks plaintext — server did not encrypt at rest")
	}

	// Pull round-trips the original scope. With shouldSeal=false the daemon
	// must NOT try to unseal the (already-plaintext) server response.
	store2 := newSyncTestStore(t)
	sc2 := NewSyncClientWithSealer(store2, base, token, newTestSealer(t))
	sc2.SetActiveBrain(brainID)
	if err := sc2.sampleSealDecision(ctx); err != nil {
		t.Fatalf("sampleSealDecision (puller): %v", err)
	}
	if _, err := sc2.pullKind(ctx, "memory"); err != nil {
		t.Fatalf("pullKind: %v", err)
	}
	got, err := store2.Memory().GetScope(ctx, "proj/api")
	if err != nil {
		t.Fatalf("GetScope after pull: %v", err)
	}
	if got.Description != "secret-scope-content" {
		t.Fatalf("round-tripped scope description = %q, want secret-scope-content", got.Description)
	}
}

// TestSealDecision_SingleTenantSeals: against a single-tenant server the
// daemon zero-knowledge seals — the blob in the upstream backend is an
// opaque envelope (no plaintext), and a peer with the same KEK pulls and
// unseals the original.
func TestSealDecision_SingleTenantSeals(t *testing.T) {
	ctx := context.Background()
	base, backend, token := newSingleTenantServer(t)

	store := newSyncTestStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	sealerA, _ := auth.NewAESGCMSealer(key)
	sc := NewSyncClientWithSealer(store, base, token, sealerA)

	if err := sc.sampleSealDecision(ctx); err != nil {
		t.Fatalf("sampleSealDecision: %v", err)
	}
	if !sc.getShouldSeal() {
		t.Fatal("shouldSeal = false for single-tenant; want true (zero-knowledge seal)")
	}

	scope := &memory.Scope{Path: "proj/api", Kind: memory.ScopeKindProject, Description: "zk-secret-content"}
	if err := sc.EnqueueScope(ctx, scope); err != nil {
		t.Fatalf("EnqueueScope: %v", err)
	}
	if err := sc.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}

	// Single-tenant keyspace is global (no brain prefix). The stored blob
	// is the sealed envelope and must not contain the plaintext.
	raw, err := backend.Get(ctx, "memory/proj:api")
	if err != nil {
		t.Fatalf("backend.Get: %v", err)
	}
	if bytes.Contains(raw, []byte("zk-secret-content")) {
		t.Fatal("stored blob leaks plaintext — daemon did not seal in single-tenant mode")
	}

	// Peer with the same KEK pulls + unseals the original.
	store2 := newSyncTestStore(t)
	sealerB, _ := auth.NewAESGCMSealer(key)
	sc2 := NewSyncClientWithSealer(store2, base, token, sealerB)
	if err := sc2.sampleSealDecision(ctx); err != nil {
		t.Fatalf("sampleSealDecision (puller): %v", err)
	}
	if _, err := sc2.pullKind(ctx, "memory"); err != nil {
		t.Fatalf("pullKind: %v", err)
	}
	got, err := store2.Memory().GetScope(ctx, "proj/api")
	if err != nil {
		t.Fatalf("GetScope after pull: %v", err)
	}
	if got.Description != "zk-secret-content" {
		t.Fatalf("round-tripped scope = %q, want zk-secret-content", got.Description)
	}
}

// TestComputeShouldSeal exercises the pure seal-decision rule.
func TestComputeShouldSeal(t *testing.T) {
	cases := []struct {
		multiTenant bool
		policy      string
		want        bool
	}{
		{false, "", true},            // single-tenant → always seal
		{false, "server-side", true}, // single-tenant ignores policy
		{true, "server-side", false}, // multi-tenant + server-side → plaintext
		{true, "e2e", true},          // multi-tenant + e2e → seal
		{true, "", false},            // multi-tenant, unknown policy → treat as server-side
	}
	for _, c := range cases {
		if got := computeShouldSeal(c.multiTenant, c.policy); got != c.want {
			t.Errorf("computeShouldSeal(%v, %q) = %v, want %v", c.multiTenant, c.policy, got, c.want)
		}
	}
}
