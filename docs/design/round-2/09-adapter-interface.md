# Adapter Go interface

The contract every CLI adapter implements. This is the boundary that lets resleeve work with any harness: each adapter knows how to capture events from one CLI, install a bridge plugin, hydrate state for resume, and translate between native and normalized formats.

Informed by `mattkorwel/ori`'s `internal/cloudcode/` and `internal/startplan/run_serve.go` (architectural reference, not migration source per `05-decisions.md`).

## Interface

```go
package adapter

import (
    "context"
    "io"
    "time"
)

// Adapter is the contract for one CLI. Implementations live under
// resleeve/adapters/<name>/ (e.g., adapters/claude_code/, adapters/opencode/).
type Adapter interface {
    // Identity
    Name() string                                  // "claude_code", "opencode", "codex", "gemini", "open_interpreter"
    Detect(ctx context.Context) (Detection, error) // is the CLI installed? path, version, quirks

    // Bridge plugin lifecycle
    InstallBridge(ctx context.Context, opts InstallOpts) error
    UninstallBridge(ctx context.Context) error

    // Capture path — live stream + file-watcher safety net
    Capture(ctx context.Context, opts CaptureOpts) (<-chan Event, error)

    // Hydrate path — write native session file into a workspace so the CLI can resume
    Hydrate(ctx context.Context, session SessionView, target Workspace) error

    // Native-resume command — what to exec to actually resume the CLI in the target workspace
    NativeResumeCmd(session SessionView) (cmd string, args []string)

    // Translation — between normalized events and native file bytes
    ToNative(events []Event, target NativeFormat) ([]byte, error)
    FromNative(b []byte, source NativeFormat) ([]Event, error)
}
```

## Supporting types

```go
type Detection struct {
    Installed bool
    Path      string            // absolute path to the CLI binary
    Version   string            // semver or vendor-flavored version string
    Quirks    map[string]string // adapter-specific notes (e.g., "session_dir": "~/.codex/sessions")
}

type InstallOpts struct {
    DaemonEndpoint string // localhost endpoint the bridge plugin posts to
    Mode           string // "auto" or "explicit"; controls auto-start behavior
}

type CaptureOpts struct {
    SessionID   string
    Slot        Slot
    Live        bool  // open the harness's hook stream
    Reconcile   bool  // also scan native session files for events missed by the live path
    SinceSeq    int64 // skip events with seq <= this (used on reconcile)
}

type Slot struct {
    Scope     string
    AgentName string
}

type SessionView struct {
    SessionID    string
    Slot         Slot
    CLI          string
    CLIVersion   string
    Cwd          string
    GitBranch    string
    Model        string
    StartedAt    time.Time
    EndedAt      *time.Time
    EventStream  func() ([]Event, error) // lazy load
}

type Workspace struct {
    Cwd          string         // absolute path to materialize into
    HomeDir      string         // for harness's HOME-relative state dirs (e.g., ~/.claude/projects)
    Env          map[string]string
}

type NativeFormat struct {
    Vendor     string
    Version    string  // empty = latest known
}

type Event struct {
    EventUUID       string
    SessionID       string
    Slot            Slot
    Seq             int64
    TurnID          *string
    ParentEventUUID *string
    TS              time.Time
    Kind            string                    // see 04-event-schema.md
    SchemaVersion   int
    Content         map[string]interface{}    // normalized, OTel-aligned
    Vendor          Vendor
}

type Vendor struct {
    Name          string
    Version       string
    NativePayload []byte // raw bytes for round-trip fidelity
}
```

## Lifecycle of a typical adapter

```
                ┌── Detect ──┐
                │            │
   resleeve up ─┤            ├── InstallBridge (write hooks, drop plugin into ~/.claude/settings.json)
                │            │
                └── (path) ──┘

   CLI starts ────────────► bridge plugin POSTs to daemon
                                │
                                ▼
                        Adapter.Capture(live=true)
                                │
                                ▼
                       events flow → daemon → server

   CLI exits ──────────► bridge emits session_end

   On a different host, resume:
        resleeve session resume <id>
          → Adapter.Hydrate(session, workspace)    # materializes native file
          → cmd, args = Adapter.NativeResumeCmd(session)
          → exec cmd args                          # CLI continues from session
```

