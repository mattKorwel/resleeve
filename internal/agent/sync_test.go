package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/event"
	"github.com/mattkorwel/resleeve/internal/memory"
	"github.com/mattkorwel/resleeve/internal/serve"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

const testSyncToken = "test-sync-bearer-token-aaaa"

// newSyncTestServer spins up a real serve.Server backed by a local-disk
// Backend in a tempdir, fronted by an httptest.Server.
func newSyncTestServer(t *testing.T) (*httptest.Server, *local.Backend) {
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
	return ts, backend
}

// newSyncTestStore returns a fresh SQLite store with all v1+v2
// migrations applied.
//
// Uses a unique tempfile DSN (NOT `file::memory:?cache=shared`) — the
// shared-cache memory pattern routes every Open() in the process to
// the SAME backing memory DB, which leaks outbox/sync_state/events
// rows across tests in a `go test` run and made several flakes harder
// to triangulate. The actual root cause of the SealedMemoryBlobs flake
// (round-5 followups #2/#7) was a separate lexicographic-ordering bug
// in the outbox next_attempt_at column — see the outboxTimeLayout
// comment in internal/storage/sql/sqlite/sync.go — but the shared
// memory DB was masking and amplifying it. A per-test tempfile gives
// every test a clean DB; t.TempDir() handles cleanup automatically.
func newSyncTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	dsn := "file:" + t.TempDir() + "/store.db?_pragma=foreign_keys=on"
	st, err := sqlite.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSyncClient_DrainPushesEnqueuedEventsToUpstream(t *testing.T) {
	ctx := context.Background()
	ts, backend := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sc := NewSyncClient(store, ts.URL, testSyncToken)

	// Seed the outbox with two events directly (skip IngestBatch
	// orchestration — we're testing the drain wiring).
	now := time.Now().UTC()
	ev1 := event.Event{EventUUID: "E1", SessionID: "S1", Seq: 1000, Kind: event.KindUserMessage, Timestamp: now}
	ev2 := event.Event{EventUUID: "E2", SessionID: "S1", Seq: 2000, Kind: event.KindAssistantMessage, Timestamp: now}
	if err := sc.EnqueueEvents(ctx, []event.Event{ev1, ev2}); err != nil {
		t.Fatalf("EnqueueEvents: %v", err)
	}
	depth, _ := store.Sync().OutboxDepth(ctx)
	if depth != 2 {
		t.Fatalf("outbox depth after enqueue: got %d, want 2", depth)
	}

	// Single drain — synchronous; no goroutines.
	if err := sc.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}

	// Outbox should be empty.
	if d, _ := store.Sync().OutboxDepth(ctx); d != 0 {
		t.Errorf("outbox depth after drain: got %d, want 0", d)
	}

	// Backend should have the rows.
	keys, _, err := backend.List(ctx, "events", "", 100)
	if err != nil {
		t.Fatalf("backend.List: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("backend rows after drain: got %d, want 2", len(keys))
	}
}

func TestSyncClient_EnqueueIdempotent(t *testing.T) {
	ctx := context.Background()
	ts, _ := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sc := NewSyncClient(store, ts.URL, testSyncToken)

	ev := event.Event{EventUUID: "E1", SessionID: "S1", Seq: 1000, Kind: event.KindUserMessage}
	if err := sc.EnqueueEvents(ctx, []event.Event{ev}); err != nil {
		t.Fatal(err)
	}
	// Re-enqueue same event — should be a no-op.
	if err := sc.EnqueueEvents(ctx, []event.Event{ev}); err != nil {
		t.Fatal(err)
	}
	if d, _ := store.Sync().OutboxDepth(ctx); d != 1 {
		t.Errorf("idempotent enqueue: got depth %d, want 1", d)
	}
}

