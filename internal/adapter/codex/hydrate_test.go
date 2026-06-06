package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
	"github.com/mattkorwel/resleeve/internal/testutil"
)

func TestHydrate_ReplayWritesDateShardedRollout(t *testing.T) {
	codexDir := filepath.Join(t.TempDir(), ".codex")
	t.Setenv("CODEX_HOME", codexDir)

	const sid = "019e90a2-c115-7523-b5a3-50c024b3ba14"
	started := time.Date(2026, 6, 4, 9, 30, 15, 0, time.UTC)
	metaLine := `{"timestamp":"2026-06-04T09:30:15Z","type":"session_meta","payload":{"id":"` + sid + `","cwd":"/home/u/proj"}}`
	msgLine := `{"timestamp":"2026-06-04T09:30:16Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`

	events := []event.Event{
		{EventUUID: "A", Seq: 1, SessionID: sid, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(metaLine)}},
		{EventUUID: "B", Seq: 2, SessionID: sid, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(msgLine)}},
	}
	session := adapter.SessionView{
		SessionID:   sid,
		CLI:         Name,
		Cwd:         "/home/u/proj",
		StartedAt:   started,
		EventStream: func() ([]event.Event, error) { return events, nil },
	}
	result, err := New().Hydrate(context.Background(), session, adapter.HydrateOpts{})
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if result.Mode != adapter.RenderModeReplay {
		t.Errorf("Mode: got %q", result.Mode)
	}
	if result.SessionID != sid {
		t.Errorf("SessionID: got %q, want %q", result.SessionID, sid)
	}
	wantPath := filepath.Join(codexDir, "sessions", "2026", "06", "04", "rollout-2026-06-04T09-30-15-"+sid+".jsonl")
	if result.Path != wantPath {
		t.Errorf("Path:\n got %q\nwant %q", result.Path, wantPath)
	}
	body, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), body)
	}
	// First line must be a session_meta whose id == sid (resume invariant).
	rl, err := ParseRolloutLine([]byte(lines[0]))
	if err != nil || rl.Type != "session_meta" {
		t.Fatalf("first line not session_meta: %v (%s)", err, lines[0])
	}
}

// TestHydrate_ReplayPrependsSessionMetaWhenMissing verifies the resume
// invariant is enforced: if the events have no leading session_meta, one
// is synthesized at the head.
func TestHydrate_ReplayPrependsSessionMetaWhenMissing(t *testing.T) {
	codexDir := filepath.Join(t.TempDir(), ".codex")
	t.Setenv("CODEX_HOME", codexDir)

	const sid = "019e90a2-c115-7523-b5a3-50c024b3ba14"
	msgLine := `{"timestamp":"2026-06-04T09:30:16Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`
	events := []event.Event{
		{EventUUID: "B", Seq: 2, SessionID: sid, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(msgLine)}},
	}
	session := adapter.SessionView{
		SessionID:   sid,
		Cwd:         "/p",
		GitBranch:   "main",
		CLIVersion:  "0.137.0",
		StartedAt:   time.Date(2026, 6, 4, 9, 30, 15, 0, time.UTC),
		EventStream: func() ([]event.Event, error) { return events, nil },
	}
	result, err := New().Hydrate(context.Background(), session, adapter.HydrateOpts{})
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	body, _ := os.ReadFile(result.Path)
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected synthesized meta + 1 msg = 2 lines, got %d: %q", len(lines), body)
	}
	rl, err := ParseRolloutLine([]byte(lines[0]))
	if err != nil || rl.Type != "session_meta" {
		t.Fatalf("expected leading session_meta, got %s", lines[0])
	}
	var meta SessionMetaPayload
	_ = json.Unmarshal(rl.Payload, &meta)
	if meta.ID != sid {
		t.Errorf("synthesized meta id = %q, want %q", meta.ID, sid)
	}
	if meta.Git == nil || meta.Git.Branch != "main" {
		t.Errorf("expected git branch main in synthesized meta, got %+v", meta.Git)
	}
}

func TestHydrate_ErrorWhenNoSessionID(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), ".codex"))
	session := adapter.SessionView{
		EventStream: func() ([]event.Event, error) { return nil, nil },
	}
	_, err := New().Hydrate(context.Background(), session, adapter.HydrateOpts{})
	if err == nil || !strings.Contains(err.Error(), "SessionID") {
		t.Errorf("expected SessionID error, got %v", err)
	}
}

