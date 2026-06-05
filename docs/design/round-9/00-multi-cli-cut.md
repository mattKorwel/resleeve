# Round 9 — Multi-CLI expansion (codex, opencode, antigravity)

Round 4 (`round-4/01-conversation-transport.md`) shipped the Stage-5
re-sleeve verbs with **claude** as the only full adapter and **opencode**
as a prime-only stub. Round 9 adds three real CLIs behind the existing
`Adapter` interface: **codex** (OpenAI Codex CLI), **opencode** (SST), and
**antigravity** / `agy` (Google Antigravity).

The `Adapter` interface does not change. Everything below is implemented
inside adapter packages plus two small registration edits.

## Decision: depth is mixed per-CLI

The first cut for each CLI is sized to what that CLI actually exposes, not
to a uniform target. Three capability tiers:

| Tier | Capture | Resume | Adapters this round |
|---|---|---|---|
| **A — Full bidirectional** | live signals + file/db reconcile (backfill) | native **replay** + prime fallback | codex, **opencode** |
| **B — Capture + prime** | native events + file backfill | **prime** only | (none — see below) |
| **C — Experimental** | best-effort file-watch, behind a flag | **prime** only, content-seed | antigravity |

> **Revised after ground-truth source reads** (`02`/`04`): opencode moved
> **B → A**. Its `dev` branch ships `opencode import <file>`, a first-class
> transcript-ingest that yields a fully resumable native session — so
> opencode gets **native replay**, not prime-only. And its capture needs
> **zero bridge** (`GET /event` SSE + read `opencode.db`). resleeve targets
> the opencode **`dev` / SQLite era only** (decided 2026-06-05); the
> legacy-JSON era is out of scope, so the Tier-B "prime-only" row is empty
> this round. Per-CLI bridge/MCP analysis lives in `04-bridges-and-mcp.md`.

Rationale (grounded in `round-9/0[1-4]-*.md`; codex/opencode verified
against cloned source @ 2026-06-05, antigravity from research only):

- **codex** is open source with date-sharded JSONL "rollout" files
  (`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`), a first-class hook
  system whose stdin envelope is nearly isomorphic to Claude Code's
  (`session_id`, `cwd`, `transcript_path`, `model`, `turn_id`), and a
  real `codex resume <id>` / `codex exec resume`. It earns full Claude
  parity: live capture via `resleeve hook --adapter codex`, replay by
  writing a rollout file and shelling `codex resume`.
- **opencode** (Tier A, revised) exposes a zero-config live event bus
  (`GET /event` SSE) and a durable **SQLite** store (`opencode.db`), so
  capture needs no bridge. And the `opencode import <file>` command ingests
  a transcript into a fully resumable native session, so **replay is
  native**, not prime-only. This upgrades the round-4 stub from "prime, no
  capture" all the way to "capture + replay". resleeve targets the
  **`dev`/SQLite era only**; an older (pre-SQLite) opencode is detected and
  refused with an upgrade message — no second reader (see `02`).