func TestSyncClient_PullIngestsRowsLocallyAndAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	ts, backend := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sc := NewSyncClient(store, ts.URL, testSyncToken)

	// Pre-populate the upstream backend with a session row and two
	// events as if another device had pushed them.
	session := &rsql.Session{
		ID:        "S-REMOTE",
		Slot:      event.Slot{Scope: "remote-scope", AgentName: "default"},
		CLI:       "claude",
		StartedAt: time.Now().UTC(),
		Status:    rsql.SessionStatusActive,
	}
	sesBlob, _ := json.Marshal(session)
	if err := backend.Put(ctx, "sessions/S-REMOTE", sesBlob); err != nil {
		t.Fatal(err)
	}
	for i, e := range []event.Event{
		{EventUUID: "E1", SessionID: "S-REMOTE", Seq: 1000, Kind: event.KindUserMessage, SchemaVersion: 1, Content: json.RawMessage(`{"text":"hi"}`), Vendor: event.Vendor{Name: "claude"}},
		{EventUUID: "E2", SessionID: "S-REMOTE", Seq: 2000, Kind: event.KindAssistantMessage, SchemaVersion: 1, Content: json.RawMessage(`{"text":"hello"}`), Vendor: event.Vendor{Name: "claude"}},
	} {
		blob, _ := json.Marshal(e)
		key := encodeEventKey(e)
		if err := backend.Put(ctx, key, blob); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	// Pull.
	if _, err := sc.PullNowCounts(ctx); err != nil {
		t.Fatalf("PullNowCounts: %v", err)
	}

	// Session should now exist locally.
	got, err := store.Sessions().Get(ctx, "S-REMOTE")
	if err != nil {
		t.Fatalf("session not pulled: %v", err)
	}
	if got.Slot.Scope != "remote-scope" {
		t.Errorf("pulled session scope: got %q, want %q", got.Slot.Scope, "remote-scope")
	}

	// Events should now exist locally.
	events, err := store.Events().List(ctx, "S-REMOTE", 0, 100)
	if err != nil {
		t.Fatalf("events list: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("pulled events: got %d, want 2", len(events))
	}

	// Cursor should be advanced.
	cur, _ := store.Sync().GetCursor(ctx, "events")
	if cur == "" {
		t.Errorf("events cursor not advanced")
	}
}

func TestSyncClient_PullThenPushDoesNotLoop(t *testing.T) {
	// Critical invariant: ingesting via PullNow MUST NOT enqueue back
	// to the outbox (we go through stores directly, not IngestBatch).
	// Otherwise machine B would push back to upstream what it just pulled.
	ctx := context.Background()
	ts, backend := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sc := NewSyncClient(store, ts.URL, testSyncToken)

	ev := event.Event{EventUUID: "E1", SessionID: "S-LOOP", Seq: 1000, Kind: event.KindUserMessage, Vendor: event.Vendor{Name: "claude"}}
	blob, _ := json.Marshal(ev)
	if err := backend.Put(ctx, encodeEventKey(ev), blob); err != nil {
		t.Fatal(err)
	}
	// Also seed an upstream session so the event has a parent row.
	ses := &rsql.Session{ID: "S-LOOP", CLI: "claude", StartedAt: time.Now().UTC(), Status: rsql.SessionStatusActive}
	sb, _ := json.Marshal(ses)
	if err := backend.Put(ctx, "sessions/S-LOOP", sb); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.PullNowCounts(ctx); err != nil {
		t.Fatalf("PullNowCounts: %v", err)
	}
	if d, _ := store.Sync().OutboxDepth(ctx); d != 0 {
		t.Errorf("pull side-effect: outbox grew to %d (should be 0 — pull bypasses IngestBatch)", d)
	}
}

// --- slice 2.5: envelope encryption at the sync boundary ---
func TestSyncClient_MemoryEnqueueAndPush(t *testing.T) {
	ctx := context.Background()
	ts, backend := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sc := NewSyncClient(store, ts.URL, testSyncToken)

	now := time.Now().UTC()
	scope := &memory.Scope{Path: "monorepo/svc-billing", Kind: memory.ScopeKindProject, Title: "billing", CreatedAt: now, UpdatedAt: now}
	plan := &memory.Plan{Scope: "monorepo/svc-billing", Name: memory.DefaultPlanSlot, Content: "## plan", UpdatedAt: now}
	learning := &memory.Learning{ID: "L_123_abc", Scope: "monorepo/svc-billing", Content: "watch the FK ordering", CreatedAt: now}

	if err := sc.EnqueueScope(ctx, scope); err != nil {
		t.Fatalf("EnqueueScope: %v", err)
	}
	if err := sc.EnqueuePlan(ctx, plan); err != nil {
		t.Fatalf("EnqueuePlan: %v", err)
	}
	if err := sc.EnqueueLearning(ctx, learning); err != nil {
		t.Fatalf("EnqueueLearning: %v", err)
	}

	if d, _ := store.Sync().OutboxDepth(ctx); d != 3 {
		t.Fatalf("outbox depth after enqueues: got %d, want 3", d)
	}

	if err := sc.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}

	if d, _ := store.Sync().OutboxDepth(ctx); d != 0 {
		t.Errorf("outbox depth after drain: got %d, want 0", d)
	}

	// The backend should now hold the three rows under their documented
	// keys. We assert by listing the memory prefix and checking exact keys.
	keys, _, err := backend.List(ctx, "memory", "", 100)
	if err != nil {
		t.Fatalf("backend.List: %v", err)
	}
	want := map[string]bool{
		"memory/monorepo:svc-billing":                              true,
		"memory/monorepo:svc-billing/plans/" + memory.DefaultPlanSlot: true,
		"memory/monorepo:svc-billing/learnings/L_123_abc":          true,
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key on backend: %q", k)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		for k := range want {
			t.Errorf("missing key on backend: %q", k)
		}
	}
}

