package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mattkorwel/resleeve/internal/event"
)

// TestActiveBrain_PersistAndClear round-trips the active-brain client
// config: write → load → clear → load.
func TestActiveBrain_PersistAndClear(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Some platforms resolve UserHomeDir via other vars; assert the path
	// lands under our temp HOME before relying on it.
	path, err := ActiveBrainPath()
	if err != nil {
		t.Fatalf("ActiveBrainPath: %v", err)
	}
	if filepath.Dir(filepath.Dir(path)) != dir {
		t.Skipf("HOME override not honored on this platform (path=%s)", path)
	}

	if got, _ := LoadActiveBrain(); got != "" {
		t.Fatalf("fresh LoadActiveBrain = %q, want empty", got)
	}
	if err := WriteActiveBrain("brain-123"); err != nil {
		t.Fatalf("WriteActiveBrain: %v", err)
	}
	if got, _ := LoadActiveBrain(); got != "brain-123" {
		t.Errorf("LoadActiveBrain = %q, want brain-123", got)
	}
	// File perms are 0600.
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("active-brain file perm = %v, want 0600", fi.Mode().Perm())
		}
	}
	// Clear removes the selection.
	if err := WriteActiveBrain(""); err != nil {
		t.Fatalf("WriteActiveBrain clear: %v", err)
	}
	if got, _ := LoadActiveBrain(); got != "" {
		t.Errorf("after clear, LoadActiveBrain = %q, want empty", got)
	}
}

// TestSyncClient_AppendsBrainSelector asserts the daemon's upstream
// push/pull/SSE requests carry ?brain=<id> exactly when an active brain
// is set, and omit it otherwise.
func TestSyncClient_AppendsBrainSelector(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	seen := map[string]string{} // path -> brain query value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.URL.Query().Get("brain")
		mu.Unlock()
		switch r.URL.Path {
		case "/v2/sync/push":
			// Echo a committed ack for whatever was pushed so drain acks.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"committed":["sessions/S1"]}`))
		case "/v2/sync/pull":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rows":[]}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	store := newSyncTestStore(t)
	sc := NewSyncClient(store, srv.URL, "tok")
	sc.SetActiveBrain("brain-xyz")

	// Push: enqueue one event then drain.
	if err := sc.EnqueueEvents(ctx, []event.Event{{EventUUID: "E1", SessionID: "S1", Seq: 1, Kind: event.KindUserMessage}}); err != nil {
		t.Fatalf("EnqueueEvents: %v", err)
	}
	if err := sc.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}
	// Pull.
	if _, err := sc.pullKind(ctx, "sessions"); err != nil {
		t.Fatalf("pullKind: %v", err)
	}

	mu.Lock()
	pushBrain := seen["/v2/sync/push"]
	pullBrain := seen["/v2/sync/pull"]
	mu.Unlock()
	if pushBrain != "brain-xyz" {
		t.Errorf("push ?brain = %q, want brain-xyz", pushBrain)
	}
	if pullBrain != "brain-xyz" {
		t.Errorf("pull ?brain = %q, want brain-xyz", pullBrain)
	}

	// Clearing the active brain drops the selector.
	sc.SetActiveBrain("")
	mu.Lock()
	for k := range seen {
		delete(seen, k)
	}
	mu.Unlock()
	if _, err := sc.pullKind(ctx, "sessions"); err != nil {
		t.Fatalf("pullKind (cleared): %v", err)
	}
	mu.Lock()
	pullBrainCleared, ok := seen["/v2/sync/pull"]
	mu.Unlock()
	if !ok {
		t.Fatal("pull request not observed after clear")
	}
	if pullBrainCleared != "" {
		t.Errorf("after clear, pull ?brain = %q, want empty", pullBrainCleared)
	}
}
