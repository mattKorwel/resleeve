// Package antigravity implements the resleeve adapter for the Google
// Antigravity CLI (`agy`, a Windsurf/Cascade-derived agent CLI).
//
// Unlike the Claude Code adapter — which captures live via hooks and
// reconciles from newline-delimited JSON transcripts — Antigravity has NO
// hook/plugin surface and does NOT persist a JSON transcript. Instead each
// conversation is one SQLite database:
//
//	~/.gemini/antigravity-cli/conversations/<conversation-uuid>.db
//
// with a `steps` table whose `step_payload` column is a protobuf-wire-format
// BLOB carrying no published .proto schema. A cwd→conversation map lives at
//
//	~/.gemini/antigravity-cli/cache/last_conversations.json   {"<cwd>":"<id>"}
//
// Capture therefore runs entirely through ReconcileOnce: it opens each
// conversation .db read-only, decodes every step's protobuf payload with the
// hand-rolled wire reader in protowire.go, classifies it by step_type, and
// ingests normalized events. There is no live bridge to install.
//
// Re-sleeve (Hydrate/ToNative) supports PRIME mode only — Antigravity has no
// documented foreign-transcript import path, and re-writing a native
// conversation .db (REPLAY) is unsupported. Same-CLI resume is delegated to
// the native CLI via `agy --conversation <id>`.
//
// Everything here is best-effort and honest about its fragility: the
// protobuf schema is undocumented and reverse-engineered, text extraction is
// heuristic, and the step_type→kind mapping was derived by inspecting real
// on-disk databases. See docs/design/round-9/03-antigravity-adapter.md.
package antigravity
