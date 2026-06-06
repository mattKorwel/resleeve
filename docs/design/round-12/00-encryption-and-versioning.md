# Round 12 — Encryption matrix + append-only plan versioning

Two independent pieces that both ride the `brains + memberships` spine from
`round-11/02-multi-tenant.md`:

1. **Encryption as a composable matrix** (simple defaults, power-user knobs),
   with zero-knowledge demoted to an opt-in layer.
2. **Append-only plan versioning + optimistic concurrency**, so shared brains
   are safe to write concurrently and we get history/provenance for free.

---

## Part A — Encryption is a matrix, not a mode

Encryption is **layers**, each defending a different threat; they compose.
It is **not** one global switch.

| Layer | Protects against | Personal brain | Shared brain | Default |
|---|---|---|---|---|
| **TLS (in transit)** | network snooping | ✓ | ✓ | always on (operator's reverse proxy) |
| **Local-at-rest** | your stolen laptop/disk | ✓ | ✓ | **on** — key in the OS keychain (already shipped) |
| **Server-at-rest** | stolen server disk/snapshot | ✓ | ✓ | **on** for team tiers (3–5) |
| **End-to-end (zero-knowledge)** | the *host/operator* reading you | ✓ (your KEK) | only via **group keys** (advanced/deferred) | **opt-in, per brain** |

**Frictionless default** = TLS + local-at-rest + server-at-rest, all
automatic. The one power-user knob is **end-to-end, per brain**.

### Why this shape
- **Zero-knowledge only buys something when you don't trust the host.** If a
  trusted admin runs the box, it costs you sharing + easy auth for protection
  you don't need. So it's opt-in (tier-2 "paranoid VPS" case), not default.
- The layers are **independent**, which is what makes the operator's mixed
  case work: a **shared brain** is server-side encrypted (members + server can
  read — it must, to mediate sharing) **and** your **local replica** of it is
  encrypted at rest with *your* local key. Those don't conflict.
- The only impossible cell is **true zero-knowledge on a *shared* brain**:
  one member's KEK can't encrypt a shared resource. That needs **group-key**
  crypto (wrap a per-brain key for each member) — real but heavy
  (membership-change re-wrapping), so **deferred**. Personal brains get full
  zero-knowledge for free.

### Key management
- **Local-at-rest:** symmetric key in the OS keychain (macOS Keychain /
  libsecret — already integrated). Encrypts the local SQLite at rest.
- **Server-at-rest:** server-managed per-brain data keys (operator's KMS or a
  master key in the serve config). The server can decrypt — by design, for
  tiers 3–5.
- **End-to-end (personal):** the existing Argon2 master-password KEK — this is
  literally today's zero-knowledge sealer, repositioned as the "end-to-end
  personal" cell. Identity (who you are: SSH/API/OIDC) stays **separate** from
  the seal secret (what encrypts: the KEK). The default mode (key-auth +
  server-side) "gets most of the way"; zero-knowledge is the bolt-on for users
  who want the host blinded.

### Relationship to today
Today's client-side AES-GCM sealing (`internal/auth` Sealer + per-user
Argon2 KEK) **becomes the end-to-end-personal layer**, opt-in. The new work is
server-at-rest (per-brain keys) + making local-at-rest a first-class default +
a per-brain `encryption_policy` the matrix reads.

### Out of scope (round 12)
- Group-key shared-brain zero-knowledge (advanced).
- KMS/HSM integrations (operator config is enough first).

---

## Part B — Append-only plan versioning + optimistic concurrency

Today plans are mutate-in-place (`PutPlan` upsert) — fine solo, lossy shared.
Move plans to the same **append-only, materialized-read** model the rest of
resleeve already uses (events, learnings).

### Model
- **Write = append, never mutate.** Each update appends an immutable version
  under `plans.d/<slot>/`:
  `version, content, author_user_id, ts, parent_version`.
- **Read = materialized HEAD.** Server returns the latest version. Plans are
  small markdown → store full content per version; HEAD = max version (no diff
  replay).
- **Optimistic concurrency.** A write carries the `base_version` it was
  derived from:
  - `base_version == HEAD` → append `HEAD+1`.
  - else → **409 Conflict, returning current HEAD** so the caller can
    reconcile. The failed call *is* the reconciliation signal and carries the
    data needed to resolve.

### Why
- **No lost writes** — every version is in the log; conflicts surface, never
  silently clobber.
- **Conflict-free reads** — always a coherent latest.
- **Free history + provenance + rollback** — solves "who recorded this"
  (author stamp) and enables diff/revert at no extra cost.
- **One mental model** — *everything is an append-only log; reads
  materialize.* Sessions/events already are; learnings already are; this
  brings plans (the last mutable thing) in line. Very ori (git-style history).

### Reconciliation strategy
- **MVP:** 409 returns `{HEAD, your_base}`; client decides (re-render + retry,
  or surface). Dumb and correct.
- **Later:** auto **3-way text merge** (base / mine / theirs) for
  non-overlapping edits; conflict markers only when they truly overlap.

### Sync-path impact (contained)
Today the outbox push is a blind idempotent `Put`. **Plan** pushes now carry
`base_version`; a 409 means that outbox item needs **reconcile, not blind
retry**. Only plans change — sessions/events/learnings stay
append-idempotent.

### Provenance for learnings
Learnings are already append-only; add the same **`author_user_id`** stamp so
shared-brain learnings show who contributed (needed once brains are shared).

### Retention
Keep full plan history initially (small markdown). Optional compaction of old
versions after N is a later, opt-in knob.

### Out of scope (round 12)
- 3-way auto-merge (ship the dumb 409 first).
- Cross-slot / cross-scope transactional writes.

---

## How A and B compose
A **version row** is the unit that gets encrypted per the brain's matrix
policy — history and crypto compose with no special cases. Both pieces sit on
round 11's brain/membership partitioning; neither needs the round-13 fan-out.
