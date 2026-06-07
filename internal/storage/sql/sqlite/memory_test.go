package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/mattkorwel/resleeve/internal/memory"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

func TestMemory_ScopeCRUD(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	ms := st.Memory()

	root := &memory.Scope{Path: "team-foo", Kind: memory.ScopeKindProgram, Title: "Team Foo"}
	if err := ms.CreateScope(ctx, root); err != nil {
		t.Fatalf("create root: %v", err)
	}

	got, err := ms.GetScope(ctx, "team-foo")
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got.Kind != memory.ScopeKindProgram || got.Title != "Team Foo" {
		t.Errorf("scope mismatch: %+v", got)
	}

	// Upsert via UpdateScope.
	got.Description = "the team that does foo"
	if err := ms.UpdateScope(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reloaded, _ := ms.GetScope(ctx, "team-foo")
	if reloaded.Description != "the team that does foo" {
		t.Errorf("update lost description: %+v", reloaded)
	}

	// List.
	all, err := ms.ListScopes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("list count: got %d, want 1", len(all))
	}
}

func TestMemory_DeleteScopeRefusesChildren(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	ms := st.Memory()

	if err := ms.CreateScope(ctx, &memory.Scope{Path: "team-foo", Kind: memory.ScopeKindProgram}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if err := ms.CreateScope(ctx, &memory.Scope{Path: "team-foo/project-alpha", Kind: memory.ScopeKindProject}); err != nil {
		t.Fatalf("create child: %v", err)
	}

	if err := ms.DeleteScope(ctx, "team-foo"); !errors.Is(err, memory.ErrScopeHasChildren) {
		t.Errorf("expected ErrScopeHasChildren deleting parent, got %v", err)
	}

	// Deleting the child succeeds.
	if err := ms.DeleteScope(ctx, "team-foo/project-alpha"); err != nil {
		t.Fatalf("delete leaf: %v", err)
	}
	// Now parent should be deletable.
	if err := ms.DeleteScope(ctx, "team-foo"); err != nil {
		t.Errorf("delete parent after children gone: %v", err)
	}
}

func TestMemory_InvalidKindRejected(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	ms := st.Memory()

	bad := &memory.Scope{Path: "x", Kind: memory.ScopeKind("not-a-kind")}
	if err := ms.CreateScope(ctx, bad); !errors.Is(err, memory.ErrInvalidKind) {
		t.Errorf("expected ErrInvalidKind, got %v", err)
	}
}

func TestMemory_PlanAppendVersionsNamedSlots(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	ms := st.Memory()

	if err := ms.CreateScope(ctx, &memory.Scope{Path: "p", Kind: memory.ScopeKindProject}); err != nil {
		t.Fatalf("create scope: %v", err)
	}

	// First write into the default slot: base_version 0 (expect-new).
	p1, err := ms.AppendPlanVersion(ctx, "p", "", "v1", "alice", memory.NewPlanBaseVersion, false)
	if err != nil {
		t.Fatalf("append default v1: %v", err)
	}
	if p1.Version != 1 || p1.ParentVersion != 0 {
		t.Errorf("v1 version/parent: got %d/%d, want 1/0", p1.Version, p1.ParentVersion)
	}

	def, err := ms.GetPlan(ctx, "p", "")
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if def.Name != memory.DefaultPlanSlot || def.Content != "v1" || def.Version != 1 || def.Author != "alice" {
		t.Errorf("default HEAD mismatch: %+v", def)
	}

	// Correct base (HEAD=1) appends version 2 with parent_version=1.
	p2, err := ms.AppendPlanVersion(ctx, "p", "", "v2", "bob", 1, false)
	if err != nil {
		t.Fatalf("append default v2: %v", err)
	}
	if p2.Version != 2 || p2.ParentVersion != 1 {
		t.Errorf("v2 version/parent: got %d/%d, want 2/1", p2.Version, p2.ParentVersion)
	}
	def2, _ := ms.GetPlan(ctx, "p", memory.DefaultPlanSlot)
	if def2.Content != "v2" || def2.Version != 2 {
		t.Errorf("HEAD after v2: %+v", def2)
	}

	// Stale base (1, but HEAD=2) => ErrPlanConflict carrying HEAD.
	_, err = ms.AppendPlanVersion(ctx, "p", "", "v3-stale", "carol", 1, false)
	if !errors.Is(err, memory.ErrPlanConflict) {
		t.Fatalf("stale base: want ErrPlanConflict, got %v", err)
	}
	var conflict *memory.PlanConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("conflict not *PlanConflictError: %v", err)
	}
	if conflict.Head == nil || conflict.Head.Version != 2 || conflict.Head.Content != "v2" {
		t.Errorf("conflict HEAD: %+v", conflict.Head)
	}

	// Expect-new (base 0) against an existing plan also conflicts.
	if _, err := ms.AppendPlanVersion(ctx, "p", "", "x", "carol", memory.NewPlanBaseVersion, false); !errors.Is(err, memory.ErrPlanConflict) {
		t.Errorf("expect-new on existing: want ErrPlanConflict, got %v", err)
	}

	// force bypasses the check and appends HEAD+1.
	p4, err := ms.AppendPlanVersion(ctx, "p", "", "forced", "carol", 0, true)
	if err != nil {
		t.Fatalf("forced append: %v", err)
	}
	if p4.Version != 3 {
		t.Errorf("forced version: got %d, want 3", p4.Version)
	}

	// History: 3 versions for the default slot.
	hist, err := ms.ListPlanVersions(ctx, "p", "")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len: got %d, want 3", len(hist))
	}
	if hist[0].Version != 1 || hist[2].Version != 3 {
		t.Errorf("history order: %d..%d", hist[0].Version, hist[2].Version)
	}

	// GetPlanVersion returns a specific historical row.
	gv, err := ms.GetPlanVersion(ctx, "p", "", 1)
	if err != nil || gv.Content != "v1" {
		t.Errorf("GetPlanVersion(1): %+v err=%v", gv, err)
	}

	// Named slot is independent.
	if _, err := ms.AppendPlanVersion(ctx, "p", "architecture", "AAA", "", memory.NewPlanBaseVersion, false); err != nil {
		t.Fatalf("append named: %v", err)
	}
	slots, err := ms.ListPlans(ctx, "p")
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	if len(slots) != 2 {
		t.Errorf("expected 2 slots (default + architecture), got %d", len(slots))
	}
}

