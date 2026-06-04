# Round 4 — Fleet Operator Cut

## What this round delivers

Round 4 designs the substrate that makes the locked **fleet-operator
persona** (`round-1/05-decisions.md`) actually shippable. The persona
is "stack persists, sleeve rotates": one human, elastic compute
(k8s / VMs / Docker / laptop / cloud) via an orchestrator, sessions
that outlive the worker that started them.

Two coupled systems, designed together because they are useless apart:

1. **Conversation-layer transport (Stage 5)** — a captured session can
   be re-hydrated into a fresh CLI instance, same machine or different,
   same CLI or different. Closes the gap that today resleeve "captures
   sessions but can't replay them."
2. **Cross-machine sync (v2)** — the substrate that moves captured
   state between machines so re-hydration on machine B can replay a
   session originally captured on machine A.

Without sync, Stage 5 is only useful in narrow local cases (CC's own
JSONL deleted, devcontainer rebuilt on the same host). Without Stage 5,
sync moves data without a way to use it cross-machine. Round 4 is
shipping them as one cohesive cut.

## Dependency map

Builds on v1 (already shipped, `e08c6fd` through `c7042ff`):

- `events` table + deterministic UUID dedup → push idempotency comes
  for free
- Memory module (`scopes`, `plans`, `learnings`) → the data being
  synced on the fast tier
- `internal/auth/` (Argon2id + KEK two-wrap, recovery key) → already
  implemented, just not yet wired into the lifecycle
- `Adapter` interface with `Hydrate` / `ToNative` / `NativeResumeCmd`
  stubs (`ErrNotImplemented`) → fleshed out in `01-conversation-transport.md`

Defers to future rounds:

- SCION federated identity (auth backend 2; round-1 locked SCION as the
  primary orchestrator target, but standalone ships first)
- Multi-user (single user remains for v2)
- Selective sync (per-scope subscriptions; v3+)
- Live websocket / sub-second latency (SSE is the upgrade ceiling for v2)
- Cross-CLI replay beyond `claude → opencode` stub
- Subagent fan-out across machines

## 8 locked decisions (from design dialogue)

| # | Decision |
|---|---|
| 1 | Conversation transport ships **both** replay (write JSONL, exec `claude --resume`) and prime (synthesize opening prompt). Replay for same-CLI fidelity, prime for cross-CLI universal. |
| 2 | **Two verbs**: `Hydrate` writes local state and returns the resume command string; `resleeve resume <sid>` is the convenience wrapper. |
| 3 | Cross-CLI translation is **lossy by design** — tool calls render as inline text in the opening prompt. v1 ships `claude→claude` for real plus a stub `claude→opencode` to prove the seam. |
| 4 | `resleeve serve` is the stable HTTP API; pluggable **`Backend` interface** behind it. v2 backends: **local-disk + object-store + git**. Cloud-DB deferred (subverts zero-knowledge for metadata). |
| 5 | Identity bootstrap: **standalone (Argon2id + master-password) first**; SCION mode layers on as a second auth backend. |
| 6 | **Two-tier sync.** Slow tier (sessions + events): push-on-commit + 30s pull + on-demand pull on explicit verbs. Fast tier (memory): SSE pull. Both share a **local outbox** for offline-then-online flush. |
| 7 | Multi-device pairing: **recovery key from machine A → master password on machine B**. CLI surface: `resleeve pair invite` / `resleeve pair accept`. |
| 8 | **MVP demo slice**: capture on machine A → `resleeve resume <sid>` on machine B → CC continues seamlessly. One user-visible verb exercises every layer. |

## Scope: what's synced, what isn't

| Layer | Synced? | Why |
|---|---|---|
| `sessions`, `events` | yes (slow tier) | Conversation history; cross-machine replay needs it |
| `scopes`, `plans`, `learnings` | yes (fast tier) | Operator's accumulated knowledge follows them |
| `slots` | no | Inherently per-machine (which session is currently live in this slot is local state) |
| `users` | no | Server-side identity table; clients don't pull user rows |
| Daemon endpoint + secret | no | Per-daemon ephemeral |

## Round 4 docs

- `00-fleet-operator-cut.md` (this file) — what ships, dependency map, locked decisions
- `01-conversation-transport.md` — Stage 5: `Hydrate`, `ToNative`, `NativeResumeCmd`
- `02-cross-machine-sync.md` — v2 sync: `serve`, backends, protocol, identity, pairing
- `03-rehydrate-ux.md` — the user-facing verbs and the MVP demo flow

## What this changes vs. the v1 cut

**Adds**

- `Adapter.Hydrate` / `Adapter.ToNative` / `Adapter.NativeResumeCmd`
  implemented (currently `ErrNotImplemented` stubs).
- `resleeve serve` — new binary surface (or `resleeve agent --serve` —
  decided in `02-...`).
- `Backend` interface + local-disk + object-store + git implementations.
- Sync subsystem in the daemon (push-on-commit, periodic pull, SSE pull).
- Encrypted outbox table in the local SQLite.
- Lifecycle: `resleeve login` / `logout` / `pair`, `resleeve resume`.
- Config file at `~/.resleeve/config.toml`.

**Doesn't change**

- Existing v1 CLI surface (capture, query, memory) — all preserved.
- Event schema (events still flow through the same wire types; sync
  just moves them).
- Memory module API (scopes / plans / learnings unchanged).
- Apache-2.0 license; OSS-first.
