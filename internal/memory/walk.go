package memory

import "strings"

// Ancestors returns the ancestor chain of a scope path, from root to
// the path itself (inclusive). For "team-foo/project-alpha/auth" it
// returns ["team-foo", "team-foo/project-alpha",
// "team-foo/project-alpha/auth"]. Empty path returns nil.
func Ancestors(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for i := range parts {
		out = append(out, strings.Join(parts[:i+1], "/"))
	}
	return out
}

// ApplyBoundary walks a chain of Scopes (root-most → leaf-most) and
// applies the `.donotinherit` boundary-reset rule per Q4: when a
// scope with DoNotInherit=true is encountered, drop everything above
// it (including itself, then include itself going forward). Nil
// entries in the input are skipped silently.
//
// Example: chain [team-foo, project-alpha (donotinherit), auth] →
// result [project-alpha, auth].
func ApplyBoundary(chain []*Scope) []*Scope {
	var result []*Scope
	for _, s := range chain {
		if s == nil {
			continue
		}
		if s.DoNotInherit {
			result = []*Scope{s}
			continue
		}
		result = append(result, s)
	}
	return result
}
