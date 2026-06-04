# v2 — Cross-machine sync

## Architecture

```
┌─────────────┐     HTTPS      ┌─────────────┐               ┌────────────────┐
│ resleeve    │ ─────────────> │ resleeve    │ ───Backend──> │ local-disk     │
│ client      │   (ciphertext) │ serve       │   interface   │ object-store   │
│ KEK local   │ <───────────── │ stateless   │               │ git            │
└─────────────┘                └─────────────┘               └────────────────┘
```

Two new processes vs. v1:

- **`resleeve serve`** — a new binary subcommand. Stateless HTTP API.
  Speaks only in opaque ciphertext blobs + a tiny envelope of (user_id,
  resource_kind, resource_id, seq, ciphertext_blob). Cannot decrypt
  anything it stores.
- **`Backend`** — a Go interface behind `serve`. The same wire format,
  three v2 implementations: local-disk, object-store (S3/R2/B2/GCS),
  git.

The **client KEK never leaves the client.** Server compromise reveals
nothing about session content. Standalone-mode KEK is derived from the
user's master password via Argon2id (already implemented in
`internal/auth/`). The pairing flow distributes the same KEK to additional
devices via the recovery-key wrap.

## What's synced

| Resource | Tier | Key shape |
|---|---|---|
| `sessions` row | slow | `users/<uid>/sessions/<sid>/meta` |
| `events` rows | slow | `users/<uid>/sessions/<sid>/events/<seq>` |
| `scopes`, `plans`, `learnings` | fast | `users/<uid>/memory/<scope-path>/<resource>/<seq>` |
| `slots` | not synced | machine-local |
| `users` | server-only | not pushed by clients |

Each row is encrypted independently (per-row envelope under the user's
KEK) so the server can list and seek by `(uid, kind, since-seq)`
without seeing content.

## Backend interface

```go
type Backend interface {
    // Put stores a blob under key. Idempotent: re-Put with same key + same
    // blob is a no-op; same key + different blob is an error (caller
    // should never do this — keys include seq).
    Put(ctx, key string, blob []byte) error

    // Get returns the blob at key, or ErrNotFound.
    Get(ctx, key string) ([]byte, error)

    // List returns keys under prefix that sort > sinceCursor. Pagination
    // via nextCursor. Sort is lexicographic on key, which encodes seq.
    List(ctx, prefix string, sinceCursor string, limit int) (
        keys []string, nextCursor string, err error)

    // Delete removes a key. Used only by retention; never by sync logic.
    Delete(ctx, key string) error
}
```

### Local disk backend

- Files under `<data-dir>/<uid>/<kind>/<id>/<seq>.enc`
- Cursor = lexicographic concat of (kind, id, seq) as a single path
- Trivial. Useful for VPS / homeserver self-hosting.

### Object store backend

- S3-compatible API (AWS S3, Cloudflare R2, Backblaze B2, GCS in
  XML-API mode)
- Same key shape as local disk
- `List` uses `ListObjectsV2` with prefix + start-after
- Lifecycle policies (retention) belong to the bucket config, not
  resleeve
- Server holds IAM credentials; clients never see them

### Git backend

- One git repo per user (or per scope; deferred decision)
- Each `Put` = a commit with the encrypted blob as the file content,
  the key as the path
- `List` walks the tree under prefix
- Free hosting via private GitHub / GitLab / Codeberg repos
- Tradeoff: push latency is git's, not HTTP's. Acceptable for hobbyist
  fleets; not for high-volume operators.

## Wire format (`resleeve serve` HTTP API)

```
POST /v2/sync/push
Headers: Authorization: Bearer <device-token>
Body: { batch: [ { key, blob (b64), prev_cursor }, ... ] }
→ 200 { committed_cursor }
→ 409 conflict on key (caller bug — keys must include seq)

GET /v2/sync/pull?kind=sessions&since=<cursor>&limit=500
Headers: Authorization: Bearer <device-token>
→ 200 { rows: [ { key, blob (b64) }, ... ], next_cursor }

GET /v2/sync/sse?kind=memory
Headers: Authorization: Bearer <device-token>
→ text/event-stream; data: { key, blob (b64) } per push from other devices
   heartbeat ":\n" every 15s
```

All bodies are JSON. All blobs are pre-encrypted by the client. Server
never sees plaintext.

## Sync protocol

### Slow tier (sessions + events)

**Push (client → server)**:

1. After every local `IngestBatch`, encrypt the batch with the device's
   per-resource sub-key (KEK-derived).
2. Append the encrypted row to the local **outbox** table.
3. Background goroutine drains the outbox: POST `/v2/sync/push`.
4. On 2xx, mark outbox rows as committed; remove on next sweep.
5. On network failure, leave in outbox; retry with exponential backoff
   (max 5min).

**Pull (client → server, periodic 30s)**:

1. Client maintains a per-kind cursor in `sync_state(kind, cursor)`
   table.
