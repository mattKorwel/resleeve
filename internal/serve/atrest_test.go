package serve

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/mattkorwel/resleeve/internal/auth"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

// newAtRestServer builds an identity server with a 32-byte master key and
// the BrainKeys store wired, returning the test server, its base URL, the
// store, and the local backend (so tests can inspect raw stored blobs and
// brain_keys rows). masterKey lets callers reuse / rotate the key. A nil
// masterKey is only valid in single-tenant mode (multi-tenant New rejects
// a missing key after the slice-2 default-flip), so the nil-key callers
// flip on singleTenant.
func newAtRestServer(t *testing.T, masterKey []byte) (string, *sqlite.Store, *local.Backend) {
	t.Helper()
	return newAtRestServerTenancy(t, masterKey, len(masterKey) == 0)
}

func newAtRestServerTenancy(t *testing.T, masterKey []byte, singleTenant bool) (string, *sqlite.Store, *local.Backend) {
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
		Backend:      backend,
		AuthToken:    testToken,
		ServerUsers:  store.ServerUsers(),
		Devices:      store.Devices(),
		Pairings:     store.Pairings(),
		ServeMeta:    store.ServeMeta(),
		Brains:       store.Brains(),
		BrainKeys:    store.BrainKeys(),
		Memberships:  store.Memberships(),
		MasterKey:    masterKey,
		SingleTenant: singleTenant,
	})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts.URL, store, backend
}

func mustMaster(t *testing.T) []byte {
	t.Helper()
	k, err := auth.GenerateDEK()
	if err != nil {
		t.Fatalf("gen master: %v", err)
	}
	return k
}

// TestAtRest_BrainCreateStoresWrappedDEK: registering a user (personal
// brain) and creating a shared brain each persist a wrapped DEK that
// unwraps under the master key.
func TestAtRest_BrainCreateStoresWrappedDEK(t *testing.T) {
	master := mustMaster(t)
	base, store, _ := newAtRestServer(t, master)
	_, tok, personal := regUser(t, base, "owner@example.com")

	var created CreateBrainResp
	post(t, base+"/v1/brains", tok, CreateBrainReq{Name: "team"}, &created, 201)

	for _, brainID := range []string{personal, created.BrainID} {
		wrapped, err := store.BrainKeys().GetBrainKey(context.Background(), brainID)
		if err != nil {
			t.Fatalf("GetBrainKey(%s): %v", brainID, err)
		}
		dek, err := auth.UnwrapDEK(master, wrapped)
		if err != nil {
			t.Fatalf("UnwrapDEK(%s): %v", brainID, err)
		}
		if len(dek) != 32 {
			t.Fatalf("dek for %s has length %d, want 32", brainID, len(dek))
		}
	}

	// encryption_policy defaults to server-side and is surfaced.
	b, err := store.Brains().Get(context.Background(), personal)
	if err != nil {
		t.Fatalf("Brains.Get: %v", err)
	}
	if b.EncryptionPolicy != rsql.EncryptionPolicyServerSide {
		t.Fatalf("encryption_policy = %q, want server-side", b.EncryptionPolicy)
	}
}

// TestAtRest_PushIsCiphertextPullIsPlaintext: a pushed blob is stored as
// ciphertext (≠ what the client sent) and pull returns the original.
func TestAtRest_PushIsCiphertextPullIsPlaintext(t *testing.T) {
	master := mustMaster(t)
	base, _, backend := newAtRestServer(t, master)
	_, tok, personal := regUser(t, base, "owner@example.com")

	plaintext := []byte("the-secret-memory-blob")
	push := PushReq{Batch: []PushRow{{Key: "memory/scope-a", Blob: plaintext}}}
	postStatus(t, base+"/v2/sync/push", tok, push, 200)

	// Raw stored blob (brain-prefixed key) must be ciphertext.
	storedKey := personal + "/memory/scope-a"
	raw, err := backend.Get(context.Background(), storedKey)
	if err != nil {
		t.Fatalf("backend.Get(%s): %v", storedKey, err)
	}
	if bytes.Equal(raw, plaintext) {
		t.Fatal("stored blob equals plaintext; at-rest encryption did not run")
	}
	if bytes.Contains(raw, plaintext) {
		t.Fatal("stored blob contains plaintext substring")
	}

	// Pull returns the original plaintext.
	var pulled PullResp
	getOK(t, base+"/v2/sync/pull?kind=memory", tok, &pulled)
	if len(pulled.Rows) != 1 {
		t.Fatalf("pull: got %d rows, want 1", len(pulled.Rows))
	}
	if !bytes.Equal(pulled.Rows[0].Blob, plaintext) {
		t.Fatalf("pulled blob = %q, want %q", pulled.Rows[0].Blob, plaintext)
	}
}

