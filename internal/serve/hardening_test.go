package serve

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

// round-15 multi-tenant DoS-hardening tests: push body cap (M-B), explicit
// key validation (L-A), per-user SSE cap (M-C), per-user brain cap (M-C).

// newHardenedServer builds a full multi-tenant identity server with the
// round-15 DoS caps overridden to tight values so the caps fire without
// huge fixtures. A zero cap falls back to the package default.
func newHardenedServer(t *testing.T, caps Config) (base string, store *sqlite.Store) {
	t.Helper()
	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	dsn := "file:" + t.TempDir() + "/id.db?_pragma=journal_mode=WAL&_pragma=foreign_keys=on"
	store, err = sqlite.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	cfg := Config{
		Backend:          backend,
		AuthToken:        testToken,
		ServerUsers:      store.ServerUsers(),
		Devices:          store.Devices(),
		Pairings:         store.Pairings(),
		ServeMeta:        store.ServeMeta(),
		Brains:           store.Brains(),
		BrainKeys:        store.BrainKeys(),
		Memberships:      store.Memberships(),
		MasterKey:        mustMaster(t),
		MaxPushBytes:     caps.MaxPushBytes,
		MaxSSEPerUser:    caps.MaxSSEPerUser,
		MaxBrainsPerUser: caps.MaxBrainsPerUser,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	s.sseHeartbeat = 50 * time.Millisecond
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts.URL, store
}

// TestPush_BodyCap (M-B): a push body over MaxPushBytes is rejected 413,
// while a small push still succeeds.
func TestPush_BodyCap(t *testing.T) {
	base, _ := newHardenedServer(t, Config{MaxPushBytes: 1 << 12}) // 4 KiB cap
	_, tok, personal := regUser(t, base, "cap@example.com")

	// Small push: under the cap → 200.
	small := PushReq{Batch: []PushRow{{Key: "memory/ok", Blob: []byte("tiny")}}}
	postStatus(t, base+"/v2/sync/push?brain="+personal, tok, small, 200)

	// Large push: a blob well over 4 KiB → body exceeds the cap → 413.
	big := PushReq{Batch: []PushRow{{Key: "memory/big", Blob: make([]byte, 64<<10)}}}
	postStatus(t, base+"/v2/sync/push?brain="+personal, tok, big, http.StatusRequestEntityTooLarge)
}

// TestPush_KeyValidationAtHandler (L-A): a key whose stored form would
// escape the brain prefix via a ".." segment is rejected with 400 at
// handlePush — not silently passed to the backend.
func TestPush_KeyValidationAtHandler(t *testing.T) {
	base, _ := newHardenedServer(t, Config{})
	_, tok, personal := regUser(t, base, "key@example.com")

	// "memory/../escape" passes the prefix allow-list (starts with memory/)
	// but contains a ".." segment; ValidateKey at the handler must 400.
	bad := PushReq{Batch: []PushRow{{Key: "memory/../escape", Blob: []byte("x")}}}
	postStatus(t, base+"/v2/sync/push?brain="+personal, tok, bad, http.StatusBadRequest)
}

// TestSSE_PerUserCap (M-C): one user may hold at most MaxSSEPerUser
// concurrent SSE streams; the next is rejected 429. Closing a stream frees
// a slot so a subsequent connect succeeds.
func TestSSE_PerUserCap(t *testing.T) {
	base, _ := newHardenedServer(t, Config{MaxSSEPerUser: 2})
	_, tok, personal := regUser(t, base, "sse@example.com")

	// Open up to the cap.
	cancel1 := mustOpenSSE(t, base, tok, personal)
	defer cancel1()
	cancel2 := mustOpenSSE(t, base, tok, personal)
	// Give the server a moment to register both increments.
	waitForSubscriber(t)

	// Third connection over the cap → 429.
	if code := openSSEStatus(t, base, tok, personal); code != http.StatusTooManyRequests {
		t.Fatalf("3rd SSE: got %d, want 429", code)
	}

	// Free one slot; a new connection should now succeed.
	cancel2()
	// Wait for the release (deferred on the handler goroutine after the
	// request context is cancelled) to land.
	deadline := time.Now().Add(2 * time.Second)
	for {
		code := openSSEStatus(t, base, tok, personal)
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after freeing a slot, reconnect still got %d, want 200", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSSE_PerUserCapIsolation (M-C): the SSE cap is PER user — user A
// saturating their cap does not block user B.
func TestSSE_PerUserCapIsolation(t *testing.T) {
	base, _ := newHardenedServer(t, Config{MaxSSEPerUser: 1})
	_, tokA, persA := regUser(t, base, "a@example.com")
	_, tokB, persB := regUser(t, base, "b@example.com")

	cancelA := mustOpenSSE(t, base, tokA, persA)
	defer cancelA()
	waitForSubscriber(t)

	// A is at cap → A's 2nd is 429.
	if code := openSSEStatus(t, base, tokA, persA); code != http.StatusTooManyRequests {
		t.Fatalf("A 2nd SSE: got %d, want 429", code)
	}
	// B is unaffected → 200.
	cancelB := mustOpenSSE(t, base, tokB, persB)
	defer cancelB()
}

// TestSSE_PerUserCapConcurrent (M-C, -race): hammer the cap from many
// goroutines and assert the counter never admits more than the cap. Runs
// under `go test -race` to catch counter data races.
func TestSSE_PerUserCapConcurrent(t *testing.T) {
	const limit = 4
	base, _ := newHardenedServer(t, Config{MaxSSEPerUser: limit})
	_, tok, personal := regUser(t, base, "race@example.com")

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ok429    int
		ok200    int
		cancels  []func()
		cancelMu sync.Mutex
	)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, code := openSSEResult(t, base, tok, personal)
			mu.Lock()
			switch code {
			case http.StatusOK:
				ok200++
			case http.StatusTooManyRequests:
				ok429++
			}
			mu.Unlock()
			if c != nil {
				cancelMu.Lock()
				cancels = append(cancels, c)
				cancelMu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, c := range cancels {
		c()
	}
	if ok200 > limit {
		t.Fatalf("admitted %d concurrent SSE, want <= cap %d", ok200, limit)
	}
	if ok200+ok429 != 20 {
		t.Fatalf("accounted %d responses, want 20 (200=%d 429=%d)", ok200+ok429, ok200, ok429)
	}
}

// TestCreateBrain_PerUserCap (M-C): a user may own at most MaxBrainsPerUser
// brains. The personal brain provisioned at registration counts, so with a
// cap of 2 the user can create exactly one shared brain; the next is 429.
func TestCreateBrain_PerUserCap(t *testing.T) {
	base, _ := newHardenedServer(t, Config{MaxBrainsPerUser: 2})
	_, tok, _ := regUser(t, base, "brains@example.com")

	// Personal brain already counts (1/2). First shared brain → 200 (2/2).
	var created CreateBrainResp
	post(t, base+"/v1/brains", tok, CreateBrainReq{Name: "one"}, &created, 201)

	// Second shared brain would be 3/2 → 429.
	postStatus(t, base+"/v1/brains", tok, CreateBrainReq{Name: "two"}, http.StatusTooManyRequests)
}

// --- SSE test helpers ---

// openSSEResult opens an SSE stream and returns a cancel func (nil unless
// the response was 200) plus the HTTP status. Unlike openSSE it does not
// fatal on a non-200 status, so cap-rejection (429) can be asserted.
func openSSEResult(t *testing.T, base, bearer, brain string) (func(), int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	u := base + "/v2/sync/sse?kind=memory"
	if brain != "" {
		u += "&brain=" + brain
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("sse GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		resp.Body.Close()
		return nil, resp.StatusCode
	}
	// Drain the body in the background so the connection stays open until
	// cancel(); closing happens when the request context is cancelled.
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			_ = sc.Text()
		}
	}()
	return cancel, http.StatusOK
}

// mustOpenSSE opens an SSE stream and fatals unless it gets 200.
func mustOpenSSE(t *testing.T, base, bearer, brain string) func() {
	t.Helper()
	cancel, code := openSSEResult(t, base, bearer, brain)
	if code != http.StatusOK {
		t.Fatalf("open SSE: got %d, want 200", code)
	}
	return cancel
}

// openSSEStatus opens an SSE stream just to read its status, immediately
// tearing any successful connection down.
func openSSEStatus(t *testing.T, base, bearer, brain string) int {
	t.Helper()
	cancel, code := openSSEResult(t, base, bearer, brain)
	if cancel != nil {
		cancel()
	}
	return code
}
