package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
)

// TestDrainLoop_ParksWithoutSealer verifies the drain goroutine does
// NOT hit upstream while the SyncClient has no sealer installed. The
// "park" guarantee is the runtime contract /v1/seal/unlock relies on:
// pre-login, sync is fully quiescent on the wire even though the
// daemon is running.
func TestDrainLoop_ParksWithoutSealer(t *testing.T) {
	var pushCalls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// round-12: whoami (GET /v2/sync/whoami) is auth-only and runs even
		// while parked, so the park assertion must count ONLY the push POST.
		// A catch-all counter would wrongly tick on the whoami handshake.
		if r.Method == http.MethodPost && r.URL.Path == "/v2/sync/push" {
			atomic.AddInt32(&pushCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"committed":[]}`))
	}))
	defer ts.Close()

	dsn := "file:" + t.TempDir() + "/park.db?_pragma=journal_mode=WAL&_pragma=foreign_keys=on"
	store, err := sqlite.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Seed a row into the outbox so drain WOULD have work if not parked.
	err = store.Sync().EnqueueOutbox(context.Background(), "sessions", "sessions/seed", []byte("blob"), time.Now().UTC())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	sc := NewSyncClient(store, ts.URL, "tok")
	sc.drainInterval = 20 * time.Millisecond
	sc.parkInterval = 20 * time.Millisecond
	sc.pullInterval = time.Hour // disable
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc.Start(ctx)

	time.Sleep(150 * time.Millisecond) // several drain ticks
	if got := atomic.LoadInt32(&pushCalls); got != 0 {
		t.Errorf("expected zero push calls while parked, got %d", got)
	}

	// Install a sealer; expect drain to fire within a couple of ticks.
	sl, _ := auth.NewAESGCMSealer(make([]byte, 32))
	// keep auth import live
	_ = auth.Argon2idParams{}
	sc.SetSealer(sl)
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&pushCalls); got == 0 {
		t.Errorf("expected push after SetSealer, got 0")
	}

	sc.Stop()
}