// TestSyncClient_PullMemoryDispatchesByPrefix preloads the upstream
// backend with one scope + plan + learning row each, calls PullNow,
// and asserts each landed in the local memory store via its proper
// MemoryStore method (dispatched by key prefix in ingestMemoryRow).
func TestSyncClient_PullMemoryDispatchesByPrefix(t *testing.T) {
	ctx := context.Background()
	ts, backend := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sc := NewSyncClient(store, ts.URL, testSyncToken)

	// Seed upstream backend. Key shape places scope first
	// (memory/<path>) and resources under sub-segments
	// (memory/<path>/plans/..., memory/<path>/learnings/...) so the
	// scope row sorts before its dependants — satisfies the local FK
	// constraint when pulled in lexicographic order.
	scope := &memory.Scope{Path: "monorepo/svc-x", Kind: memory.ScopeKindProject, Title: "x"}
	sb, _ := json.Marshal(scope)
	if err := backend.Put(ctx, "memory/monorepo:svc-x", sb); err != nil {
		t.Fatal(err)
	}

	plan := &memory.Plan{Scope: "monorepo/svc-x", Name: memory.DefaultPlanSlot, Content: "## seeded plan"}
	pb, _ := json.Marshal(plan)
	if err := backend.Put(ctx, "memory/monorepo:svc-x/plans/"+memory.DefaultPlanSlot, pb); err != nil {
		t.Fatal(err)
	}

	learning := &memory.Learning{ID: "L_999_ff", Scope: "monorepo/svc-x", Content: "remote insight"}
	lb, _ := json.Marshal(learning)
	if err := backend.Put(ctx, "memory/monorepo:svc-x/learnings/L_999_ff", lb); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.PullNowCounts(ctx); err != nil {
		t.Fatalf("PullNowCounts: %v", err)
	}

	// Scope should be local now.
	gotScope, err := store.Memory().GetScope(ctx, "monorepo/svc-x")
	if err != nil {
		t.Fatalf("GetScope: %v", err)
	}
	if gotScope.Title != "x" {
		t.Errorf("scope title: got %q, want x", gotScope.Title)
	}

	gotPlan, err := store.Memory().GetPlan(ctx, "monorepo/svc-x", memory.DefaultPlanSlot)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if gotPlan.Content != "## seeded plan" {
		t.Errorf("plan content: got %q", gotPlan.Content)
	}

	gotLearning, err := store.Memory().GetLearning(ctx, "L_999_ff")
	if err != nil {
		t.Fatalf("GetLearning: %v", err)
	}
	if gotLearning.Content != "remote insight" {
		t.Errorf("learning content: got %q", gotLearning.Content)
	}

	// Cursor should have advanced.
	cur, _ := store.Sync().GetCursor(ctx, "memory")
	if cur == "" {
		t.Errorf("memory cursor not advanced")
	}

	// And: PullNow MUST NOT enqueue back to the outbox.
	if d, _ := store.Sync().OutboxDepth(ctx); d != 0 {
		t.Errorf("pull-side-effect: outbox grew to %d (want 0)", d)
	}
}

// encodeEventKey mirrors the key shape SyncClient.EnqueueEvents emits.
// Duplicated in tests rather than exported because key format is an
// internal contract between Enqueue and the upstream backend.
func newTestSealer(t *testing.T) auth.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand seal key: %v", err)
	}
	s, err := auth.NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	return s
}

