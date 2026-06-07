# Round 12 — Part A, slice 1: server-at-rest encryption

Implements the **server-at-rest** cell of the encryption matrix
(`00-encryption-and-versioning.md` §"Part A") via **envelope encryption**:
an operator **master key** wraps a **per-brain Data Encryption Key (DEK)**;
the server transparently encrypts each blob with the brain's DEK before
`backend.Put` and decrypts after `backend.Get`.

Decisions (LOCKED, operator signed off):
1. **Master key + per-brain DEKs (envelope)** — not one global key over all
   data, and not per-blob KMS calls. Rotating the master key re-wraps the
   (small) DEKs without re-encrypting the (large) blob corpus.
2. **Server-side is the default policy** — `brains.encryption_policy`
   defaults to `'server-side'`.

## Scope of THIS slice

Server-side machinery only. This is **additive**: the daemon still
client-seals blobs before push (the existing zero-knowledge / end-to-end
personal layer), and that path is **untouched**. The server now ALSO
encrypts at rest with a per-brain DEK. Double-encryption of an
already-sealed blob is fine here — the **client default-flip** (stop
sealing for server-side brains) is the **next slice** (see below).

Out of scope: local-at-rest (already shipped via the OS keychain),
end-to-end opt-in (`'e2e'` policy), group-key shared-brain ZK, KMS/HSM.

## Master-key config

`serve.Config.MasterKey []byte` — 32 bytes (AES-256). Loaded by the serve
CLI (`internal/cli/serve.go`) from, in precedence order:

- `--master-key <file>` (file contents), else
- `$RESLEEVE_SERVER_MASTER_KEY`.

The value is decoded as **hex first, then base64** (std, then raw-std) and
must be exactly 32 bytes. **Optional this slice**: when neither source is
set, the server **skips at-rest encryption** entirely (stores blobs as
today) and logs a one-line notice (`at-rest  DISABLED ...`). A
configured-but-wrong-length key fails server construction (`serve.New`).

> The flip slice will make the master key **required** in multi-tenant
> mode.

Key material is **never logged**.

## The DEK envelope (AEAD shape)

Helpers live in `internal/auth/atrest.go`. All three operations share one
AEAD shape: **AES-256-GCM, random 12-byte nonce, output = `nonce || ciphertext+tag`**.

| Function | Purpose |
|---|---|
| `GenerateDEK() ([]byte, error)` | 32 random bytes |
| `WrapDEK(masterKey, dek) ([]byte, error)` | encrypt DEK under master key |
| `UnwrapDEK(masterKey, wrapped) ([]byte, error)` | decrypt; asserts 32-byte result |
| `EncryptAtRest(dek, plaintext) ([]byte, error)` | encrypt a blob under a brain DEK |
| `DecryptAtRest(dek, blob) ([]byte, error)` | decrypt a stored blob |

A wrong/rotated master key, a wrong DEK, or any tampering fails the GCM
tag check and returns an **error — never silent plaintext**. These are
pure functions over a caller-supplied key (no retained cipher state),
distinct from the version-prefixed `Sealer` envelope in `envelope.go`,
which suits the per-request DEK lifecycle the sync handlers use.

## Schema

Migration `0010_server_at_rest.up.sql`:

```sql
CREATE TABLE brain_keys (
    brain_id    TEXT PRIMARY KEY,
    wrapped_dek BLOB NOT NULL,   -- DEK encrypted under the master key (nonce||ct+tag)
    created_at  TEXT NOT NULL,
    FOREIGN KEY (brain_id) REFERENCES brains(id) ON DELETE CASCADE
);

ALTER TABLE brains
    ADD COLUMN encryption_policy TEXT NOT NULL DEFAULT 'server-side';  -- 'server-side' | 'e2e'
```

