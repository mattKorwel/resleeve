# Round 15 — Multi-tenant DoS hardening

The round-14 validation pass (`docs/design/round-14/00-validation-plan.md`)
+ security review found that multi-tenancy widened the documented "no rate
limiting" accepted risk: previously single-user, a registered user can now
exhaust a *shared* host. This slice adds bounded, configurable caps on the
server. Green field; no migrations. None of these change confidentiality —
they protect availability of a shared team server.

## Caps

| Cap | Default | Flag | Config field | Enforced in | Over-limit |
|---|---|---|---|---|---|
| Push body size | 32 MiB | `--max-push-bytes` | `MaxPushBytes int64` | `handlePush` | **413** |
| Explicit key validation | always on | — | — | `handlePush` | **400** |
| Concurrent SSE / user | 16 | `--max-sse-per-user` | `MaxSSEPerUser int` | `handleSSE` | **429** |
| Owned brains / user | 100 | `--max-brains-per-user` | `MaxBrainsPerUser int` | `handleCreateBrain` | **429** |

- **Push body cap** — `r.Body` wrapped in `http.MaxBytesReader`; a
  `*http.MaxBytesError` (via `errors.As`) → **413 `payload_too_large`**;
  other decode errors stay **400**.
- **Explicit key validation** — `sync.ValidateKey(storedKey)` is called on
  the fully brain-scoped key before `backend.Put`, so a `..`-segment key
  (which passes the prefix allow-list) is rejected **at the handler**, not
  only by a backend that happens to re-validate. Closes the latent invariant
  where cross-brain `..`-escape defense lived only in the local backend.
- **Per-user SSE cap** — a mutex-guarded `map[userID]int` counter; a slot is
  reserved before the 200/subscribe and released (deferred) on disconnect;
  the map entry is deleted on the last release. Single-tenant mode
  (`UserID==""`) is exempt. Over the cap → **429 `too_many_requests`**.
- **Per-user brain cap** — `brains.ListByOwner` count (the personal brain
  counts) checked before insert in `handleCreateBrain`. Over → **429**.

All caps are zero-defaulted `serve.Config` fields (defaults applied in
`New`), threaded from `cli/serve.go` flags, so existing behavior/tests are
unchanged. New error codes `payload_too_large` (413) and
`too_many_requests` (429) in `errors.go`.

## Deferred

- **Per-user storage byte cap** (`MaxStorageBytesPerUser`, default 0 =
  unlimited): the field exists but is **not enforced**. The `sync.Backend`
  interface has no cheap per-user byte sum (only `List` keys + `Get` one
  blob), so enforcement per-push needs a full scan or persistent accounting.
  Marked `round-15 follow-up:` on the field. The right home is the
  Postgres/object-store backend work, where byte accounting can be a column.

## Tests (`internal/serve/hardening_test.go`)
`TestPush_BodyCap` (413 over / 200 under), `TestPush_KeyValidationAtHandler`
(`memory/../escape` → 400), `TestSSE_PerUserCap` (429 over, freed on
disconnect), `TestSSE_PerUserCapIsolation` (per-user), `TestSSE_PerUserCapConcurrent`
(`-race`), `TestCreateBrain_PerUserCap` (429 over). Full serve package is
`-race` clean.
