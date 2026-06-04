# Architecture

> Contributor-facing overview of resleeve's internals.
> For installation and verbs see [`QUICKSTART.md`](./QUICKSTART.md) and
> [`USER_GUIDE.md`](./USER_GUIDE.md). For decision history see
> [`design/`](./design/).

## 1. What this is

Resleeve is a single Go binary plus an embedded SQLite database that
captures AI-coding-CLI sessions, maintains a hierarchical memory store
(plans + learnings under scopes), and optionally syncs both to a
self-hosted upstream so a "stack" of context can follow the operator
across machines. The locked persona is the **fleet operator**
(`design/round-1/05-decisions.md`): one human, elastic compute, stacks
persist, sleeves rotate. Three concurrent surfaces run inside the
binary: a **local daemon** on loopback that the host CLI talks to via
hooks, an **MCP server** the model talks to over stdio, and an optional
**`resleeve serve`** HTTP API the daemon syncs against over the
internet.

### Data flow

```
                                                +----------------------+
  Claude Code  --hook envelope--> resleeve hook | resleeve agent       |
  (host CLI)   (stdin JSON)       (cli/hook.go) |  (loopback :PORT)    |
       ^                                        |                      |
       | hookSpecificOutput.additionalContext   |  /v1/sessions  (POST)|
       | + systemMessage  (stdout JSON)         |  /v1/scope|plan|...  |
       |                                        |  /v1/seal/unlock     |
       +----------------------------------------|  /v1/doctor/*        |
                                                |                      |
   resleeve mcp --stdio  <-JSON-RPC->           |  IngestBatch         |
   (model)                                      |    |                 |
                                                |    v                 |
                                                |  SQLite              |
                                                |  ~/.resleeve/db      |
                                                |  sessions, events,   |
                                                |  scopes, plans,      |
                                                |  learnings, outbox   |
                                                |                      |
                                                |  SyncClient          |
                                                |   |  Enqueue->outbox |
                                                |   |  drain  (1s)     |
                                                |   |  pull   (30s)    |
                                                |   |  SSE memory tier |
                                                +---|------------------+
                                                    |
                                       HTTPS, AES-GCM ciphertext only
                                                    v
                                          +------------------------+
                                          | resleeve serve         |
                                          |   /v2/auth/register    |
                                          |   /v2/auth/login*      |
                                          |   /v2/auth/pair/*      |
                                          |   /v2/sync/push        |
                                          |   /v2/sync/pull        |
                                          |   /v2/sync/sse         |
                                          +-----------|------------+
                                                      |
                                            Backend (sync.Backend)
                                            local-disk impl ships v1
                                                      |
                                                      v
                                              +---------------+
                                              | other machine |
                                              |  pull / SSE   |
                                              +---------------+
```

Three paths share that picture:

1. **Capture path** — host CLI fires a hook; the `resleeve hook` CLI
   forwards the envelope to the daemon's `/v1/sessions` route; daemon's
   `IngestBatch` writes to SQLite and enqueues outbox rows.
2. **Memory path** — `resleeve mcp --stdio` (or `resleeve plan write`
   from the CLI) calls `/v1/scope`, `/v1/plan`, `/v1/learnings`; on
   SessionStart the hook also calls `/v1/context` and pipes the result
   back as `hookSpecificOutput.additionalContext`.
3. **Sync path** — daemon's `SyncClient` drains the outbox 1s/tick
   against the upstream, polls `/v2/sync/pull` every 30s for the slow
   tier (sessions + events), and holds an SSE connection to
   `/v2/sync/sse` for the memory fast tier. All blobs are AES-GCM
   sealed before they leave the loopback; the upstream is
   zero-knowledge.

## 2. Package layout

Everything lives under `internal/`. The boundary that matters: the
**daemon** is loopback-only; **`resleeve serve`** is internet-facing.
They share `internal/storage` and `internal/auth`, but otherwise their
concerns don't overlap.