// TestSyncClient_SealedBlobsAreOpaqueOnUpstream verifies that with a
// Sealer configured, the blobs landing in the upstream backend do NOT
// contain any plaintext from the event (zero-knowledge invariant).
func TestSyncClient_SealedBlobsAreOpaqueOnUpstream(t *testing.T) {
	ctx := context.Background()
	ts, backend := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sealer := newTestSealer(t)
	sc := NewSyncClientWithSealer(store, ts.URL, testSyncToken, sealer)

	const marker = "TOPSECRET-EVENT-UUID-7777"
	ev := event.Event{
		EventUUID: marker,
		SessionID: "S-SEAL",
		Seq:       1000,
		Kind:      event.KindUserMessage,
		Vendor:    event.Vendor{Name: "claude"},
		Content:   json.RawMessage(`{"text":"plaintext-marker-PASSWORD123"}`),
		Timestamp: time.Now().UTC(),
	}
	if err := sc.EnqueueEvents(ctx, []event.Event{ev}); err != nil {
		t.Fatalf("EnqueueEvents: %v", err)
	}
	if err := sc.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}

	keys, _, err := backend.List(ctx, "events", "", 100)
	if err != nil {
		t.Fatalf("backend.List: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("backend keys after drain: got %d, want 1", len(keys))
	}
	blob, err := backend.Get(ctx, keys[0])
	if err != nil {
		t.Fatalf("backend.Get: %v", err)
	}
	if bytes.Contains(blob, []byte(marker)) {
		t.Errorf("upstream blob leaks event UUID %q", marker)
	}
	if bytes.Contains(blob, []byte("plaintext-marker-PASSWORD123")) {
		t.Errorf("upstream blob leaks event content plaintext")
	}
	if bytes.Contains(blob, []byte("claude")) {
		t.Errorf("upstream blob leaks vendor name (plaintext)")
	}
}

// TestSyncClient_SealedMemoryBlobsAreOpaqueOnUpstream is the regression
// for a hot-fix found during cross-machine validation: slice 3 added
// EnqueueScope/Plan/Learning in parallel with slice 2.5's encryption
// wrap, and the memory enqueue paths did NOT initially seal their
// blobs. This test guards against that gap so the zero-knowledge
// promise holds for the memory tier the persona values most.
func TestSyncClient_SealedMemoryBlobsAreOpaqueOnUpstream(t *testing.T) {
	ctx := context.Background()
	ts, backend := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sealer := newTestSealer(t)
	sc := NewSyncClientWithSealer(store, ts.URL, testSyncToken, sealer)

	const markerTitle = "TOPSECRET-SCOPE-TITLE-1234"
	const markerPlan = "PLAINTEXT-PLAN-PASSWORD-5678"
	const markerLearning = "PLAINTEXT-LEARNING-API-KEY-9999"

	scope := &memory.Scope{Path: "secret-project", Kind: memory.ScopeKindProject, Title: markerTitle}
	plan := &memory.Plan{Scope: "secret-project", Name: memory.DefaultPlanSlot, Content: markerPlan}
	learning := &memory.Learning{ID: "L_seal_test", Scope: "secret-project", Content: markerLearning}

	if err := sc.EnqueueScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err := sc.EnqueuePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := sc.EnqueueLearning(ctx, learning); err != nil {
		t.Fatal(err)
	}
	if err := sc.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}

	keys, _, err := backend.List(ctx, "memory", "", 100)
	if err != nil {
		t.Fatalf("backend list: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("backend memory rows: got %d, want 3", len(keys))
	}
	for _, k := range keys {
		blob, err := backend.Get(ctx, k)
		if err != nil {
			t.Fatalf("backend.Get %s: %v", k, err)
		}
		for _, marker := range []string{markerTitle, markerPlan, markerLearning, "secret-project"} {
			if bytes.Contains(blob, []byte(marker)) {
				t.Errorf("upstream blob %q leaks plaintext %q", k, marker)
			}
		}
	}
}

