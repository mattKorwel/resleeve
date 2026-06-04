# Use case: cross-machine sync

You have a laptop, a desktop, and a small box on the internet somewhere. You
want the same scope hierarchy, the same plans, the same learnings, and the
same captured sessions on every machine you sit down at. You do **not** want
a vendor's cloud holding your prompt history or memory module in plaintext.
You want to self-host the upstream and have the host be blind to everything
it stores.

That is what `resleeve serve` plus a master-password-derived KEK is for.

## Architecture

Each workstation runs its own local daemon. Both daemons sync against a
shared `resleeve serve` upstream over HTTPS. The upstream is zero-knowledge:
sealed blobs and per-device bearer tokens, no plaintext, no KEK.

```
  laptop (A)                                desktop (B)
  resleeve agent (loopback)                 resleeve agent (loopback)
   SQLite + AESGCMSealer                     SQLite + AESGCMSealer
   KEK in RAM                                KEK in RAM
            \                               /
             \  HTTPS, AES-GCM ciphertext  /
              \  + per-device bearer      /
               +-> resleeve serve <------+
                   /v2/auth/*
                   /v2/sync/push|pull|sse
                   sealed blobs only
```

What crosses the wire: sealed `sessions/`, `events/`, `memory/` blobs (slow
tier batched through an outbox, memory tier streamed via SSE), plus identity
handshakes on `/v2/auth/*` (verifier hashes + wrapped KEK envelopes — the
unwrap happens client-side). Master password, recovery key, and unwrapped
KEK never leave the client.

## Walkthrough

### 1. Stand up the upstream

Pick a host you control. Install the same `resleeve` binary you run locally.

```bash
resleeve serve --addr 0.0.0.0:7860 --root /var/lib/resleeve
# resleeve serve listening on http://0.0.0.0:7860
#   WARNING  --addr 0.0.0.0:7860 is not loopback — operator: confirm you've fronted this with TLS + auth
#   backend  local-disk at /var/lib/resleeve
```

The non-loopback warning is intentional — `resleeve serve` does not
terminate TLS itself. Skip the proxy step and your device tokens travel in
cleartext.

### 2. Front it with TLS

Standard caddy / nginx / Traefik reverse proxy on `:443` pointed at
`127.0.0.1:7860`. Disable response buffering on `/v2/sync/sse` or the
memory fast tier falls back to the 30s pull. The rest of this walkthrough
uses `https://resleeve.example.com`.

### 3. Machine A — register

```bash
resleeve register --upstream https://resleeve.example.com
# email: you@example.com
# master password: ********
# confirm password: ********
# registered ✓
#   user id     usr_abc123
#   device id   dev_xyz789
#
# RECOVERY KEY — save this somewhere safe. It is shown ONCE.
#   1234-5678-9012-3456-7890-1234
```

The master password is the root of the KEK derivation. It is **not**
recoverable — if you lose it AND the recovery key, every server-side blob
under that account is permanently opaque. See
[`../USER_GUIDE.md`](../USER_GUIDE.md) §5.3 for the recovery key flow.

### 4. Machine A — log in and bring the daemon up

```bash
resleeve login --upstream https://resleeve.example.com
# email: you@example.com
# master password: ********
# login: daemon sealer installed ✓
# login ✓

resleeve up --upstream https://resleeve.example.com
# [1/2] daemon started at http://127.0.0.1:53421
# [2/2] installed bridge (claude)

resleeve doctor
#   sealer           ✓ unlocked
#   upstream         https://resleeve.example.com (✓ reachable, 41ms RTT)
#   sync (slow)      last drain 2s ago • last pull 2s ago • outbox depth 0
#   sync (fast/sse)  ✓ connected (3s uptime) • last event never
```

`login` derives the KEK from your password via Argon2id, unwraps the
account's stored KEK envelope (server returns ciphertext only), and POSTs
the 32-byte KEK to the daemon's loopback-only `/v1/seal/unlock`. The sealer
is now armed; sync loops wake.

### 5. Machine A — capture some work

Open Claude Code, do anything that produces tool calls.

```bash
resleeve doctor | grep outbox
#   sync (slow)      last drain 1s ago • last pull 12s ago • outbox depth 7
# a couple seconds later:
#   sync (slow)      last drain 1s ago • last pull 12s ago • outbox depth 0
```

The drain ticks every second. Each row is sealed at enqueue time; the
ciphertext is what lands in the upstream's blob store.

### 6. Machine B — pair from A

You don't re-register on B. You pair. On A:

```bash
resleeve pair invite --upstream https://resleeve.example.com --ttl 5m
# pair code published ✓
#   code id     2f1c9a8b...
#   pair code   K7Q4-9XYZ-MTPB
#   expires     2026-06-04T17:30:00Z
```

Default TTL is 5 minutes; server caps at 10. Pass the `code id` and `pair
code` to B over a side channel. On B:

```bash
resleeve pair accept --code-id=2f1c9a8b... --upstream=https://resleeve.example.com
# pair code: K7Q4-9XYZ-MTPB
# email (for local label): you@example.com
# pair accept: daemon sealer installed ✓
# pair accept ✓
#   user id     usr_abc123
#   device id   dev_def456
```

B now has its own per-device bearer token, the same account's KEK installed
in its local sealer, and an OS-keychain entry. No master password was ever
typed on B.

### 7. Machine B — bring the daemon up

```bash
resleeve up --upstream https://resleeve.example.com
resleeve doctor
#   sealer           ✓ unlocked
#   sync (slow)      last drain 1s ago • last pull 1s ago • outbox depth 0
#   sync (fast/sse)  ✓ connected (2s uptime) • last event never
```

### 8. Verify

```bash
# on B:
resleeve session list
# ses_abc123  claude  2m ago   resleeve              "investigate F14..."

resleeve resume ses_abc123
# (execs into a fresh `claude --resume ses_abc123` on B)
```

A learning written on A also lands on B in seconds — that's the SSE fast
tier:

```bash
# on A:
resleeve learning append resleeve "Cross-machine smoke worked on 2026-06-04."

# on B, within a few seconds:
resleeve learning list resleeve | head -1
# 2026-06-04: Cross-machine smoke worked on 2026-06-04.
```

## Zero-knowledge property

The upstream stores Argon2id parameters, verifier hashes, dual-wrapped KEK
envelopes (password and recovery-key wraps, both AES-GCM ciphertext),
per-device bearer tokens, and sealed `sessions/`, `events/`, `memory/`
blobs. Wire format for each blob: `<0x01><nonce:12><ciphertext><tag:16>`.

The upstream never stores the master password, the recovery key, the
unwrapped KEK, or any plaintext content. `resleeve serve` cannot decrypt
its own blob store — the KEK lives only in the RAM of authenticated client
daemons. Threat model:
[`../REVIEW_SECURITY.md`](../REVIEW_SECURITY.md).

## Operational notes

- **Pair-code TTL.** Default 5 min, server caps at 10. Use `--ttl 60s` for
  tighter windows.
- **Pair-invite ephemeral token.** `pair invite` logs in with a short-lived
  "pair-invite-ephemeral" device to fetch the KEK, then revokes it on exit
  (best-effort, 5s timeout). Server-side device TTL is the safety net —
  see [`../REVIEW_SECURITY.md`](../REVIEW_SECURITY.md) M5.
- **Login does not leak email enumeration.** The `/v2/auth/login-challenge`
  HMAC key is persisted across restarts so the synthetic-salt shape is
  identical on first probe and after a reboot (closes the M1 cross-restart
  oracle). `/v2/auth/login` itself does a dummy compare on the
  unknown-email branch (sec-followup-C / M6 constant-shape).
- **Outbox is durable.** If B is offline when A captures a session, the
  rows sit sealed in A's local outbox and drain when A reconnects.
  Idempotent — `UNIQUE(kind, key)` makes retries a no-op.
- **Resume forces a pull.** `resleeve resume <sid>` POSTs
  `/v1/sync/pull-now` before resolving the session id locally, so a
  session captured 5 seconds ago on A is resumable on B without waiting
  for the 30s tick. Falls back to local state on timeout.

## What's NOT shipped today

- **Single user per `serve`.** Multi-user / team mode is on the roadmap;
  SSE fan-out today sends every memory row to every subscriber. Per-user
  partitioning lands with team mode — see
  [`../REVIEW_SECURITY.md`](../REVIEW_SECURITY.md) M3.
- **TLS in the binary.** Front `resleeve serve` with a reverse proxy.
- **Rate limiting.** None in the binary. Rely on the reverse proxy.

## See also

- [`../USER_GUIDE.md`](../USER_GUIDE.md) §5 — sync model, identity,
  pairing, `migrate-key`.
- [`../ARCHITECTURE.md`](../ARCHITECTURE.md) §6, §7, §8 — daemon vs serve
  split, sync data flow, encryption envelope.
- [`../REVIEW_SECURITY.md`](../REVIEW_SECURITY.md) — full threat model and
  accepted-risk inventory.