```
internal/
  agent/            local daemon (loopback HTTP, IngestBatch, SyncClient)
  serve/            upstream HTTP API (/v2/auth/*, /v2/sync/*)
  sync/             Backend interface; sync/local implements local-disk
  auth/             Argon2id, KEK derivation, AES-GCM envelope (Sealer)
  adapter/          Adapter interface; claude impl + opencode stub
    common/         shared prime-mode synthesizer
  mcp/              MCP JSON-RPC server, tool registry, INSTRUCTIONS.md
  memory/           scope tree walk, plans, learnings, BuildContext
  storage/sql/sqlite/   store + migrations 0001-0005
  cli/              verb dispatch
  event/            normalized event type used across adapters
```

### `internal/agent` — the local daemon

`daemon.go` boots the loopback HTTP server (token in
`~/.resleeve/auth`). Five route registrars wire the surface:

| Registrar                | Routes                                       |
|--------------------------|----------------------------------------------|
| `registerRoutes`         | `/v1/health`, `/v1/sessions`, `/v1/sessions/`, `/v1/search` |
| `registerMemoryRoutes`   | `/v1/scope`, `/v1/scopes`, `/v1/plan`, `/v1/plans`, `/v1/learnings`, `/v1/context` |
| `registerSyncRoutes`     | `/v1/sync/push-now`, `/v1/sync/pull-now`     |
| `registerDoctorRoutes`   | `/v1/doctor/sync-status`                     |
| `registerSealRoutes`     | `/v1/seal/unlock`, `/v1/seal/lock`, `/v1/seal/status` |

`ingest.go` is the single inbound funnel: `IngestBatch` writes sessions
and events transactionally and is the only call site that may enqueue
outbox rows. `sync.go` owns the `SyncClient` (drain loop, pull loop,
SSE loop). `backfill.go` is the reconcile sweep (§9).

### `internal/serve` — the upstream

Two handler families. `server.go` mounts the data-plane:
`/v2/sync/{push,pull,sse,health}`, guarded by `requireAuth` against
a per-device bearer token. `auth_handlers.go` mounts the
identity-plane: `/v2/auth/{register,login-challenge,login,logout,
pair/publish,pair/claim}`. The upstream never holds a KEK and never
decrypts session or memory blobs — see §6.

### `internal/sync`

Defines `Backend` (Put/Get/List/Delete on opaque keys). `sync/local`
ships the only implementation: a directory tree under the upstream's
data dir, keys are filesystem paths, List is a lexicographic walk for
cursor pagination. Object-store and git backends slot in here per
`round-2/11-storage-backends.md`.

### `internal/auth`

`argon2.go` derives keys from passwords / recovery keys / pair codes
under one Argon2id parameter set. `kek.go` is the KEK marshalling: a
per-user 32-byte master key, wrapped (twice) under
password-derived-key and recovery-key-derived-key. `envelope.go` is
the `Sealer` interface and the AES-256-GCM implementation
(`AESGCMSealer`) used at the sync boundary. `recovery.go` mints
recovery phrases. `user.go` is the in-memory user record shared
between client and server identity paths.

### `internal/adapter`

`adapter.go` declares the interface:

```go
type Adapter interface {
    Name() string
    Detect(ctx context.Context) (Detection, error)
    InstallBridge(ctx context.Context, opts InstallOpts) error
    UninstallBridge(ctx context.Context) error
    FromNative(ctx context.Context, raw []byte, src Source) ([]event.Event, error)
    ToNative(ctx context.Context, events []event.Event, mode RenderMode) ([]byte, error)
    Hydrate(ctx context.Context, session SessionView, opts HydrateOpts) (HydrateResult, error)
    NativeResumeCmd(ctx context.Context, session SessionView, result HydrateResult) (cmd string, args []string, err error)
}
```

`adapter/claude` is the only fully-featured implementation:
hook-install via `~/.claude/settings.json`, JSONL parsing/emission for
session capture and replay, prime-mode synthesizer wired through
`adapter/common/prime.go`. `adapter/opencode` is a stub that
implements `Detect` + the prime path only; full opencode capture is
v3.

