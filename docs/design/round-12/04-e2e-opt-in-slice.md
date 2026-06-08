# Round 12 · Part A · Slice 3 — the e2e opt-in setting

This slice lets a brain **owner** set a brain's `encryption_policy`, so a
user can flip a personal brain back to zero-knowledge (`e2e`) on a trusted
multi-tenant server. Slices 12A.1/12A.2 already shipped the column
(`brains.encryption_policy`, default `server-side`), the per-brain wrapped
DEK (`brain_keys`), and the daemon's seal decision
(`shouldSeal = !multiTenant || policy=="e2e"`, sampled at Start + on brain
use via `GET /v2/sync/whoami`). What was missing was a way to *change* the
policy. This slice is that write path.

Green-field: no migrations, no back-compat shims. The column already
exists.

## Store

`BrainStore.UpdatePolicy(ctx, brainID, policy)`
(`internal/storage/sql/store.go`, sqlite impl in
`internal/storage/sql/sqlite/tenancy.go`):

- Validates `policy ∈ {"server-side", "e2e"}` — any other value is an
  error, so a typo can never land a junk policy the daemon would later read
  at handshake time.
- `UPDATE brains SET encryption_policy = ?, updated_at = ? WHERE id = ?`;
  `RowsAffected() == 0` → `ErrNotFound`.
- The store enforces only the *value*; **authz** (owner-only, personal-only
  for e2e) lives at the HTTP layer.

## Server endpoint

`PUT /v1/brains/{id}/policy` (`internal/serve/brains.go`,
`handleSetPolicy`), body `{"encryption_policy":"e2e"|"server-side"}`.
Wrapped like the other brain-management routes: `requireUser` (per-device
bearer → real user; legacy no-user bearer rejected with 401).

Authz + validation, in order:

1. **Owner-only** — `requireOwner` loads the brain and asserts ownership:
   `404` for an unknown brain (no info leak), `403` for a brain the caller
   can see but doesn't own.
2. **`e2e` is personal-only** — `e2e` is zero-knowledge (the server holds
   no key), so a shared brain would need *group keys* to let members read
   each other's writes. That isn't built yet, so `e2e` on a brain whose
   `kind != personal` is rejected with `400`
   (`"end-to-end encryption on a shared brain needs group keys; not
   supported yet"`).
3. **`server-side` is always allowed** (personal or shared).
4. Any other policy string → `400`.

On success: persist via `UpdatePolicy`, return **`204 No Content`**.

### No re-encryption of existing data

When a master key is configured every brain has a server-at-rest DEK. If a
brain is switched **to** `e2e`, this endpoint does **not** re-encrypt rows
already written under that server DEK — pre-release green-field, so we don't
attempt a migration. The effect of the flip is forward-only: **new** client
writes get sealed once the daemon re-samples `shouldSeal`; existing
server-side rows stay readable by the server as-is. (Symmetrically,
switching `e2e → server-side` does not retroactively unseal client-sealed
rows.) A future slice can add a re-encrypt/rewrap pass if the threat model
demands it.

## CLI

`resleeve brain encrypt <brain-id> <e2e|server-side>`
(`internal/cli/brain.go`, `runBrainEncrypt`). Talks to `resleeve serve`
with the device token, like the other `resleeve brain` verbs
(`upstreamAuth` → keychain bearer). It `PUT`s the policy and:

- prints `brain <id> encryption policy set to <policy>` on success;
- surfaces the server's `400` (e2e-on-shared) / `403` (not owner) /
  `404` (unknown) legibly via the shared `decodeHTTPError` envelope path;
- validates the policy string client-side before the call (usage error,
  exit 2, on anything but `e2e`/`server-side`).

It also prints that the running daemon **picks up the new policy on its next
handshake** — restart (`resleeve down && resleeve up`) or `resleeve brain
use` — because `shouldSeal` is sampled at Start + on brain change, not on
every request.

## Daemon pickup (12A.2, unchanged)

The daemon computes `shouldSeal` from the `whoami` handshake response and
caches it. It re-samples at **Start** and on **brain change** (`resleeve
brain use`). So a policy flip is observed by an already-running daemon only
after one of those events. The `whoami` handler reads the brain's *current*
`encryption_policy` per request (`internal/serve/server.go`,
`handleWhoami`), so once the daemon re-handshakes it sees the new value —
covered by `TestBrain_SetPolicy_PersonalE2E`, which flips a personal brain
to `e2e` and asserts `whoami` reflects it.

## Tests

- Store: `TestBrainStore_UpdatePolicy` — default `server-side`, round-trip
  to `e2e` and back, invalid value rejected (row untouched), unknown brain
  `ErrNotFound`.
- Endpoint (`internal/serve/brains_test.go`):
  - `TestBrain_SetPolicy_PersonalE2E` — `e2e` on a personal brain → `204`,
    `whoami` reflects `e2e`; revert → `server-side`.
  - `TestBrain_SetPolicy_E2EOnSharedRejected` — `e2e` on a shared brain →
    `400`; `server-side` on the same shared brain → `204`.
  - `TestBrain_SetPolicy_Authz` — non-owner → `403`; unknown brain → `404`;
    owner-on-own-personal sanity → `204`.
  - `TestBrain_SetPolicy_InvalidValue` — junk policy string → `400`.

## Deferred: LOCAL-AT-REST

This slice does **not** implement local-at-rest encryption — it's a
different problem and is deferred. The pure-Go `modernc` sqlite driver has
no SQLCipher, so there's no drop-in encrypted-database story. A separate
design is needed, weighing at least:

- **app-layer column encryption** — encrypt blob columns before they hit
  the driver (keeps the pure-Go driver, but every read/write path and every
  query that touches a sealed column has to route through the cipher); vs
- **an encrypted VFS** — encrypt at the page/file layer underneath the
  driver (transparent to queries, but needs a VFS shim the pure-Go driver
  can host, plus key management for the page key).

Until that lands, local on-disk state is unencrypted; this slice only
governs the **server-at-rest vs zero-knowledge** axis for synced data.
