package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// TestToNativeReplay_PreservesNativePayloadByteForByte verifies that
// replay output prefers vendor.native_payload verbatim and sorts by
// (seq, event_uuid).
func TestToNativeReplay_PreservesNativePayloadByteForByte(t *testing.T) {
	l1 := `{"timestamp":"2026-06-04T03:17:25Z","type":"session_meta","payload":{"id":"S1","cwd":"/p"}}`
	l2 := `{"timestamp":"2026-06-04T03:17:26Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`
	events := []event.Event{
		{EventUUID: "B", Seq: 2, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(l2)}},
		{EventUUID: "A", Seq: 1, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(l1)}},
	}
	out, err := New().ToNative(context.Background(), events, adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), out)
	}
	if lines[0] != l1 || lines[1] != l2 {
		t.Errorf("byte-for-byte / ordering failed:\n%q\n%q", lines[0], lines[1])
	}
}

// TestToNativeReplay_SynthesizesWhenNoPayload verifies the fallback
// synthesizer emits valid session_meta / message lines for events with no
// native payload (e.g. a hook-only session).
func TestToNativeReplay_SynthesizesWhenNoPayload(t *testing.T) {
	ts := time.Date(2026, 6, 4, 3, 17, 25, 0, time.UTC)
	events := []event.Event{
		{
			EventUUID: "A", Seq: 1, SessionID: "S1", Timestamp: ts,
			Kind:    event.KindSessionStart,
			Content: json.RawMessage(`{"cwd":"/p","cli_version":"0.137.0"}`),
		},
		{
			EventUUID: "B", Seq: 2, SessionID: "S1", Timestamp: ts,
			Kind:    event.KindUserMessage,
			Content: json.RawMessage(`{"text":"hello"}`),
		},
	}
	out, err := New().ToNative(context.Background(), events, adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 synthesized lines, got %d: %q", len(lines), out)
	}
	// Each line must be valid RolloutLine JSON.
	for i, l := range lines {
		rl, err := ParseRolloutLine([]byte(l))
		if err != nil {
			t.Fatalf("line %d not valid RolloutLine: %v (%s)", i, err, l)
		}
		if i == 0 && rl.Type != "session_meta" {
			t.Errorf("line 0 type = %q, want session_meta", rl.Type)
		}
	}
	var meta SessionMetaPayload
	rl, _ := ParseRolloutLine([]byte(lines[0]))
	_ = json.Unmarshal(rl.Payload, &meta)
	if meta.ID != "S1" {
		t.Errorf("synthesized session_meta id = %q, want S1", meta.ID)
	}
}

// TestToNativeReplay_SynthesizesThinking verifies a KindThinking event
// with no verbatim native payload is synthesized as a reasoning
// response_item (not silently dropped), and that the synthesized line
// round-trips back through the rollout reasoning parser to the same text.
func TestToNativeReplay_SynthesizesThinking(t *testing.T) {
	ts := time.Date(2026, 6, 4, 3, 17, 25, 0, time.UTC)
	const thought = "I should run git status first."
	events := []event.Event{
		{
			EventUUID: "A", Seq: 1, SessionID: "S1", Timestamp: ts,
			Kind:    event.KindThinking,
			Content: json.RawMessage(`{"text":"` + thought + `","resleeve.thinking":true}`),
		},
	}
	out, err := New().ToNative(context.Background(), events, adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("thinking event should synthesize 1 line, got %d: %q", len(lines), out)
	}
	rl, err := ParseRolloutLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("synthesized thinking line not valid RolloutLine: %v (%s)", err, lines[0])
	}
	if rl.Type != "response_item" {
		t.Errorf("synthesized thinking type = %q, want response_item", rl.Type)
	}
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(rl.Payload, &p)
	if p.Type != "reasoning" {
		t.Errorf("synthesized payload type = %q, want reasoning", p.Type)
	}
	// Round-trip: the rollout reasoning parser must recover the same text.
	if got := reasoningText(rl.Payload); got != thought {
		t.Errorf("reasoning text round-trip = %q, want %q", got, thought)
	}
}

func TestToNative_UnknownMode(t *testing.T) {
	_, err := New().ToNative(context.Background(), nil, adapter.RenderMode("bogus"))
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}