### `internal/mcp`

`server.go` runs JSON-RPC 2.0 over stdio. `protocol.go` declares the
initialize/tools/list/tools/call types. `tools.go` registers the
ten `resleeve_*` tools the model can call. `INSTRUCTIONS.md` is
embedded via `go:embed` and returned in the `instructions` field of
the initialize response — the standing prompt the host injects above
the model's system prompt. See §5.

### `internal/memory`

`memory.go` declares `Scope`, `Plan`, `Learning`. `walk.go` implements
`Ancestors` (split a `/`-delimited scope path into its lineage) and
`ApplyBoundary` (the `.donotinherit` reset rule). `context.go` is
`BuildContext`: walk the chain, render each surviving scope's
default-slot plan and learnings as a single markdown document. The
SessionStart hook serves this exact document back to the model.

### `internal/storage/sql/sqlite`

Embedded migrations (see §3), `sqlite.go` opens the DB and runs them
in order. `memory.go`, `sync.go`, `identity.go`, `users.go` are the
per-domain accessor layers. The package is structured so the migration
SQL is portable ANSI — Postgres / Spanner-PG / AlloyDB implementations
plug in later behind the same accessor signatures
(`round-2/11-storage-backends.md`).

### `internal/cli`

`dispatch.go` is a `switch` on `argv[1]`. Each verb has a sibling
file: `agent.go`, `auth.go`, `context_cmd.go`, `hook.go`,
`install_bridge.go`, `keychain.go`, `learning.go`, `lifecycle.go`
(up/down/doctor), `mcp.go`, `migrate_key.go`, `on_demand_pull.go`,
`pair.go`, `plan.go`, `resume.go`, `scope.go`, `serve.go`,
`session.go`, `sync.go`. Verbs that touch the daemon do it over
loopback HTTP — there is no shared in-process state with the CLI.

## 3. Data tables

Five embedded migrations under
`internal/storage/sql/sqlite/migrations/`:

### Round 1 (migration 0001) — capture

```sql
CREATE TABLE sessions (
    id            TEXT PRIMARY KEY,
    scope         TEXT NOT NULL,
    agent_name    TEXT NOT NULL,
    cli           TEXT NOT NULL,
    cli_version   TEXT NOT NULL,
    cwd           TEXT NOT NULL,
    git_branch    TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    started_at    TEXT NOT NULL,
    ended_at      TEXT,
    event_count   INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL,
    exit_status   INTEGER
);

CREATE TABLE events (
    event_uuid             TEXT NOT NULL,
    session_id             TEXT NOT NULL,
    seq                    INTEGER NOT NULL,
    turn_id                TEXT,
    parent_event_uuid      TEXT,
    ts                     TEXT NOT NULL,
    kind                   TEXT NOT NULL,
    schema_version         INTEGER NOT NULL,
    content                TEXT NOT NULL,
    vendor_name            TEXT NOT NULL,
    vendor_version         TEXT NOT NULL,
    vendor_native_payload  BLOB,
    PRIMARY KEY (session_id, event_uuid),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
```

`seq` is `ts.UnixNano()` (not a 1,2,3 counter); same-nanosecond ties
are broken by `event_uuid` ascending. All `ORDER BY` paths include the
tiebreak.

### Round 2 (migration 0002) — local user

`users` holds the client-side identity record: Argon2id parameters,
two verifiers (password + recovery), two wrapped KEKs (password +
recovery). One row per local login. Independent salts on each
verifier-vs-wrap pair so leaking the verifier doesn't leak the
wrap-key derivation.

`slots` (defined in 0001 alongside sessions/events) is the
`(scope, agent_name) -> current_session_id + last_heartbeat_at` table
that lets the fleet operator's orchestrator name target a slot
deterministically.

### Round 3 (migration 0003) — memory