func TestMemory_LearningsAppendAndSupersede(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	ms := st.Memory()

	if err := ms.CreateScope(ctx, &memory.Scope{Path: "p", Kind: memory.ScopeKindProject}); err != nil {
		t.Fatalf("create scope: %v", err)
	}

	l1 := &memory.Learning{ID: "L1", Scope: "p", Content: "typo: we use JWT validation", Author: "alice"}
	if err := ms.AppendLearning(ctx, l1); err != nil {
		t.Fatalf("append l1: %v", err)
	}
	// Provenance: author round-trips (round-12B).
	if got, err := ms.GetLearning(ctx, "L1"); err != nil {
		t.Errorf("get L1: %v", err)
	} else if got.Author != "alice" {
		t.Errorf("learning author: got %q, want alice", got.Author)
	}
	l1ID := l1.ID
	l2 := &memory.Learning{ID: "L2", Scope: "p", Content: "we use JWT", SupersedesID: &l1ID}
	if err := ms.AppendLearning(ctx, l2); err != nil {
		t.Fatalf("append l2 (supersedes l1): %v", err)
	}
	l3 := &memory.Learning{ID: "L3", Scope: "p", Content: "second independent learning"}
	if err := ms.AppendLearning(ctx, l3); err != nil {
		t.Fatalf("append l3: %v", err)
	}

	// Default list: L1 superseded, so visible = {L2, L3}.
	current, err := ms.ListLearnings(ctx, "p", false)
	if err != nil {
		t.Fatalf("list current: %v", err)
	}
	if len(current) != 2 {
		t.Fatalf("expected 2 current learnings, got %d", len(current))
	}
	seen := map[string]bool{}
	for _, l := range current {
		seen[l.ID] = true
	}
	if seen["L1"] {
		t.Errorf("L1 should be hidden (superseded)")
	}
	if !seen["L2"] || !seen["L3"] {
		t.Errorf("expected L2 + L3 visible; got %v", seen)
	}

	// With include=superseded: all three.
	all, err := ms.ListLearnings(ctx, "p", true)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 total, got %d", len(all))
	}
}

func TestMemory_NotFound(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	ms := st.Memory()

	if _, err := ms.GetScope(ctx, "nope"); !errors.Is(err, rsql.ErrNotFound) {
		t.Errorf("missing scope: expected ErrNotFound, got %v", err)
	}
	if _, err := ms.GetPlan(ctx, "p", ""); !errors.Is(err, rsql.ErrNotFound) {
		t.Errorf("missing plan: expected ErrNotFound, got %v", err)
	}
	if _, err := ms.GetLearning(ctx, "nope"); !errors.Is(err, rsql.ErrNotFound) {
		t.Errorf("missing learning: expected ErrNotFound, got %v", err)
	}
}
