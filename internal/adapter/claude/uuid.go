package claude

import "github.com/google/uuid"

// namespace is the fixed namespace UUID for resleeve's claude adapter.
// All deterministic event UUIDs derive from a UUIDv5 under this namespace.
var namespace = uuid.NewSHA1(uuid.Nil, []byte("resleeve.dev/v1/claude"))

// DeterministicEventUUID computes a stable UUIDv5 for an event uniquely
// identified by (session_id, key). The same (session_id, key) tuple
// always produces the same UUID regardless of which capture path
// observed it — hook (live) or JSONL (reconcile). This is what makes
// dedup at the storage layer work via (session_id, event_uuid) primary
// key + INSERT OR IGNORE.
//
// Key conventions:
//
//	"session_start"            — SessionStart hook
//	"session_end"              — Stop hook
//	"call:"+tool_use_id        — tool_call (PostToolUse hook or assistant JSONL record)
//	"result:"+tool_use_id      — tool_result (PostToolUse hook or user JSONL tool_result block)
//	"user:"+prompt_id          — user_message (UserPromptSubmit hook or user JSONL record)
//	<claude_uuid>              — JSONL-only events (assistant text, thinking blocks, attachments)
func DeterministicEventUUID(sessionID, key string) string {
	return uuid.NewSHA1(namespace, []byte(sessionID+"|"+key)).String()
}
