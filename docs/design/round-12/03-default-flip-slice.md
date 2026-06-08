# Round 12 — Part A, slice 2: encryption default-flip (conditional client sealing)

Slice 1 (`02-server-at-rest-slice.md`) made the server encrypt every blob
at rest with a per-brain DEK, **additively** on top of the daemon's
existing zero-knowledge client sealing (double-encryption). This slice
makes the daemon's client sealing **conditional**, so a trusted
multi-tenant server's at-rest DEK becomes the **sole** encryption layer
for the default policy — while solo self-hosters keep zero-knowledge.

## The seal-decision rule (LOCKED)

> The daemon seals (zero-knowledge) **iff** the server is **single-tenant**
> OR the acting brain's `encryption_policy == "e2e"`. It does **NOT** seal
> when **(multi-tenant AND policy == "server-side")** — there the server's
> at-rest DEK (slice 1) is the sole layer.

As a pure boolean (De Morgan form — see `computeShouldSeal`):

```
shouldSeal = !multiTenant || policy == "e2e"
```

| tenancy        | policy       | client seals? | encryption layer(s) on the wire / at rest          |
|----------------|--------------|---------------|-----------------------------------------------------|
| single-tenant  | (n/a)        | **yes**       | client envelope (zero-knowledge); server stores opaque |
| multi-tenant   | server-side  | **no**        | TLS in flight; server at-rest DEK on disk (sole layer) |
| multi-tenant   | e2e          | **yes**       | client envelope (zero-knowledge); server also at-rest  |

**Rationale.** The default policy is `server-side`, so a **multi-tenant**
trusted server gets server-side at-rest encryption and the client sends
plaintext over TLS (the server can index/share/operate on it). A
**single-tenant** solo user — possibly on an untrusted VPS — keeps
zero-knowledge: the server never sees plaintext. `e2e` brains also keep
sealing; **no endpoint SETS `policy = e2e` yet** — that's a later slice.
The `e2e` branch is implemented and tested here so it's ready; today every
brain defaults to `server-side`.

The daemon **fails closed**: `shouldSeal` starts `true`, so until the
whoami handshake answers we behave like the legacy zero-knowledge daemon
and never ship plaintext by accident.

## The whoami handshake — `GET /v2/sync/whoami`

Auth-only handshake the daemon uses to compute `shouldSeal`. Wrapped by
`requireBrain` (like push/pull/sse): it honors `?brain=<id>` (default:
the caller's personal brain) and validates membership.

Response (`serve.WhoamiResp`):

```json
{ "multi_tenant": true, "brain_id": "<id>", "encryption_policy": "server-side" }
```

- **single-tenant** mode → `{ "multi_tenant": false }` with empty
  `brain_id` / `encryption_policy` (there is no brain partitioning).
- **multi-tenant** → the acting brain's id and policy (policy defaults to
  `server-side` at the storage layer, so a missing value is normalized to
  `server-side`).

**It returns NO blob bytes** — only auth/policy facts. That's the sharp
edge that lets the daemon probe it even while sync is **parked** pre-login
(no sealer installed). Any "no requests while parked" assertion must count
**only `POST /v2/sync/push`**, not the whoami `GET` — a catch-all request
counter wrongly ticks on the handshake.

## Daemon: where `shouldSeal` is computed and threaded

`SyncClient` caches the decision (concurrency-safe via `sealDecisionMu`):

- **`sampleSealDecision(ctx)`** calls `fetchWhoami` → `computeShouldSeal`
  → `setShouldSeal`. Best-effort: on error the cached (fail-closed)
  decision is left unchanged.
- Called at **startup** (`Start`, before the drain/pull/sse loops) and on
  every **active-brain change** (`SetActiveBrain`, in the background) so a
  `resleeve brain use` into a different-policy brain re-samples without a
  daemon restart.
- Threaded into the push/pull boundary: every `Seal` site
  (`EnqueueSession`/`EnqueueEvents`/`EnqueueScope`/`EnqueuePlan`/
  `EnqueueLearning`) and every `Open` site (`ingestPulled`/
  `ingestMemoryRow`) is gated `if sl := getSealer(); sl != nil &&
  getShouldSeal()`. When `shouldSeal` is false the daemon enqueues/pushes
  **plaintext** and does **not** unseal the (already-plaintext) pull
  response. The sealer (login/KEK) stays wired for the seal path — it is
  NOT removed; `shouldSeal` only decides whether an installed sealer is
  applied.

`shouldSeal` is orthogonal to parking: a nil sealer parks the loops
regardless of the decision.

## Master key required in multi-tenant

Because a multi-tenant server's clients send plaintext under the default
`server-side` policy, the server's at-rest DEK is the only encryption
layer. So a multi-tenant server **must** have a master key:

- `serve.New` returns an error when `multiTenant()` (`!SingleTenant`) and
  `len(MasterKey) == 0` — dropping slice 1's "skip at-rest when absent"
  for the multi-tenant case.
- `cli/serve.go` surfaces a fatal before boot:
  `multi-tenant serve requires --master-key / $RESLEEVE_SERVER_MASTER_KEY`
  (with a hint to pass `--single-tenant` for a solo zero-knowledge host).
- **Single-tenant stays optional** — those clients seal zero-knowledge, so
  the server need not encrypt at rest.

## What's next

The `e2e` policy branch is implemented and tested but **no endpoint sets
`encryption_policy = "e2e"`** yet. A later slice adds the policy-setting
endpoint (and the per-brain group-key / ZK shared-brain machinery `e2e`
implies). Today every brain defaults to `server-side`.
