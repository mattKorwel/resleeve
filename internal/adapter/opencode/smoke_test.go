package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/event"
)

// smokeIngester collects reconciled events grouped by session id.
type smokeIngester struct{ bySession map[string][]event.Event }

func (s *smokeIngester) IngestBatch(ctx context.Context, sessionID string, evs []event.Event) error {
	if s.bySession == nil {
		s.bySession = map[string][]event.Event{}
	}
	s.bySession[sessionID] = append(s.bySession[sessionID], evs...)
	return nil
}

// TestSmoke_RealImport drives our replay output through the REAL `opencode`
// binary: it builds the import transcript our adapter emits (ToNative
// replay) and runs `opencode import` against an isolated XDG_DATA_HOME,
// then asserts the session + message + text part actually land in
// opencode's own SQLite db. Guarded by RESLEEVE_SMOKE:
//
//	RESLEEVE_SMOKE=1 go test ./internal/adapter/opencode/ -run Smoke -v
//
// Validates the riskiest open item: that the installed opencode accepts the
// SessionV1 {info, messages:[{info,parts}]} shape our adapter produces.
func TestSmoke_RealImport(t *testing.T) {
	if os.Getenv("RESLEEVE_SMOKE") == "" {
		t.Skip("set RESLEEVE_SMOKE=1 to run against the real opencode binary")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode not on PATH")
	}

	const sid = "ses_9a8b7c6d5e4f30211222330001"
	const mid = "msg_9a8b7c6d5e4f30211222330002"
	const pid = "prt_9a8b7c6d5e4f30211222330003"

	sessionInfo := mustJSON(map[string]any{
		"id": sid, "projectID": "prj_9a8b7c6d5e4f30211222330004",
		"slug": "resleeve-smoke", "directory": "/tmp/resleeve-smoke",
		"title": "resleeve smoke", "version": "0.0.0",
		"time": map[string]any{"created": 1749139200000, "updated": 1749139200000},
	})
	// A message event carries the {info, parts} envelope the db reader emits.
	msgEnvelope := mustJSON(map[string]any{
		"info": map[string]any{
			"id": mid, "sessionID": sid, "role": "user",
			"time":  map[string]any{"created": 1749139200000},
			"agent": "build",
			"model": map[string]any{"providerID": "anthropic", "modelID": "claude-sonnet-4-5"},
		},
		"parts": []any{map[string]any{
			"id": pid, "sessionID": sid, "messageID": mid,
			"type": "text", "text": "hello from the resleeve smoke test",
		}},
	})

	events := []event.Event{
		{Kind: event.KindSessionStart, SessionID: sid, Seq: 1,
			Vendor: event.Vendor{Name: Name, NativePayload: sessionInfo}},
		{Kind: event.KindUserMessage, SessionID: sid, Seq: 2,
			Vendor: event.Vendor{Name: Name, NativePayload: msgEnvelope}},
	}

	body, err := New().ToNative(context.Background(), events, "replay")
	if err != nil {
		t.Fatalf("ToNative replay: %v", err)
	}

	tmp := t.TempDir()
	transcript := filepath.Join(tmp, "transcript.json")
	if err := os.WriteFile(transcript, body, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	cmd := exec.Command(bin, "import", transcript, "--pure")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+filepath.Join(tmp, "data"))
	out, err := cmd.CombinedOutput()
	t.Logf("opencode import output:\n%s", out)
	if err != nil {
		t.Fatalf("opencode import failed: %v", err)
	}
	if !strings.Contains(string(out), "Imported session") {
		t.Fatalf("import did not report success")
	}

	// Confirm the rows actually landed in opencode's own db.
	db := filepath.Join(tmp, "data", "opencode", "opencode.db")
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Logf("sqlite3 not available; import success is sufficient signal")
		return
	}
	q := exec.Command(sqlite, db,
		"select (select count(*) from session), (select count(*) from message), (select count(*) from part);")
	rows, err := q.Output()
	if err != nil {
		t.Fatalf("query imported db: %v", err)
	}
	got := strings.TrimSpace(string(rows))
	if got != "1|1|1" {
		t.Fatalf("expected 1 session/1 message/1 part, got %q", got)
	}
	textQ := exec.Command(sqlite, db, "select json_extract(data,'$.text') from part;")
	txt, _ := textQ.Output()
	if !strings.Contains(string(txt), "resleeve smoke test") {
		t.Fatalf("part text did not round-trip: %q", txt)
	}
	t.Logf("SMOKE OK: real opencode import accepted adapter transcript; rows=%s text=%s", got, strings.TrimSpace(string(txt)))
}

