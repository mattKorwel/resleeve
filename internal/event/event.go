package event

import (
	"encoding/json"
	"time"
)

// Event is one session-event flowing through the firehose.
// Hybrid envelope per docs/design/round-2/04-event-schema.md:
// normalized Content (OTel-GenAI-aligned) plus vendor-faithful
// Vendor.NativePayload for same-CLI reconstruction.
type Event struct {
	EventUUID       string          `json:"event_uuid"`
	SessionID       string          `json:"session_id"`
	Slot            Slot            `json:"slot"`
	Seq             int64           `json:"seq"`
	TurnID          *string         `json:"turn_id,omitempty"`
	ParentEventUUID *string         `json:"parent_event_uuid,omitempty"`
	Timestamp       time.Time       `json:"ts"`
	Kind            Kind            `json:"kind"`
	SchemaVersion   int             `json:"schema_version"`
	Content         json.RawMessage `json:"content"`
	Vendor          Vendor          `json:"vendor"`
}

// Slot is the stable persistent-identity tuple. Sessions are runs;
// slots are what persist across re-sleeves.
type Slot struct {
	Scope     string `json:"scope"`
	AgentName string `json:"agent_name"`
}

// Vendor carries vendor identity plus the raw vendor-faithful payload
// for same-CLI reconstruction. Cross-CLI re-sleeve ignores NativePayload.
type Vendor struct {
	Name          string          `json:"name"`
	Version       string          `json:"version"`
	NativePayload json.RawMessage `json:"native_payload,omitempty"`
}

// Kind discriminates the shape of an Event's Content.
type Kind string

// Known event kinds. Adapters may emit unknown kinds for forward-compat;
// Kind.Valid is purely informational.
const (
	KindUserMessage      Kind = "user_message"
	KindAssistantMessage Kind = "assistant_message"
	KindToolCall         Kind = "tool_call"
	KindToolResult       Kind = "tool_result"
	KindThinking         Kind = "thinking"
	KindSystem           Kind = "system"
	KindAttachment       Kind = "attachment"
	KindSessionStart     Kind = "session_start"
	KindSessionEnd       Kind = "session_end"
	KindError            Kind = "error"
)

// Valid reports whether k is a known event kind. Unknown kinds are
// persisted as-is; Valid is informational only.
func (k Kind) Valid() bool {
	switch k {
	case KindUserMessage, KindAssistantMessage, KindToolCall, KindToolResult,
		KindThinking, KindSystem, KindAttachment, KindSessionStart,
		KindSessionEnd, KindError:
		return true
	}
	return false
}
