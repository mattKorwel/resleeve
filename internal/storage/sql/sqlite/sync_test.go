package sqlite

import (
	"context"
	"testing"
	"time"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

func TestOutbox_EnqueueDequeueAckRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Now().UTC()

	if err := st.Sync().EnqueueOutbox(ctx, "events", "events/S1/0001-AAA", []byte("blob-a"), now); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.Sync().EnqueueOutbox(ctx, "events", "events/S1/0002-BBB", []byte("blob-b"), now); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}

	rows, err := st.Sync().DequeueOutbox(ctx, 100)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Key != "events/S1/0001-AAA" || rows[1].Key != "events/S1/0002-BBB" {
		t.Errorf("FIFO ordering wrong: %v %v", rows[0].Key, rows[1].Key)
	}

	if err := st.Sync().AckOutbox(ctx, []int64{rows[0].Seq, rows[1].Seq}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	left, _ := st.Sync().DequeueOutbox(ctx, 100)
	if len(left) != 0 {
		t.Errorf("expected outbox empty after ack, got %d rows", len(left))
	}
}

func TestOutbox_EnqueueIdempotentOnSameKey(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Now().UTC()
	if err := st.Sync().EnqueueOutbox(ctx, "events", "events/S1/0001-A", []byte("v1"), now); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Second enqueue with same (kind, key) must not error and must not
	// duplicate the row.
	if err := st.Sync().EnqueueOutbox(ctx, "events", "events/S1/0001-A", []byte("v2"), now); err != nil {
		t.Fatalf("second: %v", err)
	}
	rows, _ := st.Sync().DequeueOutbox(ctx, 100)
	if len(rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(rows))
	}
	if string(rows[0].Blob) != "v1" {
		t.Errorf("idempotent enqueue should preserve original blob: got %q", string(rows[0].Blob))
	}
}

func TestOutbox_FutureRowsNotDequeued(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	future := time.Now().Add(1 * time.Hour).UTC()
	if err := st.Sync().EnqueueOutbox(ctx, "events", "k1", []byte("x"), future); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rows, _ := st.Sync().DequeueOutbox(ctx, 100)
	if len(rows) != 0 {
		t.Errorf("future row should not dequeue yet, got %d", len(rows))
	}
}

func TestOutbox_BumpAttempt(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Now().UTC()
	if err := st.Sync().EnqueueOutbox(ctx, "events", "k1", []byte("x"), now); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.Sync().DequeueOutbox(ctx, 100)
	if len(rows) != 1 {
		t.Fatal("expected 1 row")
	}
	later := now.Add(5 * time.Minute)
	if err := st.Sync().BumpOutboxAttempt(ctx, rows[0].Seq, later, "network timeout"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	// Row should no longer be eligible until `later`.
	immediate, _ := st.Sync().DequeueOutbox(ctx, 100)
	if len(immediate) != 0 {
		t.Errorf("row should be deferred after bump, got %d rows", len(immediate))
	}
}

func TestOutbox_Depth(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Now().UTC()
	for i, k := range []string{"a", "b", "c"} {
		_ = i
		if err := st.Sync().EnqueueOutbox(ctx, "events", k, []byte("x"), now); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.Sync().OutboxDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("depth: got %d, want 3", n)
	}
}

func TestSyncCursor_GetSetUpsert(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if c, _ := st.Sync().GetCursor(ctx, "events"); c != "" {
		t.Errorf("initial cursor: got %q, want empty", c)
	}
	if err := st.Sync().SetCursor(ctx, "events", "events/S1/0001"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if c, _ := st.Sync().GetCursor(ctx, "events"); c != "events/S1/0001" {
		t.Errorf("after set: got %q", c)
	}
	// Overwrite.
	if err := st.Sync().SetCursor(ctx, "events", "events/S1/0099"); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	if c, _ := st.Sync().GetCursor(ctx, "events"); c != "events/S1/0099" {
		t.Errorf("after re-set: got %q", c)
	}
	// Different kind, isolated cursor.
	if err := st.Sync().SetCursor(ctx, "sessions", "sessions/SX"); err != nil {
		t.Fatalf("set sessions: %v", err)
	}
	if c, _ := st.Sync().GetCursor(ctx, "sessions"); c != "sessions/SX" {
		t.Errorf("sessions cursor: got %q", c)
	}
	if c, _ := st.Sync().GetCursor(ctx, "events"); c != "events/S1/0099" {
		t.Errorf("events cursor leaked: got %q", c)
	}
}

// dummy reference so rsql import isn't dead in tests without other uses.
var _ = rsql.SessionStatusActive
