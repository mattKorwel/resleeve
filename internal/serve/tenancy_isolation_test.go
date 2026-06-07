package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
	rsync "github.com/mattkorwel/resleeve/internal/sync"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

// decodeJSON decodes an HTTP response body into out.
func decodeJSON(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// newMultiTenantServerWithBackend builds a multi-tenant identity server
// and returns the live backend so tests can inspect the on-disk storage
// keys directly (to prove the brain prefix is applied server-side).
func newMultiTenantServerWithBackend(t *testing.T) (string, rsync.Backend) {
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
		ServeMeta:   store.ServeMeta(),
		Brains:      store.Brains(),
		Memberships: store.Memberships(),
	})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts.URL, backend
}

// registerUser registers a fresh user and returns its (brainID, deviceToken).
func registerUser(t *testing.T, base, email string) (brainID, deviceToken string) {
	t.Helper()
	def := paramsToWire(auth.DefaultArgon2idParams())
	var resp RegisterResp
	post(t, base+"/v2/auth/register", "", buildRegisterReq(t, email, def), &resp, 201)
	if resp.BrainID == "" || resp.DeviceToken == "" {
		t.Fatalf("register %s: missing brain_id or device_token", email)
	}
	return resp.BrainID, resp.DeviceToken
}

// pull is a tiny GET helper returning the decoded PullResp.
func pull(t *testing.T, base, bearer, kind string) PullResp {
	t.Helper()
	req, _ := newHTTPReq(t, base+"/v2/sync/pull?kind="+kind, bearer, nil)
	req.Method = "GET"
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("pull GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pull %s: status %d (%s)", kind, resp.StatusCode, readAll(t, resp.Body))
	}
	var out PullResp
	decodeJSON(t, resp, &out)
	return out
}

// TestTenant_PullIsolation proves two registered users never see each
// other's rows: each pushes the SAME client key (`sessions/<id>`) but the
// server prefixes by the user's personal brain, so B's pull returns only
// B's blob and never A's (different brain prefix).
func TestTenant_PullIsolation(t *testing.T) {
	base, _ := newMultiTenantServerWithBackend(t)

	_, tokA := registerUser(t, base, "alice@example.com")
	_, tokB := registerUser(t, base, "bob@example.com")

	// Both push the identical CLIENT key with distinct blobs.
	postStatus(t, base+"/v2/sync/push", tokA,
		PushReq{Batch: []PushRow{{Key: "sessions/shared-id", Blob: []byte("ALICE-SECRET")}}}, 200)
	postStatus(t, base+"/v2/sync/push", tokB,
		PushReq{Batch: []PushRow{{Key: "sessions/shared-id", Blob: []byte("BOB-SECRET")}}}, 200)

	// A pulls: sees exactly its own blob, key un-prefixed.
	gotA := pull(t, base, tokA, "sessions")
	assertSingleRow(t, gotA, "sessions/shared-id", "ALICE-SECRET")

	// B pulls: sees exactly its own blob, never A's.
	gotB := pull(t, base, tokB, "sessions")
	assertSingleRow(t, gotB, "sessions/shared-id", "BOB-SECRET")
}

func assertSingleRow(t *testing.T, resp PullResp, wantKey, wantBlob string) {
	t.Helper()
	if len(resp.Rows) != 1 {
		t.Fatalf("pull returned %d rows, want exactly 1: %+v", len(resp.Rows), resp.Rows)
	}
	if resp.Rows[0].Key != wantKey {
		t.Errorf("pulled key = %q, want %q (wire protocol must be brain-prefix-free)", resp.Rows[0].Key, wantKey)
	}
	if string(resp.Rows[0].Blob) != wantBlob {
		t.Errorf("pulled blob = %q, want %q", resp.Rows[0].Blob, wantBlob)
	}
}

