# Security review — resleeve (current `main`)

Date: 2026-06-10. Scope: full crypto/auth/network/multi-tenancy surface. Read-only.

This review reflects the **current `main`** — the multi-tenant + encryption-matrix
codebase — and supersedes the prior review that was pinned to commit `cc04113`
(dated 2026-06-04, single-user-only). The older review's identity-surface findings
(H1–H4, M1) have since been remediated; this document confirms each against the
current code and re-states the posture from scratch. Where the old review flagged
"future / when multi-tenancy ships" risks, those mechanisms now exist and are
assessed here as shipped surface.

## Summary

- **Two deployment shapes, two trust models.** A **solo self-host** runs
  `resleeve serve --single-tenant`: the keyspace is global, the daemon always
  seals client-side, and the server is a zero-knowledge blob store (it never holds
  a content key). A **team server** runs multi-tenant: multiple mutually-untrusted
  users share one operator-run server, data is partitioned per "brain," and the
  server holds per-brain at-rest keys under an operator master key. The two shapes
  have materially different threat models and the doc treats them separately.
- **Crypto floor is solid and unchanged.** Argon2id (`golang.org/x/crypto/argon2`),
  AES-256-GCM with random 12-byte nonces, `crypto/rand` throughout, constant-time
  verifier compares, dual-wrapped KEK with independent salts, version-prefixed
  client envelope, and a separate envelope-encryption helper for server-at-rest
  DEKs. No CGO key-locking, so RAM scrubbing is best-effort (documented).
- **Tenant isolation holds and was verified.** Cross-tenant read/write is blocked
  at the request boundary: every `/v2/sync/*` call in multi-tenant mode resolves to
  the caller's authenticated user, the acting brain is membership-validated, and the
  server transparently prefixes/strips a `<brain_id>/` keyspace namespace the client
  never controls. The legacy no-user bearer is rejected on all user-scoped and sync
  routes in multi-tenant mode. Brain/membership management is owner-gated.
- **Encryption matrix is coherent and fail-closed.** In-transit TLS is the
  operator's job (out of scope here). At rest, a multi-tenant server is *required* to
  carry a 32-byte master key — `New` refuses to start without one — because under the
  default policy the daemon ships plaintext over TLS and the per-brain DEK is the sole
  encryption layer. A missing per-brain DEK is now **fail-secure** (lazily provisioned
  rather than passthrough-to-plaintext), and any DEK that won't unwrap surfaces an
  error rather than serving plaintext. End-to-end (zero-knowledge) client sealing is
  retained for solo and is opt-in per personal brain on a team server.
- **Prior identity-surface findings are FIXED.** Argon2 floor/ceiling on register,
  pair-claim lockout counter with indistinguishable lockout-vs-expiry responses,
  legacy-bearer rejection on user-scoped + sync routes, best-effort KEK/sealer wipe on
  logout, persisted login-challenge HMAC key (restart-oracle closed), unknown-email
  login-timing decoy compare, per-event sealer re-sample, and a clamped device-token
  TTL for ephemeral pair-invite tokens are all present in the current code.
- **Open/accepted risks are operational, not confidentiality breaks.** The shared
  host has no per-user rate limits, storage quotas, or request-body caps by default
  (a registered user can DoS a shared server); member-ids are enumerable within a
  shared brain (intentional metadata exposure); SSE reconnect re-decrypts the backlog
  (work amplification, not a leak); and the cross-brain `..`-escape defense currently
  lives at the backend `ValidateKey` layer rather than the server layer. None of these
  cross a tenant boundary or expose plaintext.

## Threat model (assumed)

This codebase now supports two deployment shapes. Each has its own adversary set.

### Solo self-host (`--single-tenant`) — client-side zero-knowledge