## Method-by-method notes

### `Detect`

Look for the binary on `$PATH`. Check version (`claude --version`, `codex --version`, etc.). Record quirks adapter-specific code needs to know — e.g., for Claude Code, the on-disk version field per-line is the per-event `vendor.version`; this can differ from the binary version if the user upgraded mid-session.

For `opencode` (per the `harness-serve-cloudcode` reference), Detect also probes for serve-mode availability and records the port-binding quirk.

### `InstallBridge`

Writes the bridge plugin / hook configuration:

- **Claude Code:** writes `SessionStart`, `PostToolUse`, `Stop` hooks to `~/.claude/settings.json`. Hook command points at the daemon endpoint.
- **Gemini CLI:** writes hooks to `~/.gemini/settings.json` (rich hook set per SCION research).
- **Opencode:** installs a JS plugin (no hooks; plugin notifies on session events).
- **Codex:** installs a shell wrapper / file-watch hook on `~/.codex/sessions/` (no native hooks).

Idempotent: re-running on the same machine should detect existing install and no-op (with `--reinstall` to force overwrite).

### `Capture`

Two paths in parallel:

1. **Live:** receives events from the bridge plugin in real time.
2. **Reconcile / safety net:** watches the harness's native session files on disk; emits events not yet seen via the live path.

Per `02-journey-01-decisions.md` Q2, events are deduped by `(session_id, event_uuid)` at the server. Adapters generate deterministic UUIDs from event content + position offset for the file-watcher path so dedup converges.

Returns a channel; closed when capture terminates.

### `Hydrate`