// TestTenant_StorageKeyIsBrainPrefixed inspects the backend directly: the
// blob a client pushed as `sessions/<id>` must be stored under
// `<brain_id>/sessions/<id>`, and the key returned to the client on pull
// must be the original un-prefixed key.
func TestTenant_StorageKeyIsBrainPrefixed(t *testing.T) {
	base, backend := newMultiTenantServerWithBackend(t)
	ctx := context.Background()

	brainID, tok := registerUser(t, base, "carol@example.com")
	postStatus(t, base+"/v2/sync/push", tok,
		PushReq{Batch: []PushRow{{Key: "sessions/abc", Blob: []byte("blob-bytes")}}}, 200)

	// Storage holds the brain-prefixed key.
	wantStorageKey := brainID + "/sessions/abc"
	keys, _, err := backend.List(ctx, brainID+"/sessions", "", 100)
	if err != nil {
		t.Fatalf("backend.List: %v", err)
	}
	if len(keys) != 1 || keys[0] != wantStorageKey {
		t.Fatalf("storage keys = %v, want [%s]", keys, wantStorageKey)
	}
	// The flat (un-prefixed) key must NOT exist in storage.
	flat, _, err := backend.List(ctx, "sessions", "", 100)
	if err != nil {
		t.Fatalf("backend.List flat: %v", err)
	}
	for _, k := range flat {
		if k == "sessions/abc" {
			t.Errorf("found un-prefixed key %q in storage; client key leaked into the global keyspace", k)
		}
	}

	// Pull returns the un-prefixed key (wire protocol unchanged).
	got := pull(t, base, tok, "sessions")
	assertSingleRow(t, got, "sessions/abc", "blob-bytes")
}

// TestTenant_SingleTenantNoPrefix asserts the escape hatch: with
// --single-tenant the legacy bearer stores flat (no brain prefix), exactly
// the pre-slice keyspace.
func TestTenant_SingleTenantNoPrefix(t *testing.T) {
	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	s, err := New(Config{Backend: backend, AuthToken: testToken, SingleTenant: true})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	postStatus(t, ts.URL+"/v2/sync/push", testToken,
		PushReq{Batch: []PushRow{{Key: "sessions/flat", Blob: []byte("x")}}}, 200)

	keys, _, err := backend.List(context.Background(), "sessions", "", 100)
	if err != nil {
		t.Fatalf("backend.List: %v", err)
	}
	if len(keys) != 1 || keys[0] != "sessions/flat" {
		t.Fatalf("single-tenant storage keys = %v, want [sessions/flat] (no brain prefix)", keys)
	}
}

// TestTenant_PullPaginationUnderBrain asserts the cursor stays correct
// under the brain prefix: a paged pull (limit=1) walks the brain's rows in
// order, the next_cursor is kind-scoped (no brain prefix), and re-sending
// it advances past the first row.
func TestTenant_PullPaginationUnderBrain(t *testing.T) {
	base, _ := newMultiTenantServerWithBackend(t)
	_, tok := registerUser(t, base, "dave@example.com")

	postStatus(t, base+"/v2/sync/push", tok, PushReq{Batch: []PushRow{
		{Key: "memory/s/learnings/1", Blob: []byte("m1")},
		{Key: "memory/s/learnings/2", Blob: []byte("m2")},
	}}, 200)

	// Page 1: limit=1.
	first := pullPaged(t, base, tok, "memory", "", 1)
	if len(first.Rows) != 1 || first.Rows[0].Key != "memory/s/learnings/1" {
		t.Fatalf("page1 = %+v, want [memory/s/learnings/1]", first.Rows)
	}
	if first.NextCursor == "" {
		t.Fatal("page1: empty next_cursor, expected more")
	}
	if strings.Contains(first.NextCursor, "/memory/") && !strings.HasPrefix(first.NextCursor, "memory/") {
		t.Errorf("next_cursor %q leaks the brain prefix; must be kind-scoped", first.NextCursor)
	}

	// Page 2: re-send the cursor; must advance to the second row.
	second := pullPaged(t, base, tok, "memory", first.NextCursor, 1)
	if len(second.Rows) != 1 || second.Rows[0].Key != "memory/s/learnings/2" {
		t.Fatalf("page2 = %+v, want [memory/s/learnings/2]", second.Rows)
	}
}