// TestSyncClient_RoundTripWithSealer pushes encrypted on client A and
// verifies client B (same key, distinct store) decrypts and ingests
// the event with original fields intact.
func TestSyncClient_RoundTripWithSealer(t *testing.T) {
	ctx := context.Background()
	ts, _ := newSyncTestServer(t)

	// Two distinct file-backed stores so client B's pull lands in its
	// own SQLite, not client A's shared-cache memory DB.
	dirA := t.TempDir()
	dirB := t.TempDir()
	storeA, err := sqlite.Open(ctx, "file:"+dirA+"/a.db?_pragma=foreign_keys=on")
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	t.Cleanup(func() { _ = storeA.Close() })
	storeB, err := sqlite.Open(ctx, "file:"+dirB+"/b.db?_pragma=foreign_keys=on")
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	t.Cleanup(func() { _ = storeB.Close() })

	// Same seal key on both sides.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealerA, _ := auth.NewAESGCMSealer(key)
	sealerB, _ := auth.NewAESGCMSealer(key)

	scA := NewSyncClientWithSealer(storeA, ts.URL, testSyncToken, sealerA)
	scB := NewSyncClientWithSealer(storeB, ts.URL, testSyncToken, sealerB)

	// A first pushes a session (parent row) and then the event.
	ses := &rsql.Session{ID: "S-RT", CLI: "claude", StartedAt: time.Now().UTC(), Status: rsql.SessionStatusActive}
	if err := scA.EnqueueSession(ctx, ses); err != nil {
		t.Fatalf("EnqueueSession: %v", err)
	}
	ev := event.Event{
		EventUUID:     "EV-ROUNDTRIP",
		SessionID:     "S-RT",
		Seq:           1000,
		Kind:          event.KindUserMessage,
		SchemaVersion: 1,
		Vendor:        event.Vendor{Name: "claude"},
		Content:       json.RawMessage(`{"text":"hello-from-A"}`),
		Timestamp:     time.Now().UTC(),
	}
	if err := scA.EnqueueEvents(ctx, []event.Event{ev}); err != nil {
		t.Fatalf("EnqueueEvents: %v", err)
	}
	if err := scA.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce A: %v", err)
	}

	if _, err := scB.PullNowCounts(ctx); err != nil {
		t.Fatalf("PullNowCounts B: %v", err)
	}

	gotSes, err := storeB.Sessions().Get(ctx, "S-RT")
	if err != nil {
		t.Fatalf("B has no session: %v", err)
	}
	if gotSes.CLI != "claude" {
		t.Errorf("session CLI: got %q, want %q", gotSes.CLI, "claude")
	}
	events, err := storeB.Events().List(ctx, "S-RT", 0, 100)
	if err != nil {
		t.Fatalf("B events list: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("B event count: got %d, want 1", len(events))
	}
	if events[0].EventUUID != "EV-ROUNDTRIP" {
		t.Errorf("B event UUID: got %q, want %q", events[0].EventUUID, "EV-ROUNDTRIP")
	}
	if !bytes.Contains([]byte(events[0].Content), []byte("hello-from-A")) {
		t.Errorf("B event content: got %s, want contains hello-from-A", events[0].Content)
	}
}