- **`brain_keys`** — one wrapped DEK per brain. `ON DELETE CASCADE` so
  dropping a brain drops its key. Store API (`rsql.BrainKeyStore`):
  `PutBrainKey(brainID, wrappedDEK)` (upsert, for future rotation),
  `GetBrainKey(brainID) ([]byte, error)` (`ErrNotFound` if none). Wired
  into the `Store` aggregate as `Store.BrainKeys()`.
- **`brains.encryption_policy`** — surfaced on `rsql.Brain.EncryptionPolicy`
  (`EncryptionPolicyServerSide` | `EncryptionPolicyE2E`). Stored and
  defaulted this slice; **not branched on** — only `'server-side'` is
  exercised.

## Where DEK generation hooks into brain create

When a brain is provisioned AND a master key is configured
(`Server.atRestEnabled()`), the server does `GenerateDEK → WrapDEK →
PutBrainKey` and warms the in-process DEK cache. No master key → skip.
Both create paths call the shared `Server.provisionBrainKey`:

- **personal brain** — `provisionPersonalBrain` (register path,
  `internal/serve/auth_handlers.go`).
- **shared brain** — `handleCreateBrain` (`internal/serve/brains.go`).

## How push/pull/sse encrypt + decrypt at rest

All in `internal/serve` (`atrest.go` helpers + the sync handlers in
`server.go`). The handlers already brain-scope keys via
`scopeKey`/`scopePrefix`/`unscopeKey`; the at-rest layer slots in around
`backend.Put`/`backend.Get`:

- **push** — `encryptForBrain(brainID, blob)` before `backend.Put`. The
  SSE fan-out still ships the **plaintext** `row.Blob`, matching what pull
  returns to clients.
- **pull** — `decryptForBrain(brainID, blob)` after `backend.Get`.
- **sse** — the **backlog replay** branch (which reads stored blobs)
  decrypts; the **live fan-out** branch already carries plaintext from
  push, so it does not.

The unwrapped per-brain DEK is memoized in an in-memory cache keyed by
`brainID` (`Server.dekCache`, guarded by `dekCacheMu`); the wrapped DEK
lives in `brain_keys`. A brain with **no key** (e.g. created while no
master key was set) falls back to passthrough for that brain. A wrapped
DEK that **won't unwrap** (wrong/rotated master key) returns an error —
the handler 500s rather than serving plaintext or garbage.

## Test coverage

- `internal/auth/atrest_test.go` — DEK wrap/unwrap + at-rest
  encrypt/decrypt round-trips; random-nonce check; wrong-master-key,
  wrong-DEK, tamper, truncation, and bad-length failures.
- `internal/storage/sql/sqlite/tenancy_test.go` —
  `TestBrainKeyStore_PutGet`: put/get/upsert/`ErrNotFound`, plus the
  `encryption_policy` default surfaced on the brain row.
- `internal/serve/atrest_test.go` — brain create stores a wrapped DEK
  (personal + shared) that unwraps under the master key; push→backend
  blob is ciphertext (≠ client bytes) and pull returns the original;
  no-master-key path stores bytes verbatim and provisions no DEK; a
  wrong/rotated master key fails to unwrap on pull (500, no silent
  plaintext); bad master-key length rejected at construction.
- `internal/cli/serve_test.go` — `loadMasterKey` hex/base64 file, env
  fallback, absent→nil, wrong-length and garbage rejection.

## Next slice: client default-flip (daemon)

This slice intentionally leaves the daemon **double-encrypting** for
server-side brains. The follow-up slice must, in the **daemon client**
(`internal/agent` seal path + sync):

1. For brains whose `encryption_policy == 'server-side'`, **stop
   client-sealing** before push (and stop unsealing after pull) — let the
   server's at-rest DEK be the only encryption layer. Keep client-sealing
   for `'e2e'` (zero-knowledge personal) brains.
2. Surface the per-brain policy to the daemon (a sync/auth response field
   or a brains lookup) so it knows which brains to flip.
3. Make the master key **required** in multi-tenant mode on the server
   (drop the "skip at-rest when absent" fallback).

Nothing in the daemon seal path was modified by this slice.
