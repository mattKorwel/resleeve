# Decisions (closing round 1, evolving)

Locked-in decisions governing resleeve's design. Updated as the design matures.

## Name: `resleeve`

The Altered Carbon verb — moving a "stack" of consciousness into a different "sleeve." Maps onto re-hydrating an AI session into a different sleeve (compute environment).

Cleared availability checks: `npm`, `pip`, `brew`, `github.com/mattkorwel/resleeve` all free. SEO clean.

Tagline candidate: *"Persistent agent infrastructure: memory and sessions that re-sleeve anywhere."* (The "memory" half is aspirational for v1 — see below.)

## Primary persona: fleet operator

One human user, elastic compute (k8s, VMs, Docker, laptop or cloud). Fans out N agent tasks across N ephemeral sleeves via an orchestrator. **Sleeves are fungible; the stack is the persistent thing.**

This is the design's center of gravity. The other four personas (single-machine, cross-machine, cross-CLI, OSS-share) are valid but framed as degenerate or derivative cases:

- *Single machine* = fleet of size 1
- *Cross-machine* = stack re-sleeving onto the next pod
- *Cross-CLI* = orchestrator schedules different harnesses for different tasks
- *OSS contributor sharing* = a slicing/redaction feature, orthogonal to the primary

Detailed walkthrough: [`../round-2/01-journey-fleet-operator.md`](../round-2/01-journey-fleet-operator.md).

## Primary orchestrator target: SCION

`GoogleCloudPlatform/scion` (April 2026, Apache-2.0, experimental). Maps cleanly:

| SCION | Resleeve |
|---|---|
| Agent | Session |
| Grove | Scope (literal naming reuse) |
| Agent Token | Identity primitive (federated mode) |
| Runtime Broker | Sidecar host |
| Harness hooks (Gemini full / Claude partial / OpenCode none) | Bridge plugin + Pod sidecar fallback |

Resleeve = *"what the agent was thinking"* (session blob).
SCION's git worktree = *"what the agent did"* (filesystem result).
They pair; they don't compete.

Stay orchestrator-agnostic at the API layer; SCION is the named, tested-against integration.

## Relationship to ori: fold deferred; build resleeve fresh

Updated 2026-05-12 (later same day, again). Earlier in this conversation we committed to folding ori as a `memory/` module. After deeper inspection of `mattkorwel/ori`'s in-flight branch (`harness-serve-cloudcode`), the fold is **paused**.

**What changed our thinking:** `harness-serve-cloudcode` turned out to contain a mix of session/adapter work and memory work:

- **Session/adapter portion** (`internal/cloudcode/`, `internal/startplan/run_serve.go`, `internal/cli/verbs_serve_supervise.go`, bridge plugin deltas): an HTTP client for opencode's serve API, session create/resume, scope-context injection, process supervision. This is **proto-resleeve** wearing an ori jersey — months of work on exactly the problem resleeve's adapter layer needs to solve.
- **Memory portion** (`internal/scopeclass/`, `internal/classregistry/`, `internal/cli/verbs_class.go`): a "scope-class" system that's a real ori feature.

**Decision:**

- **Resleeve is built fresh** in its own repo (`github.com/mattkorwel/resleeve`).
- **`harness-serve-cloudcode` becomes architectural reference** for resleeve's adapter layer, not a migration source. Re-implement informed by the existing learnings (auth quirks, port preallocation race, dispose-doesn't-actually-shut-down behavior, session resume vs. create logic, etc.).
- **Ori continues on its own track.** `harness-serve-cloudcode` is no longer load-bearing for resleeve; it can land on ori main on whatever timeline makes sense for ori, or be deferred indefinitely.
- The eventual ori+resleeve unification is still the strategic story — but it happens **after** both projects are ripe, not before.

**For v1, resleeve ships sessions only.** The memory layer is on the roadmap, either by folding ori in later or by building memory natively. The vision survives; the v1 scope shrinks honestly to the session half.

Eventual structure (when fold happens, post-v1):

```
resleeve
├── session/     # v1 — session content capture + event index (SQLite + blob store)
├── adapters/    # v1 — per-CLI bridges (claude, opencode, codex, gemini)
└── memory/      # v2+ — scope tree, pointer/heartbeat, plans, learnings (git-backed)
```

## Identity & encryption

**Standalone mode** (laptop / single-user / non-orchestrated):
Password-derived key (Argon2id). Optional offline key file for paranoid mode. Server stores ciphertext only; non-secret index fields (timestamps, cli, model, cwd hash, message count) in plaintext for queryability.

