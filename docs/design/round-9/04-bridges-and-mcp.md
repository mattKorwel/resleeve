# Bridges and MCP across the fleet

Two cross-cutting questions, answered from ground-truth source reads
(opencode `dev` @ `3151e22`, codex @ `da490ba`, both dated 2026-06-05) plus
the claude adapter and the ori baseline (`round-1/01-ori-baseline.md`).

## What "bridge" means here

In ori, a **bridge** is an embedded plugin installed into a harness
(`~/.config/<harness>/plugins/` for cloudcode, shell hooks for gemini) that
on lifecycle events: writes a pointer + session record, heartbeats every
30s, sets `ended_at` on exit, and **injects inherited plan + learnings on
context compaction**. resleeve's `InstallBridge` is the same idea: hooks in
`~/.claude/settings.json` that POST events to the daemon and inject memory
at `SessionStart`.

So a bridge does **two** jobs, and they have different requirements:

1. **Capture** — get the session's events into resleeve.
2. **Injection** — push memory (plan + learnings) *into* the live session
   (at start / compaction), and trigger resume.

The headline finding: **capture needs no bridge on any of these CLIs**, but
**injection does** (it's inherently a write into the running agent). The
per-CLI table:

| CLI | Capture without a bridge? | How | Real-time without a bridge? | Injection needs | 
|---|---|---|---|---|
| **claude** | Yes (JSONL transcripts on disk) | file reconcile | No — needs the hook bridge | hook bridge (have it) |
| **codex** | **Yes** — rollout JSONL written passively every session | watch `$CODEX_HOME/sessions/**` | No — needs a `SessionStart`/`Stop` hook | hook bridge (`SessionStart` stdout → `additionalContext`) — **but gated by hook trust**, see below |
| **opencode** | **Yes** | read `opencode.db` (SQLite) **or** subscribe `GET /event` | **Yes** — `GET /event` SSE is a zero-config live channel | a **plugin** (config `plugin`) — SSE is read-only and can't inject |
| **antigravity** | Best-effort (undocumented brain dir) | file-watch | No reliable hook | content-seed via `GEMINI.md` (soft bridge) |

### Verdict per CLI

- **codex — bridge optional for capture, recommended for injection, with a
  trust caveat.** Rollout files are written unconditionally, so file-watch
  captures everything post-hoc with zero config. A hook bridge adds
  real-time signals and — the real prize — **memory injection**: a
  `SessionStart` hook's stdout is spliced into the model context as
  `additionalContext` (verified `core/src/hook_runtime.rs`,
  `session_start.rs:257`). **Caveat that bites the installer:** user/project
  hooks are **untrusted by default** and will not execute until the user
  trusts them via a TUI review (`hooks/src/engine/discovery.rs:520`). So an
  auto-installed codex bridge is inert until the user approves it once.
  There is also no durable per-hook id (`discovery.rs:495` TODO), so the
  installer must **own a dedicated `~/.codex/hooks.json`** file wholesale to
  stay idempotent rather than surgically editing a shared file.

- **opencode — uniquely needs no bridge for capture, even live.** The
  `GET /event` SSE stream (verified `server/.../groups/event.ts`) is a
  zero-config, real-time event bus an external process subscribes to
  without touching opencode's config. For *injection*, SSE is read-only, so
  memory-at-start needs an in-process **plugin** (the `event`/`chat.params`
  hooks in `packages/plugin`), OR — better for resleeve — skip injection
  and use the **prime/import resume path** to seed context (see
  `02-opencode-adapter.md`). v1 opencode ships **bridge-free**.

- **antigravity — no real bridge available.** No verified hook payloads;
  capture is fragile file-watch, injection is content-seeding a
  `GEMINI.md`/`.agents/` file the agent reads at start. Treat as a soft,
  best-effort bridge only.

- **claude — bridge as today.** Unchanged.

**Design consequence:** `InstallBridge` is no longer "the" capture
mechanism — it's the *injection + real-time* mechanism. Capture is always
available passively (file/DB/SSE). This lets `resleeve up` offer a
"capture-only, no bridge" posture per CLI, and reserve bridge installation
(and its trust prompts / plugin files) for users who want live memory
injection. It also matches the existing `--memory-only` bridge mode
(`docs/ARCHITECTURE.md`) — same lever, other end.

## Does MCP work across the board?

**As a tool-provider: yes, uniformly. As a capture/observability bus: no,
on none of them.** All three are MCP *clients*; none expose their own
session transcript over MCP for passive capture.

| CLI | MCP client (consumes tool servers)? | MCP server (exposes itself)? | Can MCP CAPTURE a transcript? |
|---|---|---|---|
| **codex** | Yes — `[mcp_servers.<name>]` in config.toml (stdio/http) | Yes — `codex mcp-server`, but it only lets a client **drive** Codex (`codex`/`codex-reply` tools returning `{threadId, content}`) | **No** — drive-only. (Live observation is possible via `codex app-server` JSON-RPC notifications, but that's not MCP and is connection-scoped — you must be the process that started/resumed the thread, not a passive side-channel.) |
| **opencode** | Yes — client only (verified: imports only `@modelcontextprotocol/sdk/client/*`, no server SDK) | **No** — cannot expose sessions | **No** |
| **antigravity** | Yes — first-class (`mcp_config.json`, `/mcp`) | No (not documented) | **No** — "not a session-capture/event bus" |

So MCP is **not** the cross-board capture or injection backbone. What it
*is* good for, uniformly, is the inverse: **resleeve exposing an MCP server
that all three CLIs can call as a tool.** Because every one of them is an
MCP client, a single `resleeve mcp` server offering memory tools
(`get_plan`, `list_learnings`, `record_learning`, `resume_context`) would
be callable from codex, opencode, and antigravity with one implementation —
a genuinely cross-CLI **memory** surface. Limits: it's *pull* (the agent
must choose to call the tool, vs. a hook that always injects at start), and
it still doesn't capture the transcript. ori already stubs exactly this
(`ori` "MCP server (stub; forwards to HTTP API)").

### Recommendation

- **Capture/resume stays per-CLI and native** (file/DB/SSE + native
  replay/import/resume). MCP is not load-bearing for it.
- **Bridges are for injection + real-time**, installed opt-in per CLI:
  claude hooks (have), codex hooks (with the trust caveat), opencode plugin
  (optional; prefer prime/import resume), antigravity content-seed.
- **Consider a single `resleeve mcp` memory server** as a *complementary*
  cross-CLI path for memory pull — one implementation, three consumers,
  no per-CLI hook trust friction. Scope it as its own round; it does not
  block round 9. It is the cleanest "works across the board" lever, just
  for memory, not capture.
