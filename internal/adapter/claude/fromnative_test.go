package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

func TestDeriveScope_EnvOverride(t *testing.T) {
	t.Setenv("RESLEEVE_SCOPE", "monorepo/svc-billing")
	if got := deriveScope("/anywhere/else"); got != "monorepo/svc-billing" {
		t.Errorf("env override: got %q, want %q", got, "monorepo/svc-billing")
	}
}

func TestDeriveScope_EmptyCwdUnknown(t *testing.T) {
	t.Setenv("RESLEEVE_SCOPE", "")
	if got := deriveScope(""); got != "unknown" {
		t.Errorf("empty cwd: got %q, want %q", got, "unknown")
	}
}

func TestDeriveScope_NoMarkerFallbackBase(t *testing.T) {
	t.Setenv("RESLEEVE_SCOPE", "")
	dir := t.TempDir()
	if got := deriveScope(dir); got != filepath.Base(dir) {
		t.Errorf("no marker: got %q, want %q", got, filepath.Base(dir))
	}
}

func TestDeriveScope_MarkerInCwd(t *testing.T) {
	t.Setenv("RESLEEVE_SCOPE", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ScopeMarkerFile), []byte("monorepo\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if got := deriveScope(dir); got != "monorepo" {
		t.Errorf("marker in cwd: got %q, want %q", got, "monorepo")
	}
}

func TestDeriveScope_MarkerInAncestor(t *testing.T) {
	t.Setenv("RESLEEVE_SCOPE", "")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ScopeMarkerFile), []byte("monorepo"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	leaf := filepath.Join(root, "services", "billing")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := "monorepo/services/billing"
	if got := deriveScope(leaf); got != want {
		t.Errorf("marker in ancestor: got %q, want %q", got, want)
	}
}