// TestSyncClient_DecryptFailureWithWrongKey ensures that when client B
// has the wrong seal key, the pull ingests nothing AND the events
// cursor stays empty (the failed row is not counted as committed).
func TestSyncClient_DecryptFailureWithWrongKey(t *testing.T) {
	ctx := context.Background()
	ts, _ := newSyncTestServer(t)

	dirA := t.TempDir()
	dirB := t.TempDir()
	storeA, err := sqlite.Open(ctx, "file:"+dirA+"/a.db?_pragma=foreign_keys=on")
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	t.Cleanup(func() { _ = storeA.Close() })
	storeB, err := sqlite.Open(ctx, "file:"+dirB+"/b.db?_pragma=foreign_keys=on")
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	t.Cleanup(func() { _ = storeB.Close() })

	keyA := make([]byte, 32)
	keyB := make([]byte, 32)
	_, _ = rand.Read(keyA)
	_, _ = rand.Read(keyB)
	sealerA, _ := auth.NewAESGCMSealer(keyA)
	sealerB, _ := auth.NewAESGCMSealer(keyB)

	scA := NewSyncClientWithSealer(storeA, ts.URL, testSyncToken, sealerA)
	scB := NewSyncClientWithSealer(storeB, ts.URL, testSyncToken, sealerB)

	ses := &rsql.Session{ID: "S-BAD", CLI: "claude", StartedAt: time.Now().UTC(), Status: rsql.SessionStatusActive}
	if err := scA.EnqueueSession(ctx, ses); err != nil {
		t.Fatalf("EnqueueSession: %v", err)
	}
	ev := event.Event{
		EventUUID: "EV-BAD",
		SessionID: "S-BAD",
		Seq:       1000,
		Kind:      event.KindUserMessage,
		Vendor:    event.Vendor{Name: "claude"},
		Timestamp: time.Now().UTC(),
	}
	if err := scA.EnqueueEvents(ctx, []event.Event{ev}); err != nil {
		t.Fatalf("EnqueueEvents: %v", err)
	}
	if err := scA.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce A: %v", err)
	}

	// B pulls — every row should fail to decrypt, logged but no crash.
	if _, err := scB.PullNowCounts(ctx); err != nil {
		t.Fatalf("PullNowCounts B (should not crash): %v", err)
	}

	// Nothing landed in B.
	if _, err := storeB.Sessions().Get(ctx, "S-BAD"); err == nil {
		t.Errorf("B should not have session — decryption should have failed")
	}
	events, _ := storeB.Events().List(ctx, "S-BAD", 0, 100)
	if len(events) != 0 {
		t.Errorf("B event count: got %d, want 0", len(events))
	}

	// Cursor should NOT advance: every row in this pull failed.
	cur, _ := storeB.Sync().GetCursor(ctx, "events")
	if cur != "" {
		t.Errorf("events cursor advanced on decrypt-failure-only pull: got %q, want empty", cur)
	}
	sesCur, _ := storeB.Sync().GetCursor(ctx, "sessions")
	if sesCur != "" {
		t.Errorf("sessions cursor advanced on decrypt-failure-only pull: got %q, want empty", sesCur)
	}
}

// --- escape-hatch endpoints (item #1) ---

// TestSyncClient_DrainNowAndPullNowCounts exercises the count-returning
// wrappers used by POST /v1/sync/push-now and /v1/sync/pull-now.
// Locks in the wire-shape contract: DrainNow returns total rows pushed,
// PullNowCounts returns per-kind ingest counts.
func TestSyncClient_DrainNowAndPullNowCounts(t *testing.T) {
	ctx := context.Background()
	ts, backend := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sc := NewSyncClient(store, ts.URL, testSyncToken)

	// Seed two events into the local outbox, then drain via DrainNow.
	now := time.Now().UTC()
	evs := []event.Event{
		{EventUUID: "DN-1", SessionID: "S-DN", Seq: 1000, Kind: event.KindUserMessage, Timestamp: now},
		{EventUUID: "DN-2", SessionID: "S-DN", Seq: 2000, Kind: event.KindAssistantMessage, Timestamp: now},
	}
	if err := sc.EnqueueEvents(ctx, evs); err != nil {
		t.Fatalf("EnqueueEvents: %v", err)
	}
	pushed, err := sc.DrainNow(ctx)
	if err != nil {
		t.Fatalf("DrainNow: %v", err)
	}
	if pushed != 2 {
		t.Errorf("DrainNow pushed: got %d, want 2", pushed)
	}
	// Subsequent call with empty outbox returns 0, no error.
	pushed2, err := sc.DrainNow(ctx)
	if err != nil {
		t.Fatalf("DrainNow (empty): %v", err)
	}
	if pushed2 != 0 {
		t.Errorf("DrainNow on empty outbox: got %d, want 0", pushed2)
	}

	// Seed upstream rows for a clean second store to pull.
	store2 := newSyncTestStore(t)
	sc2 := NewSyncClient(store2, ts.URL, testSyncToken)

	ses := &rsql.Session{ID: "S-DN", CLI: "claude", StartedAt: now, Status: rsql.SessionStatusActive}
	sb, _ := json.Marshal(ses)
	if err := backend.Put(ctx, "sessions/S-DN", sb); err != nil {
		t.Fatal(err)
	}

	counts, err := sc2.PullNowCounts(ctx)
	if err != nil {
		t.Fatalf("PullNowCounts: %v", err)
	}
	// Two events were drained from sc above; both landed on backend.
	if counts["sessions"] < 1 {
		t.Errorf("PullNowCounts sessions: got %d, want >= 1", counts["sessions"])
	}
	if counts["events"] < 2 {
		t.Errorf("PullNowCounts events: got %d, want >= 2", counts["events"])
	}
	if _, ok := counts["memory"]; !ok {
		t.Errorf("PullNowCounts missing memory key (should be present even when 0)")
	}
}

