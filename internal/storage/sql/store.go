package sql

import (
	"context"
	"errors"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/event"
	"github.com/mattkorwel/resleeve/internal/memory"
)

// ErrNotFound is returned when a queried row does not exist.
var ErrNotFound = errors.New("storage: not found")

// SessionStatus is the lifecycle state of a session.
type SessionStatus string

const (
	SessionStatusActive  SessionStatus = "active"
	SessionStatusEnded   SessionStatus = "ended"
	SessionStatusFailed  SessionStatus = "failed"
	SessionStatusPartial SessionStatus = "partial"
)

// Session is the persistent record for one CLI run.
type Session struct {
	ID         string
	Slot       event.Slot
	CLI        string
	CLIVersion string
	Cwd        string
	GitBranch  string
	Model      string
	StartedAt  time.Time
	EndedAt    *time.Time
	EventCount int64
	Status     SessionStatus
	ExitStatus *int
}

// SessionFilter narrows SessionStore.List.
type SessionFilter struct {
	Scope     string        // exact match; empty = any
	AgentName string        // exact match; empty = any
	Status    SessionStatus // empty = any
	Since     *time.Time    // started_at >= since
	Limit     int           // 0 ⇒ default 50; >0 capped at 500; <0 ⇒ unlimited
}

// SlotState is the durable view of one slot.
type SlotState struct {
	Slot             event.Slot
	LastHeartbeatAt  time.Time
	CurrentSessionID string // empty if quiescent
}

// EventSearchHit is one result row from a cross-session search.
type EventSearchHit struct {
	Event   event.Event
	Snippet string // best-effort excerpt around the match
}

// SessionStore persists sessions.
type SessionStore interface {
	// Create inserts a new session. Idempotent on the session ID — a
	// duplicate ID is a silent no-op (INSERT OR IGNORE in the SQLite
	// implementation), so two near-simultaneous first-events for the
	// same session can both call Create without one of them racing into
	// a primary-key violation. Existing fields are preserved.
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context, f SessionFilter) ([]*Session, error)
	// SyncEventCount recomputes sessions.event_count for the given
	// session as SELECT COUNT(*) FROM events WHERE session_id = ?.
	// Recompute (not delta) is intentional: Append is idempotent via
	// INSERT OR IGNORE, so re-appending the same batch must not
	// double-count. The COUNT(*) on events is authoritative.
	SyncEventCount(ctx context.Context, sessionID string) error

	// UpdateCwd repairs sessions.cwd (and scope) for a row that was
	// created before the F1+F2+F3 reconcile fix synthesized a
	// session_start record. Used by `resleeve doctor --backfill-cwd`.
	// A no-op for an unknown session_id (UPDATE matches zero rows, no
	// error).
	UpdateCwd(ctx context.Context, sessionID, cwd, scope string) error
}

// EventStore persists session events. Appends are idempotent on
// (session_id, event_uuid) per docs/design/round-2/02-journey-01-decisions.md Q2.
type EventStore interface {
	Append(ctx context.Context, events []event.Event) error
	List(ctx context.Context, sessionID string, sinceSeq int64, limit int) ([]event.Event, error)
	Search(ctx context.Context, query string, limit int) ([]EventSearchHit, error)
}

// SlotStore tracks slot heartbeats and the current session per slot.
type SlotStore interface {
	Heartbeat(ctx context.Context, slot event.Slot, currentSessionID string) error
	Get(ctx context.Context, slot event.Slot) (*SlotState, error)
}

// UserStore persists user accounts and their KEK-wrap material.
// See docs/design/round-2/10-auth-subsystem.md.
type UserStore interface {
	Create(ctx context.Context, u *auth.User) error
	GetByID(ctx context.Context, id string) (*auth.User, error)
	GetByEmail(ctx context.Context, email string) (*auth.User, error)
	UpdateAuth(ctx context.Context, u *auth.User) error
}

// MemoryStore persists the memory module: scope tree, per-scope
// plans (named slots), append-only learnings with supersede chains.
// See docs/design/round-3/01-memory-module.md.
type MemoryStore interface {
	// Scopes
	CreateScope(ctx context.Context, s *memory.Scope) error
	GetScope(ctx context.Context, path string) (*memory.Scope, error)
	UpdateScope(ctx context.Context, s *memory.Scope) error
	DeleteScope(ctx context.Context, path string) error // memory.ErrScopeHasChildren if children exist
	ListScopes(ctx context.Context) ([]*memory.Scope, error)

	// Plans (named slots, upsert per (scope, name))
	PutPlan(ctx context.Context, p *memory.Plan) error
	GetPlan(ctx context.Context, scope, slot string) (*memory.Plan, error)
	ListPlans(ctx context.Context, scope string) ([]*memory.Plan, error)
	DeletePlan(ctx context.Context, scope, slot string) error

	// Learnings (append-only with soft-supersede)
	AppendLearning(ctx context.Context, l *memory.Learning) error
	GetLearning(ctx context.Context, id string) (*memory.Learning, error)
	ListLearnings(ctx context.Context, scope string, includeSuperseded bool) ([]*memory.Learning, error)
}

// OutboxRow is one pending row in the local sync outbox.
type OutboxRow struct {
	Seq           int64
	Kind          string
	Key           string
	Blob          []byte
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
}

