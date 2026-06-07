package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/memory"
)

// newTestDaemonForMemory builds a Daemon wired to a fresh sqlite store
// with the memory routes registered. No upstream, no sealer — pure
// local memory CRUD.
func newTestDaemonForMemory(t *testing.T) (*Daemon, *httptest.Server) {
	t.Helper()
	store := newSyncTestStore(t)
	d := &Daemon{store: store, secret: "test-bearer-fixed"}
	mux := http.NewServeMux()
	d.registerMemoryRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return d, srv
}

// TestPutScope_EmptyBodyIsAllowed guards Q14. putScope used to compare
// json.Decode's error message against the literal string "EOF"; the
// fix replaces that with errors.Is(err, io.EOF). An empty body is the
// intended "just create-with-defaults" shortcut: io.EOF should be
// swallowed and the scope created with zero-value fields. A garbage
// (non-EOF) body should still 400 with "decode: ...".
func TestPutScope_EmptyBodyIsAllowed_NoStringlyEOF(t *testing.T) {
	d, srv := newTestDaemonForMemory(t)
	t.Cleanup(func() { _ = d.store.Close() })

	// (1) Empty body → io.EOF swallowed → 200 with empty-field scope.
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/scope?path=team-x", nil)
	req.Header.Set("Authorization", "Bearer "+d.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT scope (empty): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty-body status: got %d, want 200; body=%s", resp.StatusCode, body)
	}

	// (2) Malformed JSON body → real decode error → 400 with "decode:".
	req2, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/scope?path=team-y",
		strings.NewReader("not json{"))
	req2.Header.Set("Authorization", "Bearer "+d.secret)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("PUT scope (garbage): %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage-body status: got %d, want 400; body=%s", resp2.StatusCode, body2)
	}
	if !strings.Contains(string(body2), "decode:") {
		t.Fatalf("garbage body should surface decode error; got %q", body2)
	}
}

// TestDeleteScope_ReturnsConflictWithHasChildrenBody guards Q13's
// daemon-side contract: when DeleteScope refuses (parent has
// children), the response is 409 and the body contains "scope has
// children". The client side (memory_client.go's DeleteScope) reads
// this body fragment to wrap memory.ErrScopeHasChildren, so the
// fragment is load-bearing — drifting the daemon's error text
// would silently degrade the typed-sentinel detection. See the
// round-5 lesson on one-sided ErrNoUpstream wrapping for context.
func TestDeleteScope_ReturnsConflictWithHasChildrenBody(t *testing.T) {
	d, srv := newTestDaemonForMemory(t)
	t.Cleanup(func() { _ = d.store.Close() })
	ctx := context.Background()

	// Seed parent + child.
	parent := &memory.Scope{Path: "team-foo", Kind: memory.ScopeKindProgram}
	if err := d.store.Memory().UpdateScope(ctx, parent); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	child := &memory.Scope{Path: "team-foo/project-alpha", Kind: memory.ScopeKindProject}
	if err := d.store.Memory().UpdateScope(ctx, child); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	// Delete parent via the client; expect the typed sentinel to
	// survive the HTTP boundary via errors.Is.
	c := NewClient(srv.URL, d.secret)
	err := c.DeleteScope(ctx, "team-foo")
	if err == nil {
		t.Fatal("expected error deleting parent with children")
	}
	if !errors.Is(err, memory.ErrScopeHasChildren) {
		t.Errorf("expected errors.Is(err, memory.ErrScopeHasChildren); got %v", err)
	}
}

// TestPlanWrite_OptimisticConcurrency exercises the round-12B plan
// versioning contract through the daemon HTTP boundary:
//   - first write (base 0) creates version 1
//   - correct base (1) appends version 2
//   - stale base (1, HEAD=2) => HTTP 409 with the current HEAD in the body,
//     surfaced client-side as *PlanConflict / memory.ErrPlanConflict
//   - --force bypasses the check
//   - GetPlan returns HEAD + version; history lists every version
func TestPlanWrite_OptimisticConcurrency(t *testing.T) {
	d, srv := newTestDaemonForMemory(t)
	t.Cleanup(func() { _ = d.store.Close() })
	ctx := context.Background()

	if err := d.store.Memory().UpdateScope(ctx, &memory.Scope{Path: "svc", Kind: memory.ScopeKindProject}); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	c := NewClient(srv.URL, d.secret)

	p1, err := c.PutPlan(ctx, "svc", "", "v1", memory.NewPlanBaseVersion, false)
	if err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if p1.Version != 1 {
		t.Fatalf("v1 version: got %d, want 1", p1.Version)
	}

	p2, err := c.PutPlan(ctx, "svc", "", "v2", 1, false)
	if err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if p2.Version != 2 || p2.ParentVersion != 1 {
		t.Fatalf("v2 version/parent: got %d/%d, want 2/1", p2.Version, p2.ParentVersion)
	}

	// Stale base: 409 carrying HEAD.
	_, err = c.PutPlan(ctx, "svc", "", "v3-stale", 1, false)
	if !errors.Is(err, memory.ErrPlanConflict) {
		t.Fatalf("stale write: want ErrPlanConflict, got %v", err)
	}
	var conflict *PlanConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("err not *PlanConflict: %v", err)
	}
	if conflict.Head == nil || conflict.Head.Version != 2 || conflict.Head.Content != "v2" {
		t.Fatalf("409 body HEAD: %+v", conflict.Head)
	}

	// Force bypass.
	p3, err := c.PutPlan(ctx, "svc", "", "forced", 0, true)
	if err != nil {
		t.Fatalf("forced write: %v", err)
	}
	if p3.Version != 3 {
		t.Fatalf("forced version: got %d, want 3", p3.Version)
	}

	// GetPlan returns HEAD + version.
	head, err := c.GetPlan(ctx, "svc", "")
	if err != nil {
		t.Fatalf("get HEAD: %v", err)
	}
	if head.Version != 3 || head.Content != "forced" {
		t.Fatalf("HEAD: %+v", head)
	}

	// History via the client.
	hist, err := c.ListPlanVersions(ctx, "svc", "")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len: got %d, want 3", len(hist))
	}
}