```sql
CREATE TABLE scopes (
    path TEXT PRIMARY KEY,
    kind TEXT, title TEXT, description TEXT, cwd TEXT,
    do_not_inherit INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);

CREATE TABLE plans (
    scope   TEXT NOT NULL,
    name    TEXT NOT NULL DEFAULT '_default',
    content TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (scope, name)
);

CREATE TABLE learnings (
    id            TEXT PRIMARY KEY,
    scope         TEXT NOT NULL,
    content       TEXT NOT NULL,
    supersedes_id TEXT,
    created_at    TEXT NOT NULL
);
```

Plans are named slots (default `_default`); writes overwrite. Learnings
are append-only with `supersedes_id` chains for soft retraction.

### Round 4 (migration 0004) — outbox + sync state

```sql
CREATE TABLE outbox (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    kind            TEXT NOT NULL,   -- sessions | events | memory
    key             TEXT NOT NULL,
    blob            BLOB NOT NULL,   -- sealed ciphertext
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error      TEXT,
    UNIQUE(kind, key)
);

CREATE TABLE sync_state (
    kind       TEXT PRIMARY KEY,     -- sessions | events | memory
    cursor     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);
```

`next_attempt_at` uses a fixed-width nanosecond format so lexicographic
SQL compares behave; `time.RFC3339Nano` strips trailing zeros and
breaks ordering (caught in a production flake; see learnings log).

### Round 5 (migration 0005) — server-side identity

`server_users` mirrors the client `users` table but lives in the
upstream's database. `devices` is one row per paired device, holding
the per-device bearer token presented on every `/v2/sync/*` call —
compromise of one device rotates that device, not the account.
`pairing_codes` is the short-lived (≤5 min, one-shot) bridge for
adding a second device: code-derived verifier and code-derived KEK
wrap, brute-force bounded by Argon2id cost plus the 5-minute TTL.

## 4. The hook contract

`resleeve install-bridge` writes matcher-wrapped entries to
`~/.claude/settings.json` for four events: `SessionStart`,
`PostToolUse`, `Stop`, `UserPromptSubmit`. F12 wrapper shape:

```json
"PostToolUse": [
  {
    "matcher": "",
    "hooks": [
      { "type": "command", "command": "resleeve hook --adapter=claude", "_resleeve": true }
    ]
  }
]
```

Each fire pipes the CC envelope JSON to `resleeve hook` on stdin.
`internal/cli/hook.go` decodes it, looks up the resolved scope (via
`.resleeve-scope` walk + `$RESLEEVE_SCOPE` override), and either
forwards the captured event to the daemon or emits the SessionStart
injection envelope.

### SessionStart return envelope (F13 + F14)

For `hook_event_name == "SessionStart"`, the hook calls
`GET /v1/context?scope=…` against the daemon and emits to stdout:

```json
{
  "systemMessage": "resleeve: loaded scope resleeve (4231 bytes)",
  "hookSpecificOutput": {
    "hookEventName": "SessionStart",
    "additionalContext": "## Plan (resleeve)\n\n…"
  }
}
```

- `hookSpecificOutput.additionalContext` is silently injected above the
  model's system prompt (F13 — without the wrapper, CC drops the
  whole field on the floor).
- Top-level `systemMessage` renders as a visible notice in the chat UI
  (F14 — audit trail without it makes a silent feature look broken).
- The hook also writes the injected body to
  `~/.resleeve/last-injected.md` for post-hoc inspection.

### `--memory-only` mode

`resleeve up --memory-only` (or `install-bridge --memory-only`)
installs **only** the SessionStart hook and bakes `--memory-only` into
the hook command. In that mode the hook skips `AppendEvents`
entirely — zero rows land in `sessions` / `events`. Memory injection
still fires. The bridge installer correctly strips
PostToolUse/Stop/UserPromptSubmit on a full → memory-only switch and
re-installs them on the reverse. Memory CRUD via MCP and the CLI keeps
working (it goes through `/v1/scope|plan|learnings`, not events).

