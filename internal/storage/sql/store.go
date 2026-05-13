package sql

import (
	"context"
	"errors"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
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

// SlotState is the durable view of one slot.
type SlotState struct {
	Slot             event.Slot
	LastHeartbeatAt  time.Time
	CurrentSessionID string // empty if quiescent
}

// SessionStore persists sessions.
type SessionStore interface {
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
}

// EventStore persists session events. Appends are idempotent on
// (session_id, event_uuid) per docs/design/round-2/02-journey-01-decisions.md Q2.
type EventStore interface {
	Append(ctx context.Context, events []event.Event) error
	List(ctx context.Context, sessionID string, sinceSeq int64, limit int) ([]event.Event, error)
}

// SlotStore tracks slot heartbeats and the current session per slot.
type SlotStore interface {
	Heartbeat(ctx context.Context, slot event.Slot, currentSessionID string) error
	Get(ctx context.Context, slot event.Slot) (*SlotState, error)
}

// Store is the union — each backend exposes the three sub-stores
// via typed accessors. v1 deploys the SQLite implementation; the
// interface is sized for Postgres (and Spanner-PG, AlloyDB, etc.)
// from day one. See docs/design/round-2/11-storage-backends.md.
type Store interface {
	Sessions() SessionStore
	Events() EventStore
	Slots() SlotStore
	Close() error
}
