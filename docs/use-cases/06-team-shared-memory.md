# Use case 06 — team mode with a shared brain

## The problem

A few people are working the same codebase and want their AI agents to share
context: a common plan everyone's sessions auto-load, and learnings one person
records that the rest of the team picks up on their next session. At the same
time, each person wants their *own* private memory for unrelated work, and the
operator wants a server that stores data encrypted at rest.

Team mode is the default for `resleeve serve`. Each member gets a private
memory namespace — a **brain** — and the operator (or any member who creates
one) can stand up a **shared brain** that a named group reads and writes.

## The model in one paragraph

A **brain** is a memory namespace: scopes, plans, and learnings. Every account
has a personal brain by default. A **shared brain** is owned by whoever created
it; the owner adds members, and any member can `brain use` it to make their
machine's sessions read/write that shared namespace instead of their personal
one. On a multi-user (team) server, data is encrypted at rest with a per-brain
key held under the operator's master key — so a member only ever sees the
brains they belong to, and the bytes on disk are ciphertext.

## Encryption at a glance

| Mode | What protects the data | Who can read plaintext |
| --- | --- | --- |
| In transit (all modes) | TLS (operator fronts `serve` with a reverse proxy) | n/a — transport only |
| Team server, server-side (default) | Per-brain key wrapped under the operator's master key, encrypted at rest | The server (to serve brain members); operator holds the master key |
| Team server, a personal brain opted to end-to-end | Client-held key; server stores ciphertext only | Only that member |
| Solo `--single-tenant` server | Client-held key (end-to-end); server stores ciphertext only | Only the account owner |

The default for a team server is **server-side** at-rest encryption: clients
send plaintext over TLS and the server encrypts each brain's data under a
per-brain key wrapped by the operator's master key. That's what lets the server
hand a shared brain's contents to every member of the group. A member who wants
a *personal* brain that even the operator can't read can opt it back to
end-to-end (see "Opting a personal brain to end-to-end" below). Full layering
is in [`../USER_GUIDE.md`](../USER_GUIDE.md) → "Encryption model".

## Walkthrough — operator stands up the server

A team server **requires** a master key. Generate 32 bytes, store it in a file
only the service account can read, and point `serve` at it:

```bash
# generate a 32-byte key as hex and lock it down
openssl rand -hex 32 > /var/lib/resleeve/master.key
chmod 600 /var/lib/resleeve/master.key

resleeve serve \
  --addr 127.0.0.1:7860 \
  --root /var/lib/resleeve/serve \
  --master-key /var/lib/resleeve/master.key
# resleeve serve listening on http://127.0.0.1:7860
#   tenancy  multi-tenant (brain-scoped; per-device bearer required on /v2/sync/*)
#   at-rest  server-side envelope encryption ON (per-brain DEKs wrapped under master key)
```

The key can also come from `$RESLEEVE_SERVER_MASTER_KEY` (hex or base64, 32
bytes) instead of `--master-key`. **Without one, a team server refuses to
boot** — clients send plaintext under the default policy, so the server's
at-rest key is the only thing encrypting it, and resleeve won't persist
plaintext silently.

Front it with TLS exactly as in [`05-self-hosted-serve.md`](./05-self-hosted-serve.md)
(systemd unit + Caddy/nginx reverse proxy). Keep `--addr` on loopback behind
the proxy.

> Running solo, not for a team? Pass `--single-tenant` instead. That's the old
> flat, zero-knowledge behavior: no brains, the client encrypts end-to-end, and
> `--master-key` is optional. See [`05-self-hosted-serve.md`](./05-self-hosted-serve.md).

## Walkthrough — members join

Each member registers an account against the upstream, once:

```bash
# on Alice's laptop
resleeve register --upstream https://resleeve.example.com
# prompted for email + master password; recovery key printed ONCE — save it

resleeve login --upstream https://resleeve.example.com
resleeve up --upstream https://resleeve.example.com
resleeve doctor
```

`register` mints the account and this device's token; `login` unlocks the local
sealer; `up` brings the daemon online pointed at the upstream. Repeat for each
member (Bob, Carol, …). Each person now has a working personal brain.

Adding a *second device* for the same person doesn't need another `register` —
use pairing (see [`02-cross-machine-sync.md`](./02-cross-machine-sync.md)):

```bash
resleeve pair invite --upstream https://resleeve.example.com   # on an existing device
resleeve pair accept --code-id=… --upstream https://resleeve.example.com  # on the new one
```

Note each member's user id (printed by `register` / `login`, e.g.
`usr_abc123`). The brain owner needs those ids to add members below.

## Walkthrough — create the shared brain

Whoever owns the shared namespace creates it. They become its owner:

