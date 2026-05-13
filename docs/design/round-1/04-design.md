# Design: open-source self-hosted multi-CLI session service

> **Update (2026-05-12):** This doc captures the initial design pass and its open questions. The committed decisions live in [`05-decisions.md`](./05-decisions.md) — project is now named **`resleeve`** and stands alone (not coupled to ori). §1 of this doc is historical; the rest still applies with that adjustment.

## TL;DR — the strategic picture

Three findings define the design space:

1. **Ori already owns the "where am I working" spine** — scope tree, pointer with heartbeat, fleet status, bridge plugins for cloudcode/gemini, HTTP API. What it does *not* own is the **content** of sessions — only the fact that a session happened. (Ori `internal/brain/types.go` session = `session_id, scope, broker, started_at, ended_at`.)
2. **Every target CLI except Aider stores append-mostly structured JSONL** with rich tool-call information. Sync is tractable. **Cross-CLI faithful resume is not** — Claude's `thinking.signature` blobs and Codex's reasoning items are vendor-encrypted and break on transplant.
3. **The market gap is real**: SpecStory is multi-CLI but its sync backend is proprietary SaaS; Atuin is self-hostable but shell-only; CCManager is local-only; Anthropic's own "Remote Control" is single-vendor, single-live-session. The intersection — multi-CLI capture + zero-knowledge self-hosted sync + documented schema — is empty.

**Recommendation: extend ori into this gap.** Add a session-content store and adapter system alongside the existing scope/pointer spine. One binary, one server, two storage backends matched to data frequency.

---

## 1. Relationship to ori — *extend, don't fork*

Ori's current `Session` type is a **pointer to** a session, not the session itself. The new work is to add the session body.

```
ori today          →   ori + sessions
─────────────────      ────────────────────────────────
scope tree         →   scope tree                  (unchanged, git-backed)
pointer/heartbeat  →   pointer/heartbeat           (unchanged, in-memory)
plan/learning      →   plan/learning               (unchanged, git-backed)
session pointer    →   session pointer             (unchanged) + session blob + event index (new)
cloudcode/gemini   →   cloudcode/gemini/codex/aider/oi adapters (extended)
```

**Why extend, not separate service:**

- Same auth, same server URL, same CLI binary — one self-host story.
- Cross-references already exist: `session_id` is already in ori's `Session` record.
- Avoids running two daemons; users already accept ori as the spine.

**Why two storage backends inside one binary:**

- Ori's git-as-storage is great for low-frequency spine writes (plans, learnings, pointer lifecycle) — pushes durability to a vault repo for free.
- It would be catastrophic for session events (hundreds per minute per session). Add a **SQLite + content-addressed blob store** for session bodies, kept separate from the git store. The `internal/brain/Store` interface already abstracts this — add a new `SessionStore` peer interface.

---

## 2. Data model — hybrid: opaque blob + normalized event index

Borrowing Langfuse's `Session → Trace → Observation` and OTel-GenAI's `gen_ai.conversation.id` vocabulary, mapped onto coding-agent reality:

```
Scope            (existing — ori spine: amplify/ori, byo-agents/managed-claw, ...)
  └─ Session     (NEW body: one CLI run)
        ├─ origin: cli name + version, cwd, git_branch, model, started_at, ended_at
        ├─ raw_blob: byte-for-byte original (~/.claude/projects/.../X.jsonl, etc.)
        ├─ events: normalized append-only stream  (NEW)
        │     └─ Event: { id, seq, ts, kind, role?, content?, tool_name?, ... }
        └─ sub_sessions: hierarchical (Helicone-style) — Claude Code subagents, dispatched runs
```

**Three storage layers, distinct durability stories:**

| Layer | Storage | Why |
|---|---|---|
| Raw blob | content-addressed file under `var/blobs/sha256/aa/bb/...` | Byte-exact rehydration; cheap to dedupe; format-agnostic |
| Event index | SQLite (`sessions.db`) | Search, listing, cross-CLI queries, sync cursors |
| Spine refs | git-backed (existing ori) | `Session` pointer record already lives here — extended with `blob_sha`, `event_count` |

The **event index is derived** — always rebuildable from the raw blob via an adapter. This is the cheat that lets us survive schema churn: if Anthropic adds a new JSONL record type tomorrow, we lose searchability for it until the adapter updates, but we never lose the data.

