package opencode

import "github.com/google/uuid"

// namespace is the fixed namespace UUID for resleeve's opencode adapter.
// All deterministic event UUIDs derive from a UUIDv5 under this namespace.
var namespace = uuid.NewSHA1(uuid.Nil, []byte("resleeve.dev/v1/opencode"))

// DeterministicEventUUID computes a stable UUIDv5 for an event uniquely
// identified by (sessionID, key). Keying on opencode's own time-sortable
// ids (ses_/msg_/prt_) makes reconcile idempotent: the same row observed
// via the db reader or the SSE stream collapses to one event_uuid under
// the storage layer's INSERT OR IGNORE on (session_id, event_uuid).
//
// Key conventions:
//
//	"session_start"        — SessionTable row
//	<msg_id>               — a message row (user / assistant text)
//	"call:"+<callID>       — a tool part's call
//	"result:"+<callID>     — a tool part's result
//	<prt_id>               — any other part rendered as a system event
func DeterministicEventUUID(sessionID, key string) string {
	return uuid.NewSHA1(namespace, []byte(sessionID+"|"+key)).String()
}
