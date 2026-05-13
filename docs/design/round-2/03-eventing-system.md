# Eventing system

How resleeve communicates state changes to external consumers — orchestrators (SCION first), integrations (auto-share, alerting), observability, future memory-module consumers. Fully designed per the design-then-MVP methodology; v1 slicing comes later.

## The two pipes

Resleeve exposes two distinct event surfaces:

**1. Session firehose** — per-session, intra-session events.

- Granularity: every individual event inside one session — `assistant_message`, `user_message`, `tool_call`, `tool_result`, `thinking`, `system`.
- Volume: hundreds per minute during an active session; bursty.
- Lives in: the session module.
- Access: `GET /v1/sessions/<id>/events?since=<cursor>` (pull) or `?stream=true` (SSE).
- Use cases: live monitoring of a single session, observability replay, cross-machine mirroring of one session's content.

**2. Domain bus** — system-level lifecycle events.

- Granularity: state transitions of system objects (sessions, slots, shares, subscriptions).
- Volume: 1–100 per minute across an entire deployment.
- Lives in: the eventing system (this doc).
- Access: webhook subscriptions + pull cursor endpoint.
- Use cases: orchestrator notifications, alerting, automated actions ("auto-share on session.ended"), future memory event consumers.

The split is honest about cardinality. Per-event granularity belongs in the per-session API; system-level granularity belongs in the cross-cutting bus.

A consumer that needs both subscribes to the domain bus for lifecycle markers AND streams the relevant session firehoses on demand.

## Domain event taxonomy

Naming convention: `<domain>.<past-tense-verb>`.

### v1 events

**Session lifecycle:**

- `session.started` — sidecar registered the new session; harness is ready.
- `session.ended` — session completed normally (graceful shutdown, exit 0).
- `session.failed` — session ended with a non-zero exit or unrecoverable error.
- `session.partial` — session marked partial after hard kill; awaiting reconcile.
- `session.reconciled` — partial session merged with worktree truth.

**Slot lifecycle:**

- `slot.live` — heartbeat <2 min
- `slot.idle` — 2–30 min since heartbeat
- `slot.stale` — >30 min, session not ended
- `slot.ended` — current session ended; slot quiescent

**Share lifecycle:**

- `share.created`
- `share.expired` — TTL hit
- `share.deleted` — manually removed

**Subscription meta-events (for introspection):**

- `subscription.created`
- `subscription.disabled` — auto-disabled after N consecutive failures
- `subscription.deleted`

### v2+ events

**Memory** (deferred per `05-decisions.md`):

- `memory.plan_written`, `memory.learning_appended`, `memory.scope_created`, …

### Rules

- Event types are extensible. New types are always additive.
- We do **not** emit per-session-event markers (e.g., `session.event_added` for every tool call). That would be too chatty for webhook consumers and is redundant with the session firehose for streaming consumers.

## Event payload shape

```json
{
  "event_id": "01HZX...",
  "type": "session.ended",
  "schema_version": 1,
  "ts": "2026-05-12T22:13:04.221Z",
  "scope": "auth-rewrite/claude-3",
  "data": {
    "session_id": "01HZW...",
    "slot": { "scope": "auth-rewrite", "agent_name": "claude-3" },
    "exit_status": 0,
    "ended_at": "2026-05-12T22:13:04.219Z"
  }
}
```

- `event_id` — ULID, monotonic across the deployment. Doubles as the cursor for pull/replay.
- `type` — event-name string.
- `schema_version` — payload schema for *this event type*. Default 1.
- `ts` — event-generation timestamp.
- `scope` — at the top level for filter convenience (subscription filters match on it).
- `data` — type-specific payload.

## Delivery mechanisms

### Webhook (primary, domain bus)

Consumer registers a subscription; resleeve POSTs to the consumer's URL on matching events.

