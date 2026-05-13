package event

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventRoundTrip(t *testing.T) {
	turnID := "01HZT001"
	ts, _ := time.Parse(time.RFC3339Nano, "2026-05-12T22:13:04.221Z")
	e := Event{
		EventUUID:     "01HZ001",
		SessionID:     "01HZW001",
		Slot:          Slot{Scope: "auth-rewrite", AgentName: "claude-3"},
		Seq:           4,
		TurnID:        &turnID,
		Timestamp:     ts,
		Kind:          KindToolCall,
		SchemaVersion: 1,
		Content:       json.RawMessage(`{"tool":{"id":"toolu_AB","name":"Read"}}`),
		Vendor:        Vendor{Name: "claude_code", Version: "2.1.140"},
	}

	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Event
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.EventUUID != e.EventUUID {
		t.Errorf("event_uuid: got %q, want %q", got.EventUUID, e.EventUUID)
	}
	if got.Kind != e.Kind {
		t.Errorf("kind: got %q, want %q", got.Kind, e.Kind)
	}
	if got.TurnID == nil || *got.TurnID != *e.TurnID {
		t.Errorf("turn_id: got %v, want %v", got.TurnID, e.TurnID)
	}
	if got.ParentEventUUID != nil {
		t.Errorf("parent_event_uuid should be nil for this event, got %v", got.ParentEventUUID)
	}
	if !got.Timestamp.Equal(e.Timestamp) {
		t.Errorf("timestamp: got %v, want %v", got.Timestamp, e.Timestamp)
	}
}

func TestKindValid(t *testing.T) {
	cases := []struct {
		kind Kind
		want bool
	}{
		{KindToolCall, true},
		{KindAssistantMessage, true},
		{KindError, true},
		{Kind("not_a_kind"), false},
		{Kind(""), false},
	}
	for _, tc := range cases {
		if got := tc.kind.Valid(); got != tc.want {
			t.Errorf("Kind(%q).Valid() = %v, want %v", tc.kind, got, tc.want)
		}
	}
}
