package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/serve"
)

// fakeServeBackend is an in-memory stub of the v2 sync surface
// migrateKind needs: GET /v2/sync/pull?kind=&since=&limit= and
// POST /v2/sync/push. Keys within a kind are returned in insertion
// (== sorted) order; since= is a strict-greater-than cursor.
//
// Mirrors the local-disk backend's overwrite-by-key semantics, which is
// what makes the migration idempotent + safe to rerun.
type fakeServeBackend struct {
	mu   sync.Mutex
	rows map[string][]serve.PushRow // kind -> rows in lexicographic key order
}

func newFakeServeBackend() *fakeServeBackend {
	return &fakeServeBackend{rows: map[string][]serve.PushRow{}}
}

func (f *fakeServeBackend) seed(kind string, rows ...serve.PushRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[kind] = append(f.rows[kind], rows...)
}

func (f *fakeServeBackend) get(kind string) []serve.PushRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]serve.PushRow, len(f.rows[kind]))
	copy(out, f.rows[kind])
	return out
}

func (f *fakeServeBackend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/sync/pull", func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")
		since := r.URL.Query().Get("since")
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []serve.PushRow
		for _, row := range f.rows[kind] {
			if row.Key > since {
				out = append(out, row)
			}
		}
		testWriteJSON(w, http.StatusOK, serve.PullResp{Rows: out})
	})
	mux.HandleFunc("/v2/sync/push", func(w http.ResponseWriter, r *http.Request) {
		var req serve.PushReq
		buf, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(buf, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		committed := make([]string, 0, len(req.Batch))
		for _, row := range req.Batch {
			// kind is implied by the key's first path segment, mirroring
			// the local-disk backend's prefix scheme.
			kind := strings.SplitN(row.Key, "/", 2)[0]
			replaced := false
			for i, existing := range f.rows[kind] {
				if existing.Key == row.Key {
					f.rows[kind][i] = row
					replaced = true
					break
				}
			}
			if !replaced {
				f.rows[kind] = append(f.rows[kind], row)
			}
			committed = append(committed, row.Key)
		}
		testWriteJSON(w, http.StatusOK, serve.PushResp{Committed: committed})
	})
	return mux
}

func testWriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func TestMigrateKind_RewrapsOldKeyToNew(t *testing.T) {
	oldS := mustSealer(t, 0x11)
	newS := mustSealer(t, 0x22)

	wrap := func(s auth.Sealer, b []byte) []byte {
		out, err := s.Seal(b)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		return out
	}
	be := newFakeServeBackend()
	be.seed("sessions",
		serve.PushRow{Key: "sessions/s1", Blob: wrap(oldS, []byte(`{"id":"s1"}`))},
		serve.PushRow{Key: "sessions/s2", Blob: wrap(oldS, []byte(`{"id":"s2"}`))},
		serve.PushRow{Key: "sessions/s3", Blob: wrap(oldS, []byte(`{"id":"s3"}`))},
	)
	srv := httptest.NewServer(be.handler())
	defer srv.Close()

	migrated, skipped, err := migrateKind(context.Background(), srv.URL, "tok", "sessions", oldS, newS, false)
	if err != nil {
		t.Fatalf("migrateKind: %v", err)
	}
	if migrated != 3 || skipped != 0 {
		t.Fatalf("migrated=%d skipped=%d, want 3,0", migrated, skipped)
	}
	rows := be.get("sessions")
	if len(rows) != 3 {
		t.Fatalf("row count after migrate: got %d want 3", len(rows))
	}
	for _, row := range rows {
		if _, err := newS.Open(row.Blob); err != nil {
			t.Errorf("row %s: new-key Open failed: %v", row.Key, err)
		}
		if _, err := oldS.Open(row.Blob); err == nil {
			t.Errorf("row %s: old-key still Opens, expected re-wrap", row.Key)
		}
	}
}

func TestMigrateKind_SkipsAlreadyMigratedRows(t *testing.T) {
	oldS := mustSealer(t, 0x11)
	newS := mustSealer(t, 0x22)

	wrap := func(s auth.Sealer, b []byte) []byte {
		out, _ := s.Seal(b)
		return out
	}
	be := newFakeServeBackend()
	be.seed("sessions",
		serve.PushRow{Key: "sessions/a", Blob: wrap(oldS, []byte(`{"id":"a"}`))},
		serve.PushRow{Key: "sessions/b", Blob: wrap(newS, []byte(`{"id":"b"}`))},
	)
	srv := httptest.NewServer(be.handler())
	defer srv.Close()

	migrated, skipped, err := migrateKind(context.Background(), srv.URL, "tok", "sessions", oldS, newS, false)
	if err != nil {
		t.Fatalf("migrateKind: %v", err)
	}
	if migrated != 1 || skipped != 1 {
		t.Fatalf("migrated=%d skipped=%d, want 1,1", migrated, skipped)
	}
}

func TestMigrateKind_DryRunDoesNotPush(t *testing.T) {
	oldS := mustSealer(t, 0x11)
	newS := mustSealer(t, 0x22)
	wrap := func(s auth.Sealer, b []byte) []byte {
		out, _ := s.Seal(b)
		return out
	}
	be := newFakeServeBackend()
	be.seed("sessions",
		serve.PushRow{Key: "sessions/x", Blob: wrap(oldS, []byte(`{"id":"x"}`))},
	)
	var pushes int
	mux := be.handler()
	wrappedMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/sync/push" {
			pushes++
		}
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(wrappedMux)
	defer srv.Close()

	migrated, _, err := migrateKind(context.Background(), srv.URL, "tok", "sessions", oldS, newS, true)
	if err != nil {
		t.Fatalf("migrateKind: %v", err)
	}
	if migrated != 1 {
		t.Fatalf("dry-run migrated count: got %d want 1", migrated)
	}
	if pushes != 0 {
		t.Errorf("dry-run pushed %d batches, want 0", pushes)
	}
	rows := be.get("sessions")
	if _, err := oldS.Open(rows[0].Blob); err != nil {
		t.Errorf("dry-run mutated upstream blob: old key no longer Opens: %v", err)
	}
}

func mustSealer(t *testing.T, fill byte) auth.Sealer {
	t.Helper()
	k := bytes.Repeat([]byte{fill}, 32)
	s, err := auth.NewAESGCMSealer(k)
	if err != nil {
		t.Fatalf("sealer fill=0x%02x: %v", fill, err)
	}
	return s
}
