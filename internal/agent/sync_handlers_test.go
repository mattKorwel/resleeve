package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
)

// newTestDaemonWithSync builds a Daemon wired to the given upstream
// (the local httptest serve.Server). secret is a fixed bearer used by
// the helpers below.
func newTestDaemonWithSync(t *testing.T, upstream string) *Daemon {
	t.Helper()
	store := newSyncTestStore(t)
	d := &Daemon{
		store:  store,
		secret: "test-bearer-fixed",
		sync:   NewSyncClient(store, upstream, testSyncToken),
	}
	return d
}

// newTestDaemonNoSync builds a Daemon with no upstream — used to test
// the 409 path on push-now / pull-now.
func newTestDaemonNoSync(t *testing.T) *Daemon {
	t.Helper()
	return &Daemon{
		store:  newSyncTestStore(t),
		secret: "test-bearer-fixed",
		sync:   nil,
	}
}

// muxForDaemon wires JUST the sync escape-hatch routes — the rest of
// the daemon's surface is irrelevant for these tests.
func muxForDaemon(d *Daemon) *http.ServeMux {
	mux := http.NewServeMux()
	d.registerSyncRoutes(mux)
	return mux
}

func TestSyncHandler_PushNowDrainsAndReturnsCount(t *testing.T) {
	ctx := context.Background()
	ts, _ := newSyncTestServer(t)
	d := newTestDaemonWithSync(t, ts.URL)
	srv := httptest.NewServer(muxForDaemon(d))
	t.Cleanup(srv.Close)

	// Seed two events into the daemon's outbox.
	ev1 := event.Event{EventUUID: "PN-1", SessionID: "S-PN", Seq: 1000, Kind: event.KindUserMessage, Timestamp: time.Now().UTC()}
	ev2 := event.Event{EventUUID: "PN-2", SessionID: "S-PN", Seq: 2000, Kind: event.KindAssistantMessage, Timestamp: time.Now().UTC()}
	if err := d.sync.EnqueueEvents(ctx, []event.Event{ev1, ev2}); err != nil {
		t.Fatalf("EnqueueEvents: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sync/push-now", nil)
	req.Header.Set("Authorization", "Bearer "+d.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST push-now: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("push-now status: got %d, want 200; body=%s", resp.StatusCode, string(body))
	}
	var out struct {
		Pushed int    `json:"pushed"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode push-now resp: %v", err)
	}
	if out.Pushed != 2 {
		t.Errorf("pushed count: got %d, want 2", out.Pushed)
	}
	if out.Error != "" {
		t.Errorf("unexpected error in resp: %q", out.Error)
	}
}

func TestSyncHandler_PushNowEmptyOutboxReturnsZero(t *testing.T) {
	ts, _ := newSyncTestServer(t)
	d := newTestDaemonWithSync(t, ts.URL)
	srv := httptest.NewServer(muxForDaemon(d))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sync/push-now", nil)
	req.Header.Set("Authorization", "Bearer "+d.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Pushed int }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Pushed != 0 {
		t.Errorf("empty outbox push: got %d, want 0", out.Pushed)
	}
}

func TestSyncHandler_PullNowReturnsPerKindCounts(t *testing.T) {
	ctx := context.Background()
	ts, backend := newSyncTestServer(t)
	d := newTestDaemonWithSync(t, ts.URL)
	srv := httptest.NewServer(muxForDaemon(d))
	t.Cleanup(srv.Close)

	// Seed upstream with a session row so the pull has something to ingest.
	sesBlob, _ := json.Marshal(map[string]any{
		"id": "S-PULL", "cli": "claude", "status": "active",
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err := backend.Put(ctx, "sessions/S-PULL", sesBlob); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sync/pull-now", nil)
	req.Header.Set("Authorization", "Bearer "+d.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("pull-now status: got %d, want 200; body=%s", resp.StatusCode, string(body))
	}
	var out struct {
		Pulled map[string]int `json:"pulled"`
		Error  string         `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode pull-now resp: %v", err)
	}
	if out.Pulled == nil {
		t.Fatalf("pulled map missing — wire shape regression")
	}
	if out.Pulled["sessions"] != 1 {
		t.Errorf("pulled sessions: got %d, want 1", out.Pulled["sessions"])
	}
	// Memory and events keys must be present (even if 0) for stable CLI output.
	if _, ok := out.Pulled["memory"]; !ok {
		t.Errorf("pulled map missing 'memory' key")
	}
	if _, ok := out.Pulled["events"]; !ok {
		t.Errorf("pulled map missing 'events' key")
	}
}

func TestSyncHandler_PushNowNoUpstreamReturns409(t *testing.T) {
	d := newTestDaemonNoSync(t)
	srv := httptest.NewServer(muxForDaemon(d))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sync/push-now", nil)
	req.Header.Set("Authorization", "Bearer "+d.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("no-upstream status: got %d, want 409", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "upstream") {
		t.Errorf("409 body should mention 'upstream': %q", string(body))
	}
}

func TestSyncHandler_PullNowNoUpstreamReturns409(t *testing.T) {
	d := newTestDaemonNoSync(t)
	srv := httptest.NewServer(muxForDaemon(d))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sync/pull-now", nil)
	req.Header.Set("Authorization", "Bearer "+d.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("no-upstream status: got %d, want 409", resp.StatusCode)
	}
}

func TestSyncHandler_MethodNotAllowed(t *testing.T) {
	ts, _ := newSyncTestServer(t)
	d := newTestDaemonWithSync(t, ts.URL)
	srv := httptest.NewServer(muxForDaemon(d))
	t.Cleanup(srv.Close)

	for _, path := range []string{"/v1/sync/push-now", "/v1/sync/pull-now"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+d.secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s GET status: got %d, want 405", path, resp.StatusCode)
		}
	}
}

func TestSyncHandler_RequiresBearer(t *testing.T) {
	ts, _ := newSyncTestServer(t)
	d := newTestDaemonWithSync(t, ts.URL)
	srv := httptest.NewServer(muxForDaemon(d))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sync/push-now", nil)
	// No Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-bearer status: got %d, want 401", resp.StatusCode)
	}
}