// TestAtRest_NoMasterKeyStoresAsIs: with no master key (single-tenant),
// push→pull round-trips the original blob (legacy passthrough on the
// at-rest path).
func TestAtRest_NoMasterKeyStoresAsIs(t *testing.T) {
	base, _, _ := newAtRestServer(t, nil) // single-tenant, no at-rest crypto
	_, tok, personal := regUser(t, base, "owner@example.com")

	plaintext := []byte("plain-blob-no-crypto")
	push := PushReq{Batch: []PushRow{{Key: "memory/scope-a", Blob: plaintext}}}
	postStatus(t, base+"/v2/sync/push", tok, push, 200)

	var pulled PullResp
	getOK(t, base+"/v2/sync/pull?kind=memory", tok, &pulled)
	if len(pulled.Rows) != 1 || !bytes.Equal(pulled.Rows[0].Blob, plaintext) {
		t.Fatalf("no-master-key round trip failed: %+v", pulled.Rows)
	}
	_ = personal
}

// TestAtRest_NoMasterKeyRawBytesUnchanged: with a nil master key (only
// valid in single-tenant mode after the slice-2 flip), the backend holds
// exactly what the client sent — no at-rest envelope. Note the daemon
// would still CLIENT-seal in single-tenant mode (whoami → shouldSeal);
// this test pushes raw bytes directly to assert the server's at-rest path
// is the no-op, independent of any client sealing.
func TestAtRest_NoMasterKeyRawBytesUnchanged(t *testing.T) {
	base, store, backend := newAtRestServer(t, nil) // nil master key → single-tenant, at-rest disabled
	_, tok, personal := regUser(t, base, "owner@example.com")

	// No DEK should have been provisioned.
	if _, err := store.BrainKeys().GetBrainKey(context.Background(), personal); err == nil {
		t.Fatal("a brain key was provisioned despite no master key")
	}

	plaintext := []byte("verbatim-bytes")
	push := PushReq{Batch: []PushRow{{Key: "memory/scope-a", Blob: plaintext}}}
	postStatus(t, base+"/v2/sync/push", tok, push, 200)

	// Single-tenant keyspace is global (no brain prefix).
	raw, err := backend.Get(context.Background(), "memory/scope-a")
	if err != nil {
		t.Fatalf("backend.Get: %v", err)
	}
	if !bytes.Equal(raw, plaintext) {
		t.Fatalf("stored blob = %q, want verbatim %q", raw, plaintext)
	}
	_ = personal
}

// TestAtRest_RotatedMasterKeyFailsToUnwrap: data written under one master
// key cannot be served under a different (rotated/wrong) master key — the
// pull fails rather than silently returning plaintext or garbage.
func TestAtRest_RotatedMasterKeyFailsToUnwrap(t *testing.T) {
	master := mustMaster(t)
	base, store, backend := newAtRestServer(t, master)
	_, tok, personal := regUser(t, base, "owner@example.com")

	plaintext := []byte("written-under-original-key")
	push := PushReq{Batch: []PushRow{{Key: "memory/scope-a", Blob: plaintext}}}
	postStatus(t, base+"/v2/sync/push", tok, push, 200)

	// Stand up a SECOND server over the same store + backend dir but with a
	// DIFFERENT master key (simulating a wrong/rotated key with stale
	// wrapped DEKs). The wrapped DEK won't unwrap → pull must error.
	wrongMaster := mustMaster(t)
	s2, err := New(Config{
		Backend:     backend,
		AuthToken:   testToken,
		ServerUsers: store.ServerUsers(),
		Devices:     store.Devices(),
		Pairings:    store.Pairings(),
		ServeMeta:   store.ServeMeta(),
		Brains:      store.Brains(),
		BrainKeys:   store.BrainKeys(),
		Memberships: store.Memberships(),
		MasterKey:   wrongMaster,
	})
	if err != nil {
		t.Fatalf("serve.New (wrong key): %v", err)
	}
	ts2 := httptest.NewServer(s2)
	t.Cleanup(ts2.Close)

	// Pull under the wrong master key must NOT return the plaintext.
	getStatus(t, ts2.URL+"/v2/sync/pull?kind=memory", tok, 500)
	_ = personal
}

// TestAtRest_RejectsBadMasterKeyLength: a configured-but-wrong-length
// master key fails server construction.
func TestAtRest_RejectsBadMasterKeyLength(t *testing.T) {
	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	if _, err := New(Config{Backend: backend, AuthToken: testToken, MasterKey: []byte("too-short")}); err == nil {
		t.Fatal("New accepted a 9-byte master key")
	}
}
