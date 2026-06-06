package codex

import "github.com/google/uuid"

// namespace is the fixed namespace UUID for resleeve's codex adapter.
// All deterministic event UUIDs derive from a UUIDv5 under this namespace.
var namespace = uuid.NewSHA1(uuid.Nil, []byte("resleeve.dev/v1/codex"))

// DeterministicEventUUID computes a stable UUIDv5 for an event uniquely
// identified by (session_id, key). The same (session_id, key) tuple
// always produces the same UUID regardless of which capture path
// observed it — hook (live) or rollout JSONL (reconcile). This is what
// makes dedup at the storage layer work via (session_id, event_uuid)
// primary key + INSERT OR IGNORE.
//
// Key conventions:
//
//	"session_start"            — session_meta line / SessionStart hook
//	"session_end"              — Stop hook
//	"turn_context:"+turn_id    — turn_context rollout line
//	"call:"+call_id            — function_call response_item / PostToolUse hook
//	"result:"+call_id          — function_call_output response_item / PostToolUse hook
//	"user:"+sha256(text)       — user_message (UserPromptSubmit hook or
//	                             event_msg user_message; same content hash
//	                             so live + reconcile dedup)
//	"msg:user:"+key            — response_item message(role=user); recorded
//	                             as system (superset of the prompt incl.
//	                             injected context), not a duplicate user msg
//	"assistant:"+key           — assistant_message response_item
//	"compacted:"+key           — compaction marker
//	<line-key>                 — fallback for response_item / event_msg lines
func DeterministicEventUUID(sessionID, key string) string {
	return uuid.NewSHA1(namespace, []byte(sessionID+"|"+key)).String()
}

// NewSessionUUIDv7 mints a fresh UUIDv7 — the id format Codex uses for
// session (thread) ids. Used by prime mode when minting a new session id
// and as a last-resort fallback when a hand-written rollout needs an id.
// Falls back to a v4 string if v7 generation ever fails (it won't on a
// healthy host).
func NewSessionUUIDv7() string {
	if v7, err := uuid.NewV7(); err == nil {
		return v7.String()
	}
	return uuid.NewString()
}