- **In scope.** One user; one operator running both the client/daemon and an
  honest-but-curious or fully compromised `resleeve serve` upstream. The daemon
  always seals client-side, so a compromised upstream sees only AEAD ciphertext +
  key-shape metadata. A network attacker can observe/replay TLS-terminated traffic
  (TLS termination is the operator's job, out of scope). The local daemon binds
  loopback; same-user processes on the host are trusted to the extent that anything
  readable by the user (endpoint secret, keychain entry, daemon RAM) is readable by
  them.
- **Adversary goals defended against.** Recovering plaintext sessions/events/memory
  from a compromised upstream; recovering the master password or KEK from network
  traffic; replaying/hijacking pair codes; turning the daemon's loopback API into a
  remote attack surface via a misconfigured bind.

### Team server (multi-tenant) — server-side at-rest + per-user isolation

- **In scope.** Multiple **mutually-untrusted registered users** on one
  operator-run server. The operator is trusted (they hold the master key and can read
  plaintext at the server boundary); the *other tenants* are the adversary. The
  defended properties are **tenant isolation** (user A cannot read or write user B's
  brains) and **at-rest confidentiality against disk theft** (a stolen storage
  snapshot yields only ciphertext + wrapped DEKs, useless without the master key).
  Under the default server-side policy, the operator-run server necessarily sees
  plaintext in memory while servicing a request — that is the explicit cost of
  server-side at-rest, and a user who wants the server blinded uses an end-to-end
  personal brain.
- **Adversary goals defended against.** Cross-tenant read/write; brute-forcing
  another tenant's verifier from a stolen DB; pairing/claiming into another user's
  account; using a legacy shared bearer to impersonate a real user on the sync or
  identity routes; recovering plaintext from a stolen disk image.

### Out of scope (both shapes)

- TLS termination — operator-provided reverse proxy.
- Browser/web UI — none exists.
- Third-party dependency internals (`golang.org/x/crypto`, `zalando/go-keyring`,
  `modernc.org/sqlite`) — only their wire/API surface was considered.
- The operator's own honesty on a team server (they hold the master key by design).
- Backup/restore and storage-side audit logging — none today.

---

## Findings

Finding ids (H1, M-A, …) are stable labels for cross-reference; they do not imply a
remediation milestone.

### Critical

None. The crypto envelope, KEK wrap, zero-knowledge wire shape, and tenant-isolation
boundary all match the documented model.

### High — previously flagged, now FIXED (confirmed in current code)

These were open in the `cc04113` review. Each is now resolved; they are documented as
**fixed** for traceability, not re-flagged.

**H1 (fixed). Register now enforces an Argon2id floor and ceiling.**
`validateArgon2Params` (`internal/serve/auth_handlers.go`) rejects client-supplied
params below a 64 MiB / 2-iter / 1-parallelism floor (OWASP-leaning interactive
baseline) and above a 4 GiB / 32-iter ceiling, and pins the salt length to 16 bytes
and the verifier output to 32 bytes. A verifier theft can no longer be paired with
laptop-cheap params; an absurd-params single-shot DoS that would poison every later
login is also blocked.

**H2 (fixed). Pair-claim now has a per-code failed-attempt lockout, and lockout is
indistinguishable from expiry.** `handlePairClaim` increments an in-memory
per-`code_id` failure counter after each verifier mismatch; at `pairClaimMaxAttempts`
(5) it hard-deletes the pairing row. A claim against a missing, swept-expired, or
locked-out code returns the **same** 404 body (`writePairClaimGone`), so a
brute-forcer who learns a `code_id` cannot tell lockout from natural TTL expiry. The
60-bit code's brute-force window is now bounded by the attempt budget, not just the
5-minute TTL. (Counter state is in-memory and lost on restart; codes are 5-minute TTL
anyway, so the reset doesn't widen the window meaningfully — noted in the accepted-risk
inventory.)

**H3 (fixed). The legacy no-user bearer is rejected on all user-scoped and sync
routes in multi-tenant mode.** `requireBrain` (sync) and `requireUser` (brain
management) both reject a bearer whose resolved `UserID` is empty with 401, except in
single-tenant mode where exactly one user's data exists and no partitioning applies.
`handlePairPublish` likewise refuses a legacy bearer (it cannot mint a pairing row
under an empty user), and `handleLogout` rejects the synthetic `legacy` device. A
shared bearer can therefore no longer masquerade as a real user, publish pair codes, or
write into the per-user keyspace.

**H4 (fixed). KEK and sealer key material is best-effort wiped on logout.**
`auth.Wipe` / `KEK.Wipe` / `AESGCMSealer.Wipe` zero the underlying bytes;
`SyncClient.ClearSealer` and `SetSealer` wipe the displaced sealer under the lock
before nil-ing it, and `handleSealUnlock` scrubs the request-body KEK copy after the
sealer takes ownership. This is best-effort RAM scrubbing — pure-Go cannot lock pages
against swap or core dumps without CGO, and copies made by value before the wipe can
linger until GC — but the window after an explicit `logout` is materially shortened.
Documented as a best-effort control, not a guarantee, in the accepted-risk inventory.

### Medium

**M-A (open, accepted). No per-user rate limits, storage quotas, or request-body caps
by default — a registered user can DoS a shared server.** `handlePush` decodes the
request body with no `http.MaxBytesReader` cap and writes every committed blob to the
backend; there is no per-user storage quota and no per-endpoint rate limit anywhere on
the serve path (confirmed: no `MaxBytesReader` / rate-limiter in `internal/serve`).
The push-key prefix gate (sec-M-B below, fixed) blocks *arbitrary-prefix* smuggling,
but a registered user can still fill disk with valid `sessions/`/`events/`/`memory/`
writes or exhaust CPU/socket budget with request volume. On a solo loopback daemon this
is irrelevant; on a **shared team server** it is a real denial-of-service available to
any registered user. *Mitigation today:* the operator's reverse proxy can rate-limit,
and a body/SSE/per-brain cap pass is planned as server-layer hardening. *Severity:*
medium — availability only, no confidentiality or cross-tenant impact.

**M-B (fixed). Push keys are gated to the three allowed prefixes.** `isAllowedPushKey`
in `handlePush` rejects any key not starting with `sessions/`, `events/`, or
`memory/`, closing the arbitrary-prefix blob-smuggling vector the old review flagged.
The client supplies a brain-agnostic key and the server prepends the *authenticated*
acting brain, so a client can never forge a cross-brain prefix from the request body.

**M-C (open, accepted). Member-id enumeration within a shared brain.**
`handleListMembers` returns the raw list of member `user_id`s to **any member** of the
brain. This is intentional metadata exposure — co-members of a shared brain can see who
else is in it — but it does mean a brain member learns the opaque user-ids of their
peers. It does not cross a brain boundary (a non-member gets 403 and learns nothing,
including whether the brain exists), and user-ids are opaque identifiers, not emails.
*Severity:* low-medium — intra-brain metadata exposure by design; documented so it is a
deliberate decision, not an oversight.

**M-D (open, accepted). SSE reconnect re-decrypts the backlog — work amplification.**
On every SSE (re)connect, `handleSSE` walks the memory backlog strictly after `since`
and, on a server-at-rest brain, decrypts each stored blob with the brain DEK before
streaming it. A client that reconnects frequently (flaky network, aggressive backoff
floor) re-drives that decrypt work each time. The client backoff is exponential
(1s→30s) and the live fan-out path ships already-plaintext blobs (no decrypt), so the
amplification is bounded, but a pathological reconnect loop against a large brain is
CPU work a misbehaving member can drive (you must still hold a valid device token).
*Severity:* low — availability only, intra-brain. Cross-reference M-A: the same
per-user budgeting that bounds push DoS would bound this.

**M-E (open, accepted). Cross-brain `..`-escape defense currently lives at the backend
layer, not the server layer.** The defense against a key like `../<other-brain>/...`
escaping the acting brain's namespace is `sync.ValidateKey`, which rejects `.`/`..`
path segments and leading slashes at the backend. The server-layer push handler gates
the *prefix* (M-B) and prepends the authenticated brain, but does not itself re-validate
for `..` traversal — it relies on the backend's `ValidateKey` to catch it. Today every
backend in the tree enforces it, so there is no live escape, but the invariant is one
layer deeper than ideal: a future backend that skipped `ValidateKey` would reopen it.
*Mitigation:* hardening to validate the key shape at the server layer (defense in depth)
is planned. *Severity:* medium *as a latent invariant* — no live exploit found.

**M-F (fixed). login-challenge HMAC key is persisted — restart oracle closed.** The
synthetic-salt HMAC key for unknown-email login-challenge probes is now stored in
`serve_meta` (`loadOrCreateLoginChallengeKey`) and never rotated, so two probes of the
same unknown email across a `resleeve serve` restart return the *same* decoy salt. The
"did the server restart?" / "is this salt a decoy?" oracle the old review flagged is
closed for any deployment with an identity store. (Legacy AuthToken-only mode keeps a
per-process key, but that mode predates the identity stack the oracle would target.)

**M-G (fixed). Unknown-email login now runs a constant-cost decoy compare.**
`handleLogin` compares the client verifier hash against a per-process random
`loginTimingDecoy` on the unknown-email branch, giving it the same
`subtle.ConstantTimeCompare` cost as the known-email branch and removing the
cleanly-distinct early-return timing shape between "no such email" and "wrong password."

**M-H (fixed). Sealer is re-sampled per event inside the enqueue loop.**
`EnqueueEvents` reads `getSealer()` / `getShouldSeal()` **inside** the per-event loop,
so a `logout` that races a batch takes effect mid-batch rather than sealing the whole
batch under a stale reference. Capture still proceeds regardless of sealer state (events
are never lost on a lock/unlock transition); confidentiality holds either way because a
nil sealer parks the drain loop rather than shipping plaintext.

**M-I (fixed). Ephemeral pair-invite device tokens carry a clamped TTL.** Devices now
support an `ExpiresAt`, and `mintDevice` honors a client-requested
`ExpiresAtHintNS` clamped to `deviceExpiresAtHintMax` (10 minutes).
`deviceFromBearer` rejects an expired token. The pair-invite ephemeral token is minted
with a short TTL so a failed best-effort revoke no longer leaves a year-long live
bearer — the server enforces expiry regardless.

### Low / Informational

- **L1. Endpoint file (`~/.resleeve/endpoint`, 0600) holds the daemon bearer in
  cleartext.** Any same-user process can read it and drive the local daemon. Matches the
  documented local-trust boundary; accepted.
- **L2. seal/unlock is loopback-enforced (belt-and-suspenders).** `/v1/seal/*` rejects
  non-loopback peers even if the daemon is mis-bound to a public address, so a config
  drift cannot leak the KEK over the wire. Same-user local processes remain in the
  trusted set.
- **L3. `resleeve serve` defaults to loopback but accepts any `--addr` with no Host /
  CORS check.** A casual deployment is loopback-safe; an operator who binds a public
  address must front it with TLS + the reverse proxy this review assumes. No
  `Access-Control-Allow-Origin` is sent, so browser cross-origin fetches are rejected by
  the browser — the correct default.
- **I1. PRNG audit clean.** Every key/nonce/salt/token draw uses `crypto/rand`
  (auth envelope + at-rest helpers, KEK, server login-challenge key + timing decoy,
  device/brain id minting). No `math/rand` on any non-test path.
- **I2. Constant-time compares on every secret path.** Login verifier, pair-claim
  verifier, daemon bearer, and the unknown-email decoy all use
  `subtle.ConstantTimeCompare`. Device-token and DEK lookups are DB/`map` keyed by a
  256-bit / 32-byte secret, where the timing surface is the index seek, not a
  byte-compare — acceptable given the entropy.
- **I3. At-rest envelope is tamper-evident and fail-closed.** `WrapDEK`/`UnwrapDEK`
  and `EncryptAtRest`/`DecryptAtRest` (`internal/auth/atrest.go`) are AES-256-GCM with
  random 12-byte nonces and a length-checked `nonce||ct+tag` layout. A wrong/rotated
  master key, a wrong DEK, or any tampering fails the GCM tag and returns an error —
  never silent plaintext. The unwrapped DEK length is asserted to 32 bytes.
- **I4. Push input is strict-decoded.** `handlePush` / brain-management handlers use
  `DisallowUnknownFields`, so a malformed or smuggled-field body is a 400, not a
  partial write.

---

## Encryption matrix (as built)

| Layer | Solo (`--single-tenant`) | Team server (multi-tenant) |
|---|---|---|
| In transit | Operator TLS (out of scope) | Operator TLS (out of scope) |
| Client-side (zero-knowledge) | **Always on** — daemon seals every blob; server never holds a content key | **Off by default**; opt-in per **personal** brain (`encryption_policy=e2e`) |
| Server-at-rest (per-brain DEK under operator master key) | Optional (off by default) | **Required** — `New` refuses to start a multi-tenant server without a 32-byte master key |

Key points verified in code:

- **Default-flip is enforced at startup.** `serve.New` returns an error if the server
  is multi-tenant and the master key is absent or not 32 bytes. The reasoning is sound:
  under the default `server-side` policy the daemon's seal-decision handshake
  (`GET /v2/sync/whoami` → `shouldSeal = !multiTenant || policy=="e2e"`) tells it to
  ship **plaintext** over TLS, so the per-brain DEK is the *only* encryption layer at
  rest. A multi-tenant server with no master key would persist plaintext for every
  brain — fail-fast is correct.
- **Missing-brain-DEK is fail-secure (fixed).** `brainDEK` returns "no key" *only* when
  at-rest is globally disabled. When at-rest is enabled but a specific brain's DEK is
  missing (e.g. a partial-failure create), it **lazily provisions** a fresh DEK rather
  than passing through to plaintext, and a DEK that won't unwrap under the current master
  key surfaces an error instead of serving plaintext. This is a change from the older
  behavior and closes a plaintext-passthrough gap.
- **e2e on a shared brain is rejected, not silently broken.** `handleSetPolicy` allows
  `e2e` only on a *personal* brain; flipping a shared brain to `e2e` returns 400 because
  group keys (which would let co-members read each other's zero-knowledge writes) are not
  built yet. This is the right fail-closed choice — it refuses rather than locking
  members out.
- **Seal envelope and at-rest envelope are deliberately separate.** The client `Sealer`
  is a version-prefixed, stateful, `Wipeable` AEAD; the at-rest helpers are stateless
  pure functions over a per-request DEK. Both are AES-256-GCM with random 12-byte
  nonces. Double-encryption (an already-sealed e2e blob also wrapped at rest) is benign.

---

## Tenant isolation (as built)

Verified that cross-tenant read/write is blocked:

- **Authenticated identity is mandatory.** In multi-tenant mode `requireBrain` rejects
  any bearer that does not resolve to a real `UserID` (the legacy no-user bearer is 401).
- **Brain resolution is membership-gated.** `resolveBrain` maps `(userID, ?brain=…)` to
  an acting brain: an explicit selector must pass `memberships.IsMember`
  (else 403 `errBrainForbidden`); an absent selector defaults to the caller's own
  personal brain. A user can never act in a brain they are not a member of.
- **Keyspace is server-controlled.** The wire key is brain-agnostic; the server prepends
  `<brain_id>/` (from the *authenticated* membership, never the request body) before
  `backend.Put` and strips it on read. `scopeCursor` re-scopes pagination so `List` walks
  only the acting brain's keys. A client cannot forge another brain's prefix.
- **Membership management is owner-gated.** Any member may list members and their own
  brains; only the brain owner (`requireOwner`) may add/remove members, and the owner
  cannot remove themselves (no orphaned brain). An added user must already exist (no
  phantom memberships). Unknown brain → 404; a brain you can't own → 403.
- **Fan-out is brain-keyed.** The SSE hub (`fanout.go`) routes a memory push to *only*
  the subscribers of its brain, published under the authenticated acting `brain_id`.
  This both delivers cross-member rows within a shared brain and prevents cross-brain
  amplification — a non-member subscribed to a different brain receives nothing (verified
  by `TestFanout_IsolationNonMember`).

---

## Accepted-risk inventory

Known limitations, documented with why each is accepted today.

1. **No per-user rate limits / storage quotas / request-body caps on the serve path
   (M-A).** Availability risk on a shared team server; no confidentiality or
   cross-tenant impact. Mitigated operationally by the reverse proxy; server-layer
   body/SSE/per-brain caps are planned hardening.
2. **Member-id enumeration within a shared brain (M-C).** Intentional intra-brain
   metadata exposure; co-members see each other's opaque user-ids. Does not cross a
   brain boundary.
3. **SSE reconnect re-decrypt amplification (M-D).** Bounded by exponential client
   backoff; availability only; would be covered by the same per-user budgeting as M-A.
4. **`..`-escape defense at the backend layer, not the server layer (M-E).** No live
   exploit (every current backend enforces `ValidateKey`); server-layer validation is
   planned as defense in depth.
5. **Best-effort KEK/sealer RAM scrub (H4).** Pure-Go cannot lock pages against swap or
   core dumps without CGO; the explicit-logout window is shortened but not eliminated.
6. **Pair-claim lockout counter is in-memory (H2).** Lost on restart, but codes are
   5-minute TTL, so the reset does not meaningfully widen the brute-force window.
7. **Endpoint secret + keychain entries readable by any same-user process (L1).**
   Inherent to the local-trust boundary; raising it needs OS-level isolation.
8. **Operator sees plaintext on a server-side team brain (by design).** A user who wants
   the server blinded uses an end-to-end personal brain. Server-side at-rest defends
   against disk theft, not against the trusted operator.
9. **Legacy AuthToken-only mode keeps a per-process login-challenge key.** That mode has
   no identity stack, so the restart oracle (M-F) does not bite any real registered
   email there.

---

## Out-of-scope / future (not vulnerabilities)

These are unbuilt features, listed so they are not mistaken for gaps in shipped surface:

- **Shared-brain end-to-end (group keys).** Not implemented; `e2e` is rejected on shared
  brains rather than silently locking members out (correct fail-closed behavior).
- **OIDC / SCIM enterprise identity.** Not implemented; identity is the password +
  per-device-token stack.
- **Local-at-rest encryption** of the daemon's on-disk store. Not implemented; the local
  store relies on the same-user OS boundary.
- **Webhooks / outbound callbacks** (and therefore SSRF surface). Not implemented.
- **Horizontal scale-out backplane** (Postgres `LISTEN/NOTIFY` behind the `fanoutHub`
  seam). Not implemented; single-instance only today.
- **Storage-side audit logging** and **backup/restore** flows. None today.

---

## Verdict

The current `main` is in sound security posture for both supported deployment shapes.
The crypto floor is unchanged and solid; the multi-tenant work added a coherent,
fail-closed encryption matrix and a verified tenant-isolation boundary (membership-gated
brain resolution, server-controlled keyspace prefixing, owner-gated membership,
brain-keyed fan-out); and every identity-surface finding from the prior pinned review
has been remediated in code. The remaining open items are operational
availability/metadata risks on a shared server (no per-user rate/quota/body limits,
intra-brain member-id visibility, SSE re-decrypt amplification) plus one latent
defense-in-depth gap (server-layer `..`-validation), all documented in the accepted-risk
inventory — none of which expose plaintext or cross a tenant boundary. The highest-value
next hardening is per-user resource budgeting (rate limits + storage quotas + body caps)
on the multi-tenant serve path.