2. Every 30s: GET `/v2/sync/pull?kind=sessions&since=<cursor>` and
   `&kind=events&since=<cursor>` in parallel.
3. For each returned row: decrypt, call into local
   `Sessions.Create` / `Events.Append` (idempotent already).
4. Update cursor on success.

**On-demand pull**:

- Any explicit verb (`resleeve resume`, `session show`, `session list
  --remote`, etc.) triggers an immediate pull cycle before resolving.
- Timeout 2s, then fall back to local-stale view with a stderr warning
  "(upstream unreachable; showing local state)".

### Fast tier (memory)

**Push**: same shape as slow tier — encrypted to outbox, drained to
`/v2/sync/push?kind=memory`.

**Pull**: SSE.

1. Client opens GET `/v2/sync/sse?kind=memory&since=<cursor>` and
   holds the connection.
2. Server emits one `data:` event per memory write from any of the
   user's devices.
3. Client decrypts, ingests into local SQLite (idempotent via composite
   PKs).
4. On disconnect, reconnect with exponential backoff. On successful
   reconnect, replay missed events using `since=<cursor>` (resumable).
5. **Crucially**: the daemon's `/v1/context` handler does NOT wait for
   SSE freshness. The SSE feed keeps local DB warm; reads are local.

### Local outbox

```sql
CREATE TABLE outbox (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,   -- 'sessions' | 'events' | 'memory'
    key TEXT NOT NULL,
    blob BLOB NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error TEXT,
    UNIQUE(kind, key)
);
```

Drained by a background goroutine. Survives daemon restart. Cleaned up
on successful push.

## Identity

### Standalone mode (v2 MVP)

1. **Register** (one-time per user): `resleeve register --upstream
   https://sync.example.com`. Prompts for email + master password.
   Server stores `(user_id, email, argon2id_params,
   verifier, recovery_key_wrap)`.
2. **Login** (one-time per device): `resleeve login`. Prompts for master
   password. Derives KEK locally; gets a long-lived device token from the
   server; stores token under OS keychain (macOS Keychain / libsecret /
   Windows DPAPI).
3. **Daemon auth**: every push/pull/SSE request carries `Authorization:
   Bearer <device-token>`. Server validates token → user_id mapping.
4. **Logout**: drop unlocked KEK + revoke device token.

### SCION mode (deferred to round 5+)

- Resleeve serve trusts SCION as a JWT issuer.
- `resleeve login --scion` exchanges a SCION Agent Token for a resleeve
  device token.
- KEK still derived client-side, but from a SCION-managed identity
  rather than a master password.

The point: a second auth backend, slotted in behind the same device-token
abstraction. Sync protocol is auth-agnostic.

## Pairing flow

**Machine A** (already paired):

```
$ resleeve pair invite
Generated pairing code: D7K3-NX9P-VAT2 (expires in 60s)
Run on the new machine: resleeve pair accept D7K3-NX9P-VAT2
```

Under the hood:

1. Client generates a 24-byte random `pair_code`.
2. Client encrypts the recovery-key-wrapped KEK under `pair_code`.
3. Client POSTs the wrapped KEK to server under TTL=60s, indexed by
   `pair_code`.
4. Server returns `pair_code` to display.

**Machine B** (new device):

```
$ resleeve pair accept D7K3-NX9P-VAT2
Enter master password: ********
Pairing successful. Pulling state...
✓ 12 sessions, 4 scopes, 87 events synced
```

Under the hood:

1. Client POSTs `pair_code` to server, gets the wrapped KEK.
2. Client prompts user for master password.
3. Client derives Argon2id KEK locally.
4. Client decrypts the wrapped KEK from A. If the master password is
   correct, the two KEKs match → device is paired.
5. Client registers a new device token with the server.
6. Client triggers an initial pull of all state.

Crucially: server only ever sees the wrapped-KEK blob. It cannot
decrypt it without the master password (or the recovery key).

## Open implementation questions

These don't block the design but need answers before code:

- **TLS termination**: bring-your-own (user puts caddy / nginx in
  front) or built-in (LetsEncrypt autocert). Recommend bring-your-own
  for v2; built-in adds operational surface area.
- **Rate limiting**: per-token request quotas. Defer to v3 unless we
  see abuse.
- **Multi-tenant** in `resleeve serve`: yes, design assumes it.
- **Garbage collection**: when does the server delete old rows? Never
  (durability) or after retention window (cost). Defer to backend
  config.

## What this changes vs. v1

**Adds**

- `resleeve serve` binary surface.
- `Backend` interface + 3 implementations.
- `internal/sync/` package: outbox, pull cursor, SSE client.
- New SQLite tables: `outbox`, `sync_state`.
- Daemon background goroutines: outbox drain, periodic pull, SSE
  consumer.
- CLI: `register`, `login`, `logout`, `pair invite`, `pair accept`.

**Doesn't change**

- Local capture path (events still land in local SQLite first).
- Memory module API.
- Adapter interface (orthogonal).
- Wire format for v1 daemon-local API (`/v1/...` preserved).
