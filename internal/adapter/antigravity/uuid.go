package antigravity

import (
	"strconv"

	"github.com/google/uuid"
)

// namespace is the fixed namespace UUID for resleeve's antigravity adapter.
// All deterministic event UUIDs derive from a UUIDv5 under this namespace.
// A distinct namespace from the claude adapter keeps the two adapters'
// UUID spaces from ever colliding.
var namespace = uuid.NewSHA1(uuid.Nil, []byte("resleeve.dev/v1/antigravity"))

// DeterministicEventUUID computes a stable UUIDv5 for an event uniquely
// identified by (conversationID, key). Because reconcile is the ONLY capture
// path for antigravity (no live hooks), the only requirement is idempotency:
// re-running ReconcileOnce over the same db must mint the same UUIDs so the
// storage layer's (session_id, event_uuid) primary key + INSERT OR IGNORE
// dedups cleanly.
//
// Key conventions:
//
//	"session_start"   — synthesized start (conversation id + cwd)
//	"step:"+idx       — one decoded step row
func DeterministicEventUUID(conversationID, key string) string {
	return uuid.NewSHA1(namespace, []byte(conversationID+"|"+key)).String()
}

// stepKey is the deterministic-UUID key for a step at the given index.
func stepKey(idx int64) string {
	return "step:" + strconv.FormatInt(idx, 10)
}
