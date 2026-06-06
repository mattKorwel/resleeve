package antigravity

import (
	"os"
	"path/filepath"
	"strings"
)

// ScopeMarkerFile is the filename resleeve looks for when walking cwd
// ancestors to resolve a hierarchical scope. Identical semantics to the
// claude adapter's marker — copied here to keep adapters independent.
const ScopeMarkerFile = ".resleeve-scope"

// deriveScope picks a scope path for an event given its cwd. Resolution
// order (matches the claude adapter exactly, by design — scope is a
// cross-adapter contract):
//  1. $RESLEEVE_SCOPE — verbatim override, if set.
//  2. Walk cwd ancestors for a `.resleeve-scope` marker file. If found
//     non-empty, scope = <marker-content>/<rel-from-marker-dir>.
//  3. filepath.Base(cwd), or "unknown" if cwd is empty.
func deriveScope(cwd string) string {
	if env := strings.TrimSpace(os.Getenv("RESLEEVE_SCOPE")); env != "" {
		return env
	}
	if cwd == "" {
		return "unknown"
	}
	if marker, dir := findScopeMarker(cwd); marker != "" {
		rel, err := filepath.Rel(dir, cwd)
		if err != nil || rel == "." {
			return marker
		}
		return marker + "/" + filepath.ToSlash(rel)
	}
	return filepath.Base(cwd)
}

// findScopeMarker walks cwd up to the filesystem root looking for a
// ScopeMarkerFile. Returns the trimmed file contents and the directory
// holding the marker, or ("", "") if none found.
func findScopeMarker(cwd string) (marker, markerDir string) {
	dir := cwd
	for {
		if b, err := os.ReadFile(filepath.Join(dir, ScopeMarkerFile)); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v, dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}
