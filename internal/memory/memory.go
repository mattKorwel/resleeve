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
	"fmt"
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

// Plan is a named slot of markdown content attached to a scope. As of
// round-12B plans are append-only: each write appends an immutable
// version and reads materialize HEAD (the max-version row). A Plan value
// represents one version row — a GetPlan returns HEAD, GetPlanVersion
// returns a specific one.
//
// (scope, name, version) is the storage primary key. Version is 1-based
// and monotonic per (scope, name); ParentVersion is the version this one
// was derived from (0 for the first write). Author is the user that
// recorded the version (empty = unknown / local daemon).
type Plan struct {
	Scope         string    `json:"scope"`
	Name          string    `json:"name"` // DefaultPlanSlot for the conventional one
	Version       int64     `json:"version"`
	Content       string    `json:"content"`
	Author        string    `json:"author_user_id,omitempty"`
	ParentVersion int64     `json:"parent_version"`
	UpdatedAt     time.Time `json:"updated_at"` // = the version's created_at
}

// Learning is one append-only log entry on a scope. SupersedesID may
// point at a prior learning this one corrects; the prior entry stays
// in storage but is hidden from default reads. Author is the user that
// contributed the learning (round-12B provenance; empty = unknown/local).
type Learning struct {
	ID           string    `json:"id"`
	Scope        string    `json:"scope"`
	Content      string    `json:"content"`
	SupersedesID *string   `json:"supersedes_id,omitempty"`
	Author       string    `json:"author_user_id,omitempty"`
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

// NewPlanBaseVersion is the base_version a caller passes when it expects
// no existing plan (a first write). The append rejects it with
// ErrPlanConflict if a HEAD already exists (unless force is used).
const NewPlanBaseVersion int64 = 0

// PlanConflictError is the typed optimistic-concurrency failure returned
// by AppendPlanVersion when the supplied base_version does not match the
// current HEAD version. It carries the current HEAD (version + content)
// so the caller has everything needed to reconcile (re-render against
// HEAD and retry) without a second round-trip. This *is* the
// reconciliation signal — see docs/design/round-12/01-plan-versioning-slice.md.
type PlanConflictError struct {
	Scope string
	Name  string
	// Head is the current materialized HEAD the write lost the race to.
	// nil only in the degenerate "expected-new but a plan exists" case
	// where the conflict is reported with the existing HEAD populated.
	Head *Plan
}

func (e *PlanConflictError) Error() string {
	head := int64(0)
	if e.Head != nil {
		head = e.Head.Version
	}
	return fmt.Sprintf("memory: plan %s/%s conflict: current HEAD is version %d", e.Scope, e.Name, head)
}

// ErrPlanConflict is the sentinel that every PlanConflictError matches
// under errors.Is, so callers can branch with
// `errors.Is(err, memory.ErrPlanConflict)` and then type-assert to
// *PlanConflictError to read the carried HEAD.
var ErrPlanConflict = errors.New("memory: plan version conflict")

// Is lets errors.Is(err, ErrPlanConflict) match any *PlanConflictError.
func (e *PlanConflictError) Is(target error) bool {
	return target == ErrPlanConflict
}