Given a `SessionView` (loaded from resleeve) and a `Workspace` (target host's cwd/home/env), reconstruct the harness's native session file in the workspace:

- For Claude Code: write to `<HOME>/.claude/projects/<sanitized-cwd>/<uuid>.jsonl` — JSONL lines reconstructed from `event.vendor.native_payload` where available, else translated from `event.content`.
- For Codex: write to `<HOME>/.codex/sessions/YYYY/MM/DD/<id>.jsonl(.zst)`.
- For Gemini: write to `<HOME>/.gemini/tmp/<projecthash>/chats/...`.
- For Aider (write-only, lossy): render markdown to `.aider.chat.history.md` in the cwd.

Hydration is **best-effort byte-faithful** when `vendor.name` matches the target's expected vendor. Cross-CLI hydration drops `vendor.native_payload` (different format) and synthesizes from `content` only — extended thinking continuity is lost; a warning event is appended.

### `NativeResumeCmd`

Returns the command + args to launch the harness in resume mode:

- Claude Code: `claude --resume <session_id>` (in the prepared cwd).
- Codex: `codex resume <session_id>`.
- Gemini: `gemini` followed by `/chat resume <tag>` in-session (handled via stdin injection on adapter side).
- Opencode: `opencode --session <id>` via the serve-mode endpoint (informed by `harness-serve-cloudcode` reference).

### `ToNative` / `FromNative`

Round-trip translation between normalized events and the harness's native session file bytes.

- `FromNative` is what file-watch / reconcile uses to ingest existing session files.
- `ToNative` is what `Hydrate` uses to produce native session files for resume.

Adapter is responsible for handling version differences in the native format (multi-version support via `NativeFormat.Version`).

## Per-adapter notes

### Claude Code adapter

- **Native format:** JSONL with parent-UUID DAG; rich content blocks; encrypted thinking signatures.
- **Hooks:** `SessionStart`, `PostToolUse`, `Stop` via `~/.claude/settings.json`. Hook commands `curl localhost:<port>/...`.
- **Capture quirks:** records `vendor.version` per event (Claude Code stamps every line with its version). Subagents are written under a `subagents/` subdirectory — emit as nested `sub_sessions`.
- **Hydration fidelity:** highest. Both `vendor.native_payload` and `content` carry full information.

### sst/opencode adapter

- **Native format:** TBD by v1 validation. Reference: ori's `internal/cloudcode/client.go` and `internal/startplan/run_serve.go` already implemented serve-mode HTTP client + session create/resume + scope-context injection.
- **Resume model:** via `cloudcode serve` HTTP API (`GET /session/<id>` to resume; `POST /session` to create; `POST /session/<id>/message` with `noReply:true` to inject context).
- **No native hooks:** capture is via the opencode plugin system; the plugin notifies on `assistant_message`, `tool_call`, `tool_result`, etc.
- **Auth quirks (from ori reference):** `cloudcode attach` hardcodes username; `OPENCODE_SERVER_PASSWORD` and `CLOUDCODE_SERVER_PASSWORD` both set defensively.
- **Process supervision:** ephemeral `opencode serve` subprocess managed by the adapter; supervisor goroutine handles teardown on session exit (per ori reference's `run_serve.go`).

### OpenAI Codex adapter

- **Native format:** date-sharded JSONL rollouts, recent versions zstd-compressed (`.jsonl.zst`).
- **No native hooks:** capture via file watch on `~/.codex/sessions/` + `codex exec --json` wrapper for live stream.
- **Hydration:** write to date-sharded path; handle zstd.
- **Schema churn:** Codex is actively evolving; adapter must handle rollout-format changes per release.

### Gemini CLI adapter

- **Native format:** JSONL auto-session + JSON checkpoint files in `~/.gemini/tmp/<projecthash>/chats/`.
- **Hooks:** rich set per Gemini docs (`SessionStart`, `SessionEnd`, `BeforeAgent`, `AfterAgent`, `BeforeTool`, `AfterTool`, `BeforeModel`, `AfterModel`, `PreCompress`, `Notification`).
- **Project hash:** `~/.gemini/projects.json` maps cwd → project hash dir; adapter resolves on hydrate.

### Aider adapter (v2, write-only)

- `FromNative`: not implemented (markdown round-trip is lossy).
- `ToNative`: renders normalized events to `.aider.chat.history.md` for cross-CLI hand-off into Aider.
- `Capture`: file-watcher only (no plugin); detect new content in `.aider.chat.history.md`; emit best-effort events with prose treated as `assistant_message` and edit-blocks treated as `tool_call`/`tool_result`. Acknowledged lossy.

### Open Interpreter adapter (v2)

- **Native format:** JSON files containing LMC `messages[]` array.
- **Capture:** file-watcher on conversations dir, or Python-API wrapper for live stream.
- **Hydration:** write `messages.json`; OI loads via `%load_message`.

## Versioning

Adapter interface is versioned via Go module semver. Breaking changes to the interface are released as major-version bumps. Each adapter declares the resleeve-adapter interface version it implements; the server validates compatibility at adapter-load time.

Per-adapter native-format versioning is internal — adapters handle CLI-version differences themselves.

## Testing contract

Every adapter ships with:

- **Fixture corpus:** representative session files captured from real runs of the target CLI, pinned per version.
- **Round-trip tests:** `FromNative(ToNative(events)) == events` for the lossless adapters; documented lossy bits for Aider.
- **Capture integration test:** spin up the actual CLI binary (where available in CI), exercise a minimal session, verify events captured.

## Open questions

- **Adapter discovery:** static-registered in code, or plugin-loadable at runtime? Lean: static for v1 (smaller surface, easier to test). Plugin loading is a v2 concern when external adapter authors exist.
- **Adapter versioning:** if the user upgrades Claude Code mid-session, does the adapter need to handle version transitions live? Lean: capture both versions, server reconciles on read.
- **Long-running adapter state:** does the adapter hold any state per session, or is it stateless? Lean: stateless on the resleeve side; bridge plugin handles per-session state in-harness.
- **What if `vendor.native_payload` is missing** on a `Hydrate`? E.g., for sessions ingested via cross-CLI import that didn't preserve it. Adapter must synthesize from `content` and tolerate the fidelity loss; warning event appended.
- **Test corpus management:** how do fixture files get refreshed when CLIs ship new event types? Lean: human-driven, version-pinned, with a checklist for each CLI's release cycle.
