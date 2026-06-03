package memory

import (
	"context"
	"fmt"
	"strings"
)

// StoreReader is the subset of storage that context building needs.
// Defined here to avoid a circular import with internal/storage/sql.
// The concrete sql.MemoryStore satisfies it structurally.
type StoreReader interface {
	GetScope(ctx context.Context, path string) (*Scope, error)
	GetPlan(ctx context.Context, scope, slot string) (*Plan, error)
	ListLearnings(ctx context.Context, scope string, includeSuperseded bool) ([]*Learning, error)
}

// ResolveChain returns the boundary-applied ancestor chain of
// scopePath. Ancestors that don't have a scope row are skipped
// silently (they're allowed to be implicit gaps).
func ResolveChain(ctx context.Context, store StoreReader, scopePath string) []*Scope {
	paths := Ancestors(scopePath)
	chain := make([]*Scope, 0, len(paths))
	for _, p := range paths {
		s, err := store.GetScope(ctx, p)
		if err != nil || s == nil {
			continue
		}
		chain = append(chain, s)
	}
	return ApplyBoundary(chain)
}

// BuildContext returns a markdown document containing the default-slot
// plans + non-superseded learnings from each scope in scopePath's
// ancestor chain (with `.donotinherit` boundary applied).
//
// Returns "" when the rolled-up chain produces no content — so the
// SessionStart bridge can no-op and emit nothing.
func BuildContext(ctx context.Context, store StoreReader, scopePath string) string {
	chain := ResolveChain(ctx, store, scopePath)
	if len(chain) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Resleeve memory for scope %s\n\n", scopePath)

	plans := 0
	for _, s := range chain {
		p, err := store.GetPlan(ctx, s.Path, DefaultPlanSlot)
		if err != nil || p == nil || strings.TrimSpace(p.Content) == "" {
			continue
		}
		fmt.Fprintf(&b, "## Plan (%s)\n\n%s\n\n", s.Path, strings.TrimRight(p.Content, "\n"))
		plans++
	}

	learnings := 0
	for _, s := range chain {
		ls, err := store.ListLearnings(ctx, s.Path, false)
		if err != nil || len(ls) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## Learnings (%s)\n\n", s.Path)
		for _, l := range ls {
			fmt.Fprintf(&b, "- %s: %s\n", l.CreatedAt.Format("2006-01-02"), oneLine(l.Content))
			learnings++
		}
		b.WriteString("\n")
	}

	if plans == 0 && learnings == 0 {
		return ""
	}
	return b.String()
}

// CollectPlanChain returns plans (default slot only) from each scope in
// the boundary-applied chain. Used by `GET /v1/plan?inherit=true`.
func CollectPlanChain(ctx context.Context, store StoreReader, scopePath, slot string) []*Plan {
	if slot == "" {
		slot = DefaultPlanSlot
	}
	chain := ResolveChain(ctx, store, scopePath)
	var out []*Plan
	for _, s := range chain {
		p, err := store.GetPlan(ctx, s.Path, slot)
		if err != nil || p == nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// CollectLearningsChain returns learnings from each scope in the
// boundary-applied chain. Used by `GET /v1/learnings?inherit=true`.
func CollectLearningsChain(ctx context.Context, store StoreReader, scopePath string, includeSuperseded bool) []*Learning {
	chain := ResolveChain(ctx, store, scopePath)
	var out []*Learning
	for _, s := range chain {
		ls, err := store.ListLearnings(ctx, s.Path, includeSuperseded)
		if err != nil {
			continue
		}
		out = append(out, ls...)
	}
	return out
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}
