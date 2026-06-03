# Journey #1 design resolutions

Working through the 10 open design questions surfaced in [`01-journey-fleet-operator.md`](./01-journey-fleet-operator.md). Per matt's methodology, this is the **fully-featured target design**, not the v1 cut. v1 slicing comes later.

## Q1. Sidecar buffering durability — **resolved**

EmptyDir-equivalent local volume for buffer; harness-native session file (resident in the SCION-managed git worktree) as the cross-pod recovery source.

- Sidecar maintains `/var/resleeve/buffer/<session_id>.events` — append-only JSONL, ack/nack tracked per line.
- SIGTERM: 30s graceful drain to server.
- Hard kill: local buffer lost. Recovery on next pod's reconcile pass against the worktree-resident session file (always written by the harness regardless of resleeve, so always available cross-pod).
- Reconcile algorithm: on next sidecar boot for the same slot, read worktree session file → extract events → server merges using event-UUID dedup (Q2).

**Rejected:** persistent volumes per pod. Adds cost/complexity for every SCION user. Buffer + worktree fallback gives at-least-once delivery without paying for PVs. Worktree is already SCION's source of truth for "what the agent did"; we lean on it for free.

## Q2. Event dedup — **resolved**

Bridge plugin assigns event UUID at event-creation time, before any transport. Server dedups by `(session_id, event_uuid)`.

- Bridge plugin in the harness emits UUIDs as it generates events (live stream path).
- File-watcher safety net derives UUIDs **deterministically** from event content (canonical JSON + position offset). Same event written by the harness → same UUID, whether observed via live stream or file watch.
- Server: idempotent INSERT on `(session_id, event_uuid)` primary key. Late arrivals from any transport collapse.
- Idempotency window: forever. UUIDs are space-cheap; no expiry.

**Why not server-assigned UUIDs:** dedup becomes impossible for the file-watcher path (no UUID exists until server sees it). Bridge-assigned + content-hash-fallback is the one source of truth.

## Q3. "Same agent slot" identity — **resolved**

**Slot = `(scope, agent_name)`.** Sessions are runs; slots are the persistent worker identity across re-sleeves.

- SCION mode: `scope = grove`, `agent_name = $SCION_AGENT_NAME`.
- Standalone mode: `scope = cwd-derived or explicitly set` (default: directory basename), `agent_name = "default"` or `--agent=<name>` flag.

**Cardinality:**

- One slot can have many sessions over its lifetime (a re-sleeve creates a new session under the same slot).
- One scope can have many slots (fan-out: `auth-rewrite/claude-1`, `auth-rewrite/claude-2`, …).
- `resleeve session resume` operates on **slot**, not session: "give me the latest session for this slot, or none."

**Subagents** (per `05-decisions.md`, nested `sub_sessions`): not independently re-sleevable. They live within a parent session and die with it. They don't get their own slot.

## Q4. Standalone-mode parity — **resolved**

**Decision: TCP everywhere, with random port + discovery file (option (c)+ from the original options).**

- Daemon picks a random free port on start; binds `127.0.0.1:<rand>`.
- Daemon writes endpoint to `$XDG_RUNTIME_DIR/resleeve.endpoint` (content: a single line, `http://127.0.0.1:<rand>`).
- Bridge-plugin discovery order:
  1. `$RESLEEVE_AGENT_ENDPOINT` env var (set by SCION sidecar in orchestrated mode, or by `resleeve up` in standalone).
  2. Read `$XDG_RUNTIME_DIR/resleeve.endpoint` (standalone fallback if env var unset).
  3. Bridge plugin no-ops if neither resolves (per Q10).
- SCION sidecar: TCP on Pod loopback; endpoint passed via env var. Same protocol; different host.

**Why over UNIX sockets (option (b)):** unix sockets are aesthetically nice but require bridge plugins (JS / Python / Go) to support two transport flavors. Random port + discovery file gives 95% of the benefit (no port management, no collisions, per-user isolation via `XDG_RUNTIME_DIR` perms) at 50% of the implementation cost. One HTTP transport everywhere.

**On unixy-ness:** `$XDG_RUNTIME_DIR/resleeve.endpoint` is, in fact, the modern unix idiom for ephemeral runtime metadata (XDG Base Directory spec, post-2010). It just doesn't *look* like a socket. Aesthetics vs. ergonomics — we picked ergonomics.

**Cleanup:** daemon removes the endpoint file on graceful shutdown; stale files on next start are detected via stat-and-port-probe before overwriting.

## Q5. Lifecycle notification — **resolved (generalized to eventing system)**

**Decision: build a generic eventing system as a first-class resleeve subsystem. Resleeve emits typed events; consumers — including SCION, but not specifically — subscribe via webhooks. No orchestrator-specific code in resleeve core.**

This is bigger than journey #1; full design lives in a forthcoming `03-eventing-system.md`. High-level shape:

- **Internal event bus.** Every state change in resleeve emits a typed event. Single source of truth for "something happened."
- **Event taxonomy** (initial, extensible):
  - `session.started`, `session.event_added`, `session.ended`, `session.failed`
  - `slot.live`, `slot.idle`, `slot.stale`
  - `share.created`, `share.expired`
  - (later) `memory.plan_written`, `memory.learning_appended`, …
- **Webhook subscriptions:**
  ```
  POST /v1/subscriptions
    { events: ["session.ended", "session.failed"],
      url:    "https://scion-hub.example.com/webhooks/resleeve",
      filter: { scope: "auth-rewrite/*" },
      secret: "<shared-secret-for-HMAC>" }
  ```
