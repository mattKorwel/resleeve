# Event schema

The on-disk and over-the-wire shape of events flowing through the **session firehose** (see [`03-eventing-system.md`](./03-eventing-system.md) for the two-pipe architecture). Hybrid model: every event carries both a normalized `content` block (uniform across CLIs, OTel-GenAI-aligned where possible) and a `vendor` block holding the original vendor-faithful payload for same-CLI reconstruction.

## Event envelope

Every event in the firehose:

```json
{
  "event_uuid": "01HZX...",
  "session_id": "01HZW...",
  "slot": { "scope": "auth-rewrite", "agent_name": "claude-3" },
  "seq": 142,
  "turn_id": "01HZY...",
  "parent_event_uuid": "01HZ8...",
  "ts": "2026-05-12T22:13:04.221Z",
  "kind": "tool_call",
  "schema_version": 1,
  "content": { ... },
  "vendor": {
    "name": "claude_code",
    "version": "2.1.140",
    "native_payload": { ... }
  }
}
```

| Field | Semantics |
|---|---|
| `event_uuid` | ULID, monotonic. Assigned by the bridge plugin at event creation; deterministic from content hash for the file-watcher safety net so dedup works on `(session_id, event_uuid)`. |
| `session_id` | ULID of the session this event belongs to. |
| `slot` | `(scope, agent_name)` tuple per `02-journey-01-decisions.md` Q3. |
| `seq` | Monotonic 64-bit ordering value (int64). The SQLite implementation populates this from `ts.UnixNano()` so events are naturally globally sortable without a per-session counter (no coordination, no `MAX(seq)+1` lookup). Same-nanosecond ties are broken by `event_uuid` ascending. Default ordering for replay. |
| `turn_id` | ULID identifying the model invocation that produced this event. Multiple events (one `thinking` + one `assistant_message` + two `tool_call`s) share a `turn_id` when they came from one Anthropic-style multi-block response. NULL for events not associated with a model turn (`user_message`, `system`, `tool_result`, `session_start`, `session_end`). |
| `parent_event_uuid` | Optional. References another event's UUID for DAG semantics (Claude Code's `parentUuid`). Most events don't need it. |
| `ts` | Event-generation timestamp (ISO 8601, UTC). |
| `kind` | Discriminator for `content` shape (see below). |
| `schema_version` | Payload schema version for `content`. Bumps only on breaking change. |

## Event kinds

### `user_message`

Human input to the model.

```json
"content": {
  "text": "fix the auth bug",
  "gen_ai.system": "claude_code"
}
```

### `assistant_message`

Text-only model output. Tool calls and thinking are *separate* kinds — this carries just the text portion of a turn. When a turn produces only tool calls and no text, no `assistant_message` event is emitted (only the `tool_call` events under the same `turn_id`).

```json
"content": {
  "text": "I'll start by reading the auth middleware.",
  "gen_ai.request.model": "claude-opus-4-7",
  "gen_ai.response.model": "claude-opus-4-7",
  "gen_ai.usage.input_tokens": 1842,
  "gen_ai.usage.output_tokens": 89,
  "gen_ai.usage.cache_read_tokens": 12450,
  "stop_reason": "end_turn"
}
```

### `tool_call`

Model invokes a tool.

```json
"content": {
  "tool": {
    "id": "toolu_01ABC...",
    "name": "Read",
    "args": { "file_path": "/src/auth.go" }
  },
  "gen_ai.request.model": "claude-opus-4-7"
}
```

`tool.id` is the cross-event linkage anchor (see `tool_result` below).

### `tool_result`

Output of a tool call.

```json
"content": {
  "tool": {
    "call_id": "toolu_01ABC...",
    "status": "success",
    "result": "package auth\n\nimport (...)..."
  }
}
```

- `status`: `success` | `error`
- `result`: rendered output text (always).
- `structured_result` (optional): vendor-shaped structured payload for tools that return JSON (e.g., MCP tools). Round-3 question to standardize.

### `thinking`

Extended-thinking / reasoning block. Readable text content only; encrypted signatures or vendor-opaque reasoning items live in `vendor.native_payload`.

```json
"content": {
  "text": "The auth bug likely lives in the JWT validation path...",
  "resleeve.thinking": true,
  "gen_ai.request.model": "claude-opus-4-7"
}
```

