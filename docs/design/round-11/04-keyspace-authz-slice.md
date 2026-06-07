# Round 11a slice 2 — keyspace partitioning + read authz

Slice 1 (`02-multi-tenant.md` §"What round 11 builds" + migration 0008)
landed the multi-tenant *schema*: `brains` / `memberships` / `credentials`
tables, the rsql stores, and `register` auto-provisioning a personal brain
+ owner membership. This slice consumes that foundation to actually
**isolate the blob keyspace by brain** and **enforce read authz** on
`/v2/sync/*` — without changing the client wire protocol.

This is the gap §1–3 from `02-multi-tenant.md`:

1. Keyspace was global (`sessions/<id>` with no namespace).
2. Reads weren't filtered (any bearer could pull any row).
3. The legacy no-user bearer was accepted on `/v2/sync/*` — a cross-tenant
   hole.

## The prefix scheme — brain is a *server-side* storage namespace

The cleanest isolation that keeps the client dumb: the **server** injects a
`<brain_id>/` prefix in front of the client's key on write, and strips it
on read. The client keeps speaking the kind-scoped keyspace it always has:

| Layer | Key |
|---|---|
| Client wire (push body, pull rows, `since` cursor) | `sessions/<id>`, `events/…`, `memory/…` |
| Server storage (the `sync.Backend`) | `<brain_id>/sessions/<id>`, `<brain_id>/memory/…` |

The client **never** supplies, sees, or forges the brain segment. Two users
acting in two different personal brains can push the *identical* client key
(`sessions/shared-id`) and never collide or leak, because the bytes land
under different storage prefixes.

Keys are `'/'`-joined **storage keys**, not OS paths — the prefix helpers
(`brainKey`, `brainPrefix`, `stripBrainPrefix` in `internal/serve/tenancy.go`)
use plain string concatenation with `/`, never `path/filepath`.

### Push (server-side prefixing)
`handlePush(w, r, brainID)`:
1. The sec-M2 prefix gate (`isAllowedPushKey`) runs on the **client** key
   (`sessions/`|`events/`|`memory/`), unchanged — so the allow-list still
   mirrors the wire kinds.
2. The blob is stored under `brainKey(brainID, clientKey)` =
   `<brain_id>/<client-key>`.
3. The `committed` keys echoed back to the client are the **original**
   client keys.
4. Memory rows fan out to SSE subscribers carrying the **storage** key
   (brain-prefixed), so each subscriber can filter to its own brain.

