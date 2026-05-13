# API surface

Every HTTP endpoint resleeve exposes, consolidated. Two surfaces:

- **Server API** — what's exposed by `resleeve serve`. The durable surface.
- **Daemon API** — what `resleeve agent` (the local daemon / sidecar) exposes on loopback for bridge plugins. Mostly a relay/proxy of the server API plus a small set of local-only endpoints.

All endpoints are JSON unless noted. Authentication required unless marked `(public)`.

## Conventions

- Base path: `/v1/` for the stable API; `/v0/` for experimental.
- Authentication: `Authorization: Bearer <token>`. Tokens come from `/v1/auth/login` (standalone) or an orchestrator (SCION-style).
- Errors: RFC 7807 problem-details (`{"type": ..., "title": ..., "detail": ..., "status": ...}`).
- Pagination: cursor-based via `?cursor=<opaque>` query param; response includes `"next_cursor"` when more pages exist.
- Time: ISO 8601 UTC.
- IDs: ULIDs (sortable, monotonic).

## Server API

### Sessions

```
POST   /v1/sessions                              # create (bridge plugin at session_start)
GET    /v1/sessions                              # list — filter by scope, slot, status, since, vendor, model
GET    /v1/sessions/<id>                         # session metadata
DELETE /v1/sessions/<id>                         # delete (irreversible)
POST   /v1/sessions/<id>/end                     # mark ended (lifecycle)
POST   /v1/sessions/<id>/reconcile               # trigger reconcile from worktree (after hard kill)
POST   /v1/sessions/<id>/hydrate                 # produce native session file content for a target host
GET    /v1/sessions/<id>/blob                    # raw byte-faithful blob
```

#### Session events

```
POST   /v1/sessions/<id>/events                  # append events (batch; bridge/sidecar)
GET    /v1/sessions/<id>/events                  # pull events — cursor, kind filter, turn_id filter
GET    /v1/sessions/<id>/events?stream=true      # SSE live stream of session events
```

#### Subagent (nested) sessions

```
POST   /v1/sessions/<id>/sub                     # create a nested sub-session
GET    /v1/sessions/<id>/sub                     # list sub-sessions
```

### Slots

```
GET    /v1/slots                                 # list — filter by scope glob, status
GET    /v1/slots/<scope>/<agent_name>            # slot detail with latest session + heartbeat
POST   /v1/slots/<scope>/<agent_name>/heartbeat  # heartbeat (bridge/sidecar)
DELETE /v1/slots/<scope>/<agent_name>            # retire slot explicitly
```

### Domain events (eventing system)

```
GET    /v1/events                                # pull — cursor, type filter, scope filter
                                                 # (?stream=true deferred to v2)
```

### Subscriptions

```
POST   /v1/subscriptions                         # create
GET    /v1/subscriptions                         # list (filtered to owner)
GET    /v1/subscriptions/<id>                    # get
PATCH  /v1/subscriptions/<id>                    # update (url, filter, enabled)
DELETE /v1/subscriptions/<id>                    # delete

GET    /v1/subscriptions/<id>/deliveries         # delivery history
GET    /v1/subscriptions/<id>/deliveries/<did>   # one delivery's detail
POST   /v1/subscriptions/<id>/deliveries/<did>/redeliver
GET    /v1/subscriptions/<id>/dlq                # dead-letter queue
POST   /v1/subscriptions/<id>/dlq/<did>/redeliver
```

### Shares

```
POST   /v1/shares                                # create share from session (with redact + ttl)
POST   /v1/shares/preview                        # preview redacted output without publishing
GET    /v1/shares                                # list (filtered to owner)
GET    /v1/shares/<id>                           # share metadata
DELETE /v1/shares/<id>                           # revoke

GET    /s/<shortcode>                            # (public) HTML view
GET    /s/<shortcode>?format=md                  # (public) Markdown view
GET    /s/<shortcode>?format=tar.gz              # (public) archive download
GET    /s/<shortcode>/og.png                     # (public) OpenGraph preview image

POST   /v1/shares/import                         # import a share (from any deployment) as a session
```

`/s/<shortcode>` access depends on share access level (public / password / allowlist).

### Blobs (content-addressed storage)

```
GET    /v1/blobs/<sha>                           # fetch
POST   /v1/blobs                                 # upload (idempotent on sha; body returns the sha)
```

### Auth

```
POST   /v1/auth/signup                           # (public) create account
POST   /v1/auth/login                            # (public) password challenge → access token
POST   /v1/auth/recover                          # (public) recovery key → reset password
POST   /v1/auth/rotate-recovery                  # generate new recovery key (invalidates old)
GET    /v1/auth/whoami                           # current identity + scope claims
POST   /v1/auth/logout                           # revoke current token
GET    /v1/auth/tokens                           # list active tokens for current user
DELETE /v1/auth/tokens/<id>                      # revoke a specific token
```

