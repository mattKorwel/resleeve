# opencode adapter — Tier A (capture + native replay)

SST opencode (`opencode`, github.com/sst/opencode, npm `opencode-ai`).

> **Upgraded from the first draft.** Ground-truth source read of the `dev`
> branch @ `3151e22` (2026-06-05) overturned the research-summary
> assumptions this doc originally encoded. Two findings move opencode from
> "prime only" to **full capture + native replay**:
> 1. **Storage is now SQLite (Drizzle), not JSON files.** The plain-JSON
>    `storage/` layout is legacy/migration-only on `dev`.
> 2. **`opencode import <file>` exists** — a first-class command that
>    ingests a transcript JSON and writes a *fully resumable* session.
>    That is a native replay path; we don't need prime-only.

## Target: opencode `dev` (SQLite era) only — DECIDED

Decision (2026-06-05): resleeve targets the **`dev` branch / SQLite era
only**. No legacy-JSON support, no era-detection, no dual readers. The
adapter assumes `~/.local/share/opencode/opencode.db` and the `opencode
import` command exist. If they don't, `Detect` reports a clear
"unsupported opencode (pre-SQLite); upgrade to a `dev`/SQLite build"
error rather than silently falling back.

This collapses the design to a single path and lets the adapter lean on
`opencode import` for replay without hedging. (The legacy-JSON era is
out of scope — see "Out of scope" at the end.)

## Detect

- Binary: `opencode` (npm package `opencode-ai` — name mismatch).
- Version: `opencode --version` → bare semver. Record it; era detection
  keys off on-disk artifacts, not the version string (more robust).
- Data dir: `~/.local/share/opencode` (`Global.Path.data = xdgData/opencode`,
  verified `packages/core/src/global.ts:10`). `OPENCODE_DB` overrides the db
  path (`flag/flag.ts:45`); honor it for the reader.
- Require the SQLite era: `Detect` checks `opencode.db` (or `$OPENCODE_DB`)
  exists and `opencode import --help` is recognized. If not →
  `Detection{Installed:true, Quirks{"unsupported":"pre-sqlite"}}` and the
  adapter refuses to capture/replay with a clear upgrade message.
- `Detection{Installed, Path, Version, Quirks{"data_dir":…, "db_path":…}}`.

## Capture — zero bridge required

opencode needs **no plugin and no config change** to be captured. Two
channels; use SSE for live and the DB for backfill.

### Live (zero-config, real-time): `GET /event` SSE

`opencode serve` (default `127.0.0.1:4096`) exposes an SSE stream at
`GET /event` (verified `server/routes/instance/httpapi/groups/event.ts:8`).
First frame `server.connected`, then the full bus. The daemon subscribes
when a server endpoint is reachable and ingests in real time — **no bridge,
no plugin, no config edit.** This is opencode's standout property versus
codex/claude (which need an installed hook for liveness). Auth via
`OPENCODE_SERVER_PASSWORD`.