**Normalized event schema (proposed)** — align with OTel-GenAI semantic conventions where they exist:

```json
{ "seq": 142,
  "ts": "2026-05-12T22:13:04.221Z",
  "kind": "assistant_message" | "user_message" | "tool_call" | "tool_result" | "thinking" | "system",
  "role": "assistant",
  "model": "claude-opus-4-7",
  "content": [...] | "text",
  "tool": { "name": "Edit", "id": "toolu_01...", "args": {...}, "result_ref": "evt:143" },
  "raw_offset": 1842,         // byte offset into raw blob
  "vendor_opaque": { ... }    // signed/encrypted artifacts kept for round-trip resume only
}
```

`vendor_opaque` is the honest pressure valve — Claude's `thinking.signature`, Codex's encrypted reasoning items live here unread. Same-CLI resume gets them; cross-CLI resume drops them with a one-line warning.

---

## 3. Sync model — Atuin-style, append-only, eventually consistent

**Pattern:** monotonic per-host sequence counters, idempotent upserts on `(host_id, seq)`, last-writer-wins per record id (no real conflicts because session events are append-only and host-scoped).

```
POST /v1/sessions/{id}/events           { events: [...], host_id, host_seq }
GET  /v1/sync/since?host=&seq=          { events: [...], next_cursor }
GET  /v1/sessions?cwd=&cli=&q=          (search index)
GET  /v1/sessions/{id}/blob             (raw blob, content-encoding aware)
POST /v1/sessions/{id}/share            { ttl, redactions } → public_url
```

**Two transport modes:**

- **Batch sync** (default): file watcher (`fsnotify` on `~/.claude/projects/**`, `~/.codex/sessions/**`, `~/.gemini/tmp/*/chats/**`, registered repo `.aider.chat.history.md`, OI conversations dir). Debounce 2s, ship deltas.
- **Live stream** (opt-in): for CLIs we can hook directly. Claude Code's `SessionStart` / `PostToolUse` hooks via `settings.json`. Gemini CLI extension. `codex exec --json` wrapping. Pushes events as they happen.

**Auth + encryption** — Atuin model with the rough edge sanded down:

- Per-user account, password-derived key (Argon2id) — no manual key file to move between machines.
- Optional offline key file for paranoid mode.
- Server stores ciphertext only (raw blobs, event payloads). Search index stores **non-secret fields in plaintext** (timestamps, cli, model, cwd) so list/filter works without decrypting. Free-text search is client-side after decrypt.

This is the choice with the most product juice and the most risk — Atuin's #1 user complaint is the key-file UX. Worth investing in.

---

## 4. CLI adapter strategy

| CLI | Capture | Resume target | Notes |
|---|---|---|---|
| Claude Code | `SessionStart` / `PostToolUse` hooks → live stream; fallback `fsnotify` on `projects/` | `claude --resume <id>` (same machine) or reinject from blob | Highest fidelity; record `version` per line for adapter routing |
| Codex CLI | `fsnotify` on `~/.codex/sessions/`; handle `.zst` | `codex resume <id>` | Add zstd decode; track ThreadStore index format |
| Gemini CLI | `fsnotify` on `~/.gemini/tmp/*/chats/`; OSS extension path possible | `/chat resume <tag>` in-session | Project-hash mapping via `projects.json` |
| Open Interpreter | `fsnotify` on conversations dir, or Python wrapper | `%load_message` | Single global dir — needs cwd inference |
| Aider | `fsnotify` on `.aider.chat.history.md` per registered repo | `--restore-chat-history` | Treat as **write-only export target** — markdown re-parse is lossy; don't promise cross-CLI import *from* Aider |

**Defer until validated by real installs** (called out by the audit agent): Codex internal field set in current builds, OI on-disk shape, Aider multi-edit rendering. Adapters are versioned and feature-gated.

**Subagents:** Claude Code already emits to `subagents/` — model as `sub_sessions` in the schema. Don't flatten.

---

## 5. Deployment & stack

**Confirming the choice**: Go + SQLite, single statically-linked binary. Matches ori's existing stack exactly — no language boundary, can reuse `internal/server`, `internal/api`, `internal/cli` scaffolding.

