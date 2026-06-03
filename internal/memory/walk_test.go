package memory

import "testing"

func TestAncestors(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"", nil},
		{"/", nil},
		{"team-foo", []string{"team-foo"}},
		{"team-foo/project-alpha", []string{"team-foo", "team-foo/project-alpha"}},
		{
			"team-foo/project-alpha/auth",
			[]string{"team-foo", "team-foo/project-alpha", "team-foo/project-alpha/auth"},
		},
		{"/team-foo/", []string{"team-foo"}}, // leading/trailing slashes tolerated
	}
	for _, tc := range cases {
		got := Ancestors(tc.path)
		if !sameStringSlice(got, tc.want) {
			t.Errorf("Ancestors(%q): got %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestApplyBoundary_NoBoundary(t *testing.T) {
	chain := []*Scope{
		{Path: "team-foo"},
		{Path: "team-foo/project-alpha"},
		{Path: "team-foo/project-alpha/auth"},
	}
	got := ApplyBoundary(chain)
	if len(got) != 3 {
		t.Fatalf("expected full chain (3), got %d", len(got))
	}
	if got[0].Path != "team-foo" || got[2].Path != "team-foo/project-alpha/auth" {
		t.Errorf("chain ordering: got %+v", paths(got))
	}
}

func TestApplyBoundary_ResetsAtMarker(t *testing.T) {
	chain := []*Scope{
		{Path: "team-foo"},                              // would normally be included
		{Path: "team-foo/project-alpha", DoNotInherit: true}, // resets
		{Path: "team-foo/project-alpha/auth"},
	}
	got := ApplyBoundary(chain)
	if len(got) != 2 {
		t.Fatalf("expected 2-element chain after reset, got %d (%v)", len(got), paths(got))
	}
	if got[0].Path != "team-foo/project-alpha" {
		t.Errorf("expected marker at head, got %q", got[0].Path)
	}
	if got[1].Path != "team-foo/project-alpha/auth" {
		t.Errorf("expected leaf preserved, got %q", got[1].Path)
	}
}

func TestApplyBoundary_NilSafelySkipped(t *testing.T) {
	chain := []*Scope{nil, {Path: "team-foo"}, nil}
	got := ApplyBoundary(chain)
	if len(got) != 1 || got[0].Path != "team-foo" {
		t.Errorf("expected nil entries skipped; got %v", paths(got))
	}
}

func TestApplyBoundary_MultipleMarkers(t *testing.T) {
	// Inner marker wins (it's the most recent reset).
	chain := []*Scope{
		{Path: "a"},
		{Path: "a/b", DoNotInherit: true},
		{Path: "a/b/c"},
		{Path: "a/b/c/d", DoNotInherit: true},
		{Path: "a/b/c/d/e"},
	}
	got := ApplyBoundary(chain)
	if len(got) != 2 {
		t.Fatalf("expected 2-element chain (inner marker wins), got %d (%v)", len(got), paths(got))
	}
	if got[0].Path != "a/b/c/d" || got[1].Path != "a/b/c/d/e" {
		t.Errorf("unexpected chain: %v", paths(got))
	}
}

// --- helpers ---

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func paths(scopes []*Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = s.Path
	}
	return out
}

func TestScopeKind_Valid(t *testing.T) {
	good := []ScopeKind{
		"", ScopeKindPortfolio, ScopeKindProgram, ScopeKindProject,
		ScopeKindDispatch, ScopeKindAgent, ScopeKindOther,
	}
	for _, k := range good {
		if !k.Valid() {
			t.Errorf("ScopeKind(%q).Valid() = false, want true", k)
		}
	}
	bad := []ScopeKind{"foo", "Project", "PROJECT", "team"}
	for _, k := range bad {
		if k.Valid() {
			t.Errorf("ScopeKind(%q).Valid() = true, want false", k)
		}
	}
}
