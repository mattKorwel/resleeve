package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// --- SSE (slice 3) ---

func TestSSE_RequiresAuth(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/v2/sync/sse?kind=memory")
	if err != nil {
		t.Fatalf("GET sse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("sse no-auth: got %d, want 401", resp.StatusCode)
	}
}

func TestSSE_OnlyMemoryKindSupported(t *testing.T) {
	_, base := newTestServer(t)
	req, _ := http.NewRequest("GET", base+"/v2/sync/sse?kind=sessions", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("sse wrong kind: got %d, want 400", resp.StatusCode)
	}
}

// TestSSE_BacklogThenLive seeds two memory rows, subscribes, asserts
// backlog delivery, pushes a third row, asserts live delivery.
func TestSSE_BacklogThenLive(t *testing.T) {
	_, base := newTestServer(t)

	// Seed two memory rows directly via POST /push. Keys use the
	// slice-3 shape: memory/<encoded-scope-path>.
	pushRows(t, base, []PushRow{
		{Key: "memory/alpha", Blob: []byte("scope-a")},
		{Key: "memory/beta", Blob: []byte("scope-b")},
	})

	// Open SSE in a goroutine; collect events into a channel.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v2/sync/sse?kind=memory", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status: got %d, want 200", resp.StatusCode)
	}

	events := make(chan PushRow, 16)
	go readSSE(resp.Body, events)

	// Expect 2 backlog events.
	for i := 0; i < 2; i++ {
		select {
		case ev := <-events:
			if !strings.HasPrefix(ev.Key, "memory/") {
				t.Errorf("backlog event %d key: %q", i, ev.Key)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for backlog event %d", i)
		}
	}

	// Push a third row — should arrive live.
	pushRows(t, base, []PushRow{
		{Key: "memory/gamma", Blob: []byte("scope-g")},
	})
	select {
	case ev := <-events:
		if ev.Key != "memory/gamma" {
			t.Errorf("live event key: got %q, want memory/gamma", ev.Key)
		}
		if string(ev.Blob) != "scope-g" {
			t.Errorf("live event blob: got %q, want scope-g", string(ev.Blob))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for live event")
	}
}

// --- SSE helpers ---

func pushRows(t *testing.T, base string, rows []PushRow) {
	t.Helper()
	body, _ := json.Marshal(PushReq{Batch: rows})
	req, _ := http.NewRequest("POST", base+"/v2/sync/push", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("seed push: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed push status %d", resp.StatusCode)
	}
}

// readSSE parses the SSE byte stream and sends each PushRow into out.
// Heartbeats (lines starting with ':') are ignored.
func readSSE(r io.Reader, out chan<- PushRow) {
	scanner := bufio.NewScanner(r)
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
				out <- row
			}
			data.Reset()
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

// TestWriteError_EnvelopeShape locks the Q2 wire contract: every 4xx /
// 5xx response from serve must be a structured envelope
//
//	{"error": {"code": "<machine>", "message": "<human>"}}
//
// rather than the pre-Q2 flat `{"error": "freeform text"}`. Asserts on
// three exemplar paths (missing kind = 400 invalid_request, no bearer
// = 401 unauthorized, unknown kind = 400 invalid_request). The shape +
// code matter; the human message is checked loosely (substring) so
// future wording tweaks don't churn this test.
func TestWriteError_EnvelopeShape(t *testing.T) {
	_, base := newTestServer(t)

	type expect struct {
		name    string
		req     func() *http.Request
		status  int
		code    string
		msgPart string
	}
	cases := []expect{
		{
			name: "missing kind = 400 invalid_request",
			req: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, base+"/v2/sync/pull", nil)
				r.Header.Set("Authorization", "Bearer "+testToken)
				return r
			},
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			msgPart: "kind",
		},
		{
			name: "no bearer = 401 unauthorized",
			req: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, base+"/v2/sync/pull?kind=memory", nil)
				return r
			},
			status:  http.StatusUnauthorized,
			code:    CodeUnauthorized,
			msgPart: "bearer",
		},
		{
			name: "unknown kind = 400 invalid_request",
			req: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, base+"/v2/sync/pull?kind=bogus", nil)
				r.Header.Set("Authorization", "Bearer "+testToken)
				return r
			},
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			msgPart: "invalid kind",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.DefaultClient.Do(tc.req())
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status: got %d, want %d", resp.StatusCode, tc.status)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type: got %q, want application/json", ct)
			}
			body, _ := io.ReadAll(resp.Body)
			// Envelope decode: error must be an OBJECT, not a string.
			var env errorEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode envelope: %v; body=%s", err, string(body))
			}
			if env.Error.Code != tc.code {
				t.Errorf("code: got %q, want %q", env.Error.Code, tc.code)
			}
			if !strings.Contains(env.Error.Message, tc.msgPart) {
				t.Errorf("message: %q does not contain %q", env.Error.Message, tc.msgPart)
			}
			// Regression guard: the legacy flat `{"error": "<string>"}`
			// shape must NOT decode — that's what the rest of the codebase
			// is migrating away from.
			var legacy struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(body, &legacy); err == nil && legacy.Error != "" {
				t.Errorf("legacy flat shape still decodes: %q (envelope migration regression)", legacy.Error)
			}
		})
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
