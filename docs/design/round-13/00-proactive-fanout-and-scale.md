# Round 13 — Proactive fan-out + scale

Shared brains (`round-11/02-multi-tenant.md`) need a change in brain X to reach
its other members *proactively* — otherwise a shared brain only updates on the
slow pull and feels dead. This round makes the existing real-time path
**brain-scoped** and lays out the **scale ladder**. It rides the same
`memberships` join; it does not add a new axis.

## What resleeve already does — notify-then-pull
The sync model is already the scalable shape:
- **Client → server:** outbox, push-on-commit (~1s drain).
- **Server → client:** an **SSE "fast tier"** — the daemon holds a long-lived
  SSE connection upstream; on change the server emits a tiny **notify**
  ("new data @ cursor"); the client does an immediate **pull**.
- **Fallback:** a 30s periodic pull catches anything SSE missed.

Two properties to preserve:
1. **SSE carries a cursor bump, not the payload.** Clients pull the actual
   (authorized) data. No fan-out amplification of big blobs.
2. **Correctness lives in the cursor pull, not SSE.** A dropped SSE connection
   loses notifies; the next pull catches up. The proactive layer is a
   **latency optimization over a correct pull baseline** — it degrades
   gracefully and must stay that way.

## The new requirement — brain-scoped fan-out
Today's SSE is over the global keyspace. For tiers 3–4 it must become:

> a write to brain **X** notifies **exactly the members of X**.

That's a fan-out keyed on the `memberships` table — the same join that does
partitioning. The server emits `"brain X @ cursor"` only to subscribers whose
membership includes X; each pulls, and authz returns only what they may see.
No new model, reuses round 11.

## Scale ladder
Match effort to tier; don't build Kafka for five friends.

| Stage | Who | Mechanism |
|---|---|---|
| **A — single instance** | tiers 1–3-small | today's SSE + SQLite/disk. Go holds thousands of idle long-lived SSE connections cheaply (goroutine each). **Sufficient.** |
| **B — horizontal scale-out / HA** | tier 4-bigger, tier 5 | SSE connections pin to one instance, so a write on instance A must reach subscribers on B → need a **pub/sub backplane**. First step: **Postgres `LISTEN/NOTIFY`** — *zero new infra* since these tiers already run Postgres. Plus Postgres serve-store + S3/GCS blob backend (the `Backend` interface is ready). |
| **C — outgrow Postgres NOTIFY** | large | swap the backplane for **NATS / Redis pub-sub**. Only if NOTIFY's throughput (generous — thousands msg/s) is exceeded. Kafka is overkill; avoid. |

### Operational details that bite SSE
- **Heartbeats** — reverse proxies / LBs buffer or time out idle SSE; emit
  periodic keepalive comments. (Confirm/instrument current behavior.)
- **Thundering herd** — a popular shared-brain write makes all members pull at
  once. Cursor pulls are cheap, but **coalesce/debounce** notifies if needed.
- **LB idle timeouts** + sticky routing for the connection lifetime.

## Webhooks — a *separate* feature, deferred
SSE is for resleeve's **own clients** (the daemons). **Webhooks are
server → *external* systems** ("post to Slack / hit CI / ping a dashboard when
a shared-brain learning is added"). Valuable for teams (tiers 4–5) but **not**
the sync transport, and they carry their own baggage:
- delivery guarantees + retries / dead-lettering,
- payload **HMAC signing**,
- **SSRF protection** — users configuring callback URLs on a shared server is
  a real attack surface; allowlist + block internal ranges.

Treat as a distinct, later feature with its own design; do not fold into the
sync path.

## What round 13 builds
1. **Brain-scoped SSE** — filter/route notifies by `memberships` (only emit a
   brain's cursor bumps to its members). Authz on the subsequent pull.
2. **SSE heartbeats + reconnect/backoff** hardening; debounce/coalesce.
3. **Pub/sub backplane seam** — an interface with a single-instance in-process
   impl (today) and a **Postgres `LISTEN/NOTIFY`** impl for scale-out. NATS/
   Redis behind the same seam, later.
4. (Prereq for B) **Postgres serve-store + S3/GCS blob backend** impls — may
   land with round 11/12's Postgres work; called out here as the scale gate.

## Out of scope for round 13
- **Webhooks** implementation (separate feature, separate design).
- **NATS / Redis / Kafka** backplane (until Postgres NOTIFY is outgrown).
- **Multi-region** replication.

## Tier coverage delivered
Round 13 makes shared brains feel **live** at tiers 3–4 (brain-scoped notify),
and provides the **scale-out path** (Postgres NOTIFY backplane + Postgres/S3
backends) for tier 4-big / tier 5 — without changing the notify-then-pull
contract or the brain/membership model.