```
ori serve                  # existing — extended with /v1/sessions/* routes
ori session sync           # NEW — background daemon watching CLI dirs
ori session list/show/search/resume/share  # NEW user-facing verbs
ori session adapter list   # NEW — show what's currently watching
```

**Datastore choices:**

- Spine: git (unchanged from ori).
- Session content: SQLite for index, filesystem for blobs (`var/blobs/sha256/...`). Postgres is an option later for multi-tenant team deployments — abstract behind the `SessionStore` interface.
- Don't choose Postgres now — the audit shows event volume is high but per-session, and a single dev's lifetime sessions comfortably fit SQLite.

**Self-host story:** `ori serve --data ./var` on a small VPS or a laptop. Container image as a convenience, not required.

---

## 6. Custom CLI — yes, but thin

The custom CLI is *not* an agent runner — it's a session manager that sits next to whatever CLI you actually use. Verbs (additions to ori, not a new binary):

- `ori session list [--cli=claude] [--cwd=...] [--scope=...]`
- `ori session show <id>` — pretty-print events
- `ori session search "<query>"` — full-text over decrypted index
- `ori session resume <id>` — launch the originating CLI with its native resume flag, or warn about cross-CLI fidelity loss
- `ori session share <id> [--ttl=24h] [--redact=secrets,paths]` — generate a public link
- `ori session export <id> --format={otel-genai,chatml,anthropic-messages,asciinema-like}`

Optional TUI (`ori session ui`) later — not v1.

---

## 7. What we explicitly punt on (v1)

- **Faithful cross-vendor tool-call resume** (Claude → Codex with extended thinking continuity). Vendor-encrypted artifacts make this impossible without provider cooperation.
- **Worktree/filesystem state snapshotting.** Out of scope. Sessions reference cwd + git_branch + SHA; rehydrating the *world* is a separate problem (and probably belongs in ori's start/dispatch flow, not here).
- **MCP server config sync.** Lives in per-CLI settings files; treat as separate config-sync concern.
- **Real-time multi-user editing** of a live session. Read-share, yes. Co-edit, no.
- **Aider import.** Aider's markdown export-only direction. Stick to it.

---

## 8. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Schema churn on CLIs (every minor release) | Per-record `version` capture (Claude already provides); adapters versioned & feature-gated; raw blob always preserved so re-indexing is always possible |
| Secrets in transcripts (API keys appearing in tool results / env dumps) | Client-side redaction pass before encryption with default rules; explicit per-share redaction; never index secret-suspect fields |
| Key-file UX (Atuin's known wart) | Password-derived key by default; offline key file only for paranoid users; document recovery cleanly |
| Codex `.zst` rollouts / format churn | Bundle zstd decoder; adapter integration tests against pinned sample sessions per CLI version |
| Self-host is a high bar | Single binary + `ori serve` + sqlite; no Postgres requirement; ship a Fly.io / Render template |
| Anthropic ships official cross-machine sync and obsoletes us | They'll ship single-vendor; this lives in the multi-CLI gap. Plan for *also work with* Anthropic's sync, don't compete with it. |

---

## 9. Open questions (resolved — see [`05-decisions.md`](./05-decisions.md))

1. **Naming & repo.** Does this live inside the `mattkorwel/ori` repo as `internal/sessions/...`, or as a sibling repo that imports ori as a library? Lean: same repo, same binary — the spine and body really are one product.
2. **Encryption stance.** Zero-knowledge server (Atuin model) from day one, or plaintext-on-server v1 and add E2E later? Zero-knowledge from day one is a much stronger market position but ~2× the v1 work.
3. **Aider scope.** Accept Aider as write-only export target (portability score 2), or invest in a markdown round-trip parser? Lean: write-only.
4. **Subagent depth.** Model Claude Code subagents as nested `sub_sessions` (real hierarchy) or flatten with parent_id? Lean: nested — matches the on-disk reality.
5. **First adapter to ship.** Claude Code is the obvious one (you use it, it has the richest format, audit fidelity is highest). Confirm, or pick differently?
6. **OTel-GenAI alignment.** How tightly should the normalized event schema track OTel's still-Development semconv? Lean: align field names where they exist, don't wait for stability.
