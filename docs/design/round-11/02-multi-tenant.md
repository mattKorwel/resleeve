# Round 11 — Multi-tenant: brains, memberships, credentials

Round 11's security theme (see `01-quick-wins.md`) ends with the big one:
turning resleeve from single-user into multi-tenant. This doc is the design
for that. It also sets the **north-star tier ladder** that rounds 12
(encryption matrix + plan versioning) and 13 (proactive fan-out + scale)
build on.

> Vision constraint (operator's words): this is **not** a public-signup
> internet service. It's a **self-hosted** tool, *or* something a team can
> stand up on Cloud-X with **very low friction**. That simplification is
> load-bearing — no email verification, no captchas, no abuse-at-scale,
> provisioning is always admin-controlled or IdP-fed.

## The tier ladder (north star)

Each tier flips exactly one axis — that's what keeps it an incremental path,
not five products.

| Tier | Persona | Identity | Isolation | Sharing | Store | Threat model |
|---|---|---|---|---|---|---|
| **1** | solo / local | none ("self") | n/a | across *your* CLIs + instances | local SQLite | trust your box |
| **2** | solo / remote | 1 user, N devices (**exists**) | n/a | across *your* machines | server + disk | **untrusted host** → zero-knowledge seal |
| **3** | small group, isolated | N users | per-user **brain** | none | real DB likely | trusted host, **untrusted peers** |
| **4** | group + shared | N users | per-user brain | **shared brain(s), R/W** | real DB | + shared-resource authz |
| **5** | enterprise | **external IdP** (OIDC/SCIM) | per-user | shared | robust DB | + org policy |

**Threat-model flip between tier 2 and tier 3** is the key realization: tier
2 distrusts the *host* (→ client-side zero-knowledge). Tiers 3–5 trust a
host run by a known admin but distrust *peers* (→ server-enforced authz +
server-side encryption). Zero-knowledge is demoted to an opt-in layer
(round 12), not the default.

## Core model — partition by *brain*, not by *user*

The trap that makes "shared" expensive is partitioning the keyspace by
`user_id`. Partition by **brain** instead and tiers 3 and 4 become the *same
mechanism*:

- **brain** — a named namespace = a scope tree + its memory (plans,
  learnings). The unit of isolation and of sharing.
- **user** — an identity with one or more credentials.
- **credential** — a way to prove you're a user: an SSH public key or an API
  key. (Pairing, generalized: adding a credential to your account.)
- **membership** — a row `(user_id, brain_id[, role])`. *Access rule: you may
  touch a brain iff you are a member.*

Every user auto-gets a **personal brain** (membership of one). A **shared
brain** is simply a brain with `count(members) > 1`. So:

- **Tier 3** = everyone is a member of exactly one (their own) brain.
- **Tier 4** = some brains have several members.

Same partitioning code; sharing falls out of `count(members) > 1`. No new
subsystem.

### Keep it dumb first (the "just works" constraint)
- **No roles initially** — members can read **and** write; the **creator is
  owner** and can add/remove members. Add reader-vs-writer only if missed.
- **Membership is at the brain (namespace) level**, never per-scope ACLs —
  coarse on purpose, or it balloons.
- **Sessions stay personal**; **memory is the shared substrate** (shared
  brains share plans/learnings only). A user can still **share a single
  session** on demand via a share-link (separate, user-initiated; not part of
  brain membership).

## Identity & auth — SSH keys + API keys, pluggable

Generalize "pair a device" into "**a credential proves you're a user**":

- **SSH-key auth (primary, frictionless):** server stores the user's SSH
  public key(s), like `authorized_keys`. Client signs a server challenge via
  **ssh-agent**; server verifies against the stored pubkey and issues a
  short-lived token. Devs already have keys + agent → zero new login, no IdP.
- **API key:** a long-lived bearer the user mints — or that an SSH-key auth
  **mints for them** ("ssh key gives us an api key"). Server stores only a
  **hash**. Ideal for agents/CI where ssh-agent isn't available.
- **`credentials(user_id, kind, pubkey_or_hash, label, created_at, expires_at)`**
  — many users × many keys is just rows. `kind ∈ {ssh, api, oidc}`.
- **Pluggable:** SSH/API cover tiers 1–4; **OIDC** ("login with Google") and
  **SCIM** slot in at tier 5 as additional credential kinds, behind the same
  token-issuance seam.

This is the operator's "no one wants yet another login" requirement: keys
they already have (SSH) or a single pasted token, not a new password regime.

### Fleet / agent key distribution (the "how do my little agents auth" question)
Default answer: **agents never hold the upstream key.**
- Each machine runs the resleeve **daemon on loopback**, holding **one**
  credential — ssh-agent, or an API key in the **OS keychain** (already
  shipped, round 5/6). Local agents hit `localhost`; the daemon syncs
  upstream. No secret sprawl across processes.
- **Ephemeral / remote runners** (a throwaway cloud agent box) get a
  **scoped, short-TTL, revocable API key** injected as a secret —
  least-privilege (e.g. write-only to one brain). Leak blast radius = one
  revocable token.

## What exists today (don't rebuild)
Verified in `internal/serve`:
- `server_users`, `devices`, `pairings`, `serve_meta` tables; `register` /
  `login` / `pair` flows; **per-device bearer tokens** with server-side TTL.
- `requireDevice` → resolves a token to a `device` carrying `UserID`;
  `requireUserDevice` already gates identity routes to real users.
- Push **key-prefix gate** (sec-M2): only `sessions/`/`events/`/`memory/`.
- Pairing codes scoped by `user_id`; pair-claim lockout (round 11 quick wins).

## The gap (what round 11 builds)
1. **Keyspace is global** — blobs are `sessions/<id>` etc. with no brain
   namespace. Namespace becomes `<brain_id>/<kind>/…`; **push prepends the
   brain the authenticated user is acting in** (client supplies a brain
   selector, server validates membership; client never forges the prefix).
2. **Reads aren't filtered** — `handlePull` / `handleSSE` must serve only keys
   under brains the caller is a member of.
3. **Legacy no-user bearer is accepted on `/v2/sync/*`** ("content-blind") —
   a cross-tenant hole. Disable in multi-tenant mode; keep it only behind an
   explicit **`--single-tenant`** flag for solo self-hosters (tiers 1–2).
4. **New tables + verbs:** `brains`, `memberships`, `credentials`; auto-create
   a personal brain on register; admin verbs to mint users/credentials,
   create a shared brain, add/remove members; SSH-key challenge + API-key
   mint.
5. **Per-user/brain quotas + rate limits** — storage quota + request rate
   (disk-fill / DoS). The review's accepted "no rate limiting" becomes
   required once peers are mutually untrusted.

## Storage
- The serve relational store is already an interface (`rsql.ServerUserStore`
  etc.); higher tiers add a **Postgres** implementation (own slice). The blob
  `sync.Backend` is already abstracted (local-disk; S3/GCS later).
- SQLite/local-disk is fine for tiers 1–3-small (single instance). Concurrency
  + HA needs Postgres + a blob store — see round 13's scale ladder.

## Out of scope for round 11 (handed to later rounds)
- **Encryption matrix + zero-knowledge-as-opt-in** → round 12.
- **Append-only plan versioning / optimistic concurrency** → round 12.
- **Brain-scoped proactive fan-out + scale backplane** → round 13.
- **Reader/writer roles, fine-grained ACLs** — deferred until missed.
- **OIDC / SCIM (tier 5)** — credential-kind seam now, impl later.
- **Postgres backend impl** — interface is ready; impl can be its own slice.

## Tier coverage delivered
Round 11 lands **tiers 3 and 4** (isolated + shared brains) on a
single-instance, key-authenticated server with server-side trust. Tier 5 is
"add OIDC/SCIM credential kinds + Postgres"; tier 2's zero-knowledge becomes
the round-12 opt-in.
