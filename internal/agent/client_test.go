package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDoJSON_ParsesEnvelope pins the Q2 contract on the client side:
// when the daemon emits a structured envelope
//
//	{"error": {"code": "no_upstream", "message": "..."}}
//
// Client.doJSON (via classifyDaemonError) must surface a typed
// ErrNoUpstream so callers can use errors.Is without scanning the body.
func TestDoJSON_ParsesEnvelope(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(w, http.StatusConflict, codeNoUpstream, "no upstream configured")
	}))
	t.Cleanup(ts.Close)

	c := NewClient(ts.URL, "irrelevant")
	_, err := c.SyncPushNow(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNoUpstream) {
		t.Fatalf("expected errors.Is(err, ErrNoUpstream); got %v", err)
	}
	// Status + human message should still be in the wrapped string for
	// logs/debug surfaces.
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("error string should include status: %v", err)
	}
	if !strings.Contains(err.Error(), "no upstream configured") {
		t.Errorf("error string should include human message: %v", err)
	}
}

// TestDoJSON_ParsesEnvelope_UnknownCodeIsOpaque guards the fall-through:
// envelope codes we don't recognize must still produce a sensible
// non-nil error with status+message, but NO sentinel wrap.
func TestDoJSON_ParsesEnvelope_UnknownCodeIsOpaque(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(w, http.StatusBadRequest, "some_future_code", "you sent a bad thing")
	}))
	t.Cleanup(ts.Close)

	c := NewClient(ts.URL, "irrelevant")
	_, err := c.SyncPushNow(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNoUpstream) {
		t.Fatalf("unrelated code should not match ErrNoUpstream: %v", err)
	}
	if !strings.Contains(err.Error(), "you sent a bad thing") {
		t.Errorf("error string should include human message: %v", err)
	}
}

// TestDoJSON_FallsBackToBodyString pins the back-compat path: if the
// daemon (or an interposing proxy) returns a NON-envelope 4xx body, we
// must still recognize the legacy "no upstream configured" 409
// substring as ErrNoUpstream. This is the bridge that lets a new client
// talk to an older pre-Q2 daemon binary without losing standalone-mode
// detection.
func TestDoJSON_FallsBackToBodyString(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mimic the pre-Q2 daemon's http.Error path: plain text body.
		http.Error(w, "no upstream configured — set --upstream on `resleeve up`", http.StatusConflict)
	}))
	t.Cleanup(ts.Close)

	c := NewClient(ts.URL, "irrelevant")
	_, err := c.SyncPullNow(context.Background())
	if err == nil {
		t.Fatal("expected error from pre-envelope daemon, got nil")
	}
	if !errors.Is(err, ErrNoUpstream) {
		t.Fatalf("legacy text/plain 409 path should still produce ErrNoUpstream via substring fallback; got %v", err)
	}
}

// TestDoJSON_FallsBackToBodyString_OpaqueOtherwise locks the other side
// of the fallback: a non-envelope body that ISN'T the no-upstream 409
// must not be classified as anything specific — just surfaced as an
// opaque daemon error. (Negative control for the bridge.)
func TestDoJSON_FallsBackToBodyString_OpaqueOtherwise(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "something else entirely", http.StatusBadRequest)
	}))
	t.Cleanup(ts.Close)

	c := NewClient(ts.URL, "irrelevant")
	_, err := c.SyncPullNow(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNoUpstream) {
		t.Fatalf("unrelated 400 should not classify as ErrNoUpstream: %v", err)
	}
	if !strings.Contains(err.Error(), "something else entirely") {
		t.Errorf("error string should include body: %v", err)
	}
}

// TestClassifyDaemonError_TableDriven covers the helper directly to
// guard the edge cases that would be awkward to stand up an httptest
// server for (empty body, non-JSON-looking body, JSON without an
// envelope shape).
func TestClassifyDaemonError_TableDriven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		status    string
		body      string
		wantWraps error
		wantMsg   string
	}{
		{
			name:      "envelope no_upstream",
			status:    "409 Conflict",
			body:      `{"error":{"code":"no_upstream","message":"no upstream configured"}}`,
			wantWraps: ErrNoUpstream,
			wantMsg:   "no upstream configured",
		},
		{
			name:    "envelope unknown code = opaque",
			status:  "400 Bad Request",
			body:    `{"error":{"code":"weird","message":"weird thing"}}`,
			wantMsg: "weird thing",
		},
		{
			name:      "legacy plaintext 409 no upstream",
			status:    "409 Conflict",
			body:      "no upstream configured — set --upstream",
			wantWraps: ErrNoUpstream,
			wantMsg:   "no upstream configured",
		},
		{
			name:    "legacy plaintext non-conflict",
			status:  "404 Not Found",
			body:    "not found",
			wantMsg: "not found",
		},
		{
			name:    "empty body",
			status:  "500 Internal Server Error",
			body:    "",
			wantMsg: "500 Internal Server Error",
		},
		{
			name:    "JSON without envelope (just an unrelated object)",
			status:  "400 Bad Request",
			body:    `{"detail":"something"}`,
			wantMsg: `{"detail":"something"}`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyDaemonError(tc.status, []byte(tc.body))
			if err == nil {
				t.Fatal("expected non-nil error")
			}
			if tc.wantWraps != nil && !errors.Is(err, tc.wantWraps) {
				t.Errorf("wraps: got %v, want errors.Is to match %v", err, tc.wantWraps)
			}
			if tc.wantWraps == nil && errors.Is(err, ErrNoUpstream) {
				t.Errorf("unexpected ErrNoUpstream wrap: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("message: %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestClassifyDaemonError_DoubleWrappedSentinelChain pins that
// errors.Is on the typed sentinel walks through the daemon-side wrap
// AND any further caller wrapping. Regression for the kind of chaining
// in internal/cli/on_demand_pull.go.
func TestClassifyDaemonError_DoubleWrappedSentinelChain(t *testing.T) {
	t.Parallel()
	inner := classifyDaemonError("409 Conflict",
		[]byte(`{"error":{"code":"no_upstream","message":"x"}}`))
	outer := fmt.Errorf("on-demand pull: %w", inner)
	if !errors.Is(outer, ErrNoUpstream) {
		t.Fatalf("double-wrap should still match ErrNoUpstream: %v", outer)
	}
}
