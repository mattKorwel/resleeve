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
	Limit     int           // <= 0 ⇒ default 50; capped at 500
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
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context, f SessionFilter) ([]*Session, error)
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

// Store is the union — each backend exposes the sub-stores via typed
// accessors. See docs/design/round-2/11-storage-backends.md.
type Store interface {
	Sessions() SessionStore
	Events() EventStore
	Slots() SlotStore
	Users() UserStore
	Memory() MemoryStore
	Close() error
}
