# Round 12 Part B — Plan versioning slice (LOCAL + serve enforcement)

Implements the **append-only plan versioning + optimistic concurrency**
half of `00-encryption-and-versioning.md` ("Part B"). Brings plans into
the same append-only, materialized-read model the rest of resleeve
already uses (events, learnings), and adds optimistic concurrency so
shared-brain plan writes don't silently clobber.

**Green field:** resleeve is pre-release. This slice changes the plans
schema net-new and breaks the old overwrite-`plans` shape. There is **no
data migration** of existing plan rows.

This slice ships the **local + in-process** enforcement: the store layer,
the daemon HTTP handlers, the MCP tool, and the CLI. The cross-machine
**sync reconcile** is explicitly deferred (see the last section).

---

## Schema (migration `0009_plan_versions.up.sql`)

`plans` (mutate-in-place upsert keyed `(scope, name)`) is **dropped** and
replaced by an append-only log:

```sql
CREATE TABLE plan_versions (
    scope           TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '_default',
    version         INTEGER NOT NULL,            -- 1-based, monotonic per (scope, name)
    content         TEXT NOT NULL,
    author_user_id  TEXT NOT NULL DEFAULT '',    -- provenance; '' = unknown/local
    parent_version  INTEGER NOT NULL DEFAULT 0,  -- version this was derived from; 0 = first
    created_at      TEXT NOT NULL,
    PRIMARY KEY (scope, name, version),
    FOREIGN KEY (scope) REFERENCES scopes(path) ON DELETE CASCADE
);
CREATE INDEX plan_versions_head ON plan_versions(scope, name, version);
```

- **Write = append, never mutate.** Each update inserts a new immutable
  row. `version = HEAD + 1`, `parent_version = HEAD`.
- **Read = materialized HEAD** = the row with `MAX(version)` for a
  `(scope, name)`. No diff replay — plans are small markdown.
- **HEAD scan + history walk** both ride `plan_versions_head`.

Provenance for learnings (already append-only) gets the same stamp:

```sql
ALTER TABLE learnings ADD COLUMN author_user_id TEXT NOT NULL DEFAULT '';
```

---

## Store API + `ErrPlanConflict` contract

`internal/storage/sql` `MemoryStore` (sqlite impl in
`internal/storage/sql/sqlite/memory.go`):

```go
AppendPlanVersion(ctx, scope, name, content, author string, baseVersion int64, force bool) (*memory.Plan, error)
GetPlan(ctx, scope, slot string) (*memory.Plan, error)            // HEAD + Version
ListPlans(ctx, scope string) ([]*memory.Plan, error)              // HEAD per slot
ListPlanVersions(ctx, scope, slot string) ([]*memory.Plan, error) // full history, oldest first
GetPlanVersion(ctx, scope, slot string, version int64) (*memory.Plan, error)
DeletePlan(ctx, scope, slot string) error                         // drops all versions
AppendLearning(ctx, l *memory.Learning) error                     // l.Author stamped
```