### `system`

System prompt, environment context, or tool/agent listings injected at session start or after compaction.

```json
"content": {
  "text": "...",
  "gen_ai.system": "claude_code",
  "resleeve.system.kind": "prompt"
}
```

`resleeve.system.kind` values: `prompt`, `compaction_summary`, `tool_listing`, `agent_listing`, `skill_listing`, `permission_mode`, `file_history_snapshot`.

### `attachment`

File, image, or blob attached to a turn.

**Small (≤ 256 KB) — inline:**

```json
"content": {
  "attachment": {
    "mime": "image/png",
    "size": 12480,
    "data": "iVBORw0KGgoAAAANSUh..."
  }
}
```

**Larger — by reference:**

```json
"content": {
  "attachment": {
    "mime": "application/pdf",
    "size": 4194304,
    "ref": "sha256:f3a8b1c..."
  }
}
```

`GET /v1/blobs/<sha>` returns the content (auth-gated). Threshold (256 KB) is configurable per deployment.

### `session_start`

Marker inside the firehose for session boot. Distinct from the domain bus's `session.started` event (which is a notification about *this* event).

```json
"content": {
  "cli": "claude_code",
  "cli_version": "2.1.140",
  "cwd": "/Users/.../",
  "git_branch": "main",
  "model": "claude-opus-4-7",
  "permission_mode": "default",
  "entrypoint": "cli"
}
```

### `session_end`

Marker inside the firehose for session shutdown.

```json
"content": {
  "exit_status": 0,
  "ended_at": "2026-05-12T22:13:04.219Z",
  "reason": "normal"
}
```

`reason`: `normal` | `failed` | `partial`.

### `error`

Unrecoverable failure mid-session.

```json
"content": {
  "message": "harness crashed: SIGSEGV",
  "stack": "...",
  "resleeve.error.kind": "harness_crash"
}
```

`resleeve.error.kind` values (initial): `harness_crash`, `adapter_failure`, `auth_revoked`, `quota_exceeded`.

## The `vendor` block

Every event carries:

```json
"vendor": {
  "name": "claude_code" | "opencode" | "codex" | "gemini" | "open_interpreter",
  "version": "2.1.140",
  "native_payload": { ... }
}
```

- `native_payload` is the **raw vendor record** that produced this event, byte-faithful. For Claude Code, this is one line from `~/.claude/projects/.../*.jsonl` with whatever fields Anthropic ships at that version. For Codex, one `ResponseItem`. For Aider (write-only target), this field is omitted on read paths and reconstructed from `content` on write paths.
- **Same-CLI re-sleeve** reads `native_payload` directly and reconstructs the harness's native session file.
- **Cross-CLI re-sleeve** ignores `native_payload` and uses `content`. Vendor-opaque artifacts (Claude's `thinking.signature`, Codex's reasoning items) are dropped with a one-line warning persisted on the new session.

## Cross-event linkage

| Relationship | How |
|---|---|
| Tool call → tool result | `tool_result.content.tool.call_id` references `tool_call.content.tool.id` |
| Events in one model turn | Shared `turn_id` (NULL for non-turn events) |
| Branch / fork (vendor-specific) | `parent_event_uuid` references prior event |
| Session ↔ event | `session_id` |
| Subagent → parent | `sub_session.parent_session_id` on the session record (not the event); subagent tool result may also carry `tool.spawned_session_id` |

## Ordering

- **Default:** sort by `seq` ascending, then by `event_uuid` ascending to break same-nanosecond ties.
- **DAG vendors (Claude Code):** traverse by `parent_event_uuid` if a tree view is desired. Most consumers use `seq`.

## OTel-GenAI mapping

Where OTel-GenAI semantic conventions have a field name for the thing we're capturing, use it. Where they don't, invent with the `resleeve.` prefix.

