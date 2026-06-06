# Round 11a slice 1 — multi-tenant schema foundation

This slice lands the **data model** for multi-tenancy described in
`02-multi-tenant.md` (§"Core model", §"The gap" #4): the `brains`,
`memberships`, and `credentials` tables, their stores, and the
register-time auto-provision of a personal brain. It deliberately does
**not** touch keyspace partitioning, read filtering, authz, or the
SSH/API auth verification — those are later slices. This is foundation
only.

resleeve is pre-release, so these are net-new tables; no data migration
or back-compat shim was written for any prior single-user shape.

## New tables (migration `0008_tenancy.up.sql`)

```sql
CREATE TABLE brains (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    kind           TEXT NOT NULL,          -- 'personal' | 'shared'
    owner_user_id  TEXT NOT NULL,          -- creator; can add/remove members
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    FOREIGN KEY (owner_user_id) REFERENCES server_users(id) ON DELETE CASCADE
);
CREATE INDEX brains_by_owner ON brains(owner_user_id);

CREATE TABLE memberships (
    brain_id   TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (brain_id, user_id),
    FOREIGN KEY (brain_id) REFERENCES brains(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id)  REFERENCES server_users(id) ON DELETE CASCADE
);
CREATE INDEX memberships_by_user ON memberships(user_id);

CREATE TABLE credentials (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL,
    kind                TEXT NOT NULL,      -- 'ssh' | 'api' | 'oidc'
    public_key_or_hash  TEXT NOT NULL,      -- SSH pubkey, API-key hash, or OIDC subject
    label               TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    expires_at          TEXT,               -- NULL = no expiry
    FOREIGN KEY (user_id) REFERENCES server_users(id) ON DELETE CASCADE
);
CREATE INDEX credentials_by_user ON credentials(user_id);
```

Design notes, per the spec's "keep it dumb first" constraint:

- **`memberships` has NO role column.** Membership existence *is* the
  access rule (read+write); the owner is `brains.owner_user_id`. A shared
  brain is simply a brain with `count(members) > 1` — no separate
  subsystem. Reader/writer roles are deferred until missed.
- **`brains.kind`** is `'personal'` (auto-provisioned, one per user) or
  `'shared'` (gains members later). It's an advisory label; the access
  mechanism is identical for both.
- **`credentials`** is storage shape only this slice. SSH-challenge / API
  key minting + verification land in slice 4/5; this just gives them the
  table. The server stores a public key or a hash, never a plaintext
  secret. `kind` includes `'oidc'` now so tier 5 slots in behind the same
  seam without a schema change.
- Timestamps are TEXT RFC3339Nano, matching every other table in the
  store. None of these columns are lexicographically range-compared, so
  the fixed-width layout used by `sessions`/`pairing_codes` isn't needed
  here.

## Store interfaces (`internal/storage/sql/store.go`)

New value types: `Brain` (+ `BrainKind` const `personal`/`shared`),
`Membership`, `Credential` (+ `CredentialKind` const `ssh`/`api`/`oidc`).

```go
type BrainStore interface {
    Create(ctx, *Brain) error
    Get(ctx, id) (*Brain, error)                  // ErrNotFound if absent
    ListByOwner(ctx, userID) ([]*Brain, error)    // brains created by user
    ListForUser(ctx, userID) ([]*Brain, error)    // member-of, via join
}

type MembershipStore interface {
    Add(ctx, *Membership) error                   // idempotent (INSERT OR IGNORE)
    Remove(ctx, brainID, userID) error            // idempotent
    ListBrains(ctx, userID) ([]string, error)     // brain ids
    ListMembers(ctx, brainID) ([]string, error)   // user ids
    IsMember(ctx, userID, brainID) (bool, error)
}

type CredentialStore interface {
    Add(ctx, *Credential) error
    Get(ctx, id) (*Credential, error)             // ErrNotFound if absent
    ListByUser(ctx, userID) ([]*Credential, error)
    Delete(ctx, id) error                         // idempotent
}
```

The `Store` aggregate gains `Brains()`, `Memberships()`, `Credentials()`
accessors, wired the same way `ServerUsers()` is. SQLite impls live in
`internal/storage/sql/sqlite/tenancy.go` alongside the identity stores,
with compile-time `var _ rsql.XStore = (*xStore)(nil)` assertions.

## Register change (`internal/serve/auth_handlers.go`)

`serve.Config` gains optional `Brains rsql.BrainStore` and
`Memberships rsql.MembershipStore` fields, threaded into `Server`. When
both are set, `handleRegister` calls `provisionPersonalBrain` after
minting the first device: it inserts a `kind=personal` brain owned by the
new user (named after the user's email) and a `memberships` row for that
(brain, user). The brain row is created first so the membership FK is
satisfied. `RegisterResp` gains `brain_id` (omitempty) carrying the new
personal brain's id.

When the brain stores are nil (legacy single-user / `--single-tenant`
style boot), register still succeeds and provisions nothing —
`brain_id` is simply absent. The production wiring in
`internal/cli/serve.go` is intentionally left untouched in this slice
(out of scope); it will opt in by passing the two stores when the
keyspace-partitioning slice needs them.

## Tests

- `internal/storage/sql/sqlite/tenancy_test.go` — store CRUD on a
  `t.TempDir()` file-backed sqlite (FK-enforced):
  - `TestBrainStore_CRUD` — Create/Get/ListByOwner, ErrNotFound, owner
    isolation.
  - `TestMembershipStore_AddListIsMember` — Add (incl. idempotency),
    ListBrains, ListMembers, IsMember true/false, Remove (idempotent).
  - `TestBrainStore_ListForUser` — returns brains the user is a member of
    including ones they don't own (shared membership).
  - `TestCredentialStore_CRUD` — Add/Get/ListByUser/Delete, nullable
    `expires_at` round-trip, idempotent Delete, ErrNotFound.
- `internal/serve/tenancy_test.go` — register behavior:
  - `TestRegister_ProvisionsPersonalBrain` — register creates exactly one
    personal brain owned by the new user + exactly one membership;
    `brain_id` returned; `IsMember`, `ListBrains`, `ListMembers` all
    agree.
  - `TestRegister_DistinctBrainsPerUser` — two registrations get distinct
    brains and are not members of each other's.

`go build ./...`, `go vet ./...`, and `go test ./...` are all green;
touched files are gofmt-clean.
