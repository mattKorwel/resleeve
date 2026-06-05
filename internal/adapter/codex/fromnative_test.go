package codex

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

func decodeContent(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode content: %v (%s)", err, string(raw))
	}
	return m
}

// --- hook path ---

func TestFromHook(t *testing.T) {
	a := New()
	ctx := context.Background()

	tests := []struct {
		name      string
		raw       string
		wantKinds []event.Kind
		check     func(t *testing.T, evs []event.Event)
	}{
		{
			name:      "SessionStart",
			raw:       `{"session_id":"S1","hook_event_name":"SessionStart","cwd":"/home/u/proj","model":"gpt-5","transcript_path":"/x/r.jsonl","source":"startup","permission_mode":"on-request"}`,
			wantKinds: []event.Kind{event.KindSessionStart},
			check: func(t *testing.T, evs []event.Event) {
				c := decodeContent(t, evs[0].Content)
				if c["model"] != "gpt-5" {
					t.Errorf("model: got %v", c["model"])
				}
				if c["transcript_path"] != "/x/r.jsonl" {
					t.Errorf("transcript_path: got %v", c["transcript_path"])
				}
			},
		},
		{
			name:      "SessionStart-null-transcript",
			raw:       `{"session_id":"S1","hook_event_name":"SessionStart","cwd":"/p","transcript_path":null}`,
			wantKinds: []event.Kind{event.KindSessionStart},
			check: func(t *testing.T, evs []event.Event) {
				c := decodeContent(t, evs[0].Content)
				if c["transcript_path"] != "" {
					t.Errorf("expected empty transcript_path for null, got %v", c["transcript_path"])
				}
			},
		},
		{
			name:      "UserPromptSubmit",
			raw:       `{"session_id":"S1","hook_event_name":"UserPromptSubmit","turn_id":"T1","cwd":"/p","prompt":"hello"}`,
			wantKinds: []event.Kind{event.KindUserMessage},
			check: func(t *testing.T, evs []event.Event) {
				c := decodeContent(t, evs[0].Content)
				if c["text"] != "hello" {
					t.Errorf("text: got %v", c["text"])
				}
				if evs[0].TurnID == nil || *evs[0].TurnID != "T1" {
					t.Errorf("turn id not propagated: %v", evs[0].TurnID)
				}
			},
		},
		{
			name:      "PostToolUse",
			raw:       `{"session_id":"S1","hook_event_name":"PostToolUse","turn_id":"T1","cwd":"/p","tool_name":"shell","tool_use_id":"call_1","tool_input":{"cmd":"ls"},"tool_response":"ok"}`,
			wantKinds: []event.Kind{event.KindToolCall, event.KindToolResult},
			check: func(t *testing.T, evs []event.Event) {
				if evs[0].Seq >= evs[1].Seq {
					t.Errorf("expected call seq < result seq")
				}
			},
		},
		{
			name:      "Stop",
			raw:       `{"session_id":"S1","hook_event_name":"Stop","cwd":"/p","stop_hook_active":false,"last_assistant_message":"done"}`,
			wantKinds: []event.Kind{event.KindSessionEnd},
			check: func(t *testing.T, evs []event.Event) {
				c := decodeContent(t, evs[0].Content)
				if c["last_assistant_message"] != "done" {
					t.Errorf("last_assistant_message: got %v", c["last_assistant_message"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs, err := a.FromNative(ctx, []byte(tt.raw), adapter.Source{Kind: adapter.SourceHook})
			if err != nil {
				t.Fatalf("FromNative: %v", err)
			}
			if len(evs) != len(tt.wantKinds) {
				t.Fatalf("got %d events, want %d", len(evs), len(tt.wantKinds))
			}
			for i, k := range tt.wantKinds {
				if evs[i].Kind != k {
					t.Errorf("event[%d].Kind = %q, want %q", i, evs[i].Kind, k)
				}
				if len(evs[i].Vendor.NativePayload) == 0 {
					t.Errorf("event[%d] missing native payload", i)
				}
			}
			if tt.check != nil {
				tt.check(t, evs)
			}
		})
	}
}

func TestFromHook_MissingSessionID(t *testing.T) {
	_, err := New().FromNative(context.Background(), []byte(`{"hook_event_name":"Stop"}`), adapter.Source{Kind: adapter.SourceHook})
	if err == nil {
		t.Fatal("expected error for missing session_id")
	}
}

// --- rollout JSONL path ---

func TestFromJSONL_SessionMeta(t *testing.T) {
	line := `{"timestamp":"2026-06-04T03:17:25.909Z","type":"session_meta","payload":{"id":"019e90a2-c115-7523-b5a3-50c024b3ba14","cwd":"/home/u/proj","cli_version":"0.137.0","source":"cli","git":{"branch":"dev","commit_hash":"abc"}}}`
	evs, err := New().FromNative(context.Background(), []byte(line), adapter.Source{Kind: adapter.SourceJSONL})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != event.KindSessionStart {
		t.Fatalf("want 1 session_start, got %+v", evs)
	}
	if evs[0].SessionID != "019e90a2-c115-7523-b5a3-50c024b3ba14" {
		t.Errorf("session id from in-file id not used: %q", evs[0].SessionID)
	}
	c := decodeContent(t, evs[0].Content)
	if c["git_branch"] != "dev" {
		t.Errorf("git branch: got %v, want dev", c["git_branch"])
	}
	if c["cli_version"] != "0.137.0" {
		t.Errorf("cli_version: got %v", c["cli_version"])
	}
	// model must NOT come from session_meta.
	if _, ok := c["model"]; ok {
		t.Errorf("session_meta should not carry a model")
	}
}

func TestFromJSONL_ModelFromTurnContext(t *testing.T) {
	line := `{"timestamp":"2026-06-04T03:17:26.0Z","type":"turn_context","payload":{"turn_id":"T1","cwd":"/home/u/proj","model":"gpt-5-codex","approval_policy":"on-request"}}`
	evs, err := New().FromNative(context.Background(), []byte(line), adapter.Source{Kind: adapter.SourceJSONL, SessionHint: "S1"})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != event.KindSystem {
		t.Fatalf("want 1 system event, got %+v", evs)
	}
	c := decodeContent(t, evs[0].Content)
	if c["model"] != "gpt-5-codex" {
		t.Errorf("model from turn_context: got %v", c["model"])
	}
	if c["gen_ai.request.model"] != "gpt-5-codex" {
		t.Errorf("gen_ai.request.model: got %v", c["gen_ai.request.model"])
	}
}

func TestFromJSONL_ResponseItems(t *testing.T) {
	tests := []struct {
		name string
		line string
		kind event.Kind
		want func(t *testing.T, c map[string]any)
	}{
		{
			// A response_item message(role=user) is recorded as a SYSTEM
			// event, not a user message: it is a superset of the prompt
			// (carries injected context the user never typed) and the
			// canonical user-message source is event_msg user_message.
			name: "user message response_item is system, not user",
			line: `{"timestamp":"2026-06-04T03:17:27Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi there"}]}}`,
			kind: event.KindSystem,
			want: func(t *testing.T, c map[string]any) {
				if c["resleeve.system.kind"] != "message:user" {
					t.Errorf("system kind: got %v", c["resleeve.system.kind"])
				}
				if c["text"] != "hi there" {
					t.Errorf("text: got %v", c["text"])
				}
			},
		},
		{
			name: "assistant message",
			line: `{"timestamp":"2026-06-04T03:17:28Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello back"}]}}`,
			kind: event.KindAssistantMessage,
			want: func(t *testing.T, c map[string]any) {
				if c["text"] != "hello back" {
					t.Errorf("text: got %v", c["text"])
				}
			},
		},
		{
			name: "function call",
			line: `{"timestamp":"2026-06-04T03:17:29Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_x"}}`,
			kind: event.KindToolCall,
			want: func(t *testing.T, c map[string]any) {
				tool := c["tool"].(map[string]any)
				if tool["name"] != "exec_command" || tool["id"] != "call_x" {
					t.Errorf("tool: got %v", tool)
				}
			},
		},
		{
			name: "function call output",
			line: `{"timestamp":"2026-06-04T03:17:30Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_x","output":"done"}}`,
			kind: event.KindToolResult,
			want: func(t *testing.T, c map[string]any) {
				tool := c["tool"].(map[string]any)
				if tool["call_id"] != "call_x" {
					t.Errorf("call_id: got %v", tool["call_id"])
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs, err := New().FromNative(context.Background(), []byte(tt.line), adapter.Source{Kind: adapter.SourceJSONL, SessionHint: "S1"})
			if err != nil {
				t.Fatalf("FromNative: %v", err)
			}
			if len(evs) != 1 || evs[0].Kind != tt.kind {
				t.Fatalf("want 1 %s, got %+v", tt.kind, evs)
			}
			if evs[0].SessionID != "S1" {
				t.Errorf("session hint not applied: %q", evs[0].SessionID)
			}
			if len(evs[0].Vendor.NativePayload) == 0 {
				t.Errorf("missing native payload")
			}
			tt.want(t, decodeContent(t, evs[0].Content))
		})
	}
}

func TestFromJSONL_EventMsgUserMessage(t *testing.T) {
	line := `{"timestamp":"2026-06-04T03:17:31Z","type":"event_msg","payload":{"type":"user_message","message":"exit"}}`
	evs, err := New().FromNative(context.Background(), []byte(line), adapter.Source{Kind: adapter.SourceJSONL, SessionHint: "S1"})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != event.KindUserMessage {
		t.Fatalf("want 1 user_message, got %+v", evs)
	}
}

func TestFromJSONL_EventMsgNonContentDropped(t *testing.T) {
	line := `{"timestamp":"2026-06-04T03:17:32Z","type":"event_msg","payload":{"type":"token_count","info":null}}`
	evs, err := New().FromNative(context.Background(), []byte(line), adapter.Source{Kind: adapter.SourceJSONL, SessionHint: "S1"})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("non-content event_msg should be dropped, got %+v", evs)
	}
}

func TestFromJSONL_Compacted(t *testing.T) {
	line := `{"timestamp":"2026-06-04T03:17:33Z","type":"compacted","payload":{"message":"summary"}}`
	evs, err := New().FromNative(context.Background(), []byte(line), adapter.Source{Kind: adapter.SourceJSONL, SessionHint: "S1"})
	if err != nil {
		t.Fatalf("FromNative: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != event.KindSystem {
		t.Fatalf("want 1 system (compacted), got %+v", evs)
	}
}

// TestUUIDv7RoundTrip verifies a session_meta UUIDv7 id is preserved
// verbatim from the rollout line into the event session id, and that the
// deterministic event UUID is stable across calls.
func TestUUIDv7RoundTrip(t *testing.T) {
	const id = "019e90a2-c115-7523-b5a3-50c024b3ba14"
	line := `{"timestamp":"2026-06-04T03:17:25Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"/p"}}`
	evs, _ := New().FromNative(context.Background(), []byte(line), adapter.Source{Kind: adapter.SourceJSONL})
	if evs[0].SessionID != id {
		t.Fatalf("session id round-trip failed: %q", evs[0].SessionID)
	}
	if evs[0].EventUUID != DeterministicEventUUID(id, "session_start") {
		t.Errorf("event uuid not deterministic")
	}
	got1 := DeterministicEventUUID(id, "session_start")
	got2 := DeterministicEventUUID(id, "session_start")
	if got1 != got2 {
		t.Errorf("deterministic uuid not stable: %q != %q", got1, got2)
	}
}

// TestHookAndReconcileDedup verifies the live PostToolUse hook and the
// reconciled function_call/output produce the SAME deterministic UUIDs
// (keyed on call_id), so INSERT OR IGNORE dedups them.
func TestHookAndReconcileDedup(t *testing.T) {
	a := New()
	ctx := context.Background()
	hookRaw := `{"session_id":"S1","hook_event_name":"PostToolUse","cwd":"/p","tool_name":"shell","tool_use_id":"call_z","tool_input":{},"tool_response":"r"}`
	hookEvs, _ := a.FromNative(ctx, []byte(hookRaw), adapter.Source{Kind: adapter.SourceHook})

	callLine := `{"timestamp":"2026-06-04T03:17:29Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}","call_id":"call_z"}}`
	outLine := `{"timestamp":"2026-06-04T03:17:30Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_z","output":"r"}}`
	callEvs, _ := a.FromNative(ctx, []byte(callLine), adapter.Source{Kind: adapter.SourceJSONL, SessionHint: "S1"})
	outEvs, _ := a.FromNative(ctx, []byte(outLine), adapter.Source{Kind: adapter.SourceJSONL, SessionHint: "S1"})

	if hookEvs[0].EventUUID != callEvs[0].EventUUID {
		t.Errorf("tool_call UUID mismatch hook vs reconcile: %s vs %s", hookEvs[0].EventUUID, callEvs[0].EventUUID)
	}
	if hookEvs[1].EventUUID != outEvs[0].EventUUID {
		t.Errorf("tool_result UUID mismatch hook vs reconcile: %s vs %s", hookEvs[1].EventUUID, outEvs[0].EventUUID)
	}
}

// TestUserPromptDedup verifies the three representations of a single user
// prompt reconcile to ONE event:
//   - the UserPromptSubmit hook and the rollout event_msg user_message
//     produce the SAME deterministic UUID (keyed on prompt text), so
//     INSERT OR IGNORE dedups them into a single KindUserMessage; and
//   - the rollout response_item message(role=user) is recorded as a
//     SYSTEM event (distinct UUID), not a second user message.
func TestUserPromptDedup(t *testing.T) {
	a := New()
	ctx := context.Background()
	const prompt = "do the thing"

	hookRaw := `{"session_id":"S1","hook_event_name":"UserPromptSubmit","turn_id":"T1","cwd":"/p","prompt":"` + prompt + `"}`
	hookEvs, _ := a.FromNative(ctx, []byte(hookRaw), adapter.Source{Kind: adapter.SourceHook})

	emLine := `{"timestamp":"2026-06-04T03:17:27.5Z","type":"event_msg","payload":{"type":"user_message","message":"` + prompt + `"}}`
	emEvs, _ := a.FromNative(ctx, []byte(emLine), adapter.Source{Kind: adapter.SourceJSONL, SessionHint: "S1"})

	riLine := `{"timestamp":"2026-06-04T03:17:27Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + prompt + `"}]}}`
	riEvs, _ := a.FromNative(ctx, []byte(riLine), adapter.Source{Kind: adapter.SourceJSONL, SessionHint: "S1"})

	if len(hookEvs) != 1 || hookEvs[0].Kind != event.KindUserMessage {
		t.Fatalf("hook: want 1 user message, got %+v", hookEvs)
	}
	if len(emEvs) != 1 || emEvs[0].Kind != event.KindUserMessage {
		t.Fatalf("event_msg: want 1 user message, got %+v", emEvs)
	}
	if hookEvs[0].EventUUID != emEvs[0].EventUUID {
		t.Errorf("hook vs event_msg user UUID mismatch (no dedup): %s vs %s", hookEvs[0].EventUUID, emEvs[0].EventUUID)
	}
	if len(riEvs) != 1 || riEvs[0].Kind != event.KindSystem {
		t.Fatalf("response_item user: want 1 system event, got %+v", riEvs)
	}
	if riEvs[0].EventUUID == hookEvs[0].EventUUID {
		t.Errorf("response_item user must not collide with canonical user message UUID")
	}
}