## 5. The MCP contract

`resleeve mcp --stdio` (the only transport shipped — HTTP is reserved
for a future debug surface) runs a JSON-RPC 2.0 loop.

### Initialize handshake

The host (Claude Code, etc.) sends `initialize`; the server responds
with capabilities **and** an `instructions` field — a free-form
markdown blob the host injects into the system prompt. Contents:
`internal/mcp/INSTRUCTIONS.md` plus, if the current scope can be
resolved, the rendered scope context (capped at 8 KB per section so a
runaway log can't blow the host's instructions budget).

### `tools/list`

Returns the ten registered tools, each a `{name, description,
inputSchema}` triple:

- `resleeve_scope_set`, `_get`, `_list`, `_delete`
- `resleeve_plan_write`, `_read`, `_list`
- `resleeve_learning_append`, `_learning_list`
- `resleeve_context`

### `tools/call`

Dispatches by `name` into a handler registered in `tools.go`. Each
handler validates the args via the embedded schema, calls the
corresponding daemon endpoint (`/v1/scope`, `/v1/plan`,
`/v1/learnings`, `/v1/context`), and returns a `content` array.

Curation rule (in `INSTRUCTIONS.md`): the model **never** writes
without an explicit user prompt — phrases like "save this as a
learning" or "update the plan" are the trigger. A session summary is
not a learning unless the user says so.

## 6. Daemon vs serve split

The split matters for security and deployability.

| Concern             | `resleeve agent` (daemon)            | `resleeve serve` (upstream)           |
|---------------------|--------------------------------------|---------------------------------------|
| Network             | Loopback only                        | Internet-facing                       |
| Auth                | Single bearer token in `~/.resleeve/auth` for legacy / standalone; per-device tokens from pairing for v2 mode | Per-device bearer issued at register / pair |
| Holds KEK           | Yes (after `resleeve login`)         | **Never** — zero-knowledge invariant  |
| Holds plaintext     | Yes (SQLite, sealed-at-rest is a v3 polish) | Never — only sealed blobs      |
| Hot endpoints       | `/v1/sessions`, `/v1/scope`, `/v1/plan`, `/v1/learnings`, `/v1/context` | `/v2/sync/push`, `/v2/sync/pull`, `/v2/sync/sse` |
| Identity endpoints  | `/v1/seal/{unlock,lock,status}`      | `/v2/auth/{register,login*,logout,pair/*}` |

### Sealer runtime install

The daemon boots without a Sealer; `SyncClient` parks. `resleeve login`
on the same host derives the KEK from the master password (Argon2id),
unwraps the user's wrapped KEK, then POSTs the 32-byte KEK to the
daemon's `/v1/seal/unlock`. The daemon constructs an `AESGCMSealer`
and wakes the sync loops. `/v1/seal/lock` zeros it again.

The legacy path — a `seal.key` placeholder file at
`~/.resleeve/seal.key` (mode 0600) loaded at daemon start — is still
supported for **standalone-mode** deployments (single host, no
upstream, no real identity). `resleeve migrate-key` migrates a
standalone install to the password-derived KEK; the v1 placeholder is
deprecated and called out before any "encrypted sync" claim in the
user-facing docs.

## 7. Sync data flow

`SyncClient` (`internal/agent/sync.go`) owns three concurrent loops.

### Outbox push (slow tier)

```
IngestBatch -> Sessions.Create / Events.Append (txn)
            -> SyncClient.EnqueueSession / EnqueueEvents
            -> store.Sync().EnqueueOutbox(kind, key, sealedBlob, now)
```

`drainLoop` ticks at `drainInterval = 1s`: `DequeueOutbox(batchSize)`,
POST `/v2/sync/push`, `AckOutbox` on 2xx, `BumpOutboxAttempt` with
exponential backoff (2s, 4s, 8s, … capped at 5 min) on failure.
**Idempotent enqueue** via `UNIQUE(kind, key)` — a retried write is a
no-op.