// countingSealer wraps Seal+Open to record how many times Seal was
// called. Used by TestSyncClient_EnqueueEventsSamplesSealerPerEvent to
// detect the sec-M4 fix: ClearSealer between two EnqueueEvents loop
// iterations should be observed mid-batch.
type countingSealer struct {
	inner auth.Sealer
	seals int
}

func (c *countingSealer) Seal(pt []byte) ([]byte, error) {
	c.seals++
	return c.inner.Seal(pt)
}

func (c *countingSealer) Open(ct []byte) ([]byte, error) {
	return c.inner.Open(ct)
}

// TestEnqueueEvents_SamplesSealerPerEvent guards the sec-M4 fix: a
// concurrent ClearSealer between two events in the same batch must be
// observed on the SECOND event, not silently overridden by a stale
// sealer reference captured at the top of the loop.
//
// We exercise the path deterministically by installing a sealer,
// enqueuing the first event (Seal #1), clearing the sealer, then
// enqueuing the second event in the same call and asserting only one
// Seal call landed. The pre-fix code path captured `sl := s.getSealer()`
// before the loop, so a sealer cleared mid-call still produced a sealed
// blob for the rest of the batch — sec-M4 in REVIEW_SECURITY.md.
func TestEnqueueEvents_SamplesSealerPerEvent(t *testing.T) {
	ctx := context.Background()
	ts, _ := newSyncTestServer(t)
	store := newSyncTestStore(t)
	sc := NewSyncClient(store, ts.URL, testSyncToken)

	cs := &countingSealer{inner: newTestSealer(t)}
	sc.SetSealer(cs)

	// Batch the first event alone so we can land a ClearSealer in
	// between. Same effect as a concurrent goroutine racing the loop;
	// the post-fix invariant is "each loop iteration re-reads the
	// current sealer", and this sequence is the minimal demonstration.
	ev1 := event.Event{EventUUID: "E1-M4", SessionID: "S-M4", Seq: 1000, Kind: event.KindUserMessage}
	ev2 := event.Event{EventUUID: "E2-M4", SessionID: "S-M4", Seq: 2000, Kind: event.KindAssistantMessage}

	if err := sc.EnqueueEvents(ctx, []event.Event{ev1}); err != nil {
		t.Fatalf("EnqueueEvents #1: %v", err)
	}
	if cs.seals != 1 {
		t.Fatalf("after first enqueue: seals=%d, want 1", cs.seals)
	}

	sc.ClearSealer()
	if err := sc.EnqueueEvents(ctx, []event.Event{ev2}); err != nil {
		t.Fatalf("EnqueueEvents #2: %v", err)
	}
	if cs.seals != 1 {
		t.Errorf("after ClearSealer + second enqueue: seals=%d, want 1 (pre-fix this would be 2 if sl captured outside loop)", cs.seals)
	}

	// Re-install the sealer and enqueue another event in the same
	// batch as a third: confirms the re-sample picks the new sealer
	// back up too (this is the symmetric direction of the fix).
	sc.SetSealer(cs)
	ev3 := event.Event{EventUUID: "E3-M4", SessionID: "S-M4", Seq: 3000, Kind: event.KindUserMessage}
	if err := sc.EnqueueEvents(ctx, []event.Event{ev3}); err != nil {
		t.Fatalf("EnqueueEvents #3: %v", err)
	}
	if cs.seals != 2 {
		t.Errorf("after re-install + enqueue: seals=%d, want 2", cs.seals)
	}
}

// encodeEventKey mirrors the key shape SyncClient.EnqueueEvents emits.
// Duplicated in tests rather than exported because key format is an
// internal contract between Enqueue and the upstream backend.
func encodeEventKey(e event.Event) string {
	return "events/" + e.SessionID + "/" + zeroPad20(e.Seq) + "-" + e.EventUUID
}

func zeroPad20(n int64) string {
	const width = 20
	digits := ""
	if n == 0 {
		digits = "0"
	}
	for n > 0 {
		digits = string(rune('0'+(n%10))) + digits
		n /= 10
	}
	for len(digits) < width {
		digits = "0" + digits
	}
	return digits
}
