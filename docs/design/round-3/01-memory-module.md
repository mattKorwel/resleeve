# Memory module — v1 inclusion (round-3 addendum)

## Context

In [`05-decisions.md`](../round-1/05-decisions.md) we deferred the memory layer to v2+ on the assumption ori would eventually fold in as `memory/`. The current call:

- **ori stays as Matt's private playground** (messy, not for public, but running in his work fleet today).
- **resleeve is the fresh public face** of the same idea.

Given that, shipping resleeve as "session capture only" undersells the project — the *stack* (Altered Carbon sense) needs both transcripts *and* the working brain (plans, learnings, scope). The "lift design from ori, implement fresh" call is the right one; we just need to do it in v1 rather than after.

This doc specifies what the memory module looks like in resleeve v1.

## Scope

**In:**

- Scope tree (hierarchical path strings).
- Per-scope **Plan** (one current document) and **Learnings** (append-only log).
- **Inheritance walks** with `.donotinherit` boundary.
- **Context injection on SessionStart** — bridge returns `additionalContext` to Claude Code.
- CLI verbs for scope / plan / learning CRUD + `resleeve context <scope>` for preview.

**Out:**

- Ori's untyped items system (we only need plans + learnings).
- A separate `pointer` table (existing `slots` already covers heartbeat + current_session_id).
- Path-defaults config file (cwd → scope mapping; v2+).
- Multi-user (single user remains for v1).
- Git-backed durability (SQLite only — one DB file, clean self-host).

## Data model

### Scope

A path string is the primary identity. Slash-separated, arbitrary depth — like ori v3 but in SQL.

`Kind` is a small validated enum (not free-form text) so vocabulary stays consistent and the CLI can render kind-aware:

| Kind | Meaning |
|---|---|
| `portfolio` | top-level grouping (often whole-team work) |
| `program` | a multi-project initiative |
| `project` | a single named project |
| `dispatch` | a one-off run / task / experiment |
| `agent` | a specific agent role attached to a scope |
| `other` | catch-all if none of the above fit |

Validated at the API layer (`PUT /v1/scope` returns 400 on unknown kind). DB column is plain `TEXT` so future kinds are a code change, no migration.

```go
type Scope struct {
    Path         string    // "auth-rewrite/middleware/jwt"; primary key
    Kind         string    // enum: portfolio | program | project | dispatch | agent | other
    Title        string
    Description  string
    Cwd          string    // default working directory for this scope
    DoNotInherit bool      // boundary marker
    CreatedAt    time.Time
    UpdatedAt    time.Time
}
```

### Plan

