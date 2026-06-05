# Codex adapter — Tier A (full bidirectional)

OpenAI Codex CLI (`codex`, github.com/openai/codex, Rust). The closest
analogue to claude in the ecosystem: JSONL session files + a stdin-JSON
hook system + a native `resume`. This adapter mirrors the claude adapter
almost method-for-method.

> Basis: ground-truth source read of codex @ `da490ba` (2026-06-05),
> `codex-rs/{hooks,rollout,protocol,cli,exec,mcp-server,app-server}`.
> Re-verify against a live `codex --version` before implementation —
> Codex is fast-moving. See `04-bridges-and-mcp.md` for the bridge/MCP
> cross-cutting analysis.

## Two install-time gotchas (verified, design-shaping)

1. **Hook trust gate.** User/project hooks are **untrusted by default** and
   will be discovered but **not executed** until the user trusts them via a
   TUI review (`hooks/src/engine/discovery.rs:520-526`). An auto-installed
   `resleeve hook --adapter codex` bridge is therefore **inert until the
   user approves it once**. `install-bridge` must tell the user this and
   `doctor` must surface "codex bridge installed but untrusted → hooks are
   no-ops" (analogous to the existing claude "bridge ✓ daemon ✗" guard).
2. **No durable per-hook id** (`discovery.rs:495` TODO). Hooks are keyed
   positionally. So the installer must **own a dedicated `~/.codex/hooks.json`
   file** and write it wholesale (read → merge our block → write), never
   surgically edit a shared file. Uninstall = remove our entries / our file.

Capture itself does **not** require the bridge — rollout JSONL is written
passively for every session (`recorder.rs`), so file-watch reconcile
captures everything with zero config and no trust prompt. The bridge buys
**real-time signals + memory injection** (`SessionStart` stdout is spliced
into model context as `additionalContext`, `session_start.rs:257`). Treat
the bridge as opt-in for injection, file-watch as the always-on capture
floor. (Details: `04-bridges-and-mcp.md`.)

## Detect

- Binary: `codex` (`exec.LookPath`).
- Version: `codex --version` → `codex-cli <semver>` (e.g. `codex-cli 0.121.0`).
  Parse the trailing semver; tolerate the `codex-cli ` prefix.
- `CODEX_HOME` env overrides the home dir (default `~/.codex`). Detect must
  honor it for path derivation.
- Returns `Detection{Installed, Path, Version, Quirks{"codex_home": …}}`.

## Capture

Two routes, same as claude — live hooks (optimization) + file reconcile
(source of truth).

### Live: `resleeve hook --adapter codex`

`InstallBridge` writes `~/.codex/hooks.json` (wholesale, see gotcha 2) so
Codex shells `resleeve hook --adapter codex` on lifecycle events. The
config shape Codex parses (verified `config/src/hook_config.rs`):

```jsonc
// ~/.codex/hooks.json
{ "hooks": {
  "SessionStart":     [ { "hooks": [ { "type": "command",
                          "command": "resleeve hook --adapter codex" } ] } ],
  "UserPromptSubmit": [ { "hooks": [ { "type": "command", "command": "…" } ] } ],
  "PostToolUse":      [ { "hooks": [ { "type": "command", "command": "…" } ] } ],
  "Stop":             [ { "hooks": [ { "type": "command", "command": "…" } ] } ]
} }
```

Each group is a `MatcherGroup{ matcher?, hooks: []{ type:"command",
command, commandWindows?, timeout? } }`. (Skip `PreToolUse`/
`PermissionRequest`/compaction for v1 — no captured state. `async` and
`prompt`/`agent` handler types are not supported by Codex yet.)

`FromNative(raw, SourceHook)` decodes the per-event envelope (verified
`hooks/src/schema.rs`, snake_case, `deny_unknown_fields`). Field sets
differ per event:

| Event | Envelope fields | event.Kind | Notes |
|---|---|---|---|
| `SessionStart` | `session_id, transcript_path, cwd, model, permission_mode, source` (startup\|resume\|clear\|compact) — **no `turn_id`** | `KindSessionStart` | `transcript_path` points straight at the rollout JSONL |
| `UserPromptSubmit` | `…, turn_id, prompt` | `KindUserMessage` | |
| `PostToolUse` | `…, turn_id, tool_name, tool_input, tool_response, tool_use_id` | `KindToolCall` + `KindToolResult` | pair by `tool_use_id` |
| `Stop` | `…, stop_hook_active, last_assistant_message` | `KindSessionEnd` | |

`turn_id` → `event.TurnID`. Scope derived from `cwd`. A no-op observational
hook (exit 0, empty stdout) does not block or alter the turn (verified) —
the bridge stays read-only on the capture path. Hook envelopes carry
headlines, not full per-message content; the reconcile pass backfills full
fidelity from `transcript_path`.

### Backfill: rollout-file reconcile

Add `internal/adapter/codex/watcher.go` with `ReconcileOnce` modeled on
`claude/watcher.go`. Walk:

```
$CODEX_HOME/sessions/<YYYY>/<MM>/<DD>/rollout-<ts>-<uuid>.jsonl
```

Each line is a `RolloutLine`: `{ "timestamp", "type", "payload" }` where
`type ∈ {session_meta, response_item, compacted, turn_context, event_msg}`.

`FromNative(line, SourceJSONL)` per line type:

| `type` | Carries | event.Kind | Notes |
|---|---|---|---|
| `session_meta` | `id` (UUIDv7), `cwd`, `cli_version`, flattened `git{branch,commit_hash,repository_url}` | `KindSessionStart` | **session id + cwd + git branch live here**. No `model` on this line. |
| `turn_context` | per-turn `model`, `cwd`, `approval_policy`, `sandbox_policy` | `KindSystem` | **model is read from here**, not session_meta. First turn_context sets the session model. |
| `response_item` | OpenAI Responses-API item: `Message{role,content}`, function-call items, tool outputs | `KindUserMessage` / `KindAssistantMessage` / `KindToolCall` / `KindToolResult` | role/content/tool calls all live here |
| `event_msg` | UI events e.g. `{type:"user_message", message, kind}` | `KindUserMessage` (for `user_message`) | drop non-content UI events |
| `compacted` | compaction marker | `KindSystem` | record but don't expand |

Preserve the raw line bytes in `event.Vendor.NativePayload` for
byte-faithful replay. Deterministic event UUIDs via
`DeterministicEventUUID(sessionID, key)` where `key` is the response-item
id / turn id (mirror `claude/uuid.go`), so reconcile is idempotent under
`INSERT OR IGNORE`.

**Gotchas baked into the parser:**
- Session id is **UUIDv7** and appears both in the filename and in
  `session_meta.payload.id`; trust the in-file id (renames are safe).
- `model` comes from `turn_context`, never `session_meta`.
- Path is **date-sharded**, not flat (ignore the stale flat path in
  upstream doc-comments).

## Re-sleeve (replay + prime)

### Replay — codex → codex (full fidelity)

`ToNative(events, RenderModeReplay)`:
- Sort by `(seq, event_uuid)`.
- Emit one rollout line per event, preferring `vendor.NativePayload`
  verbatim. Synthesize `session_meta` / `turn_context` lines only when a
  needed payload is absent (e.g. a hook-only session never reconciled).
- Drop events that have no payload and no synthesizable native form.

`Hydrate(session, opts)` (replay):
- Compute the rollout path. The filename timestamp uses `-` for `:`
  (filesystem-safe). Derive dir `$CODEX_HOME/sessions/<YYYY>/<MM>/<DD>/`
  from the session's `StartedAt`; filename
  `rollout-<startedAt as YYYY-MM-DDThh-mm-ss>-<sessionID>.jsonl`.
- Write atomically (tempfile + rename), as claude does.
- Return `HydrateResult{Mode: replay, Path: …, SessionID: <same uuid>}`.

`NativeResumeCmd(session, result)` (replay):
- Return `("codex", ["resume", sessionID])`.
- (`codex resume <id>` binds to the original recorded cwd.)

