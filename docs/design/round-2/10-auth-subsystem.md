# Auth subsystem

How resleeve handles identity, authentication, encryption, multi-user, and orchestrator-federated trust. Consolidates auth-related decisions from `05-decisions.md`, `02-journey-01-decisions.md` (Q8 trust direction), and the per-journey UX in `05` and `06`.

## Identity model

Every resleeve deployment supports two identity flavors:

**Standalone identity:** a `user` account, anchored by an email address and password. Used for laptop / cross-machine / single-machine personas. Default model.

**Federated identity:** a `subject` derived from an external orchestrator's signed token. Used for SCION mode and any future orchestrator integration. Resleeve doesn't store credentials for federated subjects; it validates incoming tokens against registered issuer public keys.

Both flavors converge on the same authorization model: tokens carry **scope claims** ("`auth-rewrite/*`"), and resource access is checked against the scopes the bearer holds.

## Single-user vs. multi-user

For v1, a deployment serves *one* user by default — most users self-host resleeve for themselves. The auth surface still supports multiple users from day one (the cost is small, the retrofit is large per `05-decisions.md`).

| Deployment shape | What it looks like |
|---|---|
| **Personal** (default) | One user account. All scopes accessible. Auth feels like a formality. |
| **Team** (v2+) | Multiple users with overlapping scope access. Subscription auth and share allowlists become meaningful. Server config gates this mode (`resleeve.config: { multi_user: true }`). |
| **Federated / fleet** | Subjects come from SCION (or future orchestrators). User accounts may or may not exist — depends on operator policy. |

## Account lifecycle

### Signup

```
POST /v1/auth/signup
  {
    "email": "matt@example.com",
    "password": "..."
  }
```

Response:

```
{
  "user_id": "01HZ...",
  "recovery_key": "RESL-XXXX-YYYY-ZZZZ-AAAA-BBBB",
  "warning": "This recovery key is shown ONCE. Save it now. Resleeve cannot recover your data if you lose both your password and this key."
}
```

Server-side:

- Password is **never stored**. Server stores: a *verifier* (Argon2id-hashed password with a per-user salt) and a *KEK* (key-encryption key) that's encrypted with the password-derived key.
- The recovery key is a second password-equivalent: it can decrypt the KEK independently. Store its hash on the server.
- A user can have many access tokens; tokens carry scope claims.

### Login (standalone)

```
POST /v1/auth/login
  { "email": "...", "password": "..." }
```

Flow:

1. Server returns a per-user Argon2id salt.
2. Client derives the password key locally (Argon2id with deployment params).
3. Client proves possession of the password via a salted hash (server compares against the stored verifier).
4. On success: server returns an access token + the KEK ciphertext.
5. Client decrypts the KEK locally; subsequent crypto operations use the KEK.
6. **The plaintext password never leaves the client.**

Token storage:

- CLI: `~/.config/resleeve/auth.json` (mode 0600).
- Daemon: same file, read at boot.
- macOS Keychain / Linux Secret Service / Windows Credential Manager via `KeyStore` abstraction. Falls back to encrypted-on-disk file if no keychain available.

### Recovery

```
POST /v1/auth/recover
  { "email": "...", "recovery_key": "RESL-...", "new_password": "..." }
```

Flow:

1. Server validates the recovery key against its stored hash.
2. Server returns the KEK ciphertext, encrypted with a recovery-key-derived key.
3. Client decrypts the KEK using the recovery key.
4. Client re-encrypts the KEK with the new password's derived key.
5. Client uploads the new ciphertext; server replaces.
6. Recovery key is **rotated** automatically (a new one is generated; old one invalid).

Recovery is offline-derivable: even if the resleeve server forgets a user's password, the user can recover with the key alone.

### Rotate recovery key

```
POST /v1/auth/rotate-recovery
  { "current_password": "..." }   # auth required
```

Generates a new recovery key; invalidates the old one; returned once to the client.

### Logout & token revocation

```
POST   /v1/auth/logout                  # revoke current token
GET    /v1/auth/tokens                  # list active tokens for current user
DELETE /v1/auth/tokens/<id>             # revoke a specific token
```

## Tokens

### User access tokens (standalone)

- Issued at login.
- TTL: configurable per deployment, default 30 days.
- Scope claims default to all of the user's scopes; can be down-scoped at issuance time (`POST /v1/auth/login` with `scope: "auth-rewrite/*"` produces a narrower token).
- Stored locally in keychain / encrypted file.
- Sent as `Authorization: Bearer <token>` on every request.

### Bridge-plugin / daemon tokens

The local daemon needs a credential to talk to the upstream server.

- `resleeve login` provisions a long-lived **daemon token** (TTL 1 year, scope-limited to event submission + slot heartbeats).
- Stored in `~/.config/resleeve/agent.json` (mode 0600); read by `resleeve agent` at start.
- Separate from the user access token (which is for CLI operations like list/search/share).

This separation matters: a stolen daemon token can submit events but can't delete sessions or create public shares.

### Federated tokens (SCION mode)

Per `02-journey-01-decisions.md` Q8:

- Resleeve = OAuth2 resource server.
- SCION Hub mints JWTs (claims: `iss`, `sub`, `aud=resleeve`, `scope`, `exp`).
- Resleeve validates signature using the orchestrator's registered public key.
- No standalone account is required — the federated subject is its own identity.

Token registration:

```
POST /v1/orchestrators
  {
    "name": "scion-hub-prod",
    "kind": "scion",
    "hub_url": "https://hub.example.com",
    "issuer": "scion-hub-prod",
    "public_key_pem": "..."
  }
```

Resleeve thereafter accepts tokens with `iss = "scion-hub-prod"` and `aud = "resleeve"` signed by the registered key.

Token rotation: SCION rotates the signing key → operator runs `POST /v1/orchestrators/<id>` with the new public key; resleeve accepts both during the rotation window.

## Encryption model

Per `05-decisions.md`: zero-knowledge data-at-rest in both standalone and SCION modes.

Three keys per user:

- **Password key:** Argon2id-derived from the user's password. Used only to wrap/unwrap the KEK; never leaves the client.
- **Recovery key:** an independent key that also wraps the KEK; saved by the user offline.
- **KEK (Key Encryption Key):** the user's actual data key. Encrypts all session blobs, event payloads, and the user's own data.

Server stores:

- Password verifier (Argon2id hash).
- Recovery-key hash.
- KEK ciphertext (encrypted with both password key and recovery key — two independent wraps).
- Encrypted data (session blobs, event payloads, etc.) encrypted with the KEK.

Plaintext index fields (cleartext on server for queryability):

- Timestamps, CLI name, model name, cwd hash (SHA256, not the cwd itself), event counts, slot tuple, session_id.

This is the **Atuin model with two-wrap KEK** for usable recovery. The "must copy the key file" UX is replaced by "save your recovery key once."

### What's NOT encrypted

Server-side, for operational reasons:

- Account email (for login lookup).
- Subscription URLs and HMAC secrets (for delivery).
- Audit log entries.

This is honest: zero-knowledge applies to *user data* (sessions, events, plans), not to operational metadata.

### SCION mode encryption

The KEK is delivered to the daemon via the orchestrator's secret-distribution channel (per the SCION research: env vars / mounted secrets / Secret Manager CSI). The federated subject doesn't have a password; the operator is responsible for provisioning the KEK securely.

Optional refinement: tie the KEK to a derivation function over the Agent Token + a per-orchestrator master secret, so KEK rotation follows token rotation.

## Authorization (scope claims)

Every token carries one or more scope claims. Authorization is checked at every endpoint:

- Sessions: token must hold the session's scope (or an ancestor in glob form).
- Subscriptions: filter scope must be ⊆ token scopes.
- Shares: must own the source session.
- Memory (v2+): same scope-glob check as sessions.

Glob semantics:

- Token claim `auth-rewrite/*` grants access to `auth-rewrite/claude-1`, `auth-rewrite/claude-2`, but NOT `auth-rewrite/sub/x`.
- Token claim `auth-rewrite/**` grants any depth.
- Token claim `*` grants everything (admin / single-user default).

## Audit log

```
GET /v1/audit?since=<cursor>&filter=...     # admin-only
```

Records:

- Login / logout
- Token issued / revoked
- Recovery key used / rotated
- Subscription created / deleted
- Share created / revoked
- Session deleted
- Orchestrator registered / de-registered

Each entry: `(timestamp, actor, action, target, outcome, source_ip)`.

Audit log is encrypted at rest with a separate **audit key** (held server-side, not user-derived) so it survives user password resets.

## Open questions

- **MFA / TOTP:** v2+. Layer on top of password as a second factor. JWT claims grow to include `amr`.
- **Hardware-key (FIDO2 / passkey) signup:** v2+. Skips password entirely; user proves possession of a registered authenticator. Recovery flow needs adapting.
- **Session-level encryption granularity:** is one KEK enough, or do we want per-session keys (so revoking access to one session doesn't compromise another)? Lean: per-session keys derived from KEK + session_id, for forward-secrecy on individual session compromise.
- **Federation between resleeve deployments:** if Alice (resleeve.alice.com) wants to invite Bob (resleeve.bob.com) to a shared scope — cross-deployment auth handshake. Lean: out of scope for v1.
- **Token introspection endpoint:** RFC 7662 style `POST /v1/auth/introspect`. Useful for proxies and bridges. Lean: yes, v1.
- **Refresh tokens:** for long-lived bridge plugins, do we use refresh tokens or just long-TTL access tokens? Lean: long-TTL access tokens; refresh tokens are complexity for marginal benefit at our scale.
- **Single-user-deployment shortcut:** in `resleeve.config { multi_user: false }`, skip the email field entirely? Lean: still require email, but only one account allowed; signup endpoint returns 409 after first use.
- **Sub-user / API-key model:** non-human accounts (CI bots, etc.). Lean: model as a regular user with a restricted token; no separate "API key" concept.
- **Argon2id parameters:** memory cost, iterations, parallelism. Pick defaults that match OWASP recommendations + deployment-configurable.
- **Public-share decryption keying:** how do public shares stay accessible without the user's KEK? Lean: share creation re-encrypts the snapshot with a share-specific key derivable from the shortcode (effectively unencrypted, but format-uniform); `--password` shares derive an additional layer from the password.