### Pull (slow tier)

`pullLoop` ticks at `pullInterval = 30s`: GET
`/v2/sync/pull?kind={sessions|events|memory}&cursor=…`. The pull path
**bypasses IngestBatch** and writes directly via the store accessors,
so a re-pull cannot loop back into the local outbox.
`TestSyncClient_PullThenPushDoesNotLoop` guards this invariant.

### SSE (fast tier, memory only)

`sseLoop` holds a long-lived GET to `/v2/sync/sse` on a separate
`http.Client` with no timeout. On disconnect: reconnect with backoff,
backfill via the last seen cursor in `sync_state`, then resume
streaming. Memory updates (scope / plan / learning) are pushed in
near-real-time.

### Zero-knowledge invariant

The seal/open boundary is at `Enqueue*` (push side) and
`ingestPulled` / SSE delivery (pull side). The upstream sees nothing
but ciphertext. Slice 3 introduced `EnqueueScope/Plan/Learning`
without slice 2.5's seal wrap and shipped plaintext memory to the
upstream — caught only by a live cross-machine smoke
(commit f402da9). Lesson is baked into
`TestSyncClient_SealedMemoryBlobsAreOpaqueOnUpstream`.

## 8. Encryption envelope

`internal/auth/envelope.go` defines:

```go
type Sealer interface {
    Seal(plaintext []byte) ([]byte, error)
    Open(ciphertext []byte) ([]byte, error)
}
```

`AESGCMSealer` is AES-256-GCM with a fresh 12-byte nonce per Seal.
Wire format:

```
<version:1=0x01> <nonce:12> <ciphertext> <gcm-tag:16>
```

The version byte lets us roll the cipher without re-encrypting
existing data — `Open` dispatches on the prefix.

### KEK source

```
master password ──Argon2id (password_kek_salt)──> KEK-wrap key
                                                      │
                                                AES-GCM unwrap
                                                      │
                                                      v
                                                  KEK (32 bytes)
                                                      │
                              POST /v1/seal/unlock (loopback)
                                                      │
                                                      v
                                            daemon AESGCMSealer
                                                      │
                                                      v
                                       Seal/Open at sync boundary
```

`resleeve login` runs the Argon2id derivation on the CLI side, fetches
the wrapped KEK from the upstream's `/v2/auth/login`, unwraps locally,
and ships the plaintext KEK across loopback to the daemon. The
upstream never sees the password and never sees the unwrapped KEK.

### Legacy `seal.key` path (deprecated)

Pre-round-5 installs used a 32-byte random key at `~/.resleeve/seal.key`
(mode 0600). It still works for standalone single-host operation, but
multi-device required manual `seal.key` copy + matching auth token.
`resleeve migrate-key` migrates a standalone install onto the
password-derived KEK once the user has registered with an upstream.

## 9. Reconcile sweep

On daemon startup the Claude adapter (`internal/agent/backfill.go`)
walks `~/.claude/projects/**/*.jsonl`, builds a `session_id -> path`
index, and for each session row in the local SQLite that is missing a
`cwd` (a pre-F1+F2+F3 artifact), parses the JSONL to recover and
backfill it. Sessions whose JSONL is no longer on disk are skipped
without erroring.

The hook path **synthesizes** a `session_start` event from
`SessionStart` envelope data when one hasn't been seen — F1 + F2 + F3.
This is what lets resleeve survive the case where the operator opens a
CC window before the daemon learns of the session, then the daemon
starts and finds the JSONL on disk.

## 10. Adapter interface

```go
type Adapter interface {
    Name() string
    Detect(ctx context.Context) (Detection, error)

    // Bridge lifecycle
    InstallBridge(ctx context.Context, opts InstallOpts) error
    UninstallBridge(ctx context.Context) error

    // Capture
    FromNative(ctx context.Context, raw []byte, src Source) ([]event.Event, error)

    // Re-sleeve
    ToNative(ctx context.Context, events []event.Event, mode RenderMode) ([]byte, error)
    Hydrate(ctx context.Context, session SessionView, opts HydrateOpts) (HydrateResult, error)
    NativeResumeCmd(ctx context.Context, session SessionView, result HydrateResult) (cmd string, args []string, err error)
}
```