`memory.Plan` gained `Version int64`, `Author string`,
`ParentVersion int64` (and `UpdatedAt` is now the version's `created_at`).

**Optimistic concurrency contract (`AppendPlanVersion`):**

- `baseVersion` is the HEAD version the caller derived its edit from.
- `baseVersion == current HEAD version` → append `HEAD+1`
  (`parent_version = baseVersion`). Success returns the new version via
  `p.Version`.
- `baseVersion != current HEAD version` → return a typed
  `*memory.PlanConflictError` (matches `memory.ErrPlanConflict` under
  `errors.Is`) **carrying the current HEAD** (`.Head`: version + content)
  so the caller can reconcile in one round-trip. The failed call *is* the
  reconciliation signal.
- `baseVersion == memory.NewPlanBaseVersion` (0) means **"expect no
  existing plan"** (a first write). If a HEAD already exists this is a
  conflict (carries the existing HEAD).
- `force == true` bypasses the base-version check entirely and appends
  `HEAD+1` unconditionally. Used by sync **pull** ingest (materializing
  the server's already-resolved log) and the CLI `--force` path.

The HEAD read + insert run inside one transaction; the PK on
`(scope, name, version)` is the backstop against a concurrent racer.

`memory.PlanConflictError` lives in `internal/memory/memory.go`; the
client-side mirror `agent.PlanConflict` (in
`internal/agent/memory_client.go`) also matches `memory.ErrPlanConflict`.

---

## 409 wire shape (daemon handlers)

Handlers live in `internal/agent/memory_handlers.go`. (`resleeve serve`
itself is opaque blob KV — it stores plan-version blobs under
`memory/<scope>/plans/<slot>/<version>` and does not parse them; the
structured plan store + enforcement is the **daemon**, which is the
local + serve-side write path for in-process callers.)

`PUT /v1/plan?scope=&slot=` request body:

```json
{ "content": "…", "base_version": 2, "force": false }
```

On success → `200` with the appended version (`memory.Plan` JSON,
includes `version`, `parent_version`, `author_user_id`).

On a stale `base_version` → **HTTP 409** with the current HEAD in the body:

```json
{
  "error": "plan version conflict: base_version is stale; reconcile against head and retry",
  "head": {
    "scope": "svc",
    "name": "_default",
    "version": 2,
    "content": "…current HEAD markdown…",
    "parent_version": 1,
    "updated_at": "2026-06-06T…Z"
  }
}
```

Reads: `GET /v1/plan` returns the HEAD **including `version`**. History:
`GET /v1/plan/versions?scope=&slot=[&version=N]` returns
`{"versions":[…]}` (oldest first) or a single version row when
`?version=N` is given.

**Author stamping:** the daemon is a single-user local process with a
shared-secret bearer (no per-request identity), so it stamps
`Config.LocalAuthor` (default empty = "unknown/local") via
`Daemon.localAuthor()`. The serve-side per-device-bearer path is where
the real authenticated user is stamped once that write path is structured;
this slice leaves the daemon stub returning the configured local identity.

---

## MCP tool changes (`internal/mcp`)

- `resleeve_plan_write` gains optional `base_version` (int) and `force`
  (bool). On conflict it returns a **clear conflict result**
  (`isError=true`) whose text carries the current HEAD version + content
  and tells the agent to merge and retry with `base_version=<HEAD>` —
  not a generic error.
- `resleeve_plan_read` surfaces the version as a leading
  `<!-- version: N -->` comment so an agent that intends to write back
  has the `base_version` to pass.
- `resleeve_learning_append` records the author (stamped server-side; the
  tool surface is unchanged for the caller).

## CLI changes (`internal/cli/plan.go`)

- `resleeve plan write` reads the current HEAD version (`GetPlan`) and
  passes it as `base_version`. A `--force` flag bypasses the check.
- A 409 conflict prints a legible message (current HEAD version + how to
  re-read/merge or `--force`) and exits non-zero.
- `resleeve plan read` prints the version to **stderr** (`# plan … version N`)
  so stdout stays pure content for piping / re-feeding to `plan write`.

---

## Sync-path impact + the deferred reconcile hook

The fast-tier outbox key for a plan is now per-version and immutable:

```
memory/<encoded-scope>/plans/<slot>/<zero-padded-version>
```

**Pull (ingest)** is conflict-free: `SyncClient.ingestMemoryRow`
force-appends the pulled version (`AppendPlanVersion(..., force=true)`),
because pull materializes the server's already-resolved log.

**Push is NOT yet conflict-aware — this is the deferred slice.** Today
the outbox push is a blind idempotent `Put`. Once `resleeve serve`
enforces plan optimistic concurrency *across machines*, a plan push the
server `409`s (another machine advanced HEAD first) must **reconcile, not
blind-retry**: re-derive the local version against the server's returned
HEAD and re-enqueue. Only **plan** rows change — sessions / events /
learnings stay append-idempotent.

The hook for that work is marked with `// round-12B follow-up:` in:

- `internal/agent/sync.go` — `SyncClient.EnqueuePlan` (doc block:
  the reconcile branch belongs in the outbox **push result handling**,
  keyed off the `"/plans/"` key prefix and a 409 from the upstream push).
- `internal/agent/sync.go` — `ingestMemoryRow` plan case (notes that the
  pull direction is correct/force-append but push is not yet
  conflict-aware).

### Out of scope (unchanged from Part B)
- 3-way auto-merge (ship the dumb 409 first).
- Cross-slot / cross-scope transactional writes.
- Group-key shared-brain zero-knowledge (Part A, deferred).
