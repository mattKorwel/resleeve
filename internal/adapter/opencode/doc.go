// Package opencode is resleeve's adapter for SST opencode
// (github.com/sst/opencode, npm "opencode-ai").
//
// Target: the opencode `dev` / SQLite era ONLY. resleeve assumes
// ~/.local/share/opencode/opencode.db (override via $OPENCODE_DB) and the
// `opencode import` command exist. On a pre-SQLite build, Detect reports
// Installed:true with Quirks["unsupported"]="pre-sqlite" and the adapter
// refuses to capture / replay with a clear upgrade message rather than
// silently falling back to a legacy reader.
//
// Capture is bridge-free. There is no plugin and no config edit:
//   - Backfill (source of truth): ReconcileOnce opens opencode.db
//     READ-ONLY and reads SessionTable / MessageTable / PartTable
//     (sqlite_read.go).
//   - Live (optional): a GET /event SSE subscriber decodes the bus into
//     events (sse.go); only used when a server endpoint is reachable.
//
// InstallBridge / UninstallBridge are intentional no-ops returning nil
// (not ErrNotImplemented) — see install.go.
//
// Re-sleeve has two modes:
//   - Replay (opencode → opencode): ToNative emits the import JSON
//     ({info, messages:[{info, parts}]}), Hydrate writes it and runs
//     `opencode import`, NativeResumeCmd returns `opencode run --session
//     <id>`. opencode import preserves the provided session id
//     (verified import.ts: it inserts exportData.info.id).
//   - Prime (cross-CLI / replay fallback): common.SynthesizePrime →
//     markdown → `opencode run "<prompt>"` (positional; there is no
//     --prompt flag).
//
// See docs/design/round-9/02-opencode-adapter.md for the full design.
package opencode
