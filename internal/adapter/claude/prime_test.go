package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

func TestPrime_EmitsExpectedSections(t *testing.T) {
	a := New()
	ts := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	events := []event.Event{
		mkUser("E1", "S1", 1, ts, "fix the auth bug"),
		mkAssistant("E2", "S1", 2, ts.Add(time.Second), "looking at auth.go"),
		mkToolCall("E3", "S1", 3, ts.Add(2*time.Second), "T1", "Read", `{"file_path":"/x/auth.go"}`),
		mkToolResult("E4", "S1", 4, ts.Add(3*time.Second), "T1", "success", `"package auth"`),
		mkUser("E5", "S1", 5, ts.Add(4*time.Second), "now run the tests"),
	}
	view := adapter.SessionView{
		SessionID:  "S1",
		CLI:        Name,
		CLIVersion: "2.1.140",
		Cwd:        "/x",
		GitBranch:  "main",
		Model:      "claude-opus-4-7",
		StartedAt:  ts,
	}
	body, err := a.toNativePrime(view, events)
	if err != nil {
		t.Fatalf("toNativePrime: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"# Resumed session: S1",
		"## Context",
		"- Source CLI: claude 2.1.140",
		"- Working directory: /x",
		"- Git branch: main",
		"- Model: claude-opus-4-7",
		"## Plan",
		"(none captured)",
		"## Recent activity",
		"user: fix the auth bug",
		"assistant: looking at auth.go",
		"called Read(",
		"## Next",
		"now run the tests",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in prime output:\n---\n%s", want, s)
		}
	}
}

func TestPrime_DropsThinkingEvents(t *testing.T) {
	a := New()
	events := []event.Event{
		mkUser("E1", "S1", 1, time.Now(), "hi"),
		{
			EventUUID: "T1",
			SessionID: "S1",
			Seq:       2,
			Kind:      event.KindThinking,
			Content:   json.RawMessage(`{"text":"SECRET INTERNAL THOUGHT"}`),
		},
		mkAssistant("E2", "S1", 3, time.Now().Add(time.Second), "hello"),
	}
	body, err := a.toNativePrime(adapter.SessionView{SessionID: "S1", CLI: Name}, events)
	if err != nil {
		t.Fatalf("toNativePrime: %v", err)
	}
	if strings.Contains(string(body), "SECRET INTERNAL THOUGHT") {
		t.Errorf("thinking content leaked into prime output:\n%s", body)
	}
	if strings.Contains(string(body), "thinking") {
		// We deliberately don't even render a placeholder for thinking.
		t.Errorf("thinking kind leaked into prime output:\n%s", body)
	}
}

func TestPrime_HandlesUnpairedToolCalls(t *testing.T) {
	a := New()
	events := []event.Event{
		mkToolCall("E1", "S1", 1, time.Now(), "T1", "Read", `{"file_path":"/x"}`),
		// no matching tool_result for T1
	}
	body, err := a.toNativePrime(adapter.SessionView{SessionID: "S1", CLI: Name}, events)
	if err != nil {
		t.Fatalf("toNativePrime: %v", err)
	}
	if !strings.Contains(string(body), "(no result captured)") {
		t.Errorf("unpaired tool call should render '(no result captured)':\n%s", body)
	}
}

func TestPrime_RespectsLengthCap(t *testing.T) {
	a := New()
	// Build many large user messages so we exceed the default 16KB cap.
	ts := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	big := strings.Repeat("x", 150) // each line ~190 bytes rendered
	var events []event.Event
	for i := 0; i < 150; i++ {
		events = append(events, mkUser(
			fmt.Sprintf("E%d", i),
			"S1",
			int64(i+1),
			ts.Add(time.Duration(i)*time.Second),
			big+fmt.Sprintf(" %d", i),
		))
	}
	body, err := a.toNativePrime(adapter.SessionView{SessionID: "S1", CLI: Name}, events)
	if err != nil {
		t.Fatalf("toNativePrime: %v", err)
	}
	s := string(body)
	if len(s) > 17*1024 {
		// Allow a tiny grace for the trailing "Next" section.
		t.Errorf("prime output too large (%d bytes); cap not enforced", len(s))
	}
	if !strings.Contains(s, "earlier events omitted") {
		t.Errorf("expected truncation note in prime output:\n%s", s)
	}
}

// --- builders ---

func mkUser(uuid, sid string, seq int64, ts time.Time, text string) event.Event {
	return event.Event{
		EventUUID: uuid,
		SessionID: sid,
		Seq:       seq,
		Timestamp: ts,
		Kind:      event.KindUserMessage,
		Content:   mustMarshal(map[string]any{"text": text}),
	}
}

func mkAssistant(uuid, sid string, seq int64, ts time.Time, text string) event.Event {
	return event.Event{
		EventUUID: uuid,
		SessionID: sid,
		Seq:       seq,
		Timestamp: ts,
		Kind:      event.KindAssistantMessage,
		Content:   mustMarshal(map[string]any{"text": text}),
	}
}

func mkToolCall(uuid, sid string, seq int64, ts time.Time, toolID, name, argsJSON string) event.Event {
	return event.Event{
		EventUUID: uuid,
		SessionID: sid,
		Seq:       seq,
		Timestamp: ts,
		Kind:      event.KindToolCall,
		Content: mustMarshal(map[string]any{
			"tool": map[string]any{
				"id":   toolID,
				"name": name,
				"args": json.RawMessage(argsJSON),
			},
		}),
	}
}

func mkToolResult(uuid, sid string, seq int64, ts time.Time, callID, status, bodyJSON string) event.Event {
	return event.Event{
		EventUUID: uuid,
		SessionID: sid,
		Seq:       seq,
		Timestamp: ts,
		Kind:      event.KindToolResult,
		Content: mustMarshal(map[string]any{
			"tool": map[string]any{
				"call_id": callID,
				"status":  status,
				"result":  json.RawMessage(bodyJSON),
			},
		}),
	}
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
