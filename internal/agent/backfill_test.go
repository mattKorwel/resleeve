package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
	"github.com/mattkorwel/resleeve/internal/testutil"
)

// TestBackfillSessionCwd_RepairsLegacyRows verifies the full happy
// path: a session row with cwd='' is repaired from the first JSONL
// record carrying a real cwd. Rows that already have a cwd are left
// alone, and sessions with no on-disk JSONL are counted as skipped.
func TestBackfillSessionCwd_RepairsLegacyRows(t *testing.T) {
	ctx := context.Background()

	// Sandbox HOME so we can stage a fake ~/.claude/projects tree.
	home := t.TempDir()
	testutil.SetHomeDir(t, home)
	// Clear RESLEEVE_SCOPE: deriveScope short-circuits on it and would
	// make the scope-assertion below depend on the runner's env.
	t.Setenv("RESLEEVE_SCOPE", "")

	// Stage a JSONL file for the broken session at the standard path.
	// The dir name doesn't matter for indexing (we look up by file
	// basename), but use the real cwd-encoded convention for realism.
	projectsDir := filepath.Join(home, ".claude", "projects", "-tmp-fake-proj")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	// First two records have cwd:null (mirrors the live permission-mode
	// preamble that triggered the F1+F2 bug); third record carries the
	// real cwd. Backfill must walk past the nulls and pick up
	// "/tmp/fake/proj".
	jsonl := strings.Join([]string{
		`{"type":"permission-mode","sessionId":"sess-broken","uuid":"u1","timestamp":"2026-06-01T00:00:00Z"}`,
		`{"type":"permission-mode","sessionId":"sess-broken","uuid":"u2","timestamp":"2026-06-01T00:00:01Z"}`,
		`{"type":"user","sessionId":"sess-broken","uuid":"u3","cwd":"/tmp/fake/proj","timestamp":"2026-06-01T00:00:02Z","message":"\"hello\""}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(projectsDir, "sess-broken.jsonl"), []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	// Open a fresh in-memory store and seed three sessions:
	//   sess-broken — legacy: empty cwd, has JSONL on disk — REPAIRED
	//   sess-no-jsonl — legacy: empty cwd, no on-disk JSONL — SKIPPED
	//   sess-ok — already has cwd — left alone
	st, err := sqlite.Open(ctx, "file:"+t.TempDir()+"/store.db?_pragma=foreign_keys=on")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC().Truncate(time.Microsecond)
	seed := []*rsql.Session{
		{ID: "sess-broken", CLI: "claude_code", Cwd: "", StartedAt: now, Status: rsql.SessionStatusActive},
		{ID: "sess-no-jsonl", CLI: "claude_code", Cwd: "", StartedAt: now, Status: rsql.SessionStatusActive},
		{ID: "sess-ok", CLI: "claude_code", Cwd: "/already/set", StartedAt: now, Status: rsql.SessionStatusActive},
	}
	for _, s := range seed {
		if err := st.Sessions().Create(ctx, s); err != nil {
			t.Fatalf("seed %s: %v", s.ID, err)
		}
	}

	repaired, skipped, err := BackfillSessionCwd(ctx, st, nil)
	if err != nil {
		t.Fatalf("BackfillSessionCwd: %v", err)
	}
	if repaired != 1 {
		t.Errorf("repaired: got %d, want 1", repaired)
	}
	if skipped != 1 {
		t.Errorf("skipped: got %d, want 1", skipped)
	}

	got, err := st.Sessions().Get(ctx, "sess-broken")
	if err != nil {
		t.Fatalf("get sess-broken after backfill: %v", err)
	}
	if got.Cwd != "/tmp/fake/proj" {
		t.Errorf("cwd after backfill: got %q, want %q", got.Cwd, "/tmp/fake/proj")
	}
	// deriveScope on a non-existent dir falls back to filepath.Base.
	if got.Slot.Scope != "proj" {
		t.Errorf("scope after backfill: got %q, want %q", got.Slot.Scope, "proj")
	}

	untouched, err := st.Sessions().Get(ctx, "sess-ok")
	if err != nil {
		t.Fatalf("get sess-ok: %v", err)
	}
	if untouched.Cwd != "/already/set" {
		t.Errorf("sess-ok cwd was modified: got %q, want %q", untouched.Cwd, "/already/set")
	}
}