func TestHydrate_PrimeWritesScratchFile(t *testing.T) {
	home := t.TempDir()
	testutil.SetHomeDir(t, home)
	session := adapter.SessionView{
		SessionID: "S1",
		CLI:       "claude", // source != codex → prime semantics
		Cwd:       "/x",
		EventStream: func() ([]event.Event, error) {
			return []event.Event{
				{EventUUID: "E1", SessionID: "S1", Seq: 1, Kind: event.KindUserMessage, Content: json.RawMessage(`{"text":"hi"}`)},
			}, nil
		},
	}
	result, err := New().Hydrate(context.Background(), session, adapter.HydrateOpts{Mode: adapter.RenderModePrime})
	if err != nil {
		t.Fatalf("Hydrate prime: %v", err)
	}
	if result.Mode != adapter.RenderModePrime {
		t.Errorf("Mode: got %q", result.Mode)
	}
	if result.SessionID == "S1" || result.SessionID == "" {
		t.Errorf("expected freshly minted SessionID, got %q", result.SessionID)
	}
	// minted id must be UUIDv7.
	if len(result.SessionID) == 36 && result.SessionID[14] != '7' {
		t.Errorf("minted session id is not UUIDv7: %q", result.SessionID)
	}
	wantDir := filepath.Join(home, ".resleeve", "hydrate")
	if !strings.HasPrefix(result.Path, wantDir+string(filepath.Separator)) {
		t.Errorf("Path %q not under %q", result.Path, wantDir)
	}
	if !strings.HasSuffix(result.Path, ".md") {
		t.Errorf("Path %q should end in .md", result.Path)
	}
	body, _ := os.ReadFile(result.Path)
	if !strings.Contains(string(body), "# Resumed session:") {
		t.Errorf("scratch file missing prime header: %q", string(body))
	}
}

func TestHydrate_PrimeForwardsPlanContent(t *testing.T) {
	home := t.TempDir()
	testutil.SetHomeDir(t, home)
	session := adapter.SessionView{
		SessionID: "S1",
		Cwd:       "/x",
		EventStream: func() ([]event.Event, error) {
			return []event.Event{
				{EventUUID: "E1", SessionID: "S1", Seq: 1, Kind: event.KindUserMessage, Content: json.RawMessage(`{"text":"hi"}`)},
			}, nil
		},
	}
	plan := "## Plan (resleeve)\n\nShip the codex adapter.\n"
	result, err := New().Hydrate(context.Background(), session, adapter.HydrateOpts{Mode: adapter.RenderModePrime, PlanContent: plan})
	if err != nil {
		t.Fatalf("Hydrate prime: %v", err)
	}
	body, _ := os.ReadFile(result.Path)
	if strings.Contains(string(body), "(none captured)") {
		t.Errorf("plan placeholder still present; PlanContent didn't flow through:\n%s", body)
	}
	if !strings.Contains(string(body), "Ship the codex adapter.") {
		t.Errorf("rendered prime missing PlanContent body:\n%s", body)
	}
}

func TestNativeResumeCmd_ReplayReturnsCodexResume(t *testing.T) {
	cmd, args, err := New().NativeResumeCmd(context.Background(),
		adapter.SessionView{SessionID: "S1"},
		adapter.HydrateResult{Mode: adapter.RenderModeReplay, SessionID: "S1"})
	if err != nil {
		t.Fatalf("NativeResumeCmd: %v", err)
	}
	if cmd != "codex" || len(args) != 2 || args[0] != "resume" || args[1] != "S1" {
		t.Errorf("got %q %v, want codex [resume S1]", cmd, args)
	}
}

func TestNativeResumeCmd_PrimeReturnsExecStdin(t *testing.T) {
	cmd, args, err := New().NativeResumeCmd(context.Background(),
		adapter.SessionView{SessionID: "S1"},
		adapter.HydrateResult{Mode: adapter.RenderModePrime, Path: "/tmp/x.md"})
	if err != nil {
		t.Fatalf("NativeResumeCmd prime: %v", err)
	}
	wantCmd, wantFlag := "sh", "-c"
	if runtime.GOOS == "windows" {
		wantCmd, wantFlag = "cmd", "/c"
	}
	if cmd != wantCmd {
		t.Errorf("cmd: got %q, want %q", cmd, wantCmd)
	}
	if len(args) != 2 || args[0] != wantFlag || !strings.Contains(args[1], "codex exec -") || !strings.Contains(args[1], "/tmp/x.md") {
		t.Errorf("args wrong: got %#v", args)
	}
}

func TestNativeResumeCmd_PrimeRejectsMissingPath(t *testing.T) {
	_, _, err := New().NativeResumeCmd(context.Background(),
		adapter.SessionView{SessionID: "S1"},
		adapter.HydrateResult{Mode: adapter.RenderModePrime})
	if err == nil {
		t.Fatal("expected error for missing Path")
	}
}