`InstallOpts.MemoryOnly` toggles the memory-only bridge mode (§4).

### Replay vs prime

Two `RenderMode` values:

- **Replay** (`RenderModeReplay`) — emit the target CLI's native local
  format (JSONL for Claude Code). Full fidelity. Only meaningful when
  source CLI == target CLI on a machine that supports the format.
- **Prime** (`RenderModePrime`) — emit a synthesized opening prompt
  summarizing prior activity. Lossy by design. Cross-CLI universal
  path. The shared synthesizer lives at
  `internal/adapter/common/prime.go` so opencode, codex, gemini all
  benefit when their adapters land.

`RenderModeAuto` (the default when `HydrateOpts.Mode` is empty) lets
the adapter pick: replay if it can, prime if it can't.

### Per-CLI implementations

| Adapter           | Capture (FromNative) | Replay (ToNative)    | Prime (Hydrate) |
|-------------------|----------------------|----------------------|-----------------|
| `adapter/claude`  | Hook + JSONL         | JSONL                | via common      |
| `adapter/opencode`| stub (`ErrNotImplemented`) | stub          | via common      |

## 11. Design doc index

Round-by-round under `docs/design/`. One-liner per doc:

### Round 1 — research, thinking, design (`design/round-1/`)

- `01-ori-baseline.md` — review of the predecessor private project.
- `02-cli-audit.md` — on-disk session format of every target CLI;
  decided opaque-blob vs normalized schema.
- `03-prior-art.md` — competitor survey (SpecStory, CCManager, etc.).
- `04-design.md` — initial design pass (historical; superseded by 05).
- `05-decisions.md` — locked-in decisions: name `resleeve`, fleet
  operator persona, single Go binary + SQLite.

### Round 2 — fully-featured design (`design/round-2/`)

- `01-journey-fleet-operator.md` — primary persona, drives the design.
- `02-journey-01-decisions.md` — resolutions to the 10 fleet-operator
  open questions.
- `03-eventing-system.md` — two-pipe architecture (session firehose +
  state events).
- `04-event-schema.md` — normalized + vendor event envelope.
- `05-journey-cross-machine.md` — cross-machine solo dev journey.
- `06-journey-single-machine.md` — single-machine solo dev (the MVP
  user).
- `07-journey-oss-share.md` — OSS contributor sharing a session.
- `08-api-surface.md` — every HTTP endpoint, consolidated.
- `09-adapter-interface.md` — the Adapter Go interface (impl'd in
  round 4).
- `10-auth-subsystem.md` — identity, KEK, Argon2id, multi-user.
- `11-storage-backends.md` — SQL + blob backend pluggability.

### Round 3 — v1 cut + memory (`design/round-3/`)

- `00-v1-cut.md` — thinnest vertical slice: single-machine, Claude
  Code only.
- `01-memory-module.md` — scopes, plans (named slots), learnings
  (append-only with soft-supersede). What landed in migration 0003.

### Round 4 — fleet-operator MVP (`design/round-4/`)

- `00-fleet-operator-cut.md` — what the round delivers.
- `01-conversation-transport.md` — Stage 5: filling in `ToNative`,
  `Hydrate`, `NativeResumeCmd`.
- `02-cross-machine-sync.md` — v2 architecture: `resleeve serve`,
  Backend interface, outbox + cursor pull + SSE memory tier,
  pairing flow, identity schema.
- `03-rehydrate-ux.md` — new CLI verbs: `register`, `login`,
  `logout`, `pair invite | accept`, `migrate-key`.

Round 5 is in-flight; see the polish followups listed in the
`resleeve` plan.