- **antigravity** is a fast-moving, partly-closed VS Code fork + `agy`
  CLI. Session transcripts live in an **undocumented** tree under
  `~/.gemini/antigravity*/brain/`, format not pinned (JSONL logs, possibly
  a SQLite index). Native resume (`agy --conversation=<uuid>`) only
  resumes Antigravity's *own* sessions — there is no documented way to
  inject foreign context as a resumable session, and clean headless ID
  emission is an open, unshipped request (their issue #7). So
  antigravity ships **experimental**: capture is file-watch behind a
  config flag, resume is prime-only content-seeding.

## What stays shared

- The normalized `event.Event` model, store, sync, and `common.Synthesize`
  Prime are unchanged. Every new adapter funnels into the same pipeline
  described in `round-2/04-event-schema.md` and `round-4/*`.
- Scope derivation (`.resleeve-scope` marker → else sanitized cwd) is
  reused verbatim; it is CLI-agnostic.
- Prime-mode output is identical across CLIs — `common.SynthesizePrime`
  already takes a `SessionView` + events and emits the markdown opening
  prompt. New adapters only differ in *where they write it* and *how they
  shell the target CLI to read it*.

## Registry refactor (prerequisite slice)

Today adapter selection is two hand-maintained `switch` statements:

- `internal/cli/hook.go:145` `pickAdapter(name)` — capture side.
- `internal/cli/resume.go:162` `hydrateForCLI(...)` — re-sleeve side.

Round 9 adds three names to each. Before that, collapse both into a single
registry to avoid drift:

```go
// internal/adapter/registry/registry.go
package registry

type Factory func() adapter.Adapter

var factories = map[string]Factory{}

func Register(name string, f Factory) { factories[name] = f }

func New(name string) (adapter.Adapter, bool) {
    f, ok := factories[name]
    if !ok { return nil, false }
    return f(), true
}

func Names() []string { /* sorted keys */ }
```

Each adapter package registers itself in an `init()`; `cli` imports the
registry instead of each adapter. `pickAdapter` and `hydrateForCLI` become
thin lookups (the opencode "force prime when Auto" rule moves into the
opencode adapter's `Hydrate`, where it already belongs). This is one
self-contained commit with no behavior change for claude.

## Sequencing

1. **Registry refactor** — no new CLIs, claude behavior unchanged. Lands
   first so the three adapters drop in without touching `cli` again.
2. **codex (Tier A)** — the highest-value, highest-confidence adapter and
   the one that exercises the full capture+replay path a second time
   (proves the interface generalizes beyond claude). See
   `01-codex-adapter.md`.
3. **opencode (Tier B)** — upgrades the stub; first real cross-CLI capture
   without a Claude-shaped hook. See `02-opencode-adapter.md`.
4. **antigravity (Tier C)** — experimental, flag-gated, lowest blast
   radius. See `03-antigravity-adapter.md`.

Each adapter is independently shippable and independently revertable.

## Registration points a new adapter touches

Per adapter, after the registry refactor:

- **Add** `internal/adapter/<name>/` (package — see per-adapter scope for
  file list).
- **Add** one `registry.Register("<name>", New)` in the package `init()`.
- **Add** a blank import `_ "…/internal/adapter/<name>"` in the `cli`
  package's adapter-wiring file so the `init()` runs.
- **Docs**: a row in the user-facing "supported CLIs" table + the
  install-bridge help text.

No interface changes, no migrations, no store changes.

## Out of scope for round 9

- **Cross-CLI replay** (e.g. resume a codex session *as* claude with full
  fidelity). Cross-CLI is always prime; native replay is same-CLI only.
- **opencode native transcript injection.** Revisit if upstream adds a
  "load session from file" API.
- **antigravity IDE (desktop) surface** and its VS Code `state.vscdb`
  layer. Round 9 targets only the `agy` CLI brain dir.
- **antigravity schema hardening.** The experimental adapter is explicitly
  allowed to break on upstream changes; it is feature-flagged off by
  default.
- **Subagent fan-out, forks, mixed-mode partial replay** — still deferred
  (carried over from `round-4/01`).

## Risks

| Risk | Mitigation |
|---|---|
| codex hook envelope drifts from documented fields | Reconcile path (JSONL file walk) is the source of truth; hooks are an optimization. Same belt-and-suspenders as claude. |
| opencode `/message` vs `/prompt` endpoint name varies by version | Resolve against the server's own `/doc` OpenAPI at runtime; pin nothing. Resume defaults to the `opencode run --session` CLI path, which is stable. |
| antigravity brain-dir schema undocumented / SQLite index appears | Adapter is flag-gated and explicitly experimental; capture failures are logged, never fatal. Validate against a live install before enabling. |
| Registry refactor regresses claude | Pure mechanical change with the existing claude tests as the regression gate; no event-shape changes. |
