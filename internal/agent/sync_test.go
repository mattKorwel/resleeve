package agent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
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
	srv, err := serve.New(serve.Config{Backend: backend, AuthToken: testSyncToken})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, backend
}

// newSyncTestStore returns a fresh in-memory SQLite store with all v1+v2
// migrations applied.
func newSyncTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), "file::memory:?cache=shared&_pragma=foreign_keys=on")
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
	if err := sc.PullNow(ctx); err != nil {
		t.Fatalf("PullNow: %v", err)
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

	if err := sc.PullNow(ctx); err != nil {
		t.Fatalf("PullNow: %v", err)
	}
	if d, _ := store.Sync().OutboxDepth(ctx); d != 0 {
		t.Errorf("pull side-effect: outbox grew to %d (should be 0 — pull bypasses IngestBatch)", d)
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