- HTTP POST with JSON body containing one event per request.
- Headers:
  - `X-Resleeve-Event-Id`
  - `X-Resleeve-Event-Type`
  - `X-Resleeve-Signature` (HMAC-SHA256 of body using the subscription's shared secret)
  - `X-Resleeve-Delivery-Id`
  - `X-Resleeve-Cursor`
- Consumer responds with 2xx for success; anything else triggers retry.

**Retry policy:**

- Immediate retry, then exponential backoff: 30s, 2 m, 10 m, 1 h, 6 h, 24 h.
- After 7 retries OR N consecutive hours of failure (configurable, default **72 h**), subscription is auto-disabled and event moves to DLQ.
- Manual redelivery via `POST /v1/subscriptions/<id>/deliveries/<delivery_id>/redeliver`.

### Pull (always available, domain bus)

Consumer polls `GET /v1/events?since=<cursor>&filter=...` to fetch events incrementally.

- Returns events with `event_id > cursor`, ordered ascending.
- Page size configurable; next cursor returned for continuation.
- Useful for: consumers that can't host an HTTP receiver, periodic syncers, batch processing, post-incident replay.

### Live stream on domain bus (deferred)

SSE on `GET /v1/events?stream=true` — **deferred to v2.** Domain volume is low enough that webhook+pull covers known use cases. Additive when added.

### Live stream on session firehose (v1)

Available from v1 — but lives in the session module, not the eventing system:

```
GET /v1/sessions/<id>/events?stream=true
  → SSE stream of session events as they arrive
```

Documented path for live monitoring of a session's content. WebSocket intentionally not provided — one-way streaming; SSE suffices, with better proxy/browser compatibility.

## Persistence and replay

Every domain event is written to an append-only event log (small SQLite table):

```sql
CREATE TABLE events (
  event_id        TEXT PRIMARY KEY,    -- ULID
  type            TEXT NOT NULL,
  schema_version  INTEGER NOT NULL,
  scope           TEXT,
  ts              TEXT NOT NULL,
  data            JSON NOT NULL
);
CREATE INDEX events_by_type ON events(type);
CREATE INDEX events_by_scope ON events(scope, ts);
```

- **Indefinite retention.** Low volume (1–100/min × kB/row) makes this cheap. Operator can opt into retention later if data growth ever matters.
- **Pull cursor = `event_id`.** Webhook delivery includes the cursor as `X-Resleeve-Cursor` so consumers can resume from a known point after downtime.
- **Replay:** `GET /v1/events?since=<old_cursor>` returns everything since that cursor in order. Idempotent.

## Subscription model

### Endpoints

```
POST   /v1/subscriptions
GET    /v1/subscriptions                            # filtered to owner
GET    /v1/subscriptions/<id>
PATCH  /v1/subscriptions/<id>                       # url, filter, enabled
DELETE /v1/subscriptions/<id>

GET    /v1/subscriptions/<id>/deliveries            # delivery history
GET    /v1/subscriptions/<id>/deliveries/<did>      # one delivery
POST   /v1/subscriptions/<id>/deliveries/<did>/redeliver
GET    /v1/subscriptions/<id>/dlq
```

### Create payload

```json
{
  "url": "https://scion-hub.example.com/webhooks/resleeve",
  "events": ["session.ended", "session.failed"],
  "scope": "auth-rewrite/*",
  "secret": "<hmac-shared-secret>",
  "description": "SCION Hub lifecycle ingress"
}
```

`secret` is optional; resleeve generates one and returns it on create if omitted.

### Filter shape

Two dimensions:

- **`events`** — array of event-type strings (exact match) or globs within type (e.g. `session.*`).
- **`scope`** — glob over the event's `scope` field:
  - `auth-rewrite` (exact)
  - `auth-rewrite/*` (one segment)
  - `auth-rewrite/**` (any depth)
  - omitted = all scopes the user can access

A subscription with `events: ["*"]` and no scope filter receives every event the user has scope access to.

### Auth model

Subscriptions are owned by the user/agent who created them.

- Each subscription is tagged with the creator's identity (user-token `sub`, or SCION Agent-Token `sub`).
- Users can only filter on scopes they have access to. The auth layer checks scope claims against the subscription filter at create-time AND per-event delivery time.
- SCION Agent Tokens carry scope claims of the form `scope: "<grove>/<agent_name>"` — the agent can subscribe to its own grove's events.
- Single-user self-host: one user, all scopes — auth is mostly a formality, but the mechanism is in place from day one.

### Lifecycle

- Created → enabled.
- **Auto-disabled** after configurable N hours of consecutive delivery failures (default 72 h).
- Re-enabled manually via `PATCH {"enabled": true}`.
- Hard-deleted via `DELETE` (irreversible; deliveries history retained for audit, marked as orphaned).

## Schema versioning

**Rule:** new fields in event `data` are always optional with sensible defaults. Old subscribers continue working without changes.

**Escape hatch:** every event includes `schema_version: <int>` for its type. If we ever truly must break compat (rare), bump the version. Subscriptions can pin via `expects_schema: { "session.ended": "<=1" }` to receive only old-version payloads. Default: latest version delivered.

Event *types* themselves are namespaced (`session.ended`); adding `slot.preempted` later doesn't touch `slot.ended` subscribers.

## Worked example: SCION integration

How a non-resleeve-specific consumer plugs in.

1. SCION operator runs a small SCION-side service ("SCION Resleeve Bridge") on Hub infrastructure.
2. The Bridge calls `POST /v1/subscriptions`:

   ```json
   {
     "url": "https://hub.example.com/webhooks/resleeve",
     "events": ["session.started", "session.ended", "session.failed"],
     "scope": "**"
   }
   ```

3. Resleeve fires webhooks to the Bridge on matching events.
4. The Bridge translates resleeve events into SCION Hub activity updates (`POST /agents/<id>/activity` on the SCION Hub API).
5. SCION operators see resleeve-tracked agents converge / fail in their Hub UI.

Resleeve has no SCION-specific code. The Bridge is the orchestrator-specific glue.

Same shape for Cloud Run sandboxes, ADK, Discord notifiers, an auto-share daemon, a CI trigger, etc.

## Open questions for round 3

- Concrete authentication mechanics for `POST /v1/subscriptions` — which tokens are accepted at subscription-creation time (user PAT? SCION Agent Token? both?).
- Subscription-quota model — max subscriptions per user; max delivery-rate per subscription.
- Bulk redelivery — what if 1000 events failed during a long outage; one-shot replay vs. paginated?
- Cross-subscription delivery ordering — within a subscription, ordered; across subscriptions, unordered. Confirm this is the right invariant.
- Test/dry-run subscriptions — a way for a developer to verify their webhook handler without polluting real audit logs.
- Webhook signature *rotation* — how does a consumer rotate the shared secret without missing events?
