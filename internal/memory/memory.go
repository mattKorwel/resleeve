// Package memory implements resleeve's memory module — scope tree,
// per-scope plans (named slots), append-only learnings with soft
// supersede chains, and inheritance walks for context injection.
//
// Lifted as design lessons from mattkorwel/ori (private playground),
// implemented fresh against resleeve's SQL storage. See
// docs/design/round-3/01-memory-module.md for the full design.
package memory

import (
	"errors"
	"time"
)

// ScopeKind is the categorization of a scope. Discrete enum per Q8.
type ScopeKind string

const (
	ScopeKindPortfolio ScopeKind = "portfolio"
	ScopeKindProgram   ScopeKind = "program"
	ScopeKindProject   ScopeKind = "project"
	ScopeKindDispatch  ScopeKind = "dispatch"
	ScopeKindAgent     ScopeKind = "agent"
	ScopeKindOther     ScopeKind = "other"
)

// Valid reports whether k is one of the recognized scope kinds.
// The empty kind is also accepted (treated as unset / "other").
func (k ScopeKind) Valid() bool {
	switch k {
	case "",
		ScopeKindPortfolio, ScopeKindProgram, ScopeKindProject,
		ScopeKindDispatch, ScopeKindAgent, ScopeKindOther:
		return true
	}
	return false
}

// Scope is one node in the memory tree. Path is the slash-separated
// identity; ancestors are derived from it. See `Ancestors` in walk.go.
type Scope struct {
	Path         string    `json:"path"`
	Kind         ScopeKind `json:"kind"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Cwd          string    `json:"cwd"`
	DoNotInherit bool      `json:"do_not_inherit"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DefaultPlanSlot is the conventional plan slot name used when the
// CLI / API caller doesn't specify one.
const DefaultPlanSlot = "_default"

// Plan is a named slot of markdown content attached to a scope.
// (scope, name) is the storage primary key.
type Plan struct {
	Scope     string    `json:"scope"`
	Name      string    `json:"name"` // DefaultPlanSlot for the conventional one
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Learning is one append-only log entry on a scope. SupersedesID may
// point at a prior learning this one corrects; the prior entry stays
// in storage but is hidden from default reads.
type Learning struct {
	ID           string    `json:"id"`
	Scope        string    `json:"scope"`
	Content      string    `json:"content"`
	SupersedesID *string   `json:"supersedes_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Sentinel errors.
var (
	// ErrScopeHasChildren is returned by storage.DeleteScope when the
	// target has child scopes (per Q3).
	ErrScopeHasChildren = errors.New("memory: scope has children; clean up explicitly")

	// ErrInvalidKind is returned when a ScopeKind value isn't recognized.
	ErrInvalidKind = errors.New("memory: invalid scope kind")

	// ErrEmptyPath is returned when an operation receives an empty path.
	ErrEmptyPath = errors.New("memory: empty scope path")
)