### Pull + SSE (read authz + strip)
`handlePull` / `handleSSE` list/serve **only** under the caller's brain
prefix — read authz falls out of prefix scoping (you physically cannot list
another brain's keyspace):
1. List prefix = `brainPrefix(brainID, kind)` = `<brain_id>/<kind>`.
2. The client `since` cursor is a kind-scoped key; it's promoted into the
   brain namespace (`brainKey`) before `List`, so the backend's
   lexicographic cursor stays correct under the prefix.
3. Every returned key — and the `next_cursor` — is stripped of the brain
   prefix (`stripBrainPrefix`) so the wire keyspace is unchanged. The client
   stores and re-sends a kind-scoped cursor, never a brain-aware one.
4. Defense-in-depth: a listed key lacking the expected brain prefix is
   **dropped** rather than returned (impossible given prefix-scoped List,
   but never leak another brain's row).
5. SSE live events arrive on a **global** fan-out channel carrying storage
   keys; each subscriber drops events outside its brain prefix and strips
   the prefix off its own before emitting.

Cursor correctness under the prefix: because all of a brain's keys share
the same `<brain_id>/` prefix, their relative lexicographic ordering is
preserved, so `key > sinceCursor` paginates identically to the flat
keyspace. (Covered by `TestTenant_PullPaginationUnderBrain`.)

## Token → brain resolution

The per-device bearer already resolves to a `*rsql.Device` carrying
`UserID` (`deviceFromBearer`). On top of that:

- **`requireBrain`** is the sync-route middleware in multi-tenant mode. It
  resolves the bearer, **rejects the legacy no-user bearer** (synthetic
  device with `UserID==""` — it cannot identify a tenant, 403), resolves the
  acting brain, and passes the `brainID` to the handler.
- **Default acting brain = the user's personal brain.** `personalBrainID`
  picks the `BrainKindPersonal` brain from `Brains.ListByOwner(userID)`
  (auto-provisioned on register; exactly one).
- **Optional `?brain=<id>` selector** (the seam for shared-brain selection
  in a later slice): validated via `Memberships.IsMember(userID, brainID)`,
  **403 if not a member**. Only the personal-default path is exercised this
  slice; shared-brain client selection is later work.

## `--single-tenant` semantics

`--single-tenant` (serve CLI flag → `serve.Config.SingleTenant`) is the
escape hatch for solo self-hosters (tiers 1–2):

| | multi-tenant (default) | `--single-tenant` |
|---|---|---|
| brain prefixing on `/v2/sync/*` | yes (`<brain_id>/…`) | no (flat keyspace) |
| legacy no-user bearer on `/v2/sync/*` | **rejected** (403) | accepted |
| sync-route middleware | `requireBrain` | `requireAuth` (legacy) |
| per-device token | required | optional |

`Server.multiTenant()` = `!SingleTenant && brains != nil && memberships !=
nil`. With the brain stores unset the server is content-blind regardless of
the flag (there is nothing to resolve an acting brain from) — that's the
pure-`AuthToken` legacy deployment, which keeps working. The single-tenant
routes bind a fixed empty `brainID`, and the prefix helpers treat `""` as a
no-op, so the on-disk keyspace is **byte-identical to the pre-slice flat
layout**.

> Green-field note: resleeve is pre-release. There is no data migration off
> the old global keyspace — multi-tenant simply becomes the default and the
> flat keyspace is reachable only via `--single-tenant`.

## Client-side change

**None.** The wire protocol (push body, pull rows, `since`/`next_cursor`,
SSE frames) is unchanged. `internal/agent` keeps setting `cursor =
row.Key` and re-sending it as `since`; the server re-prefixes/strips
transparently. The client never learns about brain ids in this slice.

## Tests (isolation proofs)

In `internal/serve/tenancy_isolation_test.go`:

- **`TestTenant_PullIsolation`** — two users push the *same* client key;
  each pull returns only its own blob, keys un-prefixed, never the other's.
- **`TestTenant_StorageKeyIsBrainPrefixed`** — inspects the backend: the
  blob is stored under `<brain_id>/sessions/<id>`, the flat key does **not**
  exist, and the pulled key comes back un-prefixed.
- **`TestTenant_SSEIsolation`** — A's live memory push reaches A's SSE
  stream (key un-prefixed) and **never** B's, despite the global fan-out.
- **`TestTenant_PullPaginationUnderBrain`** — paged pull (`limit=1`) walks
  the brain's rows in order; `next_cursor` is kind-scoped (no brain prefix)
  and advances correctly when re-sent.
- **`TestTenant_BrainSelectorRejectsNonMember`** — `?brain=<other-brain>`
  → 403; `?brain=<own-brain>` → 200 (the selector seam).
- **`TestTenant_SingleTenantNoPrefix`** — `--single-tenant` stores flat
  (no brain prefix).
- **`TestSync_LegacyBearerRejectedMultiTenant`** /
  **`TestSync_LegacyBearerAcceptedSingleTenant`** /
  **`TestSyncPush_LegacyBearerAcceptedSingleTenant`** — the legacy no-user
  bearer is 403 on sync in multi-tenant mode, 200 under `--single-tenant`.

Existing single-tenant round-trip tests (`newTestServer`, pure `AuthToken`)
still pass — they run content-blind because no brain stores are wired.