### Orchestrator integration

```
POST   /v1/orchestrators                         # register an orchestrator (e.g., SCION Hub public key)
GET    /v1/orchestrators                         # list registered
GET    /v1/orchestrators/<id>                    # detail
DELETE /v1/orchestrators/<id>                    # de-register
POST   /v1/orchestrators/<id>/test               # round-trip test with the orchestrator
```

### Memory (v2+, deferred)

```
GET    /v1/memory/scopes                         # list scopes
GET    /v1/memory/<scope>                        # scope metadata + inherited content
PUT    /v1/memory/<scope>                        # update scope
POST   /v1/memory/<scope>/plans                  # write plan
GET    /v1/memory/<scope>/plans                  # list plans (with --inherit)
POST   /v1/memory/<scope>/learnings              # append learning
GET    /v1/memory/<scope>/learnings              # list learnings (with --inherit)
```

### Admin

```
GET    /v1/health                                # (public) liveness probe
GET    /v1/version                               # (public) server version + schema versions
GET    /v1/metrics                               # Prometheus format (admin-only)
GET    /v1/audit                                 # admin-only access log
```

## Daemon API (loopback, bridge plugins)

The local daemon (`resleeve agent`) exposes a smaller surface on `127.0.0.1:<random>` (per `02-journey-01-decisions.md` Q4). Mostly relays/proxies the server API; adds a few local-only endpoints.

### Identity

```
GET    /v1/agent/discover                        # what mode? SCION? standalone? endpoint URL of upstream server?
GET    /v1/agent/whoami                          # current user/identity for this daemon
```

### Relayed (forwarded upstream after local buffering)

The bridge plugin talks to these; the daemon forwards to the server API with buffering + retry:

```
POST   /v1/sessions/<id>/events                  # buffered + ack'd locally, then upstream
POST   /v1/slots/<scope>/<agent_name>/heartbeat  # debounced (lots of "still alive" pings)
POST   /v1/sessions                              # session_start
POST   /v1/sessions/<id>/end                     # session_end
```

### Local-only

```
GET    /v1/agent/buffer                          # local buffer state (event count, ack/nack)
POST   /v1/agent/flush                           # force-flush buffer to upstream
GET    /v1/agent/log                             # tail daemon log
```

## Headers

Webhook deliveries (per `03-eventing-system.md`):

- `X-Resleeve-Event-Id`
- `X-Resleeve-Event-Type`
- `X-Resleeve-Signature` — HMAC-SHA256 of body using subscription secret
- `X-Resleeve-Delivery-Id`
- `X-Resleeve-Cursor` — for resume after downtime
- `X-Resleeve-Schema-Version`

Bridge → daemon requests:

- `X-Resleeve-Bridge-Vendor`, `X-Resleeve-Bridge-Version` (so daemon knows the calling adapter)
- `X-Resleeve-Idempotency-Key` (for event submission; daemon dedups on retry)

## Auth flows on different endpoints

| Endpoint class | Accepted credential |
|---|---|
| Public (`/s/...`, `/v1/health`, signup, login, recover) | none required |
| User-action endpoints (sessions, shares, subscriptions) | user access token from `/v1/auth/login` |
| Bridge → daemon (loopback) | shared secret in `~/.config/resleeve/agent.json` (local-only; not over the wire) |
| Sidecar → server (SCION mode) | SCION Agent Token (JWT validated against registered orchestrator public key) |
| Webhook delivery → consumer | HMAC signature in `X-Resleeve-Signature` (consumer's responsibility to verify) |

## Open questions

- **Per-user vs. global tokens for `POST /v1/subscriptions`** — already locked as per-user with scope claims; reconfirm endpoints carry this through.
- **`POST /v1/sessions/<id>/hydrate` shape** — does it stream the hydrated file as a download, or return the bytes inline? Lean: stream (sessions can be big).
- **Idempotency on `POST /v1/sessions/<id>/events`** — required given retry semantics. Lean: idempotency key based on `(event_uuid)`; server upserts.
- **WebSocket on any endpoint** — decided against per `03-eventing-system.md`. Reconfirm no WS endpoints exist anywhere.
- **`/v1/audit`** — what's audited? Login/logout, share creation, subscription creation, session deletion. Detailed spec in auth doc.
- **CORS** — sharing UI is served from same origin (`/s/...`). API endpoints: same-origin only by default; allowlist in config for cross-origin clients.
- **Rate limits per endpoint class** — bridge-plugin event submission has a different rate envelope than user-facing endpoints. Per-endpoint limits configurable.