Per scope, **named slots** (matching ori's `(scope, type, name)` pattern). The default slot is `_default` — what `resleeve plan write <scope>` operates on. Named slots (`--slot=v2`, `--slot=architecture`) allow multiple plans per scope for power-user workflows; 99% of usage stays single-slot.

```go
type Plan struct {
    Scope     string    // composite primary key with Name
    Name      string    // "_default" for the conventional one
    Content   string    // markdown
    UpdatedAt time.Time
}
```

### Learning

Append-only log per scope, with **soft-supersede** for corrections. Each entry is its own row. Appending with `--supersedes <prior-id>` chains the new entry to the one it replaces; the prior entry is preserved (not deleted) but marked as superseded so reads filter it out by default.

```go
type Learning struct {
    ID           string    // ULID
    Scope        string
    Content      string    // markdown
    SupersedesID *string   // optional; chains correction history
    CreatedAt    time.Time
}
```

Read semantics:

- `GET /v1/learnings?scope=X` returns only learnings *not superseded* by another in the chain (newest in each supersede-chain).
- `GET /v1/learnings?scope=X&include=superseded` returns the whole history.

## Inheritance walk

When an agent boots in scope `auth-rewrite/middleware/jwt`, the rolled-up context is the concatenation of plans + learnings from each scope in the ancestor chain, stopping at a `.donotinherit` boundary.

```
walkAncestors(target):
  parts = target.split("/")
  chain = []
  for i in 1..len(parts):
    path = parts[:i].join("/")
    s = getScope(path)
    if s == nil: continue
    if s.DoNotInherit: chain = [s]   // boundary: reset to just this scope
    else: chain.append(s)
  return chain   // root-most → leaf-most
```

Context output (returned by bridge as `additionalContext`):

```
# Resleeve memory for scope auth-rewrite/middleware/jwt

## Plan (auth-rewrite)
<plan markdown>

## Plan (auth-rewrite/middleware)
<plan markdown>

## Plan (auth-rewrite/middleware/jwt)
<plan markdown>

## Learnings (auth-rewrite)
- 2026-06-01: <learning>
- 2026-06-02: <learning>

## Learnings (auth-rewrite/middleware)
- ...
```

Plans concatenate root → leaf. Learnings concatenate same order. Empty scopes are skipped.

## Storage

Migration `0003_memory.up.sql`:

```sql
CREATE TABLE scopes (
    path             TEXT PRIMARY KEY,
    kind             TEXT NOT NULL DEFAULT '',
    title            TEXT NOT NULL DEFAULT '',
    description      TEXT NOT NULL DEFAULT '',
    cwd              TEXT NOT NULL DEFAULT '',
    do_not_inherit   INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE TABLE plans (
    scope       TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '_default',
    content     TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (scope, name),
    FOREIGN KEY (scope) REFERENCES scopes(path) ON DELETE CASCADE
);

CREATE TABLE learnings (
    id              TEXT PRIMARY KEY,
    scope           TEXT NOT NULL,
    content         TEXT NOT NULL,
    supersedes_id   TEXT,
    created_at      TEXT NOT NULL,
    FOREIGN KEY (scope) REFERENCES scopes(path) ON DELETE CASCADE,
    FOREIGN KEY (supersedes_id) REFERENCES learnings(id)
);

CREATE INDEX learnings_by_scope ON learnings(scope, created_at);
CREATE INDEX learnings_by_supersedes ON learnings(supersedes_id);
```

ANSI-portable per [`11-storage-backends.md`](../round-2/11-storage-backends.md); Postgres backend in v2 picks this up unchanged.

## Daemon API

All bearer-auth gated (same secret as sessions).

```
PUT    /v1/scope?path=<path>                     # create or update scope metadata
GET    /v1/scope?path=<path>                     # get scope metadata
DELETE /v1/scope?path=<path>                     # delete scope (refuses if children exist)
GET    /v1/scopes                                # list all scopes

PUT    /v1/plan?scope=<path>&slot=<name>         # body: markdown; replaces; slot defaults to _default
GET    /v1/plan?scope=<path>&slot=<name>&inherit=false  # one slot only
GET    /v1/plan?scope=<path>&slot=<name>&inherit=true   # rolled-up across ancestors (same slot name)
GET    /v1/plans?scope=<path>                    # list all slots for this scope

POST   /v1/learnings?scope=<path>[&supersedes=<id>]   # body: markdown; appends; optional supersede chain
GET    /v1/learnings?scope=<path>&inherit=false       # this scope's learnings (newest of each supersede chain)
GET    /v1/learnings?scope=<path>&inherit=true        # rolled-up across ancestors
GET    /v1/learnings?scope=<path>&include=superseded  # full history including superseded entries

GET    /v1/context?scope=<path>                  # full plan + learnings rolled up; for bridge injection
```

`?path=` and `?scope=` are URL-encoded slash-separated paths so we don't need fancy routing.

## CLI verbs

```
resleeve scope set <path> [--kind X] [--title T] [--description D] [--cwd C] [--do-not-inherit]
resleeve scope get <path>
resleeve scope list [--ancestors-of <path>]
resleeve scope delete <path>

resleeve plan write <path> [--slot=<name>] [--content "<text>" | --file <path> | --edit]
                                              # default: reads stdin (agent-driven case)
                                              # --edit: open $EDITOR with current content (human convenience)
                                              # slot defaults to _default
resleeve plan read  <path> [--slot=<name>] [--inherit]
resleeve plan list  <path>                    # list slots for this scope

resleeve learning append <path> "<text>" [--supersedes <id>]   # or read stdin if "-"
resleeve learning list <path> [--inherit] [--include-superseded]

resleeve context <path>             # print exactly what the bridge would inject
```

Plus `--scope <path>` filter on existing `session list`.

## Bridge integration — SessionStart context injection

Claude Code's SessionStart hook accepts a stdout JSON payload with `additionalContext`. We use it.

`internal/cli/hook.go` changes:

- On `SessionStart`, after POSTing the event to the daemon, also `GET /v1/context?scope=<derived-from-cwd>`.
- If non-empty, print `{"additionalContext": "<context>"}` to stdout.
- If empty, print nothing.
- Other hooks unchanged (silent).

Scope derivation in v1, in resolution order:
1. `$RESLEEVE_SCOPE` env var (verbatim override).
2. `.resleeve-scope` marker file: walk cwd ancestors for the file; if found non-empty, scope = `<marker-content>/<rel-from-marker-dir>`. Lets a project root opt into a hierarchical scope (e.g. `monorepo/svc-billing`) that descendant directories inherit automatically.
3. `filepath.Base(cwd)`; `"unknown"` if cwd is empty. Matches the existing slot derivation.

Path-defaults config (longest-prefix cwd → scope match via persisted state) remains a v2+ alternative for users who don't want a checked-in marker file.

## What this changes vs. the existing v1 cut

**Adds**

- New SQLite tables (`scopes`, `plans`, `learnings`) via migration 0003.
- New daemon routes (`/v1/scope`, `/v1/scopes`, `/v1/plan`, `/v1/learnings`, `/v1/context`).
- New CLI verbs (`scope set/get/list/delete`, `plan write/read`, `learning append/list`, `context`).
- Bridge SessionStart returns `additionalContext`.

**Doesn't change**

- Session capture pipeline (unchanged).
- Adapter interface.
- Auth subsystem.
- Storage portability rules.
- The existing `slots` table (it stays as the pointer abstraction).

## Open questions to settle before code

1. **Plan model.** ✅ Resolved: **named slots, default `_default`**, matching ori's pattern. Single-slot usage is the 99% case; named slots unlock multi-plan workflows without redesign.
2. **Learnings append-only vs editable?** ✅ Resolved: **append-only with soft-supersede chains**. New entries can supersede prior ones (preserved in storage, hidden from default reads). Full history accessible via `?include=superseded`.
3. **Scope deletion with children?** ✅ Resolved: **refuse**. `DELETE /v1/scope?path=X` returns 409 if children exist; clean up explicitly.
4. **`.donotinherit` semantics.** ✅ Resolved: **boundary-reset**. The marker scope is included; everything above it is dropped from the walk. Sub-projects under a marker see only themselves + the chain from the marker down.
5. **Default cwd → scope derivation.** ✅ Resolved (amended post-v1): three-stage resolution. (a) `$RESLEEVE_SCOPE` env var override. (b) Walk cwd ancestors for a `.resleeve-scope` marker file; if found non-empty, scope = `<marker>/<rel-from-marker-dir>`. (c) `filepath.Base(cwd)`; `"unknown"` if cwd is empty. The amendment landed because v1 validation found that flat `filepath.Base` mapping made nested directories unable to inherit a parent scope's plans/learnings via the SessionStart context-injection path.
6. **`resleeve plan write` input flow.** ✅ Resolved: **stdin by default** (the 99% case is the agent writing). `--content "..."` and `--file <path>` for inline / file input. `--edit` opt-in opens `$EDITOR` for human convenience.
7. **Context injection default.** ✅ Resolved: **on by default**. Empty rolled-up context (no plans/learnings on the chain) → bridge emits no `additionalContext` (silent no-op; nothing injected).
8. **`Scope.Kind` discrete or free-form?** ✅ Resolved: **discrete enum** — `portfolio | program | project | dispatch | agent | other`. Validated at the API layer; DB column stays plain `TEXT` so future kinds are a code change, not a migration.

## Implementation slicing — three commits

1. **Memory data plane** — `internal/memory/` types + migration 0003 + `MemoryStore` interface (+ Scopes/Plans/Learnings sub-stores) + SQLite impl + unit tests (CRUD round-trip + inheritance walk).
2. **API + CLI** — daemon routes + CLI verbs (`scope`, `plan`, `learning`, `context`). Smoke-test against running daemon.
3. **Bridge context injection** — `hook.go` adds SessionStart → context fetch → `additionalContext` stdout. Verify against a real `claude` session.

## What "v1 done" looks like after this lands

Acceptance criteria additions to `00-v1-cut.md`:

- After `resleeve up`, user can `resleeve scope set my-project --title "..." --kind project`, `resleeve plan write my-project`, `resleeve learning append my-project "..."`.
- Starting `claude` in a directory that derives to that scope causes Claude to receive the plan + learnings as session-start context.
- `resleeve context my-project` prints exactly what was injected.