**Replay-by-writing-a-file is confirmed feasible** (verified
`rollout/src/list.rs`): `codex resume <uuid>` resolves the id to a path,
and when there's no state-DB entry it **falls back to walking the
`sessions/` tree and matching the UUID out of the filename**
(`find_rollout_path_by_id_from_filenames`, `list.rs:1402`). So a
hand-written `rollout-<ts>-<uuid>.jsonl` is natively discoverable. Hard
requirements: filename UUID present; **first line a valid `session_meta`
whose `meta.id` equals that UUID** (the DB read-repair asserts this,
`list.rs:1294`); every line valid `RolloutLine` JSON.

**Fidelity invariant:** same as claude — the resumed codex session is
byte-equivalent up to the resume point when `native_payload` is present.

### Prime — codex as a target from another CLI

`Hydrate(session, opts)` with `Mode=prime` (auto-selected when source CLI
≠ codex):
- `common.SynthesizePrime(session, events, {PlanContent})` → markdown.
- Write to `~/.resleeve/hydrate/<new-uuid>.md`; mint a fresh SessionID.
- Return `HydrateResult{Mode: prime, Path: …, SessionID: <new>, Notes}`.

`NativeResumeCmd` (prime): use Codex's non-interactive prompt input.
`codex exec` reads a prompt from stdin via `codex exec -` (verified
`exec/src/cli.rs:81`):
- Unix: `("sh", ["-c", "codex exec - < <quoted path>"])`
- Windows: `("cmd", ["/c", "codex exec - < <quoted path>"])`

(Use `codex exec`, **not** a non-existent `--continue`. Split the
platform forms into `native_resume_unix.go` / `native_resume_windows.go`
exactly like claude.) Note an even cleaner option exists when the source
*is* codex but we want to graft a new instruction onto a resumed session:
`codex exec resume <uuid> "PROMPT"` (verified `ResumeArgs` carries a
trailing `prompt`, `exec/src/cli.rs:207`) — resume + inject in one call.

## Files

```
internal/adapter/codex/
  adapter.go              # Name, New, Detect, init()→registry.Register
  install.go              # InstallBridge / UninstallBridge (hooks.json, _resleeve-tagged)
  fromnative.go           # FromNative: hook envelope + rollout JSONL → events
  rollout.go              # RolloutLine / SessionMeta / TurnContext / ResponseItem structs
  tonative.go             # ToNative replay (rollout JSONL) + prime delegate
  hydrate.go              # Hydrate replay (sessions/Y/M/D path) + prime
  native_resume.go        # NativeResumeCmd dispatch
  native_resume_unix.go   # codex exec - < file
  native_resume_windows.go
  prime.go                # toNativePrime / hydratePrime (delegate to common)
  watcher.go              # ReconcileOnce over $CODEX_HOME/sessions/**
  uuid.go                 # DeterministicEventUUID
  doc.go
  *_test.go
```

Plus: register the codex reconciler with the daemon's startup reconcilers
(alongside claude's), gated on `Detect().Installed`.

## Tests

- `fromnative_test.go`: hook envelope → events; each rollout `type` → events;
  model-from-turn_context assertion; UUIDv7 round-trip.
- `tonative_test.go`: events → rollout JSONL, `native_payload` preserved
  byte-for-byte.
- `hydrate_test.go`: replay writes to the date-sharded path; prime writes
  scratch md + mints new id.
- `install_test.go`: idempotent install/uninstall of `_resleeve` hook
  entries, user entries preserved (use `t.TempDir()` + `CODEX_HOME`).
- Integration: synthesize a rollout file → `ReconcileOnce` → `ToNative`
  → diff against original (byte-exact where payload preserved).

## Open items to confirm on a live install

(Most of the original open items are now resolved by the source read; these
remain.)

1. **Hook trust UX**: the exact prompt/flow a user follows to trust a
   freshly installed hook, and whether a managed/enterprise layer can
   pre-trust it (affects whether `install-bridge` can ever be friction-free).
2. The precise `response_item` tool-call sub-shapes (Responses-API
   variants) for the replay synthesizer's *fallback* path (when
   `native_payload` is absent). Replay with payload present is byte-exact.
3. Whether `codex exec resume <id> "PROMPT"` is preferable to the
   stdin-pipe form for same-CLI prime (cleaner, but resumes the original
   session rather than minting a new one — decide the desired semantics).
