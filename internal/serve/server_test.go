package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/sync/local"
)

const testToken = "test-bearer-token-aaaa"

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	s, err := New(Config{Backend: backend, AuthToken: testToken})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, ts.URL
}

func TestNew_RejectsEmptyAuthToken(t *testing.T) {
	backend, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	if _, err := New(Config{Backend: backend, AuthToken: ""}); err == nil {
		t.Error("expected error for empty AuthToken")
	}
}

func TestHealth_NoAuthRequired(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/v2/sync/health")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("health status: got %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("health body: %v", body)
	}
}

func TestPush_Requires401WithoutAuth(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Post(base+"/v2/sync/push", "application/json", strings.NewReader(`{"batch":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthorized status: got %d, want 401", resp.StatusCode)
	}
}

func TestPush_WrongTokenRejected(t *testing.T) {
	_, base := newTestServer(t)
	req, _ := http.NewRequest("POST", base+"/v2/sync/push", strings.NewReader(`{"batch":[{"key":"x","blob":"AAA="}]}`))
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-token status: got %d, want 401", resp.StatusCode)
	}
}

func TestPushPull_RoundTrip(t *testing.T) {
	_, base := newTestServer(t)

	// Push 3 rows.
	reqBody := PushReq{Batch: []PushRow{
		{Key: "sessions/S1/events/000001", Blob: []byte("ciphertext-a")},
		{Key: "sessions/S1/events/000002", Blob: []byte("ciphertext-b")},
		{Key: "sessions/S1/events/000003", Blob: []byte("ciphertext-c")},
	}}
	body, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", base+"/v2/sync/push", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("Push status: got %d", resp.StatusCode)
	}
	var pushResp PushResp
	_ = json.NewDecoder(resp.Body).Decode(&pushResp)
	if len(pushResp.Committed) != 3 {
		t.Errorf("Push committed: got %d, want 3", len(pushResp.Committed))
	}

	// Pull all three via ?kind=sessions.
	pullReq, _ := http.NewRequest("GET", base+"/v2/sync/pull?kind=sessions", nil)
	pullReq.Header.Set("Authorization", "Bearer "+testToken)
	pullResp, err := http.DefaultClient.Do(pullReq)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	defer pullResp.Body.Close()
	if pullResp.StatusCode != 200 {
		t.Fatalf("Pull status: got %d", pullResp.StatusCode)
	}
	var pull PullResp
	_ = json.NewDecoder(pullResp.Body).Decode(&pull)
	if len(pull.Rows) != 3 {
		t.Fatalf("Pull rows: got %d, want 3", len(pull.Rows))
	}
	if string(pull.Rows[0].Blob) != "ciphertext-a" {
		t.Errorf("Pull row 0 blob: got %q, want %q", string(pull.Rows[0].Blob), "ciphertext-a")
	}
	if pull.Rows[0].Key != "sessions/S1/events/000001" {
		t.Errorf("Pull row 0 key: %q", pull.Rows[0].Key)
	}
}

func TestPull_Pagination(t *testing.T) {
	_, base := newTestServer(t)

	for i := 1; i <= 5; i++ {
		body, _ := json.Marshal(PushReq{Batch: []PushRow{
			{Key: "memory/scope-x/learnings/" + zeroPad(i, 6), Blob: []byte("blob")},
		}})
		req, _ := http.NewRequest("POST", base+"/v2/sync/push", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("seed push %d: %v", i, err)
		}
		resp.Body.Close()
	}

	// Page 1: limit=2.
	req, _ := http.NewRequest("GET", base+"/v2/sync/pull?kind=memory&limit=2", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("page1 Do: %v", err)
	}
	defer resp.Body.Close()
	var page1 PullResp
	_ = json.NewDecoder(resp.Body).Decode(&page1)
	if len(page1.Rows) != 2 {
		t.Fatalf("page1 rows: got %d, want 2", len(page1.Rows))
	}
	if page1.NextCursor == "" {
		t.Errorf("page1 should have a next_cursor")
	}

	// Page 2: continue.
	req2, _ := http.NewRequest("GET", base+"/v2/sync/pull?kind=memory&since="+page1.NextCursor+"&limit=2", nil)
	req2.Header.Set("Authorization", "Bearer "+testToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("page2 Do: %v", err)
	}
	defer resp2.Body.Close()
	var page2 PullResp
	_ = json.NewDecoder(resp2.Body).Decode(&page2)
	if len(page2.Rows) != 2 {
		t.Fatalf("page2 rows: got %d, want 2", len(page2.Rows))
	}
}

func TestPull_RejectsUnknownKind(t *testing.T) {
	_, base := newTestServer(t)
	req, _ := http.NewRequest("GET", base+"/v2/sync/pull?kind=bogus", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown kind: got %d, want 400", resp.StatusCode)
	}
}

func zeroPad(n int, width int) string {
	s := ""
	for i := 0; i < width; i++ {
		s = "0" + s
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+(n%10))) + digits
		n /= 10
	}
	if len(digits) >= width {
		return digits
	}
	return s[:width-len(digits)] + digits
}
