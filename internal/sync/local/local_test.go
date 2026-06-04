package local

import (
	"context"
	"errors"
	"reflect"
	"testing"

	rsync "github.com/mattkorwel/resleeve/internal/sync"
)

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestBackend_PutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	key := "sessions/S1/events/000001"
	blob := []byte("opaque ciphertext bytes here")

	if err := b.Put(ctx, key, blob); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := b.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, blob) {
		t.Errorf("blob mismatch: got %q, want %q", got, blob)
	}
}

func TestBackend_GetMissingReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	_, err := b.Get(ctx, "sessions/never/written")
	if !errors.Is(err, rsync.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestBackend_PutIsIdempotentOnIdenticalBlob(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	key := "sessions/S1/events/000001"
	blob := []byte("same content")
	if err := b.Put(ctx, key, blob); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := b.Put(ctx, key, blob); err != nil {
		t.Fatalf("second Put: %v", err)
	}
}

func TestBackend_ListAscendingOrder(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	keys := []string{
		"sessions/S1/events/000003",
		"sessions/S1/events/000001",
		"sessions/S1/events/000002",
	}
	for _, k := range keys {
		if err := b.Put(ctx, k, []byte("x")); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	got, _, err := b.List(ctx, "sessions/S1/events", "", 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{
		"sessions/S1/events/000001",
		"sessions/S1/events/000002",
		"sessions/S1/events/000003",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List order:\n got: %v\nwant: %v", got, want)
	}
}

func TestBackend_ListPagination(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	for _, k := range []string{
		"sessions/S1/events/000001",
		"sessions/S1/events/000002",
		"sessions/S1/events/000003",
		"sessions/S1/events/000004",
	} {
		if err := b.Put(ctx, k, []byte("x")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Page 1: limit=2, no cursor.
	page1, cur1, err := b.List(ctx, "sessions/S1/events", "", 2)
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(page1) != 2 || cur1 == "" {
		t.Fatalf("page1: got %v cur %q; expected 2 items + cursor", page1, cur1)
	}
	if page1[0] != "sessions/S1/events/000001" || page1[1] != "sessions/S1/events/000002" {
		t.Errorf("page1 contents: %v", page1)
	}
	// Page 2: continuing from cur1.
	page2, cur2, err := b.List(ctx, "sessions/S1/events", cur1, 2)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2: got %d items, want 2", len(page2))
	}
	if page2[0] != "sessions/S1/events/000003" || page2[1] != "sessions/S1/events/000004" {
		t.Errorf("page2 contents: %v", page2)
	}
	if cur2 != "" {
		// Returned exactly the last 2 items — cursor may or may not be empty
		// depending on whether we filled the page; current impl emits the
		// last key as cursor when len(page)==limit. Acceptable; next call
		// will return empty.
	}
	// Page 3 from cur2: should be empty.
	page3, _, err := b.List(ctx, "sessions/S1/events", cur2, 2)
	if err != nil {
		t.Fatalf("List page3: %v", err)
	}
	if len(page3) != 0 {
		t.Errorf("page3 should be empty, got %d items", len(page3))
	}
}

func TestBackend_ListMissingPrefixEmpty(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	got, _, err := b.List(ctx, "memory/nonexistent", "", 10)
	if err != nil {
		t.Fatalf("List missing prefix: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list, got %d entries", len(got))
	}
}

func TestBackend_DeleteRemovesKey(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	key := "memory/scope-x/plans/_default/000001"
	if err := b.Put(ctx, key, []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Get(ctx, key); !errors.Is(err, rsync.ErrNotFound) {
		t.Errorf("after Delete, expected ErrNotFound, got %v", err)
	}
	// Delete on missing key is idempotent (no error).
	if err := b.Delete(ctx, key); err != nil {
		t.Errorf("Delete on missing key should be no-op, got: %v", err)
	}
}

func TestBackend_RejectsInvalidKeys(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	bad := []string{"", "/leading", "trailing/", "has//empty", "has/../traversal", "has/./dot"}
	for _, k := range bad {
		if err := b.Put(ctx, k, []byte("x")); err == nil {
			t.Errorf("Put with bad key %q should have errored", k)
		}
	}
}