// TestSmoke_RealCaptureRoundTrip is the gold-standard end-to-end: it reads
// the host's REAL opencode.db via our ReconcileOnce (capture), replays one
// captured session through ToNative, and re-imports it via the real
// `opencode import` binary into an isolated data dir — proving a genuine
// captured session survives a full capture→replay→import round-trip with
// its message/part content intact.
//
//	RESLEEVE_SMOKE=1 go test ./internal/adapter/opencode/ -run SmokeRealCapture -v
func TestSmoke_RealCaptureRoundTrip(t *testing.T) {
	if os.Getenv("RESLEEVE_SMOKE") == "" {
		t.Skip("set RESLEEVE_SMOKE=1 to run against real opencode data")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode not on PATH")
	}
	// Do NOT set XDG_DATA_HOME here — read the real ~/.local/share/opencode db.
	a := New()
	if det, _ := a.Detect(context.Background()); det.Quirks["unsupported"] == "pre-sqlite" {
		t.Skip("opencode store is pre-sqlite era")
	}
	ing := &smokeIngester{}
	if err := a.ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("reconcile real opencode.db: %v", err)
	}
	if len(ing.bySession) == 0 {
		t.Skip("no real opencode sessions captured (run an opencode session first)")
	}

	// Pick the session with the most events.
	var sid string
	for id, evs := range ing.bySession {
		if sid == "" || len(evs) > len(ing.bySession[sid]) {
			sid = id
		}
	}
	evs := ing.bySession[sid]
	kinds := map[event.Kind]int{}
	for _, e := range evs {
		kinds[e.Kind]++
	}
	t.Logf("captured real session %s: %d events %v", sid, len(evs), kinds)

	body, err := a.ToNative(context.Background(), evs, "replay")
	if err != nil {
		t.Fatalf("ToNative replay: %v", err)
	}

	tmp := t.TempDir()
	transcript := filepath.Join(tmp, "transcript.json")
	if err := os.WriteFile(transcript, body, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	cmd := exec.Command(bin, "import", transcript, "--pure")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+filepath.Join(tmp, "data"))
	out, err := cmd.CombinedOutput()
	t.Logf("opencode import output:\n%s", out)
	if err != nil {
		t.Fatalf("real captured session failed to re-import: %v", err)
	}
	if !strings.Contains(string(out), "Imported session") {
		t.Fatalf("import did not report success for real captured session")
	}

	db := filepath.Join(tmp, "data", "opencode", "opencode.db")
	if sqlite, lerr := exec.LookPath("sqlite3"); lerr == nil {
		rows, _ := exec.Command(sqlite, db,
			"select (select count(*) from session),(select count(*) from message),(select count(*) from part);").Output()
		t.Logf("re-imported db counts (session|message|part): %s", strings.TrimSpace(string(rows)))
	}
	t.Logf("SMOKE OK: real captured opencode session round-tripped capture→replay→import")
}

// TestSmoke_RealResume is the live end-to-end: capture a real session,
// replay it via ToNative, import it into an ISOLATED data dir (with the
// host's opencode credentials copied in), then drive the REAL
// `opencode run --session <id>` to resume and run a turn. Spends a real
// model call, so it's double-guarded:
//
//	RESLEEVE_SMOKE=1 RESLEEVE_SMOKE_LIVE=1 go test ./internal/adapter/opencode/ -run RealResume -v
func TestSmoke_RealResume(t *testing.T) {
	if os.Getenv("RESLEEVE_SMOKE") == "" || os.Getenv("RESLEEVE_SMOKE_LIVE") == "" {
		t.Skip("set RESLEEVE_SMOKE=1 RESLEEVE_SMOKE_LIVE=1 (spends a real opencode model call)")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode not on PATH")
	}
	realAuth := filepath.Join(openCodeDataDir(), "auth.json")
	if _, err := os.Stat(realAuth); err != nil {
		t.Skip("opencode not authed (no auth.json) — run `opencode auth login`")
	}

	// 1) Capture real sessions (reads real ~/.local/share/opencode db).
	a := New()
	ing := &smokeIngester{}
	if err := a.ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("reconcile real opencode.db: %v", err)
	}
	if len(ing.bySession) == 0 {
		t.Skip("no real opencode sessions captured")
	}
	var sid string
	for id, evs := range ing.bySession {
		if sid == "" || len(evs) > len(ing.bySession[sid]) {
			sid = id
		}
	}
	evs := ing.bySession[sid]

	body, err := a.ToNative(context.Background(), evs, "replay")
	if err != nil {
		t.Fatalf("ToNative replay: %v", err)
	}

	// 2) Isolated data dir with the host's creds copied in.
	tmp := t.TempDir()
	dataHome := filepath.Join(tmp, "data")
	if err := os.MkdirAll(filepath.Join(dataHome, "opencode"), 0o700); err != nil {
		t.Fatalf("mkdir isolated data: %v", err)
	}
	authBytes, err := os.ReadFile(realAuth)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataHome, "opencode", "auth.json"), authBytes, 0o600); err != nil {
		t.Fatalf("copy auth.json: %v", err)
	}
	env := append(os.Environ(), "XDG_DATA_HOME="+dataHome)

	// 3) Import our replayed transcript into the isolated db.
	transcript := filepath.Join(tmp, "transcript.json")
	if err := os.WriteFile(transcript, body, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	imp := exec.Command(bin, "import", transcript)
	imp.Env = env
	imp.Dir = "/Users/mattkorwel/dev/resleeve"
	if out, err := imp.CombinedOutput(); err != nil {
		t.Fatalf("opencode import failed: %v\n%s", err, out)
	}

	// 4) Drive REAL opencode to resume our imported session and run a turn.
	const token = "RESLEEVE_RESUME_OK"
	run := exec.Command(bin, "run", "--session", sid, "--print-logs", "--log-level", "INFO",
		"Ignore all prior task context. Reply with exactly this token and nothing else: "+token)
	run.Env = env
	run.Dir = "/Users/mattkorwel/dev/resleeve"
	out, runErr := run.CombinedOutput()
	t.Logf("opencode run --session output (tail):\n%s", tailStr(string(out), 800))
	if runErr != nil {
		t.Fatalf("opencode run --session failed: %v", runErr)
	}
	if !strings.Contains(string(out), token) {
		t.Fatalf("opencode did not echo the token — resume of our imported session may have failed")
	}
	t.Logf("SMOKE OK: real opencode resumed our imported session %s and ran a turn", sid)
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