// SyncStore persists the local sync state: the outbox of rows pending
// upload to upstream `resleeve serve`, and the per-kind cursor tracking
// the high-water-mark we've already pulled from upstream.
// See docs/design/round-4/02-cross-machine-sync.md.
type SyncStore interface {
	// EnqueueOutbox inserts a row pending upload. Idempotent on
	// (kind, key) — a second enqueue with the same key is a no-op.
	EnqueueOutbox(ctx context.Context, kind, key string, blob []byte, nextAttemptAt time.Time) error

	// DequeueOutbox returns up to batchSize rows where next_attempt_at
	// <= now, in seq ascending order (FIFO). Does NOT delete — caller
	// must AckOutbox after successful upload.
	DequeueOutbox(ctx context.Context, batchSize int) ([]OutboxRow, error)

	// AckOutbox removes successfully-uploaded rows by seq.
	AckOutbox(ctx context.Context, seqs []int64) error

	// BumpOutboxAttempt records a transient failure: increments
	// attempts, sets next_attempt_at, records the error.
	BumpOutboxAttempt(ctx context.Context, seq int64, nextAttemptAt time.Time, errMsg string) error

	// OutboxDepth returns the count of pending outbox rows; surfaced
	// in `resleeve doctor`.
	OutboxDepth(ctx context.Context) (int, error)

	// GetCursor returns the last upstream pull cursor for kind, or
	// empty when none recorded.
	GetCursor(ctx context.Context, kind string) (string, error)

	// SetCursor records a new upstream pull cursor for the kind.
	SetCursor(ctx context.Context, kind, cursor string) error
}

// ServerUser is the server-side mirror of an account: holds the
// Argon2id verifier + the password-wrapped + recovery-wrapped KEK. The
// server never sees the plaintext KEK; the verifier and the KEK-wrap
// derivation use independent salts so verifier exposure does not
// compromise the KEK. See docs/design/round-4/02-cross-machine-sync.md
// §"Identity".
type ServerUser struct {
	ID        string
	Email     string
	CreatedAt time.Time
	UpdatedAt time.Time

	Params           auth.Argon2idParams
	PasswordVerifier auth.Verifier
	PasswordKEK      auth.WrappedKEK
	RecoveryVerifier auth.Verifier
	RecoveryKEK      auth.WrappedKEK
}

// Device is one paired device on a server account. The DeviceToken is
// the bearer secret the device presents on every /v2/sync/* call; it is
// per-device so a single device compromise can be revoked without
// affecting the others.
type Device struct {
	ID          string
	UserID      string
	Name        string
	DeviceToken string
	CreatedAt   time.Time
	LastSeenAt  time.Time
}

// PairingCode is a short-lived (≤5 min) one-time code used to bridge a
// new device onto an existing account. Holds the KEK wrapped under a
// key derived from the (small) code itself, plus a separate verifier
// so a brute-force attempt is bounded by Argon2id cost AND the TTL.
type PairingCode struct {
	CodeID    string
	UserID    string
	Params    auth.Argon2idParams
	Verifier  auth.Verifier
	WrappedK  auth.WrappedKEK
	CreatedAt time.Time
	ExpiresAt time.Time
	ClaimedAt *time.Time
}

// ServerUserStore is the server-side account store, distinct from the
// client-side UserStore. Persisted by `resleeve serve`; never read by
// the client daemon. See docs/design/round-4/02-cross-machine-sync.md.
type ServerUserStore interface {
	Create(ctx context.Context, u *ServerUser) error
	GetByID(ctx context.Context, id string) (*ServerUser, error)
	GetByEmail(ctx context.Context, email string) (*ServerUser, error)
}

// DeviceStore persists per-device bearer tokens for `resleeve serve`.
type DeviceStore interface {
	// Create inserts a new device row. DeviceToken must be unique.
	Create(ctx context.Context, d *Device) error
	// GetByToken returns the device matching the bearer presented by the
	// client. ErrNotFound if no row matches.
	GetByToken(ctx context.Context, token string) (*Device, error)
	// ListByUser returns all devices for the given account.
	ListByUser(ctx context.Context, userID string) ([]*Device, error)
	// TouchLastSeen updates last_seen_at to now. Best-effort.
	TouchLastSeen(ctx context.Context, deviceID string) error
	// Delete removes a device (e.g. revoke on logout). Idempotent.
	Delete(ctx context.Context, deviceID string) error
}

// PairingStore persists short-lived pair codes.
type PairingStore interface {
	// Create publishes a fresh code. CodeID is the public identifier the
	// inviter shares with the accepter alongside the typed code.
	Create(ctx context.Context, c *PairingCode) error
	// Get returns a code by its public CodeID; ErrNotFound if absent.
	Get(ctx context.Context, codeID string) (*PairingCode, error)
	// Claim marks the code consumed atomically. Returns ErrNotFound if
	// the row was already claimed (preventing replays) or expired.
	Claim(ctx context.Context, codeID string, now time.Time) error
	// Delete hard-removes a code row. Used by the pair-claim rate-limiter
	// to lock out a code that exceeded the failed-attempt threshold so
	// subsequent claims see the same ErrNotFound a swept-expired row
	// would produce. Idempotent.
	Delete(ctx context.Context, codeID string) error
	// SweepExpired deletes any rows past expires_at. Cheap maintenance.
	SweepExpired(ctx context.Context, now time.Time) error
}

// Store is the union — each backend exposes the sub-stores via typed
// accessors. See docs/design/round-2/11-storage-backends.md.
type Store interface {
	Sessions() SessionStore
	Events() EventStore
	Slots() SlotStore
	Users() UserStore
	Memory() MemoryStore
	Sync() SyncStore
	ServerUsers() ServerUserStore
	Devices() DeviceStore
	Pairings() PairingStore
	Close() error
}
