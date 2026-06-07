package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// args is a tiny arg-bag for tool handlers. Tools accept an arbitrary
// JSON object; we lazily extract typed fields.
type args struct {
	m map[string]any
}

func decodeArgs(raw json.RawMessage) args {
	a := args{m: map[string]any{}}
	if len(raw) == 0 {
		return a
	}
	_ = json.Unmarshal(raw, &a.m)
	return a
}

func (a args) string(k string) string {
	if v, ok := a.m[k].(string); ok {
		return v
	}
	return ""
}

func (a args) stringOr(k, fallback string) string {
	if v := a.string(k); v != "" {
		return v
	}
	return fallback
}

func (a args) bool(k string) bool {
	v, _ := a.m[k].(bool)
	return v
}

// int64Or extracts an integer arg. JSON numbers decode to float64, so we
// coerce; a string-typed number is also tolerated (some hosts stringify
// args). Returns fallback when the key is absent or unparseable.
func (a args) int64Or(k string, fallback int64) int64 {
	switch v := a.m[k].(type) {
	case float64:
		return int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

// scope returns the scope arg, falling back to the server's default
// (resolved at boot from env / marker / filepath.Base). Empty string
// means "no scope" — caller decides whether that's an error.
func (a args) scope(defaultScope string) string {
	if v := a.string("scope"); v != "" {
		return v
	}
	return defaultScope
}

// requireScope returns the scope arg or an error referencing both the
// arg and the env fallback.
func requireScope(a args, defaultScope string) (string, error) {
	v := a.scope(defaultScope)
	if v == "" {
		return "", fmt.Errorf("no scope: pass {\"scope\": \"...\"} or set $RESLEEVE_SCOPE in the host env / drop a .resleeve-scope marker in cwd")
	}
	return v, nil
}

// ----- JSON Schema helpers (the subset MCP hosts read) -----

func schemaObject(props map[string]any, required []string) map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func schemaString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func schemaBool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func schemaInt(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// ----- scope resolution (matches adapter.deriveScope semantics) -----

// scopeMarkerFile is the marker filename resleeve walks for, mirrors
// internal/adapter/claude.ScopeMarkerFile. Kept as a const here to
// avoid pulling the adapter package into mcp (which would create an
// awkward import cycle once adapters reach back for richer context).
const scopeMarkerFile = ".resleeve-scope"

// ResolveScope picks the server's default scope using the same order
// the claude adapter uses for event tagging:
//  1. $RESLEEVE_SCOPE — verbatim override.
//  2. Walk cwd ancestors for a .resleeve-scope marker. If non-empty,
//     scope = <marker-content>/<rel-from-marker-dir>.
//  3. filepath.Base(cwd), or "" if cwd is empty (server treats "" as
//     "no auto-loaded context section").
//
// Exposed so the CLI verb can resolve once at boot and pass it as
// Config.DefaultScope; tools also consult it on each call as the
// fallback for missing scope arguments.
func ResolveScope(cwd string) string {
	if env := strings.TrimSpace(os.Getenv("RESLEEVE_SCOPE")); env != "" {
		return env
	}
	if cwd == "" {
		return ""
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

func findScopeMarker(cwd string) (marker, markerDir string) {
	dir := cwd
	for {
		if b, err := os.ReadFile(filepath.Join(dir, scopeMarkerFile)); err == nil {
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

// ----- truncation helper for instructions sections -----

// truncateToCap returns s capped at cap bytes, appending a brief
// "truncated" notice when it had to cut. Used by buildInstructions to
// bound the auto-loaded context so a 100 KB plan doesn't blow the
// host's instructions buffer.
func truncateToCap(s string, capBytes int, tail string) string {
	if len(s) <= capBytes {
		return s
	}
	if capBytes <= len(tail) {
		return s[:capBytes]
	}
	return s[:capBytes-len(tail)] + tail
}

// safeText guards against the omitempty regression — even valid empty
// strings from upstream are converted to a friendly placeholder so the
// host's content-block validator stays happy AND the model has
// something to chew on. The contentBlock struct tag (no omitempty) is
// the primary fix; this is the second layer.
func safeText(s, placeholder string) string {
	if s == "" {
		return placeholder
	}
	return s
}

// shorten produces a 1-line preview for ack messages.
func shorten(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

