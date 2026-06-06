package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// TestSmoke_RealRollouts exercises the codex adapter against the host's
// REAL ~/.codex/sessions rollout files (codex 0.137.0). Guarded by
// RESLEEVE_SMOKE so the normal suite stays hermetic:
//
//	RESLEEVE_SMOKE=1 go test ./internal/adapter/codex/ -run Smoke -v
//
// Read-only against codex's data; reconcile → ToNative replay round-trip,
// asserting the replayed JSONL preserves the user/assistant text and tool
// calls that appear in the captured events.
func TestSmoke_RealRollouts(t *testing.T) {
	if os.Getenv("RESLEEVE_SMOKE") == "" {
		t.Skip("set RESLEEVE_SMOKE=1 to run against real ~/.codex data")
	}
	// Do NOT override CODEX_HOME — read the real install.
	ing := newCaptureIngester()
	if err := New().ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("reconcile real rollouts: %v", err)
	}
	if len(ing.bySession) == 0 {
		t.Fatal("no sessions captured from ~/.codex/sessions")
	}

	totalEvents := 0
	for sid, evs := range ing.bySession {
		totalEvents += len(evs)
		// Seq strictly increasing per session (the co-timestamp fix).
		for i := 1; i < len(evs); i++ {
			if evs[i].Seq <= evs[i-1].Seq {
				t.Errorf("session %s: Seq not strictly increasing at %d (%d <= %d)",
					sid, i, evs[i].Seq, evs[i-1].Seq)
			}
		}
		// Replay must parse and preserve every native-payload line.
		out, err := New().ToNative(context.Background(), evs, adapter.RenderModeReplay)
		if err != nil {
			t.Fatalf("session %s: ToNative replay: %v", sid, err)
		}
		replay := string(out)
		kinds := map[event.Kind]int{}
		for _, e := range evs {
			kinds[e.Kind]++
			if len(e.Vendor.NativePayload) == 0 {
				continue
			}
			// Each captured native rollout line must reappear verbatim.
			if !strings.Contains(replay, strings.TrimSpace(string(e.Vendor.NativePayload))) {
				t.Errorf("session %s: replay dropped a native-payload %s event", sid, e.Kind)
			}
		}
		t.Logf("session %s: %d events %v → %d bytes replayed", sid, len(evs), kinds, len(out))
	}
	t.Logf("SMOKE OK: %d sessions, %d events captured from real ~/.codex", len(ing.bySession), totalEvents)
}

// TestSmoke_RealResume is the live end-to-end: capture a real session, run
// it through Hydrate (replay) into an ISOLATED CODEX_HOME, then drive the
// REAL `codex exec resume` to continue it — proving our re-serialized
// rollout is natively resumable. Spends a real model call + network, so
// it's double-guarded:
//
//	RESLEEVE_SMOKE=1 RESLEEVE_SMOKE_LIVE=1 go test ./internal/adapter/codex/ -run RealResume -v
func TestSmoke_RealResume(t *testing.T) {
	if os.Getenv("RESLEEVE_SMOKE") == "" || os.Getenv("RESLEEVE_SMOKE_LIVE") == "" {
		t.Skip("set RESLEEVE_SMOKE=1 RESLEEVE_SMOKE_LIVE=1 (spends a real codex model call)")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not on PATH")
	}
	realHome := codexHome()
	authSrc := filepath.Join(realHome, "auth.json")
	if _, err := os.Stat(authSrc); err != nil {
		t.Skip("codex not authed (no auth.json)")
	}

	// 1) Capture real sessions (reads real ~/.codex — env not yet overridden).
	ing := newCaptureIngester()
	if err := New().ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Pick the session with the FEWEST events (cheapest to resume).
	var sid string
	for id, evs := range ing.bySession {
		if sid == "" || len(evs) < len(ing.bySession[sid]) {
			sid = id
		}
	}
	if sid == "" {
		t.Skip("no real sessions captured")
	}
	evs := ing.bySession[sid]

	// 2) Isolated CODEX_HOME with copied creds/config so we don't touch real ~/.codex.
	tmpHome := t.TempDir()
	copyFile(t, authSrc, filepath.Join(tmpHome, "auth.json"))
	if _, err := os.Stat(filepath.Join(realHome, "config.toml")); err == nil {
		copyFile(t, filepath.Join(realHome, "config.toml"), filepath.Join(tmpHome, "config.toml"))
	}
	t.Setenv("CODEX_HOME", tmpHome) // Hydrate writes its rollout here now

	// 3) Hydrate (replay) — write our re-serialized rollout into the isolated home.
	var started time.Time
	if len(evs) > 0 {
		started = evs[0].Timestamp
	}
	view := adapter.SessionView{
		SessionID:   sid,
		CLI:         Name,
		StartedAt:   started,
		EventStream: func() ([]event.Event, error) { return evs, nil },
	}
	res, err := New().Hydrate(context.Background(), view, adapter.HydrateOpts{Mode: adapter.RenderModeReplay})
	if err != nil {
		t.Fatalf("hydrate replay: %v", err)
	}
	t.Logf("hydrated rollout → %s (mode=%s)", res.Path, res.Mode)
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("hydrated rollout missing: %v", err)
	}

	// 4) Drive REAL codex to resume OUR rollout and run a turn.
	const token = "RESLEEVE_RESUME_OK"
	cmd := exec.Command(bin, "exec", "resume", sid,
		"Ignore all prior task context. Reply with exactly this token and nothing else: "+token)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+tmpHome)
	out, _ := cmd.CombinedOutput()
	t.Logf("codex exec resume output (tail):\n%s", tailStr(string(out), 1200))

	// Success = codex found+loaded our hand-written rollout and produced a turn.
	if !strings.Contains(string(out), token) {
		t.Fatalf("codex did not echo the token — resume of our rollout may have failed")
	}
	t.Logf("SMOKE OK: real codex resumed our replayed rollout for session %s and ran a turn", sid)
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
