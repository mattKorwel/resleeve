# Use case 05 — self-hosted `resleeve serve`

## The problem

You don't want your AI session history sitting on a vendor cloud. You want a
small box you control — a $5 VPS, a homelab NAS, an old Mac mini — to act as
the upstream for one or more of your devices. Clients hold the keys; the
server stores opaque blobs and arbitrates ordering.

## What the server actually stores

Client blobs are AEAD-sealed (AES-256-GCM, 32-byte KEK derived from your
master password via Argon2id) before they cross the wire. The server stores
ciphertext keyed by `sessions/<id>`, `events/<id>`,
`memory/<encoded-scope>/...`. Operational metadata is real but small: key
shape includes scope path segments, plus blob sizes, push timestamps, and
the `identity.db` rows (`server_users`, `devices`, `pairings`).

The master password and the unwrapped KEK **never leave the client**. A
compromised server can leak operational metadata and reorder blobs; it
cannot decrypt sessions, events, or memory. See
[`../REVIEW_SECURITY.md`](../REVIEW_SECURITY.md) §"Threat model" and
§"Accepted-risk inventory".

## Sizing

- **Disk.** Dominated by `sessions/` + `events/` blobs (events is typically
  the heaviest table; memory is tiny). A heavy daily user generates tens of
  MB/day. 20 GB lasts a year for one person.
- **Network.** Outbox pushes are small JSON batches. SSE fast-tier emits one
  event per memory row, low rate.
- **CPU.** Negligible except during `resleeve login` (Argon2id work is
  client-side; the server does a constant-time compare).
- **Class.** A 1 vCPU / 512 MB VPS handles single-user comfortably.

## Walkthrough — install on a VPS

Pull the linux/amd64 release binary, drop it in `/usr/local/bin`, create a
state directory owned by a dedicated user, smoke-test on loopback:

```bash
# as root on the VPS
useradd --system --home /var/lib/resleeve --shell /usr/sbin/nologin resleeve
install -d -o resleeve -g resleeve -m 0700 /var/lib/resleeve/serve
curl -L https://github.com/mattkorwel/resleeve/releases/latest/download/resleeve-linux-amd64 \
  -o /usr/local/bin/resleeve
chmod +x /usr/local/bin/resleeve

sudo -u resleeve /usr/local/bin/resleeve serve \
  --addr 127.0.0.1:7860 --root /var/lib/resleeve/serve
# resleeve serve listening on http://127.0.0.1:7860
#   backend  local-disk at /var/lib/resleeve/serve
#   auth     bearer (token gated)
```

Bind a non-loopback address and the daemon prints a stderr warning at
startup (`WARNING  --addr 0.0.0.0:7860 is not loopback ...`). The warning
is **suppressed** only for `127.0.0.1:*`, `localhost:*`, and other loopback
IPs; bare port forms (`:7860`) count as non-loopback (safe default). Keep
the daemon on loopback behind a reverse proxy.

systemd unit at `/etc/systemd/system/resleeve-serve.service`:

```ini
[Unit]
Description=resleeve serve (self-hosted sync upstream)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=resleeve
Group=resleeve
StateDirectory=resleeve
ExecStart=/usr/local/bin/resleeve serve --addr 127.0.0.1:7860 --root /var/lib/resleeve/serve
Restart=on-failure
RestartSec=2s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

`systemctl daemon-reload && systemctl enable --now resleeve-serve` and check
`journalctl -u resleeve-serve` for the first-run bearer line (see below).

## Walkthrough — reverse proxy with Caddy

Caddy grabs a Let's Encrypt cert automatically and proxies to loopback:

```caddyfile
resleeve.example.com {
  reverse_proxy 127.0.0.1:7860
}
```

`systemctl reload caddy` and the cert lands on first hit. nginx is the same
shape (`proxy_pass http://127.0.0.1:7860;`) with manual `certbot`.

## First-run smoke

On first boot with no identity store and no `--auth-token` / `$RESLEEVE_SERVE_TOKEN`,
the daemon generates a one-time bearer and prints it to stderr:

```
serve: generated bearer token (one-time): 7c3f4...e9
       set RESLEEVE_SERVE_TOKEN or --auth-token to use a stable value
       (or use `resleeve register` + per-device tokens)
```

Two paths from here:

1. **Quick check (legacy bearer).** Copy that hex string and use it as
   `--upstream-token` from a client for a 60-second pipe test.
2. **Real deployment (per-device tokens).** Skip the bearer. Run `resleeve
   register` from your client; the server mints per-device tokens you can
   revoke individually with `resleeve logout`.

Use the per-device path. The legacy bearer is a migration shim — it carries
`/v2/sync/*` traffic but has known constraints on identity routes (see
`REVIEW_SECURITY.md` H3).

## Walkthrough — connect a client

From your laptop:

```bash
# one-time: create the account on the upstream
resleeve register --upstream https://resleeve.example.com
# prompted for email + master password
# recovery key is printed ONCE — save it

# unlock the local KEK for this device
resleeve login --upstream https://resleeve.example.com

# bring the daemon up pointed at the upstream
resleeve up --upstream https://resleeve.example.com

# verify
resleeve doctor
```

`doctor` should show the upstream URL, the keychain-loaded device token, an
unlocked sealer, and the bridge installed. Captured sessions drain to your
VPS sealed.

For a second device, see [`02-cross-machine-sync.md`](./02-cross-machine-sync.md)
— `resleeve pair invite` / `pair accept` avoids retyping the master password.

## Operational notes

- **Backups.** Snapshot `/var/lib/resleeve/serve/` to S3, Backblaze, USB —
  blobs are opaque without the master password. `identity.db` holds verifier
  hashes (Argon2id), wrapped KEK envelopes, and device tokens; back it up
  too — losing it forces every device to re-register.
- **Identity DB.** Defaults to `/var/lib/resleeve/serve/identity.db`.
  Override with `--dsn`. WAL mode + foreign keys on by default.
- **login-challenge HMAC key.** Persisted in `serve_meta` on first boot
  (sec-M1 fix); no restart oracle.
- **Logs.** Daemon writes to stderr; systemd captures it. Tokens only print
  at first-run generation.

## What this isn't ready for

- **Multi-user / team mode.** One `resleeve serve` is effectively single-user
  today. Per-user SSE partitioning (sec-M3) ships with the round-8 team
  feature; until then, don't give an account to anyone you wouldn't share a
  SQLite file with.
- **Public internet without TLS.** The daemon speaks plaintext HTTP. A
  reverse proxy doing TLS termination is non-negotiable off-LAN.
- **Hardened application-layer rate limiting.** None at the resleeve layer
  yet. Rely on the proxy (`caddy` `rate_limit`, nginx `limit_req`) for
  IP-level throttling. Protocol-level brute-force protection exists
  (Argon2id cost + 5-attempt pair-claim lockout) but is not a substitute.

## See also

- [`02-cross-machine-sync.md`](./02-cross-machine-sync.md) — adding a second
  device with `pair invite` / `pair accept`.
- [`../USER_GUIDE.md`](../USER_GUIDE.md) §5 — full sync / identity surface.
- [`../REVIEW_SECURITY.md`](../REVIEW_SECURITY.md) — threat model,
  findings, accepted-risk inventory.