| OTel field | Resleeve location |
|---|---|
| `gen_ai.conversation.id` | `session_id` |
| `gen_ai.request.model` | `content.gen_ai.request.model` on assistant events |
| `gen_ai.response.model` | `content.gen_ai.response.model` on assistant events |
| `gen_ai.usage.input_tokens` | `content.gen_ai.usage.input_tokens` on `assistant_message` |
| `gen_ai.usage.output_tokens` | `content.gen_ai.usage.output_tokens` on `assistant_message` |
| `gen_ai.usage.cache_read_tokens` | `content.gen_ai.usage.cache_read_tokens` (resleeve extension; OTel may adopt later) |
| `gen_ai.system` | `content.gen_ai.system` (e.g., `claude_code`, `opencode`) |
| `gen_ai.execute_tool` (span) | derived: `tool_call` + `tool_result` pair sharing `tool.id` |
| `gen_ai.invoke_agent` (span) | the session itself |
| (no OTel equivalent) | `content.resleeve.thinking`, `content.resleeve.system.kind`, `content.resleeve.error.kind`, etc. |

Export: `resleeve session export <id> --format=otel-genai` emits OTel-compatible spans/events from the underlying event stream.

## Example: a complete model turn

User asks; model thinks → responds → calls a tool → sees the result. Five events, all under the same session, three sharing one `turn_id`.

```jsonl
{"event_uuid":"01HZ001...","kind":"user_message","seq":1,"turn_id":null,"content":{"text":"fix the auth bug"}}
{"event_uuid":"01HZ002...","kind":"thinking","seq":2,"turn_id":"01HZT001","content":{"text":"likely JWT validation...","resleeve.thinking":true}}
{"event_uuid":"01HZ003...","kind":"assistant_message","seq":3,"turn_id":"01HZT001","content":{"text":"I'll read the middleware first.","gen_ai.request.model":"claude-opus-4-7","gen_ai.usage.output_tokens":18}}
{"event_uuid":"01HZ004...","kind":"tool_call","seq":4,"turn_id":"01HZT001","content":{"tool":{"id":"toolu_AB","name":"Read","args":{"file_path":"/src/auth.go"}}}}
{"event_uuid":"01HZ005...","kind":"tool_result","seq":5,"turn_id":null,"content":{"tool":{"call_id":"toolu_AB","status":"success","result":"package auth..."}}}
```

(`vendor`, `session_id`, `slot`, `ts`, `schema_version` elided for brevity.)

## Schema evolution

Per [`03-eventing-system.md`](./03-eventing-system.md):

- **Additive only.** New fields in `content` are always optional with sensible defaults.
- **`schema_version` bump** is the escape hatch for breaking changes. Subscriptions can pin via `expects_schema`.
- **New kinds** are always additive — adding `tool_streaming_chunk` later doesn't affect existing consumers.

## Storage

- Event rows live in `sessions.db` (per `05-decisions.md`'s SQLite + blob store).
- `vendor.native_payload` may grow large; for v1, store inline in the event row. Future optimization: content-address and dedupe identical `native_payload` blobs.
- Attachments over 256 KB live in `var/blobs/sha256/aa/bb/...`.
- Indices: `(session_id, seq)`, `(session_id, turn_id)`, `(kind)`, and `(tool.id)` extracted via virtual column for cross-event linkage queries.

## Open questions for round 3

- **`structured_result` for tools:** standardize a `content.tool.structured_result` field for JSON-returning tools (MCP, etc.), or punt to `vendor.native_payload`?
- **MCP tool provenance:** which MCP server provided the tool? Add `content.tool.provider` (resleeve extension)?
- **Streaming partial events:** if an assistant message arrives in chunks (token-stream), do we emit partial events with a `partial: true` flag, or buffer until complete? Affects live-stream consumers — they want low-latency tokens.
- **Large `tool.args`:** what if a tool call has megabytes of arguments (e.g., `Write` with a huge file)? Apply the same hybrid storage threshold as attachments?
- **`permission_mode` transitions:** Claude Code emits `permission-mode` records that aren't really "system" content — possibly their own kind?
- **Native fork support:** Claude Code's `--fork` creates branched sessions. Lean: each branch is its own session_id with `parent_session_id`, like subagents — NOT a `parent_event_uuid` divergence on one session.
- **Cross-session references:** when session A's tool spawns session B (subagent), how does A's `tool_result` reference B? Lean: `tool_result.content.tool.spawned_session_id`.
- **Event compaction:** Claude Code summarizes old turns when context fills. Captured as a `system` event with `resleeve.system.kind: "compaction_summary"` — but do the *summarized-away* events stay in the stream, or get marked superseded?
