package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

// newFanoutServer builds a full multi-tenant identity server (like
// newIdentityServer) but with a fast SSE heartbeat so heartbeat assertions
// don't wait 15s. Returns the httptest base URL + the registration store.
func newFanoutServer(t *testing.T, heartbeat time.Duration) (base string, store *sqlite.Store) {
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
	s, err := New(Config{
		Backend:     backend,
		AuthToken:   testToken,
		ServerUsers: store.ServerUsers(),
		Devices:     store.Devices(),
		Pairings:    store.Pairings(),
		ServeMeta:   store.ServeMeta(),
		Brains:      store.Brains(),
		BrainKeys:   store.BrainKeys(),
		Memberships: store.Memberships(),
		MasterKey:   mustMaster(t),
	})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	s.sseHeartbeat = heartbeat
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts.URL, store
}

// openSSE opens an SSE stream against base for the given brain selector
// (empty = personal) authenticated with bearer, and streams parsed
// PushRows into rows and heartbeat pings into pings. It returns a cancel
// func to tear the connection down.
func openSSE(t *testing.T, base, bearer, brain string, rows chan<- PushRow, pings chan<- struct{}) func() {
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
		t.Fatalf("sse status: got %d, want 200", resp.StatusCode)
	}
	go func() {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var data strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if data.Len() == 0 {
					continue
				}
				var row PushRow
				if err := json.Unmarshal([]byte(data.String()), &row); err == nil {
					select {
					case rows <- row:
					case <-ctx.Done():
						return
					}
				}
				data.Reset()
			case strings.HasPrefix(line, ":"):
				if pings != nil {
					select {
					case pings <- struct{}{}:
					default:
					}
				}
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
	}()
	return cancel
}

// TestFanout_CrossMemberLiveDelivery is the core round-13 guarantee:
// member A pushes a memory row to a SHARED brain X; member B, subscribed
// to X over SSE, receives it LIVE — not just A's own other devices.
func TestFanout_CrossMemberLiveDelivery(t *testing.T) {
	base, _ := newFanoutServer(t, 0)
	ownerID, ownerTok, _ := regUser(t, base, "owner@example.com")
	memberID, memberTok, _ := regUser(t, base, "member@example.com")
	_ = ownerID

	var created CreateBrainResp
	post(t, base+"/v1/brains", ownerTok, CreateBrainReq{Name: "team"}, &created, 201)
	brain := created.BrainID
	postStatus(t, base+"/v1/brains/"+brain+"/members", ownerTok, AddMemberReq{UserID: memberID}, 204)

	// Member B subscribes to shared brain X.
	bRows := make(chan PushRow, 8)
	cancelB := openSSE(t, base, memberTok, brain, bRows, nil)
	defer cancelB()
	// Give the subscription a beat to register before A pushes.
	waitForSubscriber(t)

	// Member A (the owner) pushes into the shared brain.
	push := PushReq{Batch: []PushRow{{Key: "memory/team-learning", Blob: []byte("shared-live")}}}
	postStatus(t, base+"/v2/sync/push?brain="+brain, ownerTok, push, 200)

	select {
	case ev := <-bRows:
		if ev.Key != "memory/team-learning" {
			t.Errorf("B live key = %q, want memory/team-learning", ev.Key)
		}
		if string(ev.Blob) != "shared-live" {
			t.Errorf("B live blob = %q, want shared-live", ev.Blob)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("member B did not receive A's shared-brain push live over SSE")
	}
}

// TestFanout_IsolationNonMember asserts a member of a DIFFERENT brain does
// NOT receive brain X's pushes: the hub is keyed on brain_id, so a push to
// X is never delivered to a subscriber acting in Y.
func TestFanout_IsolationNonMember(t *testing.T) {
	base, _ := newFanoutServer(t, 0)
	ownerID, ownerTok, _ := regUser(t, base, "owner@example.com")
	_, strangerTok, strangerPersonal := regUser(t, base, "stranger@example.com")
	_ = ownerID

	var created CreateBrainResp
	post(t, base+"/v1/brains", ownerTok, CreateBrainReq{Name: "team"}, &created, 201)
	brain := created.BrainID

	// Stranger subscribes to their OWN personal brain (a different brain).
	sRows := make(chan PushRow, 8)
	cancelS := openSSE(t, base, strangerTok, strangerPersonal, sRows, nil)
	defer cancelS()
	waitForSubscriber(t)

	// Owner pushes into the shared brain the stranger is NOT a member of.
	push := PushReq{Batch: []PushRow{{Key: "memory/secret", Blob: []byte("not-for-stranger")}}}
	postStatus(t, base+"/v2/sync/push?brain="+brain, ownerTok, push, 200)

	select {
	case ev := <-sRows:
		t.Fatalf("stranger received a row for a brain they don't subscribe to: %q", ev.Key)
	case <-time.After(500 * time.Millisecond):
		// Good: no cross-brain delivery.
	}
}

// TestFanout_PusherOwnOtherDevice: A's push is delivered to A's own other
// device subscribed to the SAME brain (self-fan-out still works after the
// brain-keying change).
func TestFanout_PusherOwnOtherDevice(t *testing.T) {
	base, _ := newFanoutServer(t, 0)
	_, ownerTok, ownerPersonal := regUser(t, base, "owner@example.com")

	// The owner's "other device" subscribes to the owner's personal brain.
	rows := make(chan PushRow, 8)
	cancel := openSSE(t, base, ownerTok, ownerPersonal, rows, nil)
	defer cancel()
	waitForSubscriber(t)

	push := PushReq{Batch: []PushRow{{Key: "memory/note", Blob: []byte("self")}}}
	postStatus(t, base+"/v2/sync/push?brain="+ownerPersonal, ownerTok, push, 200)

	select {
	case ev := <-rows:
		if ev.Key != "memory/note" || string(ev.Blob) != "self" {
			t.Errorf("own-device got %q=%q, want memory/note=self", ev.Key, ev.Blob)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner's own other device did not receive the push live")
	}
}

// TestFanout_Heartbeat asserts the SSE path emits keepalive comments on the
// configured interval.
func TestFanout_Heartbeat(t *testing.T) {
	base, _ := newFanoutServer(t, 50*time.Millisecond)
	_, tok, personal := regUser(t, base, "owner@example.com")

	pings := make(chan struct{}, 8)
	rows := make(chan PushRow, 8)
	cancel := openSSE(t, base, tok, personal, rows, pings)
	defer cancel()

	select {
	case <-pings:
		// Good: a heartbeat arrived.
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE heartbeat received within 3s")
	}
}

// waitForSubscriber gives an opened SSE connection a moment to register its
// subscription server-side before the test pushes. The handler subscribes
// before the backlog walk, but the HTTP round trip + goroutine scheduling
// is async, so a short settle avoids a publish-before-subscribe race in the
// test (production correctness is covered by the periodic-pull backstop).
func waitForSubscriber(t *testing.T) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
}
