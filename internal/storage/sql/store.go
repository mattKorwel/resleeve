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
// compromise the KEK. There is no client-side counterpart — the
// pre-pivot client-side User persistence (UserStore, migration 0002)
// was removed in Q1 — see docs/REVIEW_QUALITY.md. See
// docs/design/round-4/02-cross-machine-sync.md §"Identity".
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
//
// ExpiresAt is the server-side TTL added in sec-M5: when non-nil, the
// bearer is rejected after this time even if the row hasn't been
// explicitly deleted. Used by `resleeve pair invite` to mint a 60-second
// ephemeral device for the login that fetches the wrapped KEK, so a
// failed best-effort revoke doesn't leave a year-long bearer alive.
// NULL = no expiry (regular paired devices).
type Device struct {
	ID          string
	UserID      string
	Name        string
	DeviceToken string
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   *time.Time
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

// ServerUserStore is the server-side account store, persisted by
// `resleeve serve`; never read by the client daemon. See
// docs/design/round-4/02-cross-machine-sync.md.
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

// ServeMetaStore persists `resleeve serve` process-level secrets that
// need to outlive a restart but don't belong on any one user row. v1
// holds exactly one key: the HMAC secret used to derive synthetic salts
// for unknown-email /v2/auth/login-challenge responses (sec-M1). Storing
// it here means the salt for a probed-unknown email stays stable across
// reboots, removing the cross-restart "did the server reboot?" oracle.
//
// Intentionally narrow API — this is NOT a general kv store, it's a
// per-process-secret bucket. Add new methods only when the secret
// genuinely belongs at the serve-process scope, not at the user/device
// scope.
type ServeMetaStore interface {
	// GetOrCreateBytes returns the value for key, generating it via
	// genFn and persisting on first call. Subsequent calls return the
	// persisted value. The generator runs at most once per key in the
	// table's lifetime; concurrent first-callers may both generate but
	// only one Insert wins — losers re-read.
	GetOrCreateBytes(ctx context.Context, key string, genFn func() ([]byte, error)) ([]byte, error)
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

// BrainKind classifies a brain. A personal brain is auto-provisioned for
// every user on register; a shared brain is simply a brain with more than
// one member. See docs/design/round-11/02-multi-tenant.md §"Core model".
type BrainKind string

const (
	BrainKindPersonal BrainKind = "personal"
	BrainKindShared   BrainKind = "shared"
)

// Brain is a named namespace — a scope tree plus its memory (plans,
// learnings). It is the unit of isolation and of sharing. OwnerUserID is
// the creator, who can add/remove members; there is no role column —
// every member reads and writes (round-11 "keep it dumb" constraint).
type Brain struct {
	ID          string
	Name        string
	Kind        BrainKind
	OwnerUserID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Membership is the access edge: a (BrainID, UserID) pair. Its existence
// is the access rule — you may touch a brain iff you are a member. No
// role yet.
type Membership struct {
	BrainID   string
	UserID    string
	CreatedAt time.Time
}

// CredentialKind classifies how a credential proves identity. ssh = an
// SSH public key (authorized_keys style); api = a long-lived bearer the
// server stores only as a hash; oidc = an external IdP subject (tier 5,
// stored here behind the same seam, verified later).
type CredentialKind string

const (
	CredentialKindSSH  CredentialKind = "ssh"
	CredentialKindAPI  CredentialKind = "api"
	CredentialKindOIDC CredentialKind = "oidc"
)

// Credential is one way a user can authenticate: an SSH pubkey, an API
// key hash, or an OIDC subject. Many users × many keys is just rows. This
// slice persists the shape only; SSH-challenge / API-key verification
// lands in a later slice. The server stores only a public key or a hash —
// never a plaintext secret.
type Credential struct {
	ID              string
	UserID          string
	Kind            CredentialKind
	PublicKeyOrHash string
	Label           string
	CreatedAt       time.Time
	ExpiresAt       *time.Time // nil = no expiry
}

// BrainStore persists brains. Server-side only.
type BrainStore interface {
	// Create inserts a new brain row.
	Create(ctx context.Context, b *Brain) error
	// Get returns a brain by id; ErrNotFound if absent.
	Get(ctx context.Context, id string) (*Brain, error)
	// ListByOwner returns brains owned (created) by userID.
	ListByOwner(ctx context.Context, userID string) ([]*Brain, error)
	// ListForUser returns every brain userID is a member of (via the
	// memberships join), regardless of ownership.
	ListForUser(ctx context.Context, userID string) ([]*Brain, error)
}

// MembershipStore persists the brain↔user access edges. Server-side only.
type MembershipStore interface {
	// Add inserts a membership. Idempotent on (brain_id, user_id).
	Add(ctx context.Context, m *Membership) error
	// Remove deletes a membership. Idempotent.
	Remove(ctx context.Context, brainID, userID string) error
	// ListBrains returns the brain ids userID is a member of.
	ListBrains(ctx context.Context, userID string) ([]string, error)
	// ListMembers returns the user ids that are members of brainID.
	ListMembers(ctx context.Context, brainID string) ([]string, error)
	// IsMember reports whether userID is a member of brainID.
	IsMember(ctx context.Context, userID, brainID string) (bool, error)
}

// CredentialStore persists per-user credentials. Server-side only.
type CredentialStore interface {
	// Add inserts a new credential row.
	Add(ctx context.Context, c *Credential) error
	// Get returns a credential by id; ErrNotFound if absent.
	Get(ctx context.Context, id string) (*Credential, error)
	// ListByUser returns all credentials for userID.
	ListByUser(ctx context.Context, userID string) ([]*Credential, error)
	// Delete removes a credential by id (e.g. revoke). Idempotent.
	Delete(ctx context.Context, id string) error
}

// Store is the union — each backend exposes the sub-stores via typed
// accessors. See docs/design/round-2/11-storage-backends.md.
type Store interface {
	Sessions() SessionStore
	Events() EventStore
	Slots() SlotStore
	Memory() MemoryStore
	Sync() SyncStore
	ServerUsers() ServerUserStore
	Devices() DeviceStore
	Pairings() PairingStore
	ServeMeta() ServeMetaStore
	Brains() BrainStore
	Memberships() MembershipStore
	Credentials() CredentialStore
	Close() error
}