```bash
# Alice creates the team brain
resleeve brain create "acme-backend" --upstream https://resleeve.example.com
# brn_7h2k…              ← the new brain id, printed to stdout
```

Add the other members by user id (owner only):

```bash
resleeve brain member add brn_7h2k… usr_bob456  --upstream https://resleeve.example.com
resleeve brain member add brn_7h2k… usr_carol99 --upstream https://resleeve.example.com

resleeve brain member list brn_7h2k… --upstream https://resleeve.example.com
# usr_alice123
# usr_bob456
# usr_carol99
```

List the brains you belong to anytime — the active one is starred:

```bash
resleeve brain list --upstream https://resleeve.example.com
# ID            NAME           KIND     ROLE    ACTIVE
# brn_7h2k…     acme-backend   shared   owner   *
```

Remove someone with `resleeve brain member rm <brain-id> <user-id>`.

## Walkthrough — everyone switches to the shared brain

Each member points their machine at the shared brain. `brain use` is local — it
sets the machine's active brain so the daemon's sync reads and writes that
namespace:

```bash
resleeve brain use brn_7h2k…
# active brain set to brn_7h2k…
# restart the daemon to apply: resleeve down && resleeve up

resleeve down && resleeve up
```

The restart matters: the running daemon samples the active brain at startup, so
it needs to be bounced (or re-run `brain use`) to pick up the switch.

To go back to your private memory:

```bash
resleeve brain use --clear
resleeve down && resleeve up
```

## Walkthrough — share plans and learnings

Now ordinary memory verbs operate on the shared brain. Whatever scope a member
opens a session in resolves against the shared namespace, so the rolled-up plan
+ learnings inject into every member's sessions:

```bash
# Alice sets the team plan
resleeve scope set acme-backend --kind project --title "Acme backend"
echo "Sprint goal: migrate the billing service to the new event schema." \
  | resleeve plan write acme-backend

# Bob, working the same scope, records a learning the whole team should keep
resleeve learning append acme-backend \
  "The staging DB ignores the FK on invoices — don't trust it for migration tests."
```

On Carol's next session in that scope, the SessionStart hook injects Alice's
plan and Bob's learning — she didn't have to ask "catch me up." Learnings
record **who added them**, so the team can tell whose insight is whose.

Plans keep **version history** and use optimistic concurrency. If Alice and Bob
both edit the team plan from a stale copy, the second write is **rejected** with
the current version rather than silently clobbering the first — `plan write`
prints a conflict and tells you to re-read, merge, and retry (or `--force` to
overwrite). See [`../USER_GUIDE.md`](../USER_GUIDE.md) → "Plan versioning".

## Opting a personal brain to end-to-end

A team server's default is server-side at-rest encryption, which means the
server can read brain contents to serve members. If a member wants a *personal*
brain whose contents even the operator can't decrypt, the brain owner switches
its policy to end-to-end:

```bash
resleeve brain encrypt <personal-brain-id> e2e --upstream https://resleeve.example.com
# brain … encryption policy set to e2e
# the running daemon picks this up on its next handshake — restart
# (resleeve down && resleeve up) or `resleeve brain use` to re-sample.
```

End-to-end is only valid on a **personal** brain — the server rejects it on a
shared brain (a shared brain has to be server-readable to fan out to members).
The policy applies to **new** writes after the daemon re-samples; existing rows
aren't re-encrypted. Use `server-side` to switch back.

## Operational notes

- **The master key is the crown jewel.** It wraps every brain's at-rest key on
  a team server. Back it up out of band; if you lose it, every brain's at-rest
  data is unrecoverable. Never check it into the repo or logs.
- **Per-device tokens.** Identity is per-device (`register` / `login` / `pair`),
  so a member can revoke a single lost laptop with `resleeve logout` without
  disturbing their other devices or the rest of the team.
- **Membership is owner-gated.** Only a shared brain's owner can add or remove
  members or change its encryption policy; the server returns 403 otherwise.
- **Back up the server.** Snapshot `--root` and `identity.db` as in
  [`05-self-hosted-serve.md`](./05-self-hosted-serve.md). On a team server the
  blobs are ciphertext under the master key.

## See also

- [`05-self-hosted-serve.md`](./05-self-hosted-serve.md) — standing up the
  server, reverse proxy, systemd, sizing (solo `--single-tenant` variant).
- [`02-cross-machine-sync.md`](./02-cross-machine-sync.md) — adding a second
  device with `pair invite` / `pair accept`.
- [`../USER_GUIDE.md`](../USER_GUIDE.md) → "Team mode", "Encryption model",
  "Plan versioning" — the full reference for brains, encryption, and concurrency.
