package opencode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// captureEvents returns a small captured stream with native payloads,
// shaped like what the db reader produces.
func captureEvents() []event.Event {
	ts := time.UnixMilli(1000).UTC()
	start := event.Event{
		EventUUID: DeterministicEventUUID("ses_1", "session_start"),
		SessionID: "ses_1",
		Seq:       ts.UnixNano() - 1,
		Timestamp: ts,
		Kind:      event.KindSessionStart,
		Content:   json.RawMessage(`{"cli":"opencode","cwd":"/work/p","model":"anthropic/claude","title":"hi"}`),
		Vendor: event.Vendor{
			Name:          Name,
			Version:       "0.4.0",
			NativePayload: json.RawMessage(`{"id":"ses_1","slug":"hi","directory":"/work/p","title":"hi","version":"0.4.0","time":{"created":1000,"updated":1000}}`),
		},
	}
	// The message event carries a {info, parts} envelope: the full
	// SessionV1.Info plus the message's text part(s), exactly as the db
	// reader now emits (ids re-attached out of the hoisted columns).
	user := event.Event{
		EventUUID: DeterministicEventUUID("ses_1", "msg_1"),
		SessionID: "ses_1",
		Seq:       ts.UnixNano() + 10,
		Timestamp: ts,
		Kind:      event.KindUserMessage,
		Content:   json.RawMessage(`{"text":"port this","gen_ai.system":"opencode"}`),
		Vendor: event.Vendor{
			Name: Name,
			NativePayload: json.RawMessage(`{"info":{"id":"msg_1","sessionID":"ses_1","role":"user","time":{"created":1100}},` +
				`"parts":[{"id":"prt_t","sessionID":"ses_1","messageID":"msg_1","type":"text","text":"port this"}]}`),
		},
	}
	call := event.Event{
		EventUUID: DeterministicEventUUID("ses_1", "call:c1"),
		SessionID: "ses_1",
		Seq:       ts.UnixNano() + 20,
		Timestamp: ts,
		Kind:      event.KindToolCall,
		Content:   json.RawMessage(`{"tool":{"id":"c1","name":"bash"}}`),
		Vendor: event.Vendor{
			Name:          Name,
			NativePayload: json.RawMessage(`{"id":"prt_1","sessionID":"ses_1","messageID":"msg_1","type":"tool","callID":"c1","tool":"bash","state":{"status":"completed","input":{},"output":"ok","title":"bash","metadata":{},"time":{"start":1,"end":2}}}`),
		},
	}
	return []event.Event{start, user, call}
}

func TestToNative_ReplayEmitsImportJSON(t *testing.T) {
	body, err := New().ToNative(context.Background(), captureEvents(), adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative replay: %v", err)
	}

	var file ImportFile
	if err := json.Unmarshal(body, &file); err != nil {
		t.Fatalf("import JSON did not parse: %v\n%s", err, body)
	}

	// info.id must be the captured session id (import preserves it).
	if got := importSessionID(body); got != "ses_1" {
		t.Errorf("info.id: got %q, want ses_1", got)
	}
	if len(file.Messages) != 1 {
		t.Fatalf("messages: got %d, want 1", len(file.Messages))
	}
	// The message must carry a decodable info with role.
	var mi struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(file.Messages[0].Info, &mi); err != nil {
		t.Fatalf("message info did not parse: %v", err)
	}
	if mi.Role != "user" {
		t.Errorf("message role: got %q, want user", mi.Role)
	}
	// Both the text part and the tool part must be attached to the message,
	// each a complete SessionV1.Part (hoisted ids re-attached).
	if len(file.Messages[0].Parts) != 2 {
		t.Fatalf("parts: got %d, want 2 (text + tool)\n%s", len(file.Messages[0].Parts), body)
	}
	var sawText, sawTool bool
	for _, raw := range file.Messages[0].Parts {
		var pt struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
			MessageID string `json:"messageID"`
			Type      string `json:"type"`
			Text      string `json:"text"`
			CallID    string `json:"callID"`
		}
		if err := json.Unmarshal(raw, &pt); err != nil {
			t.Fatalf("part did not parse: %v", err)
		}
		if pt.ID == "" || pt.SessionID == "" || pt.MessageID == "" {
			t.Errorf("part missing hoisted ids: %s", raw)
		}
		switch pt.Type {
		case "text":
			sawText = pt.Text == "port this"
		case "tool":
			sawTool = pt.CallID == "c1"
		}
	}
	if !sawText {
		t.Errorf("text part did not round-trip into message parts\n%s", body)
	}
	if !sawTool {
		t.Errorf("tool part missing from message parts\n%s", body)
	}
}

func TestToNative_ReplayNoSessionStartSynthesizesInfo(t *testing.T) {
	events := []event.Event{{
		EventUUID: "e1",
		SessionID: "ses_z",
		Seq:       1,
		Timestamp: time.Now(),
		Kind:      event.KindUserMessage,
		Content:   json.RawMessage(`{"text":"hi"}`),
		Vendor:    event.Vendor{Name: Name, Version: "0.4.0", NativePayload: json.RawMessage(`{"id":"msg_z","sessionID":"ses_z","role":"user","time":{"created":1}}`)},
	}}
	body, err := New().ToNative(context.Background(), events, adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative: %v", err)
	}
	if got := importSessionID(body); got != "ses_z" {
		t.Errorf("synthesized info.id: got %q, want ses_z", got)
	}
}

func TestToNative_PrimeAndAuto(t *testing.T) {
	// Prime renders markdown.
	body, err := New().ToNative(context.Background(), captureEvents(), adapter.RenderModePrime)
	if err != nil {
		t.Fatalf("ToNative prime: %v", err)
	}
	if !strings.Contains(string(body), "# Resumed session:") {
		t.Errorf("prime output missing markdown header: %q", body)
	}

	// Auto picks replay (valid import JSON).
	autoBody, err := New().ToNative(context.Background(), captureEvents(), adapter.RenderModeAuto)
	if err != nil {
		t.Fatalf("ToNative auto: %v", err)
	}
	var f ImportFile
	if err := json.Unmarshal(autoBody, &f); err != nil {
		t.Errorf("auto mode should produce import JSON (replay), got: %v", err)
	}
}

func TestToNative_UnknownModeErrors(t *testing.T) {
	_, err := New().ToNative(context.Background(), nil, adapter.RenderMode("bogus"))
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}