func TestDeriveScope_EmptyMarkerIgnored(t *testing.T) {
	t.Setenv("RESLEEVE_SCOPE", "")
	dir := t.TempDir()
	// Empty (whitespace-only) marker should be treated as no marker.
	if err := os.WriteFile(filepath.Join(dir, ScopeMarkerFile), []byte("   \n\t\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if got := deriveScope(dir); got != filepath.Base(dir) {
		t.Errorf("empty marker should fall through: got %q, want %q", got, filepath.Base(dir))
	}
}

func TestFromNative_Hook_SessionStart(t *testing.T) {
	a := New()
	raw := []byte(`{"session_id":"S1","hook_event_name":"SessionStart","cwd":"/Users/x/proj","source":"startup"}`)
	events, err := a.FromNative(context.Background(), raw, adapter.Source{Kind: adapter.SourceHook})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	if e.Kind != event.KindSessionStart {
		t.Errorf("kind: got %q, want %q", e.Kind, event.KindSessionStart)
	}
	if e.Slot.Scope != "proj" {
		t.Errorf("scope: got %q, want %q", e.Slot.Scope, "proj")
	}
}

func TestFromNative_Hook_PostToolUse_TwoEvents(t *testing.T) {
	a := New()
	raw := []byte(`{"session_id":"S1","hook_event_name":"PostToolUse","cwd":"/x","tool_name":"Read","tool_use_id":"toolu_AB","tool_input":{"file_path":"/y"},"tool_response":"file body"}`)
	events, err := a.FromNative(context.Background(), raw, adapter.Source{Kind: adapter.SourceHook})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Kind != event.KindToolCall {
		t.Errorf("[0].Kind: got %q, want %q", events[0].Kind, event.KindToolCall)
	}
	if events[1].Kind != event.KindToolResult {
		t.Errorf("[1].Kind: got %q, want %q", events[1].Kind, event.KindToolResult)
	}
}

// Verifies the central invariant: same logical event seen via hook and
// JSONL paths produces the same UUID, so INSERT OR IGNORE dedups.
func TestFromNative_DedupKeyConvergence_ToolUse(t *testing.T) {
	a := New()
	sessionID := "S1"
	toolUseID := "toolu_DEDUP"

	// Hook path: PostToolUse with the tool_use_id.
	hookRaw := []byte(`{"session_id":"` + sessionID + `","hook_event_name":"PostToolUse","cwd":"/x","tool_name":"Read","tool_use_id":"` + toolUseID + `","tool_input":{},"tool_response":""}`)
	hookEvents, err := a.FromNative(context.Background(), hookRaw, adapter.Source{Kind: adapter.SourceHook})
	if err != nil {
		t.Fatalf("hook FromNative: %v", err)
	}

	// JSONL path: an assistant record containing a tool_use block with the same id.
	jsonlLine := []byte(`{"type":"assistant","uuid":"asst-uuid-1","sessionId":"` + sessionID + `","timestamp":"2026-05-12T22:13:04Z","cwd":"/x","gitBranch":"main","version":"2.1.140","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-7","content":[{"type":"tool_use","id":"` + toolUseID + `","name":"Read","input":{"file_path":"/y"}}],"stop_reason":"tool_use"}}`)
	jsonlEvents, err := a.FromNative(context.Background(), jsonlLine, adapter.Source{Kind: adapter.SourceJSONL})
	if err != nil {
		t.Fatalf("jsonl FromNative: %v", err)
	}

	hookCallUUID := ""
	for _, e := range hookEvents {
		if e.Kind == event.KindToolCall {
			hookCallUUID = e.EventUUID
		}
	}
	jsonlCallUUID := ""
	for _, e := range jsonlEvents {
		if e.Kind == event.KindToolCall {
			jsonlCallUUID = e.EventUUID
		}
	}
	if hookCallUUID == "" || jsonlCallUUID == "" {
		t.Fatalf("did not find tool_call from both paths (hook=%q jsonl=%q)", hookCallUUID, jsonlCallUUID)
	}
	if hookCallUUID != jsonlCallUUID {
		t.Errorf("tool_call UUIDs diverge across paths:\n  hook  = %s\n  jsonl = %s", hookCallUUID, jsonlCallUUID)
	}
}

func TestFromNative_JSONL_UserText(t *testing.T) {
	a := New()
	line := []byte(`{"type":"user","uuid":"u1","promptId":"p1","sessionId":"S1","timestamp":"2026-05-12T22:13:04Z","cwd":"/x","version":"2.1.140","message":{"role":"user","content":"fix the auth bug"}}`)
	events, err := a.FromNative(context.Background(), line, adapter.Source{Kind: adapter.SourceJSONL})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != event.KindUserMessage {
		t.Errorf("kind: got %q, want %q", events[0].Kind, event.KindUserMessage)
	}

	// User-message UUID should match what UserPromptSubmit hook would produce for the same promptId.
	hookRaw := []byte(`{"session_id":"S1","hook_event_name":"UserPromptSubmit","cwd":"/x","prompt_id":"p1","prompt":"fix the auth bug"}`)
	hookEvents, err := a.FromNative(context.Background(), hookRaw, adapter.Source{Kind: adapter.SourceHook})
	if err != nil {
		t.Fatalf("hook FromNative: %v", err)
	}
	if len(hookEvents) != 1 {
		t.Fatalf("expected 1 hook event, got %d", len(hookEvents))
	}
	if hookEvents[0].EventUUID != events[0].EventUUID {
		t.Errorf("user_message UUIDs diverge:\n  hook  = %s\n  jsonl = %s",
			hookEvents[0].EventUUID, events[0].EventUUID)
	}
}

func TestFromNative_JSONL_AssistantTurnSplit(t *testing.T) {
	a := New()
	// Assistant turn with text + tool_use + thinking → 3 events, shared turn_id.
	line := []byte(`{"type":"assistant","uuid":"asst-uuid-2","sessionId":"S1","timestamp":"2026-05-12T22:13:04Z","cwd":"/x","version":"2.1.140","message":{"id":"msg_2","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"reading file"},{"type":"tool_use","id":"toolu_X","name":"Read","input":{"path":"/y"}},{"type":"thinking","thinking":"hm","signature":"abc"}],"stop_reason":"tool_use"}}`)
	events, err := a.FromNative(context.Background(), line, adapter.Source{Kind: adapter.SourceJSONL})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	kinds := []event.Kind{events[0].Kind, events[1].Kind, events[2].Kind}
	want := []event.Kind{event.KindAssistantMessage, event.KindToolCall, event.KindThinking}
	for i := range kinds {
		if kinds[i] != want[i] {
			t.Errorf("events[%d].Kind: got %q, want %q", i, kinds[i], want[i])
		}
	}
	// All three should share a turn_id.
	if events[0].TurnID == nil || events[1].TurnID == nil || events[2].TurnID == nil {
		t.Fatalf("turn_id missing on some events")
	}
	if *events[0].TurnID != *events[1].TurnID || *events[1].TurnID != *events[2].TurnID {
		t.Errorf("turn_id diverges across events: %v / %v / %v",
			*events[0].TurnID, *events[1].TurnID, *events[2].TurnID)
	}
}

func TestFromNative_JSONL_UserToolResult(t *testing.T) {
	a := New()
	line := []byte(`{"type":"user","uuid":"u2","sessionId":"S1","timestamp":"2026-05-12T22:13:04Z","cwd":"/x","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_DEDUP","content":"file body"}]}}`)
	events, err := a.FromNative(context.Background(), line, adapter.Source{Kind: adapter.SourceJSONL})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != event.KindToolResult {
		t.Errorf("kind: got %q, want %q", events[0].Kind, event.KindToolResult)
	}

	// Should match hook-side PostToolUse result UUID for the same tool_use_id.
	hookRaw := []byte(`{"session_id":"S1","hook_event_name":"PostToolUse","cwd":"/x","tool_use_id":"toolu_DEDUP","tool_name":"Read","tool_input":{},"tool_response":"file body"}`)
	hookEvents, _ := a.FromNative(context.Background(), hookRaw, adapter.Source{Kind: adapter.SourceHook})
	var hookResultUUID string
	for _, e := range hookEvents {
		if e.Kind == event.KindToolResult {
			hookResultUUID = e.EventUUID
		}
	}
	if events[0].EventUUID != hookResultUUID {
		t.Errorf("tool_result UUIDs diverge:\n  hook  = %s\n  jsonl = %s",
			hookResultUUID, events[0].EventUUID)
	}
}

func TestDeterministicUUID_Stability(t *testing.T) {
	// Same (session, key) must always produce the same UUID.
	a := DeterministicEventUUID("S1", "session_start")
	b := DeterministicEventUUID("S1", "session_start")
	if a != b {
		t.Errorf("UUID not stable: %s vs %s", a, b)
	}

	// Different sessions → different UUIDs.
	c := DeterministicEventUUID("S2", "session_start")
	if a == c {
		t.Errorf("expected distinct UUIDs across sessions, got same: %s", a)
	}
}

// Sanity: ParseRecord round-trips a representative line.
func TestParseRecord_Smoke(t *testing.T) {
	line := []byte(`{"type":"user","uuid":"u","sessionId":"S","timestamp":"2026-05-12T22:13:04Z","cwd":"/x","version":"2.1.140","message":{"role":"user","content":"hi"}}`)
	rec, err := ParseRecord(line)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if rec.Type != "user" || rec.UUID != "u" || rec.SessionID != "S" {
		t.Errorf("ParseRecord mismatch: %+v", rec)
	}
	var msg userMessage
	if err := json.Unmarshal(rec.Message, &msg); err != nil {
		t.Fatalf("inner unmarshal: %v", err)
	}
	if msg.Role != "user" {
		t.Errorf("inner role: got %q, want %q", msg.Role, "user")
	}
}
