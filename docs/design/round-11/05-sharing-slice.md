# Round 11b — SHARED-BRAIN management + brain selection (tier 4)

This slice lands **tier 4** of the brain ladder (`02-multi-tenant.md`):
multiple users sharing a read/write brain. It builds on the round-11
schema (`brains` / `memberships` / `credentials` stores) and the
brain-scoped sync keyspace + `?brain=<id>` selector validated via
`IsMember`.

Three deliverables: **server endpoints** for brain + membership
management, **CLI verbs** to drive them, and a client-side **active-brain
selection** that routes a machine's sync into a chosen shared brain.

## 1. Server endpoints (`internal/serve/brains.go`)

All endpoints authenticate with the **per-device bearer**, resolved to a
real user via `deviceFromBearer`. The legacy no-user bearer is **rejected**
(mirrors `requireBrain` / `requireUserDevice`): a content-blind bearer
can't identify the user whose authz we'd be checking. Routes are wired
only when an identity store is configured (`registerBrainRoutes`, called
from `registerAuthRoutes`).

| Method + path | Authz | Behavior |
|---|---|---|
| `POST /v1/brains` `{name}` | any user | Create a `kind=shared` brain owned by the caller + the caller's owner membership. Returns `{brain_id}`. |
| `GET /v1/brains` | any user | List brains the caller is a member of (`Brains.ListForUser`). Each row carries `role` = `owner`/`member`. |
| `GET /v1/brains/{id}/members` | **member** | List member user-ids. Non-member → **403**. |
| `POST /v1/brains/{id}/members` `{user_id}` | **owner** | Add a member. Non-owner → **403**. Unknown user → **400**. Unknown brain → **404**. |
| `DELETE /v1/brains/{id}/members/{user_id}` | **owner** | Remove a member. Non-owner → **403**. Removing the **owner** → **400** (orphan guard). |

Authz model (round-11 "keep it dumb"):

- **Membership is the access edge** — you may touch a brain iff a
  `(brain_id, user_id)` row exists.
- **No roles** beyond owner-vs-member: every member reads *and* writes.
  The only owner-gated actions are add/remove member.
- **Owner-cannot-orphan**: the owner can't remove themselves, so a brain
  never becomes memberless/ownerless. Ownership transfer + brain deletion
  are deferred.
- **Personal brains are unaffected** — they're auto-provisioned on
  register (`kind=personal`) and never appear in the member-management
  flows (no one else can be added by these verbs in normal use; the
  owner-only gate plus the orphan guard keep them single-member).

`requireOwner` returns **404** for an unknown brain (no info leak) and
**403** for a real brain the caller doesn't own.

### Brain-scoped sync keyspace (the substrate this slice shares)

`requireBrain` (`internal/serve/tenancy.go`) gates `/v2/sync/*`:

1. resolve the per-device bearer → user (reject legacy no-user bearer
   unless `--single-tenant`);
2. resolve the acting brain from `?brain=<id>` (default: the caller's
   personal brain) and **validate membership** via `IsMember`
   (forbidden → 403);
3. stash `(device, brainID)` on the request context.

The wire keyspace stays **brain-agnostic** (`sessions/<id>`,
`memory/<scope>/…`). The server transparently **prepends** the acting
brain (`<brain_id>/sessions/<id>`) on push and **strips** it on
pull/SSE, so the daemon's ingest + cursor logic is unchanged across the
single→multi-tenant flip. The client never supplies the prefix → can't
forge a cross-brain write. `--single-tenant` (`serve.Config.SingleTenant`)
disables partitioning entirely (global keyspace, legacy bearer accepted)
for tier-1/2 solo self-hosters.

## 2. CLI verbs (`internal/cli/brain.go`)

Talk to the upstream serve with the device token (same pattern as
`pair`/`login`: `--upstream`/`$RESLEEVE_UPSTREAM` + the keychain bearer
under `(upstream, email)`).

```
resleeve brain create <name>                 # POST /v1/brains, prints brain id
resleeve brain list                          # GET  /v1/brains (id, name, kind, role, active marker)
resleeve brain member list <brain-id>        # GET  /v1/brains/{id}/members
resleeve brain member add  <brain-id> <user> # POST /v1/brains/{id}/members
resleeve brain member rm   <brain-id> <user> # DELETE /v1/brains/{id}/members/{user}
resleeve brain use <brain-id>                # set the machine's active brain (local)
resleeve brain use --clear                   # revert to your personal brain (local)
```

Wired into `dispatch.go` + help text. Server 403/404/400 messages are
surfaced cleanly via the shared error-envelope decode (`getJSON` /
`deleteJSON` mirror `postJSON`).

## 3. Active-brain selection (client)

`brain use` is **local-only**: it persists a **GLOBAL active brain** in
the client config and the daemon's sync client appends `?brain=<id>` to
its upstream push/pull/SSE so the machine acts in that shared brain.

- **Storage**: `~/.resleeve/active_brain` (under `agent.DataDir()`), a
  one-line file written atomically (tempfile + rename, 0600).
  `agent.LoadActiveBrain` / `WriteActiveBrain` (empty id = clear =
  remove file). Cross-platform via `path/filepath`.
- **Thread**: `brain use <id>` → `WriteActiveBrain` → daemon `New()`
  calls `LoadActiveBrain` and `SyncClient.SetActiveBrain` → the
  drain/pull/SSE loops call `brainQuery()` on each request, appending
  `&brain=<id>` (URL-escaped) when set. Unset = no selector = personal
  brain (server default).
- **Server validation**: the selector is membership-checked on every
  request, so a bad/forbidden brain → **403**. The CLI surfaces that.
- **Runtime switch**: the daemon reads the config at construction;
  `brain use` prints a "restart the daemon to apply" hint
  (`resleeve down && resleeve up`). `SetActiveBrain` is concurrent-safe
  and re-sampled per request, so a future hot-reload (config watch or a
  daemon endpoint) is a drop-in.

## Follow-up — per-scope routing

The active brain is **global per machine** in this slice: every scope's
sessions/memory route to the one selected brain. The likely refinement is
**per-scope routing** — e.g. `~/work/*` → the team brain, everything else
→ personal — so a single daemon can fan a push/pull set across multiple
brains by scope. That needs:

- a scope→brain mapping in the client config (not a single id);
- the enqueue path tagging each outbox row with its target brain;
- the drain/pull loops grouping by brain and issuing one `?brain=<id>`
  request set per brain.

Deferred to keep this slice's blast radius small. Reader/writer roles,
ownership transfer, and brain deletion are likewise deferred until missed
(`02-multi-tenant.md` "keep it dumb").

## Tests

- **Endpoint authz matrix** (`internal/serve/brains_test.go`):
  owner-vs-member-vs-non-member for create/list/add/remove; legacy-bearer
  rejection; unknown-user 400; unknown-brain 404; owner-cannot-orphan 400.
- **Shared-keyspace round-trip**: create → add member → owner pushes into
  `?brain=<id>` → member pulls the same row (brain-agnostic key); stranger
  403'd on the selector; personal keyspace stays isolated.
- **Active-brain persist + clear** and **daemon sends the selector**
  (`internal/agent/brain_sync_test.go`): `?brain` present iff set, on both
  push and pull.
- Legacy `/v2/sync/*` tests moved to `SingleTenant:true`; the multi-tenant
  legacy-bearer-rejection is now asserted (round-11 gap #3).