// TestTenant_SSEIsolation proves the live SSE fan-out is brain-scoped: two
// users subscribe to kind=memory; when A pushes a memory row, only A's SSE
// stream receives it (with the key un-prefixed) and B's never does, even
// though the fan-out set is global.
func TestTenant_SSEIsolation(t *testing.T) {
	base, _ := newMultiTenantServerWithBackend(t)
	_, tokA := registerUser(t, base, "ssea@example.com")
	_, tokB := registerUser(t, base, "sseb@example.com")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	openSSE := func(tok string) chan PushRow {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v2/sync/sse?kind=memory", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("sse GET: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("sse status %d", resp.StatusCode)
		}
		t.Cleanup(func() { resp.Body.Close() })
		ch := make(chan PushRow, 16)
		go readSSE(resp.Body, ch)
		return ch
	}

	evA := openSSE(tokA)
	evB := openSSE(tokB)

	// Give both subscriptions time to register in the fan-out set.
	time.Sleep(150 * time.Millisecond)

	// A pushes a memory row.
	postStatus(t, base+"/v2/sync/push", tokA,
		PushReq{Batch: []PushRow{{Key: "memory/scope/learnings/x", Blob: []byte("A-live")}}}, 200)

	// A receives it, key un-prefixed.
	select {
	case ev := <-evA:
		if ev.Key != "memory/scope/learnings/x" {
			t.Errorf("A live key = %q, want memory/scope/learnings/x (no brain prefix)", ev.Key)
		}
		if string(ev.Blob) != "A-live" {
			t.Errorf("A live blob = %q, want A-live", ev.Blob)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A did not receive its own live memory event")
	}

	// B must NOT receive A's event (different brain).
	select {
	case ev := <-evB:
		t.Fatalf("B received a cross-tenant SSE event: %+v", ev)
	case <-time.After(500 * time.Millisecond):
		// Correct: B's stream stays silent for A's push.
	}
}

// TestTenant_BrainSelectorRejectsNonMember exercises the optional
// ?brain=<id> selector seam: a user requesting a brain they are NOT a
// member of gets 403. Membership of their own brain (the default) is the
// only path otherwise exercised this slice.
func TestTenant_BrainSelectorRejectsNonMember(t *testing.T) {
	base, _ := newMultiTenantServerWithBackend(t)
	brainA, tokA := registerUser(t, base, "sela@example.com")
	_, tokB := registerUser(t, base, "selb@example.com")

	// B asks to act in A's brain via the selector → 403 (not a member).
	q := neturl.Values{}
	q.Set("kind", "sessions")
	q.Set("brain", brainA)
	req, _ := newHTTPReq(t, base+"/v2/sync/pull?"+q.Encode(), tokB, nil)
	req.Method = "GET"
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("pull GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("B pull with brain=%s: status %d, want 403", brainA, resp.StatusCode)
	}

	// A acting in its own brain via the selector → 200 (member).
	postStatus(t, base+"/v2/sync/push", tokA,
		PushReq{Batch: []PushRow{{Key: "sessions/own", Blob: []byte("a")}}}, 200)
	q2 := neturl.Values{}
	q2.Set("kind", "sessions")
	q2.Set("brain", brainA)
	req2, _ := newHTTPReq(t, base+"/v2/sync/pull?"+q2.Encode(), tokA, nil)
	req2.Method = "GET"
	resp2, err := newHTTPClient().Do(req2)
	if err != nil {
		t.Fatalf("A pull GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("A pull with own brain selector: status %d, want 200", resp2.StatusCode)
	}
}

func pullPaged(t *testing.T, base, bearer, kind, since string, limit int) PullResp {
	t.Helper()
	q := neturl.Values{}
	q.Set("kind", kind)
	q.Set("limit", strconv.Itoa(limit))
	if since != "" {
		q.Set("since", since)
	}
	req, _ := newHTTPReq(t, base+"/v2/sync/pull?"+q.Encode(), bearer, nil)
	req.Method = "GET"
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("pull GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pull %s: status %d (%s)", kind, resp.StatusCode, readAll(t, resp.Body))
	}
	var out PullResp
	decodeJSON(t, resp, &out)
	return out
}