- **Delivery semantics:** at-least-once. HMAC-signed payloads. Exponential backoff with bounded retry. Dead-letter queue for inspection.
- **Pull endpoint** stays available (`GET /v1/events?since=<cursor>`) for consumers that don't want to host an HTTP receiver.
- **SCION integration** = a SCION-side service (or future-resleeve sidecar feature) that subscribes to relevant events and translates them into SCION Hub activity updates. Lives outside resleeve core.

**What this gives us beyond journey #1:**

- Resleeve stays orchestrator-agnostic. SCION is one consumer; Cloud Run sandboxes, ADK, future orchestrators all plug in the same way.
- Cross-cutting use cases get free wins: *"auto-share on session_ended"* is just a consumer; *"notify Discord on session_failed"* is just a consumer; *"trigger CI on slot.ended"* is just a consumer.
- The eventing system becomes resleeve's primary extension point — third-party integrations don't need to fork the project.

**Why generalize now rather than v2:** the eventing system is the *primary integration surface* for everything outside the core (sessions, sharing, memory eventually). Building it from day one means SCION integration is a thin webhook consumer, not a one-off codepath. The "premature generalization" worry doesn't apply — this is the abstraction; SCION is the specialization.

**Open subsystem questions** (to resolve in `03-eventing-system.md`): retry policy and backoff schedule, max payload size, replay/backfill semantics, subscription auth model, event-schema versioning, fan-out limits.

## Q6, Q7. Memory module questions — **deferred to v2+**

No-op in v1 (sessions-only scope per `05-decisions.md` §"Relationship to ori"). Questions are preserved in `01-journey-fleet-operator.md` for when the memory module ships.

## Q8. Trust direction — **resolved**

Resleeve = resource server validating tokens minted by SCION (or any orchestrator).

- During `resleeve orchestrator register scion --hub-url=<url>`: SCION Hub provides its public key. Resleeve stores it as a trusted issuer.
- SCION mints Agent Tokens during `CreateAgent` (JWT). Claims: `iss=scion-hub-xyz`, `sub=agent-id`, `aud=resleeve`, `scope=(grove, agent_name)`, `exp=...`.
- Resleeve validates signature with the registered public key on every request.
- Token refresh: SCION rotates tokens; resleeve accepts new tokens signed by the same key.

**Why this direction:**

- Resleeve doesn't integrate with SCION's secret distribution path (which would couple us to SCION internals).
- Standard OAuth2/JWT semantics — works with any orchestrator that can mint signed tokens.
- Resleeve stays orchestrator-agnostic at the protocol layer; SCION becomes one configured issuer among potentially many.

## Q9. Sidecar image distribution — **resolved**

- Primary: `ghcr.io/mattkorwel/resleeve-sidecar:vX.Y.Z`.
- Mirror: `docker.io/mattkorwel/resleeve-sidecar:vX.Y.Z` (mirrored via CI).
- Tags: semver per release, plus `latest`, plus `vX-stable` rolling.
- Image is the same Go binary as the server, shipped with sidecar-mode entry point. Single artifact, multiple deploy shapes.
- If the project moves to a GitHub org, namespace shifts; legacy paths redirect.

## Q10. No-orchestrator-at-all auto-detect — **resolved**

Bridge plugin checks env vars; auto-selects mode.

```
if $RESLEEVE_MODE explicitly set       → use that
elif $SCION_AGENT_TOKEN present        → SCION mode (talk to $RESLEEVE_AGENT_ENDPOINT)
elif $RESLEEVE_AGENT_ENDPOINT set      → standalone-with-daemon mode
else                                   → bridge plugin no-ops; native session file is the only record
```

The "no resleeve" branch is important. Users who install resleeve on some machines but not all should not have the bridge plugin error out on the non-installed machines. **Silent no-op > loud failure** when resleeve is optional infrastructure.

---

## Summary of v1-relevant resolutions

| Q | Decision | v1? |
|---|---|---|
| Q1 | EmptyDir buffer + worktree fallback | ✓ |
| Q2 | Bridge-assigned UUIDs, server dedup on `(session_id, event_uuid)` | ✓ |
| Q3 | Slot = `(scope, agent_name)`; subagents nested but not slottable | ✓ |
| Q4 | TCP everywhere; random port + discovery file at `$XDG_RUNTIME_DIR/resleeve.endpoint` | ✓ |
| Q5 | Generic eventing system; webhook subscriptions + pull endpoint; SCION = one consumer. Full design in `03-eventing-system.md`. | ✓ |
| Q6 | Memory/session interplay | v2+ |
| Q7 | Concurrent memory writers | v2+ |
| Q8 | Resleeve = JWT resource server; SCION = trusted issuer | ✓ |
| Q9 | GHCR primary, Docker Hub mirror | ✓ |
| Q10 | Auto-detect via env var, silent no-op if no resleeve | ✓ |

## What this design now implies for the schema (preview)

The slot model + dedup model means the event schema needs:

- `session_id` (UUID, per run)
- `event_uuid` (UUID, per event — bridge-assigned or content-derived)
- `slot` (object: `{scope, agent_name}`)
- `seq` (monotonic 64-bit ordering value; ties broken by `event_uuid`. SQLite implementation uses `ts.UnixNano()` — see `04-event-schema.md`.)
- `ts` (timestamp)
- `kind` (assistant_message | user_message | tool_call | tool_result | thinking | system | session_start | session_end)
- … plus the OTel-GenAI-aligned content fields enumerated in the schema doc (next).

This drops naturally into `docs/round-2/03-event-schema.md` once Q4 and Q5 are settled.