Relevant events (verified from source, `EventV2.define`): `session.created/
updated/idle/compacted/deleted`, `message.updated`, `message.part.updated/
delta`, and the new streaming engine `session.next.tool.called/success/
failed`, `session.next.tool.input.*`, `session.next.text.*`,
`session.next.step.*`. MCP tool calls **do** fire `tool.execute.*` on `dev`
(issue #2319 fixed — verified `session/tools.ts:121-151`).

### Backfill (source of truth): read `opencode.db`

`ReconcileOnce` opens the SQLite db read-only and reads `SessionTable`,
`MessageTable`, `PartTable` (verified `packages/core/src/session/sql.ts`).
Use a read-only connection (the daemon must never write opencode's db).

Schema (verified):
- **SessionTable** (`session/sql.ts:22-65`) — the session row itself carries
  `directory` (`DatabasePath.directoryColumn().notNull()`) and `path`. So
  **cwd is on the session**, not only the project (corrects the first draft).
- **ProjectTable** (`project/sql.ts:6-19`) — `worktree`, `vcs`, `name`.
- **PartTable** tool parts (`v1/session.ts:306`) — `type:"tool"`, `callID`,
  `tool` (name), `state` (discriminated on `status`:
  pending/running/completed/error; `completed` carries `input` + `output`).

Map to events:

| opencode row | event.Kind | Notes |
|---|---|---|
| SessionTable row | `KindSessionStart` | id, `directory`→cwd→scope, model, time |
| Message (role=user) | `KindUserMessage` | |
| Message (role=assistant) | `KindAssistantMessage` | text parts concatenated |
| Part `type:tool` | `KindToolCall` + `KindToolResult` | `state.input` / `state.output`, paired by `callID` |

IDs are time-sortable (`ses_`/`msg_`/`prt_` + 6-byte-hex-ms + base62;
verified `id/id.ts`), so ordering is cheap. Store raw row JSON in
`vendor.NativePayload` (audit + import round-trip). Deterministic event
UUIDs keyed on opencode's `msg_`/`prt_` ids → idempotent reconcile.

`InstallBridge` / `UninstallBridge`: **no-ops** returning nil with a note
("opencode capture is bridge-free: SSE + db read"). Not `ErrNotImplemented`.

## Re-sleeve — native replay via `opencode import`

opencode ships a sanctioned transcript-ingest command (verified
`packages/opencode/src/cli/cmd/import.ts`, registered `index.ts:132`):

```
opencode import <file>     # "import session data from JSON file or URL"
```

It decodes `{ info: Session, messages: Array<{ info: Message, parts: Part[] }> }`
(`transformShareData`, `import.ts:49-77`), validates each against
`SessionV1.Info` / `SessionV1.Part`, and inserts `SessionTable` /
`MessageTable` / `PartTable` rows (`import.ts:173-219`). The result is a
**fully resumable native session**.

### Replay — opencode → opencode (now possible)

`ToNative(events, RenderModeReplay)`:
- Emit the import JSON: `{info, messages:[{info, parts}]}`. Reconstruct
  `Session.Info`, per-message `Message.Info`, and `Part[]` from captured
  events, preferring `vendor.NativePayload` (the original row JSON) so the
  shape validates against `SessionV1.*` byte-for-byte where available.

`Hydrate(session, opts)` (replay):
- Write the import JSON to `~/.resleeve/hydrate/<sessionID>.json`.
- Return `HydrateResult{Mode: replay, Path:…, SessionID:<same or import's>}`.
  Note: `import` may mint its own session id; capture it from import's
  output and thread it into `NativeResumeCmd`.

`NativeResumeCmd` (replay): two-step — import then resume. Since
`NativeResumeCmd` returns a single command, model it as:
- `("sh", ["-c", "opencode import <path> && opencode run --session <id>"])`
  (unix; `cmd /c` on windows). Or have `cli` run `opencode import` itself
  during `Hydrate`, then return `("opencode", ["run", "--session", <id>])`.
- **Decision:** run `opencode import` inside `Hydrate` (it's a state-mat
  step, exactly what Hydrate is for), capture the resulting session id, and
  return the clean `opencode run --session <id>`. Mirrors how claude's
  Hydrate writes the JSONL before `NativeResumeCmd` returns `--resume`.

### Prime — opencode as a cross-CLI target (fallback)

When source CLI ≠ opencode, or `import` fails at runtime: fall back to
prime. `common.SynthesizePrime` → markdown → `opencode run "<prompt>"`
(positional; there is **no `--prompt` flag** — verified `cli/cmd/run.ts`).
For a multi-KB prompt, pipe via stdin if the positional arg proves
unwieldy (confirm on a live install).

Server-API alternative (not v1 default): `POST /session` then
`POST /session/:id/message` (prompt) or `/session/:id/prompt_async`
(both verified `groups/session.ts:95-96`). Resolve against `/doc` at
runtime if ever enabled.

## Files

```
internal/adapter/opencode/
  adapter.go        # Name, New, Detect (require sqlite era), init()→registry.Register
  install.go        # no-op InstallBridge/UninstallBridge (+ note)
  sqlite_read.go    # NEW: read-only opencode.db → events
  fromnative.go     # SSE event frame → events; + db reader dispatch
  sse.go            # NEW: GET /event subscriber (live)
  schema.go         # Session/Message/Part import-JSON structs ({info,messages:[{info,parts}]})
  tonative.go       # replay (import JSON) + prime delegate
  hydrate.go        # replay: write import JSON + run `opencode import`; prime fallback
  native_resume.go  # opencode run --session <id>  (replay) / run "<prompt>" (prime)
  watcher.go        # ReconcileOnce: read-only opencode.db
  doc.go
  *_test.go
```

Register the reconciler with daemon startup, gated on `Detect().Installed`
and SQLite-era support. SSE subscription is opt-in (only if a server
endpoint is reachable). The daemon's opencode db reader must be
**read-only** (never mutate opencode's store).

## Tests

- `sqlite_read_test.go`: a fixture `opencode.db` → ordered events;
  `directory` on session resolves scope; tool part `state.input/output`
  → call/result.
- `detect_test.go`: sqlite-era db present → supported; absent → clear
  unsupported/upgrade result (no silent fallback).
- `tonative_test.go`: events → import JSON validating against the
  `{info,messages:[{info,parts}]}` shape.
- `hydrate_test.go`: replay writes import JSON, invokes `opencode import`
  (mock the binary), returns `run --session`; prime fallback path.
- Defensive: import failure → fall back to prime with a Note.

## Open items to confirm on a live install

1. The exact `SessionV1.Info` / `SessionV1.Part` JSON `import` accepts
   (dump a real session via opencode's share/export and diff).
2. Whether `opencode import` mints a new session id or preserves the
   provided one (affects `NativeResumeCmd` id threading).
3. `GET /doc` for the authoritative event + endpoint list on the pinned
   `dev` build.

## Out of scope

- **Legacy plain-JSON opencode** (pre-SQLite releases). resleeve targets
  the `dev`/SQLite era only (decided 2026-06-05). On an unsupported build,
  the adapter detects and refuses with an upgrade message rather than
  shipping a second reader. Revisit only if a stable release ships the
  SQLite rewrite and we want to support an intermediate version.
