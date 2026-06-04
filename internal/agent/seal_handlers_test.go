package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
)

// newSealTestDaemon builds a Daemon with a SyncClient pointed at a
// throwaway upstream (we never hit it; we only exercise the in-process
// sealer wiring). The mux is served via httptest so we can pretend to
// be a loopback client.
func newSealTestDaemon(t *testing.T) (*Daemon, *httptest.Server) {
	t.Helper()
	dsn := "file:" + t.TempDir() + "/seal.db?_pragma=journal_mode=WAL&_pragma=foreign_keys=on"
	store, err := sqlite.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d := &Daemon{
		store: store,
		// upstream URL is dummy — drain/pull are parked anyway until
		// SetSealer fires, and even after that we don't tick within
		// the test window.
		sync:   NewSyncClient(store, "http://localhost:1", "tok"),
		secret: "test-secret",
	}
	mux := http.NewServeMux()
	d.registerRoutes(mux)
	d.registerMemoryRoutes(mux)
	d.registerSealRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return d, ts
}

func TestSealUnlock_InstallsSealer(t *testing.T) {
	d, ts := newSealTestDaemon(t)

	// Status before: no sealer.
	if got := d.sync.getSealer(); got != nil {
		t.Errorf("initial sealer should be nil, got %v", got)
	}

	// Build a 32-byte KEK and POST.
	kek := bytes.Repeat([]byte{0xAB}, 32)
	body, _ := json.Marshal(SealUnlockReq{KEK: kek})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/seal/unlock", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+d.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("unlock status: got %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Sealer should now be installed; and it should be functional.
	sl := d.sync.getSealer()
	if sl == nil {
		t.Fatal("sealer not installed after unlock")
	}
	ct, err := sl.Seal([]byte("hi"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	pt, err := sl.Open(ct)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(pt) != "hi" {
		t.Errorf("round-trip: got %q, want %q", pt, "hi")
	}

	// Lock clears it.
	req2, _ := http.NewRequest("POST", ts.URL+"/v1/seal/lock", nil)
	req2.Header.Set("Authorization", "Bearer "+d.secret)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("lock Do: %v", err)
	}
	resp2.Body.Close()
	if got := d.sync.getSealer(); got != nil {
		t.Errorf("sealer should be nil after lock, got %v", got)
	}
}

func TestSealUnlock_RejectsBadKEKLength(t *testing.T) {
	d, ts := newSealTestDaemon(t)
	body, _ := json.Marshal(SealUnlockReq{KEK: []byte("short")})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/seal/unlock", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+d.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
	if d.sync.getSealer() != nil {
		t.Error("sealer must not be installed on bad-length KEK")
	}
}

func TestSealUnlock_RequiresBearer(t *testing.T) {
	_, ts := newSealTestDaemon(t)
	body, _ := json.Marshal(SealUnlockReq{KEK: bytes.Repeat([]byte{1}, 32)})
	resp, err := http.Post(ts.URL+"/v1/seal/unlock", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

func TestSealStatus(t *testing.T) {
	d, ts := newSealTestDaemon(t)

	get := func() SealStatusResp {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/seal/status", nil)
		req.Header.Set("Authorization", "Bearer "+d.secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		defer resp.Body.Close()
		var s SealStatusResp
		_ = json.NewDecoder(resp.Body).Decode(&s)
		return s
	}

	s := get()
	if s.Sealed || s.SyncReady {
		t.Errorf("pre-unlock status: %+v, want both false", s)
	}

	d.sync.SetSealer(mustSealer(t))
	s = get()
	if !s.Sealed || !s.SyncReady {
		t.Errorf("post-unlock status: %+v, want both true", s)
	}
}

func TestSealUnlock_NonLoopbackRejected(t *testing.T) {
	d, _ := newSealTestDaemon(t)
	// Drive the handler directly with a faked RemoteAddr that isn't loopback.
	body, _ := json.Marshal(SealUnlockReq{KEK: bytes.Repeat([]byte{1}, 32)})
	req := httptest.NewRequest("POST", "/v1/seal/unlock", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.5:4242"
	req.Header.Set("Authorization", "Bearer "+d.secret)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	d.registerSealRoutes(mux)
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-loopback unlock: got %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "loopback") {
		t.Errorf("expected loopback error, got %q", w.Body.String())
	}
}

func mustSealer(t *testing.T) auth.Sealer {
	t.Helper()
	sl, err := auth.NewAESGCMSealer(bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return sl
}
