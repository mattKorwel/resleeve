# Prior art survey

## 1. Direct competitors — session managers for AI coding CLIs

- **SpecStory CLI** ([github.com/specstoryai/getspecstory](https://github.com/specstoryai/getspecstory)) — TypeScript, Apache-2.0. **Multi-CLI** (Claude Code, Cursor CLI, Codex, Droid, Gemini CLI). Saves AI conversations to local `.specstory/history` as markdown. **Active** (v1.12.0, March 2026). The CLI/local capture is OSS; cloud sync/search is a **proprietary SaaS** (SpecStory Cloud). Does well: capture + local-first markdown across many CLIs, agent-skills ecosystem on top of history. Doesn't solve: **no open self-hostable backend**; cloud is vendor-locked. **Single closest competitor.**
- **CCManager** ([github.com/kbwo/ccmanager](https://github.com/kbwo/ccmanager)) — TypeScript, MIT, ~747 stars. Multi-CLI session orchestrator (Claude/Gemini/Codex/Cursor Agent/Copilot/Cline/OpenCode/Kimi) across git worktrees. **Very active** (v4.1.15 dated May 12, 2026). Does well: local TUI for parallel session management, status (busy/idle/waiting), copy session data between worktrees. Doesn't solve: **no cross-machine sync, no shared backend, no replay/share**. Strictly a local launcher.
- **ccusage** ([github.com/ryoppippi/ccusage](https://github.com/ryoppippi/ccusage)) — TypeScript, ~9.7k stars, active. Reads `~/.claude/projects/*.jsonl` and reports tokens/cost. **Not** a session manager — analytics only. Useful prior art for *how to parse Claude Code's on-disk session format*.
- **claude-code-router** ([github.com/musistudio/claude-code-router](https://musistudio.github.io/claude-code-router/)) — ~25k stars, active. Proxies Claude Code to other backends (DeepSeek, Gemini, Groq). **Not session management** — model routing. Often mislabeled as "session" tooling because it touches the wire.
- **claudia / opcode** ([github.com/getAsterisk/claudia](https://github.com/getAsterisk/claudia)) — Tauri desktop GUI, AGPL-3.0. **Single-CLI** (Claude Code only). Active (v0.2.0 Aug 2025, ongoing commits). Does well: local GUI, timeline/checkpoints, "branch from checkpoint." Doesn't solve: **explicitly local-only, no sync**. AGPL is meaningful for downstream reuse.
- **Claudiatron** ([github.com/Haleclipse/Claudiatron](https://github.com/Haleclipse/Claudiatron)) — **archived/dead**. Repo notes a planned rewrite; not active prior art.
- **CloudCLI / claudecodeui** ([github.com/siteboon/claudecodeui](https://github.com/siteboon/claudecodeui)) — open-source web UI for Claude Code / Cursor CLI / Codex over the existing `~/.claude` directory. Active. Focused on **remote-control / mobile UI**, not history sync/sharing.
- **Claude Sync** ([dev.to article](https://dev.to/tawanorg/claude-sync-sync-your-claude-code-sessions-across-all-your-devices-simplified-49bl)) — community CLI that rsyncs `~/.claude` via encrypted cloud storage. Small/personal project, not a service.
- **Anthropic official** — Claude Code Web ([code.claude.com](https://code.claude.com/docs/en/claude-code-on-the-web)) and the Feb 2026 "Remote Control" feature sync **one live session** across phone/browser/terminal — but **no CLI-to-CLI handoff, no multi-machine history**. Issues [#31992](https://github.com/anthropics/claude-code/issues/31992), [#45358](https://github.com/anthropics/claude-code/issues/45358), [#47926](https://github.com/anthropics/claude-code/issues/47926), [#49657](https://github.com/anthropics/claude-code/issues/49657) on `anthropics/claude-code` confirm this is an **open user request, not shipped**. OpenAI/Codex and Google/Gemini CLI: no equivalent official cross-machine session sync.

## 2. Adjacent inspiration

- **Atuin** ([atuin.sh](https://atuin.sh/), [github.com/atuinsh/atuin](https://github.com/atuinsh/atuin)) — Rust, MIT. **Closest architectural analog.**
  - **Data model:** local SQLite replacing shell history, recording command + cwd + exit + duration + session id + hostname.
  - **Sync protocol:** HTTP to an Atuin server backed by Postgres; rows are E2E-encrypted client-side using a locally generated key (stored in `~/.local/share/atuin`); server is zero-knowledge.
  - **Conflict resolution:** monotonic per-host counters + idempotent upserts (CRDT-ish append log, last-writer-wins per record id).
  - **Auth:** username/password + key file you must move between machines.
  - **Self-host:** first-class — `atuin server start` with a Postgres URI.
  - **Lessons:** zero-knowledge sync is achievable and users expect it; "you must copy the key" UX is the main rough edge to beat.
- **asciinema** ([asciinema.org](https://asciinema.org/), [github.com/asciinema/asciinema](https://github.com/asciinema/asciinema)) — terminal recorder. `.cast` is tiny line-delimited JSON (header + `[t, "o", data]` events). Reusable: **the cast format itself** is a great template for a portable session-event file. Self-host server exists.
- **tmate** / **sshx** — live terminal sharing via reverse SSH or WebSocket. Reusable for "watch my agent run" but **not** for persisted/replayable history. Don't confuse the two problem domains.
- **MCP** ([blog.modelcontextprotocol.io/posts/2026-mcp-roadmap](https://blog.modelcontextprotocol.io/posts/2026-mcp-roadmap/), [modelcontextprotocol.io/specification/2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25)) — The 2026 roadmap explicitly calls out **stateless operation, standardized session creation/resumption/migration**, and `.well-known` server metadata. Today, MCP defines server-side session lifecycle but **says nothing about persisting or porting a user-facing agent conversation**. No "MCP session export" standard. A session service should align with MCP's resumable-session vocabulary but not wait for it.
- **Langfuse** ([langfuse.com/docs/observability/data-model](https://langfuse.com/docs/observability/data-model), [github.com/langfuse/langfuse](https://github.com/langfuse/langfuse)) — MIT, self-hostable via Docker/Helm. **Three-tier schema worth stealing:** `Session` → `Trace` → `Observation` (generation | tool-call | retrieval | span). Recently denormalized so session/user/metadata attributes are propagated onto every observation for single-table queries. Most field-tested data model for grouped LLM interactions; maps cleanly onto coding-agent sessions.
- **LangSmith** — proprietary; uses `Run → Thread → Project`. Less interesting for self-host but `Thread = grouped runs for one user request` matches multi-turn agent sessions.
- **Helicone** — adds **`Helicone-Session-Id` + `Helicone-Session-Path`** headers for hierarchical sub-sessions. Useful pattern for nested agent calls (parent session → spawned sub-agent).

## 3. Standards & schemas

- **OpenAI Chat Completions** message array (`role`/`content`) — the **de-facto** lingua franca; almost every provider has an adapter. Lossy for tool calls, reasoning blocks, multi-modal content.
- **Anthropic Messages API** — superset with content blocks (`text`, `tool_use`, `tool_result`, `thinking`, image). Richer but Claude-specific. Anthropic's OpenAI-SDK compatibility layer is explicitly "testing only, not production."
- **OpenTelemetry GenAI semantic conventions** ([opentelemetry.io/docs/specs/semconv/gen-ai/](https://opentelemetry.io/docs/specs/semconv/gen-ai/)) — Still labeled **Development** as of semconv 1.40.0 (April 2026). Defines `gen_ai.conversation.id` for session correlation, plus agent spans, events, and metrics. **The only cross-vendor schema with an actual standards body behind it** — Langfuse, Phoenix, Laminar, LangSmith all ingest OpenLLMetry/OTel-GenAI spans.
- **MCP** — no portable session export today; 2026 roadmap will define resumable-session semantics but not user-facing portability.
- **asciinema cast v2** — not LLM-specific, but cleanest "append-only event log" format around.

**Bottom line on standards:** There is **no portable AI-agent session schema**. The closest thing today is `OTel GenAI semconv + a Langfuse-style Session/Trace/Observation envelope`, with Chat Completions messages as the payload.

## What's actually missing

- **No open, self-hostable, multi-CLI session backend exists.** SpecStory has multi-CLI capture but ships a proprietary cloud; Atuin has the self-host story but is shell-only; Anthropic's sync is single-vendor, single-live-session. The intersection — multi-CLI capture + zero-knowledge self-hosted sync + a documented schema — is empty.
- **No portable session file format.** Every CLI invents its own. A small `.agentcast` / OTel-GenAI-aligned format that round-trips through Claude Code, Codex, and Aider would be genuinely novel and useful.
- **Replay and branching are unsolved cross-CLI.** opcode does checkpoint/branch for Claude Code only. Nothing lets you fork a Codex session into Claude Code at turn N.
- **Sharing primitives are absent.** asciinema-style "share this session at a URL with redacted secrets" doesn't exist for agent sessions. Today people paste screenshots.
- **Discovery/search across one developer's own history is weak.** ccusage analyzes cost; nobody offers "find the session where I solved the auth bug last March" across all CLIs and machines. Atuin proved this UX matters for shell commands — it matters more for AI sessions.

## Sources

- [SpecStory CLI](https://github.com/specstoryai/getspecstory), [docs](https://docs.specstory.com/quickstart)
- [CCManager](https://github.com/kbwo/ccmanager)
- [ccusage](https://github.com/ryoppippi/ccusage)
- [claude-code-router](https://musistudio.github.io/claude-code-router/)
- [opcode / claudia](https://github.com/getAsterisk/claudia)
- [Claudiatron (archived)](https://github.com/Haleclipse/Claudiatron)
- [CloudCLI / claudecodeui](https://github.com/siteboon/claudecodeui)
- [Claude Code on the Web](https://code.claude.com/docs/en/claude-code-on-the-web)
- [anthropics/claude-code #31992](https://github.com/anthropics/claude-code/issues/31992), [#45358](https://github.com/anthropics/claude-code/issues/45358), [#47926](https://github.com/anthropics/claude-code/issues/47926), [#49657](https://github.com/anthropics/claude-code/issues/49657)
- [Claude Sync (community)](https://dev.to/tawanorg/claude-sync-sync-your-claude-code-sessions-across-all-your-devices-simplified-49bl)
- [Atuin](https://atuin.sh/), [Atuin GitHub](https://github.com/atuinsh/atuin), [Atuin sync docs](https://docs.atuin.sh/cli/reference/sync/)
- [asciinema](https://asciinema.org/), [asciinema GitHub](https://github.com/asciinema/asciinema)
- [tmate](https://opensource.com/article/22/6/share-linux-terminal-tmate)
- [MCP 2026 roadmap](https://blog.modelcontextprotocol.io/posts/2026-mcp-roadmap/), [MCP spec 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25)
- [Langfuse data model](https://langfuse.com/docs/observability/data-model), [Langfuse GitHub](https://github.com/langfuse/langfuse), [self-host](https://langfuse.com/self-hosting)
- [LangSmith observability](https://www.langchain.com/langsmith/observability)
- [Helicone vs LangSmith](https://www.helicone.ai/blog/langsmith-vs-helicone)
- [OpenTelemetry GenAI semconv](https://opentelemetry.io/docs/specs/semconv/gen-ai/), [GenAI client spans](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-spans/), [GenAI agent spans](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-agent-spans/)
- [Anthropic Messages API](https://docs.anthropic.com/en/api/messages), [OpenAI SDK compatibility](https://docs.anthropic.com/en/api/openai-sdk)