**SCION mode** (fleet operator):
Identity federated via SCION Agent Token. Token validated by resleeve server; scope = `(grove, agent_name)`. Zero-knowledge data-at-rest still applies — the encryption key is now derived from an orchestrator-delivered secret rather than a user password.

Both modes ship in v1. The bridge plugin / sidecar auto-detects which mode it's in.

## Subagents: nested `sub_sessions`

Each subagent is its own Session record with `parent_session_id`. Recursive — a subagent can spawn its own subagent. Matches Claude Code's on-disk `subagents/` layout. Matches SCION's ancestry-chain model. Enables "share just this subagent's slice" cleanly.

## v1 adapters (priority order)

1. **Claude Code** — bridge plugin via `SessionStart` / `PostToolUse` hooks; sidecar fallback. Highest fidelity.
2. **sst/opencode** — sidecar pattern + serve-mode HTTP API (the `/session` create/resume + `/session/<id>/message` injection model already explored in ori's `internal/cloudcode/`, which referred to opencode under the older internal name "cloudcode"). Per SCION docs, opencode requires the plugin system to notify the orchestrator (no native hooks).
3. **OpenAI Codex CLI** — file watch on `~/.codex/sessions/` + zstd decode; live stream via `codex exec --json` wrapper.
4. **Gemini CLI** — bridge plugin via Gemini's rich hook set (`SessionStart`, `SessionEnd`, `BeforeAgent`, `AfterAgent`, ...).

**Deferred to v2:**

- **Aider** — write-only export target.
- **Open Interpreter** — structured JSON capture when prioritized.

## Schema: align with OTel-GenAI

Normalized event field names track OpenTelemetry GenAI semantic conventions where they exist (`gen_ai.conversation.id`, `gen_ai.request.model`, etc.). Spec is tagged "Development" but production-deployed by Langfuse, Phoenix, et al. Don't wait for "Stable."

**Net benefit:** `resleeve session export --format=otel-genai` pipes straight into observability tools.

## Storage backends — pluggable SQL from v1

Updated 2026-05-12 (still later same day). SQLite is the v1 default — perfect for self-host / single-machine. **PostgreSQL is a first-class alternative in v1** so users on any cloud (RDS, Cloud SQL, Supabase, Neon, etc.) can point resleeve at managed Postgres without forking.

MySQL, Turso, CockroachDB and friends are v2+. DynamoDB / Firestore / Mongo are out of scope (we need SQL joins, transactions, indexes).

Schema portability discipline (ANSI types only, no backend-specific operators in queries, JSON-as-`TEXT` with a portable jsonpath wrapper) is enforced from day one. Full design in [`../round-2/11-storage-backends.md`](../round-2/11-storage-backends.md).

## Carry-overs (round 2 and beyond)

- ~~Land `harness-serve-cloudcode` on ori main.~~ **No longer on resleeve's critical path.** Whatever happens to it happens on ori's track.
- Walk fleet-operator journey end-to-end (see [`../round-2/01-journey-fleet-operator.md`](../round-2/01-journey-fleet-operator.md)).
- Write additional round-2 journeys for the derivative personas.
- Validate current sst/opencode and Codex storage layouts against running installs.
- Extend `02-cli-audit.md` with an explicit sst/opencode section informed by ori's `internal/cloudcode/` learnings.
- Concrete schema doc: every event `kind`, required/optional fields, OTel-GenAI mapping.
- Adapter Go interface spec — let ori's `harness-serve-cloudcode` inform the shape but write fresh.
- Sidecar deployment pattern: container spec, env vars expected from SCION, fallback for non-SCION.
- Auth protocols: AEAD + KDF for standalone; Agent Token validation for SCION.
- Repo init: `go mod init github.com/mattkorwel/resleeve`, license (MIT vs. Apache-2.0), README.

## Changelog

- **2026-05-12 (even later same day):** Storage backend made pluggable from v1. SQLite default; PostgreSQL first-class alternative. Schema portability rules enforced from day one. Full design in `../round-2/11-storage-backends.md`.
- **2026-05-12 (later same day, again):** Fold of ori paused. `harness-serve-cloudcode` becomes architectural reference, not migration source. Resleeve built fresh. Memory layer becomes v2+ roadmap. Confirmed: "cloudcode" in ori's code = sst/opencode under older internal name.
- **2026-05-12 (later same day):** Pivot to persona-5 primary. Re-merged ori as `memory/` module. SCION as primary orchestrator target. Identity federates to SCION Agent Token when orchestrated. Reflected real cost of ori fold (months of WIP on `harness-serve-cloudcode`).
- **2026-05-12:** Initial decisions — name `resleeve`, independent from ori, zero-knowledge sync, subagents nested, v1 adapters Claude/opencode/Codex/Gemini, OTel-GenAI alignment.
