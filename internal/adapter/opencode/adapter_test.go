package opencode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// compile-time assertion that *Adapter satisfies the full interface.
var _ adapter.Adapter = (*Adapter)(nil)

func TestInstallUninstall_AreNoOps(t *testing.T) {
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{}); err != nil {
		t.Errorf("InstallBridge should be a no-op (nil), got %v", err)
	}
	if err := a.UninstallBridge(context.Background()); err != nil {
		t.Errorf("UninstallBridge should be a no-op (nil), got %v", err)
	}
}

func TestNativeResumeCmd_Replay(t *testing.T) {
	cmd, args, err := New().NativeResumeCmd(context.Background(),
		adapter.SessionView{SessionID: "ses_1"},
		adapter.HydrateResult{Mode: adapter.RenderModeReplay, SessionID: "ses_1", Path: "/h/ses_1.json"},
	)
	if err != nil {
		t.Fatalf("NativeResumeCmd: %v", err)
	}
	if cmd != "opencode" {
		t.Errorf("cmd: got %q, want opencode", cmd)
	}
	want := []string{"run", "--session", "ses_1"}
	if len(args) != 3 || args[0] != want[0] || args[1] != want[1] || args[2] != want[2] {
		t.Errorf("args: got %v, want %v", args, want)
	}
}

func TestNativeResumeCmd_Prime(t *testing.T) {
	cmd, args, err := New().NativeResumeCmd(context.Background(),
		adapter.SessionView{},
		adapter.HydrateResult{Mode: adapter.RenderModePrime, Path: "/h/x.md"},
	)
	if err != nil {
		t.Fatalf("NativeResumeCmd: %v", err)
	}
	if cmd != "opencode" || len(args) != 2 || args[0] != "run" || args[1] != "/h/x.md" {
		t.Errorf("got %q %v, want opencode [run /h/x.md]", cmd, args)
	}
}

func TestNativeResumeCmd_ReplayMissingIDErrors(t *testing.T) {
	_, _, err := New().NativeResumeCmd(context.Background(),
		adapter.SessionView{},
		adapter.HydrateResult{Mode: adapter.RenderModeReplay},
	)
	if err == nil {
		t.Fatal("expected error when replay result has no SessionID")
	}
}

func TestFromNative_SSEPartFrame(t *testing.T) {
	frame := `{"type":"message.part.updated","properties":{"part":{"id":"prt_1","sessionID":"ses_1","messageID":"msg_1","type":"tool","callID":"c1","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"out"}}}}`
	events, err := New().FromNative(context.Background(), []byte(frame), adapter.Source{Kind: adapter.SourceHook})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	var haveCall, haveResult bool
	for _, e := range events {
		switch e.Kind {
		case event.KindToolCall:
			haveCall = true
		case event.KindToolResult:
			haveResult = true
		}
	}
	if !haveCall || !haveResult {
		t.Errorf("expected tool call+result from part frame, got %d events (call=%v result=%v)", len(events), haveCall, haveResult)
	}
}

func TestFromNative_SSESessionFrame(t *testing.T) {
	frame := `{"type":"session.created","properties":{"info":{"id":"ses_1","slug":"s","directory":"/work/p","title":"t","version":"0.4.0","time":{"created":1000,"updated":1000}}}}`
	events, err := New().FromNative(context.Background(), []byte(frame), adapter.Source{Kind: adapter.SourceHook})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(events) != 1 || events[0].Kind != event.KindSessionStart {
		t.Fatalf("expected one session_start event, got %+v", events)
	}
	if events[0].Slot.Scope != "p" {
		t.Errorf("scope: got %q, want p", events[0].Slot.Scope)
	}
}

func TestFromNative_SSEControlFrameNoEvents(t *testing.T) {
	frame := `{"type":"server.connected","properties":{}}`
	events, err := New().FromNative(context.Background(), []byte(frame), adapter.Source{Kind: adapter.SourceHook})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("control frame should yield no events, got %d", len(events))
	}
}

func TestFromNative_UnsupportedSourceErrors(t *testing.T) {
	_, err := New().FromNative(context.Background(), []byte(`{}`), adapter.Source{Kind: adapter.SourceJSONL})
	if err == nil {
		t.Fatal("expected error for unsupported source kind")
	}
}

// fakeIngester records batches for reconcile assertions.
type fakeIngester struct {
	batches [][]event.Event
}

func (f *fakeIngester) IngestBatch(_ context.Context, _ string, events []event.Event) error {
	cp := make([]event.Event, len(events))
	copy(cp, events)
	f.batches = append(f.batches, cp)
	return nil
}

func TestReconcileOnce_ReadsDBAndBatchesPerSession(t *testing.T) {
	sessions := []sessionRow{{ID: "ses_1", ProjectID: "p", Directory: "/d", Title: "t", Version: "0.4.0", TimeCreated: 1, TimeUpdated: 1}}
	// Hoisted-column layout (ids live in columns, not the data blob).
	messages := []messageRow{{ID: "msg_1", SessionID: "ses_1", TimeCreated: 2, Data: json.RawMessage(`{"role":"user","time":{"created":2}}`)}}
	parts := []partRow{{ID: "prt_1", MessageID: "msg_1", SessionID: "ses_1", TimeCreated: 2, Data: json.RawMessage(`{"type":"text","text":"hi"}`)}}
	dbPath := newFixtureDB(t, sessions, messages, parts)
	t.Setenv("OPENCODE_DB", dbPath)

	ing := &fakeIngester{}
	if err := New().ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(ing.batches) != 1 {
		t.Fatalf("expected 1 per-session batch, got %d", len(ing.batches))
	}
	if len(ing.batches[0]) == 0 || ing.batches[0][0].Kind != event.KindSessionStart {
		t.Errorf("first reconcile event should be session_start, got %+v", ing.batches[0])
	}
}

func TestReconcileOnce_PreSqliteIsNoOp(t *testing.T) {
	// Point OPENCODE_DB at a non-existent absolute path → no db, no error.
	t.Setenv("OPENCODE_DB", "/nonexistent/path/opencode.db")
	ing := &fakeIngester{}
	if err := New().ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("ReconcileOnce should be a no-op when db absent, got %v", err)
	}
	if len(ing.batches) != 0 {
		t.Errorf("expected no batches, got %d", len(ing.batches))
	}
}
