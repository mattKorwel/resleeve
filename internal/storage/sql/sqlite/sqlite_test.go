package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, "file::memory:?cache=shared&_pragma=foreign_keys=on")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	slot := event.Slot{Scope: "auth-rewrite", AgentName: "claude-3"}
	now := time.Now().UTC().Truncate(time.Microsecond)

	ses := &rsql.Session{
		ID:         "01HZW001",
		Slot:       slot,
		CLI:        "claude_code",
		CLIVersion: "2.1.140",
		Cwd:        "/Users/test/proj",
		StartedAt:  now,
		Status:     rsql.SessionStatusActive,
	}
	if err := st.Sessions().Create(ctx, ses); err != nil {
		t.Fatalf("create session: %v", err)
	}

	got, err := st.Sessions().Get(ctx, ses.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.ID != ses.ID || got.Slot != slot || got.CLI != ses.CLI {
		t.Errorf("session mismatch: got %+v, want %+v", got, ses)
	}
	if got.Status != rsql.SessionStatusActive {
		t.Errorf("status: got %q, want %q", got.Status, rsql.SessionStatusActive)
	}

	turnID := "01HZT001"
	events := []event.Event{
		{
			EventUUID:     "01HZ001",
			SessionID:     ses.ID,
			Slot:          slot,
			Seq:           1,
			Timestamp:     now,
			Kind:          event.KindUserMessage,
			SchemaVersion: 1,
			Content:       json.RawMessage(`{"text":"fix the auth bug"}`),
			Vendor:        event.Vendor{Name: "claude_code", Version: "2.1.140"},
		},
		{
			EventUUID:     "01HZ002",
			SessionID:     ses.ID,
			Slot:          slot,
			Seq:           2,
			TurnID:        &turnID,
			Timestamp:     now.Add(time.Second),
			Kind:          event.KindAssistantMessage,
			SchemaVersion: 1,
			Content:       json.RawMessage(`{"text":"reading the middleware first."}`),
			Vendor: event.Vendor{
				Name:          "claude_code",
				Version:       "2.1.140",
				NativePayload: json.RawMessage(`{"raw":"vendor payload"}`),
			},
		},
	}
	if err := st.Events().Append(ctx, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Idempotency: re-appending must not error or duplicate.
	if err := st.Events().Append(ctx, events); err != nil {
		t.Fatalf("re-append should be idempotent: %v", err)
	}

	listed, err := st.Events().List(ctx, ses.ID, 0, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 events after idempotent re-append, got %d", len(listed))
	}
	if listed[0].EventUUID != "01HZ001" || listed[1].EventUUID != "01HZ002" {
		t.Errorf("event ordering wrong: %v, %v", listed[0].EventUUID, listed[1].EventUUID)
	}
	if listed[1].TurnID == nil || *listed[1].TurnID != turnID {
		t.Errorf("turn_id round-trip failed: %v", listed[1].TurnID)
	}
	if len(listed[1].Vendor.NativePayload) == 0 {
		t.Errorf("vendor native_payload not round-tripped")
	}

	// Slot heartbeat + get.
	if err := st.Slots().Heartbeat(ctx, slot, ses.ID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	state, err := st.Slots().Get(ctx, slot)
	if err != nil {
		t.Fatalf("get slot: %v", err)
	}
	if state.CurrentSessionID != ses.ID {
		t.Errorf("slot current_session_id: got %q, want %q", state.CurrentSessionID, ses.ID)
	}

	// Heartbeat again with the same slot to verify upsert semantics.
	if err := st.Slots().Heartbeat(ctx, slot, ""); err != nil {
		t.Fatalf("re-heartbeat: %v", err)
	}
	state2, err := st.Slots().Get(ctx, slot)
	if err != nil {
		t.Fatalf("get slot after re-heartbeat: %v", err)
	}
	if state2.CurrentSessionID != "" {
		t.Errorf("slot current_session_id should be cleared, got %q", state2.CurrentSessionID)
	}
}

// TestSessions_CreateIdempotent is the regression test for the
// TOCTOU window in IngestBatch's auto-create-on-first-event path.
// Two near-simultaneous first-event hooks for the same session must
// both succeed (and not clobber each other's fields).
func TestSessions_CreateIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	slot := event.Slot{Scope: "scope", AgentName: "default"}
	original := &rsql.Session{
		ID:         "S-RACE",
		Slot:       slot,
		CLI:        "claude",
		CLIVersion: "2.1.140",
		StartedAt:  time.Now().UTC(),
		Status:     rsql.SessionStatusActive,
	}
	if err := st.Sessions().Create(ctx, original); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// A second Create with the same ID but different fields must not
	// return an error AND must not overwrite the existing row.
	racer := *original
	racer.CLI = "would-have-been-clobbered"
	racer.CLIVersion = "9.9.9"
	if err := st.Sessions().Create(ctx, &racer); err != nil {
		t.Fatalf("second create (idempotent): %v", err)
	}
	got, err := st.Sessions().Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CLI != "claude" || got.CLIVersion != "2.1.140" {
		t.Errorf("idempotent create clobbered existing fields: cli=%q cli_version=%q",
			got.CLI, got.CLIVersion)
	}
}

// TestSessions_EventCountSynced is the regression test for the F7 bug
// where session.event_count remained 0 because nothing updated it
// after Events.Append. SyncEventCount must reflect the actual COUNT(*)
// and must be safe against re-Append (Append is INSERT OR IGNORE, so
// idempotent re-Append must not double-count).
func TestSessions_EventCountSynced(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	slot := event.Slot{Scope: "scope", AgentName: "default"}
	now := time.Now().UTC().Truncate(time.Microsecond)
	ses := &rsql.Session{
		ID:         "S-COUNT",
		Slot:       slot,
		CLI:        "claude_code",
		CLIVersion: "2.1.140",
		StartedAt:  now,
		Status:     rsql.SessionStatusActive,
	}
	if err := st.Sessions().Create(ctx, ses); err != nil {
		t.Fatalf("create session: %v", err)
	}

	events := []event.Event{
		{
			EventUUID:     "E-COUNT-1",
			SessionID:     ses.ID,
			Slot:          slot,
			Seq:           1,
			Timestamp:     now,
			Kind:          event.KindUserMessage,
			SchemaVersion: 1,
			Content:       json.RawMessage(`{"text":"one"}`),
			Vendor:        event.Vendor{Name: "claude_code", Version: "2.1.140"},
		},
		{
			EventUUID:     "E-COUNT-2",
			SessionID:     ses.ID,
			Slot:          slot,
			Seq:           2,
			Timestamp:     now.Add(time.Second),
			Kind:          event.KindAssistantMessage,
			SchemaVersion: 1,
			Content:       json.RawMessage(`{"text":"two"}`),
			Vendor:        event.Vendor{Name: "claude_code", Version: "2.1.140"},
		},
		{
			EventUUID:     "E-COUNT-3",
			SessionID:     ses.ID,
			Slot:          slot,
			Seq:           3,
			Timestamp:     now.Add(2 * time.Second),
			Kind:          event.KindUserMessage,
			SchemaVersion: 1,
			Content:       json.RawMessage(`{"text":"three"}`),
			Vendor:        event.Vendor{Name: "claude_code", Version: "2.1.140"},
		},
	}
	if err := st.Events().Append(ctx, events); err != nil {
		t.Fatalf("append events: %v", err)
	}
	if err := st.Sessions().SyncEventCount(ctx, ses.ID); err != nil {
		t.Fatalf("sync event_count: %v", err)
	}
	got, err := st.Sessions().Get(ctx, ses.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.EventCount != 3 {
		t.Errorf("after first sync: event_count = %d, want 3", got.EventCount)
	}

	// Re-append the same batch. INSERT OR IGNORE means rows are not
	// duplicated; SyncEventCount must therefore still see 3.
	if err := st.Events().Append(ctx, events); err != nil {
		t.Fatalf("idempotent re-append: %v", err)
	}
	if err := st.Sessions().SyncEventCount(ctx, ses.ID); err != nil {
		t.Fatalf("sync event_count (re-append): %v", err)
	}
	got, err = st.Sessions().Get(ctx, ses.ID)
	if err != nil {
		t.Fatalf("get session (re-append): %v", err)
	}
	if got.EventCount != 3 {
		t.Errorf("after re-append: event_count = %d, want 3 (no double-count)", got.EventCount)
	}
}

func TestStore_NotFound(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if _, err := st.Sessions().Get(ctx, "nope"); !errors.Is(err, rsql.ErrNotFound) {
		t.Errorf("missing session: expected ErrNotFound, got %v", err)
	}
	if _, err := st.Slots().Get(ctx, event.Slot{Scope: "x", AgentName: "y"}); !errors.Is(err, rsql.ErrNotFound) {
		t.Errorf("missing slot: expected ErrNotFound, got %v", err)
	}
}

// TestMigrations_DroppedUsersTable is the Q1 regression: the
// pre-pivot client-side `users` table (created by migration 0002) is
// dropped by migration 0006 because the round-4/round-5 identity
// pivot moved persistence to `server_users` + `devices` + `pairings`.
// A fresh install must end with NO `users` table — only the
// server-side identity tables — even though 0002 still runs.
func TestMigrations_DroppedUsersTable(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	row := st.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='users'`)
	var name string
	if err := row.Scan(&name); err == nil {
		t.Errorf("`users` table still present after migrations: %q (should be dropped by 0006)", name)
	}

	// Sanity-check the server-side identity tables are still there.
	for _, want := range []string{"server_users", "devices", "pairing_codes"} {
		row := st.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, want)
		var got string
		if err := row.Scan(&got); err != nil {
			t.Errorf("expected table %q after migrations, got err: %v", want, err)
		}
	}

	// And that migration 0006 was actually recorded.
	var v int
	if err := st.db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations WHERE version = 6`).Scan(&v); err != nil {
		t.Errorf("migration 0006 not recorded in schema_migrations: %v", err)
	}
}
