# Security review — resleeve @ cc04113

Date: 2026-06-04. Scope: full crypto/auth/network surface. Read-only.
Reviewer: subagent (parent: fix/review-security).

## Summary

- Crypto floor is solid. Argon2id (golang.org/x/crypto/argon2), AES-256-GCM with random 12-byte nonces, crypto/rand throughout, constant-time verifier compare, dual-wrapped KEK with independent salts, version-prefixed envelope. Round-2/10 spec lands faithfully.
- Wire-side zero-knowledge holds. KEK is unwrapped on the client; only verifier hashes + wrapped KEK envelopes traverse the network on register/login/pair. `serve` never sees the master password or the KEK in cleartext.
- Identity surface has correctness wrinkles that don't break confidentiality but soften round-2's guarantees: register accepts client-supplied Argon2id params with no floor, login-challenge HMAC key is per-process (rotates on restart, accepted), and the legacy single-bearer fallback in `requireDevice` returns a synthetic device with UserID="" that — for `/v2/sync/*` routes — is indistinguishable from a real bearer.
- Loopback boundary on the local daemon is enforced twice (default 127.0.0.1 bind plus a `requireLoopback` middleware on `/v1/seal/unlock`), so a config drift to 0.0.0.0 still cannot leak the KEK over the wire. Any same-user process on the host can still read seal.key, the keychain entry, the endpoint secret, and process memory — accepted in the documented threat model.
- The accepted-risk inventory (placeholder seal.key, file-keychain fallback, ErrNoUpstream string-body coupling, opencode stub, no rate limiting) is internally consistent with the punch list and the round-5 follow-ups. The two server-side items most worth tracking next: pair-claim verifier-hash brute force window (gated by the 5-minute TTL but no per-code attempt counter), and KEK lingering in daemon RAM with no clearing path beyond GC.

## Threat model (assumed)

- **In scope.** Single user; one operator running both client and an honest-but-curious or compromised `resleeve serve` upstream. Network attacker can observe and replay TLS-terminated traffic (TLS is the operator's job; out of scope here). Local daemon binds loopback; same-user processes on the host are trusted to the extent that anything readable by the user is readable by them (seal.key, endpoint file, keychain entries, RAM).
- **Out of scope.** Browser/web UI (none yet). v1/serve TLS termination (operator-provided reverse proxy). Third-party deps' internals. Multi-tenant adversarial users on the same upstream (post-#3 per-user partitioning ships later).
- **Adversary goals being defended against.** Recovering plaintext sessions/events/memory from a compromised upstream; recovering the master password or KEK from network traffic; replaying or hijacking pair codes; turning the daemon's loopback API into a remote attack surface via a misconfigured bind.

---

## Findings

### Critical — fix before announcing "encrypted sync"

None found at the round-5 floor. The crypto envelope, KEK wrap, and zero-knowledge wire shape match the published threat model.

### High — fix in next round

**H1. Register accepts client-supplied Argon2id params without a floor; verifier theft + low-cost params = offline cracker.**

- Where: `internal/serve/auth_handlers.go:556-569` (`validateRegister`), `internal/serve/auth_handlers.go:148-219` (`handleRegister`).
- What: The server takes `Argon2idParams{MemoryKiB, TimeIters, Parallelism}` verbatim from the client and persists them on `server_users`. `validateRegister` only rejects zero values. A client (or a malicious process posing as one) can register with `{MemoryKiB: 8, TimeIters: 1, Parallelism: 1}` — the same params `pair.deterministicSalt` uses internally — and then a server-side dump of `password_verifier_hash + password_verifier_salt + argon2_*` is brute-forceable on a laptop CPU.
- Why it matters: The whole point of Argon2id is to make verifier-hash theft un-crackable for years. A self-hosted operator who registers via the supplied CLI is fine (it uses `auth.DefaultArgon2idParams()` = 64 MiB / 3 iters / 1). But a single bad-faith client erodes that for that account, and there's no server-side floor.
- Fix sketch: add a minimum-cost predicate in `validateRegister` — refuse params weaker than OWASP floor (MemoryKiB ≥ 19 MiB, TimeIters ≥ 2, Parallelism ≥ 1), or just `auth.DefaultArgon2idParams()` minus tolerance. Same floor on `handlePairPublish` (the inviter ships params for the pair envelope). Server-side change only; clients unaffected.
- Detection notes: a register integration test with weakened params should now return 400.

**H2. Pair-claim verifier hash brute force window — no per-code attempt counter, gated only by the 5-minute TTL.**

- Where: `internal/serve/auth_handlers.go:422-467` (`handlePairClaim`), `internal/storage/sql/sqlite/identity.go:213-232` (`pairingStore.Claim`).
- What: A `pair invite` publishes a verifier hash derived from a 60-bit base32 pair code (`internal/cli/pair.go:125-130`, `formatPairCode`). `handlePairClaim` does `subtle.ConstantTimeCompare` against the stored verifier and atomically marks the code as claimed on first match. Until the code is claimed or expires, an attacker who learns `code_id` (which the operator may share over an out-of-band channel before the accepter types the code) can submit unlimited verifier hashes — only the Argon2id cost (16 MiB / 2 iters) and the 5-min TTL throttle them. 60 bits is fine if Argon2id is 200 ms but the window is wide if anyone can grind it from a beefy box; for the published pair-invite UX that prints both `code_id` and `pair_code` to the operator's terminal, the threat is limited but real.
- Why it matters: If `code_id` ever leaks (logged, screen-shared, copied into a chat that's compromised), an attacker has minutes to brute-force the 60-bit pair code at the upstream's Argon2id cost. Compromise = device token + ability to unwrap the KEK from the published envelope.
- Fix sketch: persist a failed-attempt counter on `pairing_codes`; expire/delete the code after N failed claims (5-10). Or shorten the default TTL (currently 5 min with 10-min ceiling) when the brief says "60s one-time code." Or sleep on failed Claim before returning (constant cost per attempt regardless of params).
- Detection notes: add a fuzz-claim test that submits 10+ wrong hashes for a single code_id and asserts the row is deleted/locked.

**H3. Legacy single-bearer fallback masquerades as a real device on `/v2/sync/*` routes.**

- Where: `internal/serve/auth_handlers.go:491-510` (`deviceFromBearer`), `internal/serve/server.go:128-136` (`requireAuth`).
- What: When `cfg.AuthToken` is set AND a matching bearer is presented, `deviceFromBearer` returns `&rsql.Device{ID: "legacy", UserID: "", Name: "legacy-bearer"}`. `/v2/sync/*` doesn't care about UserID and accepts it. `/v2/auth/pair/publish` would, too (it calls `requireDevice` and uses `dev.UserID` to scope the pairing row — a publisher with UserID="" would create a pairing row with `user_id=""`, which `handlePairClaim` then mints a device for under UserID="").
- Why it matters: If a deployment runs with both `--auth-token` AND a populated identity store (a normal migration overlap, per the comment block), a single shared bearer becomes equivalent to a per-device token for sync, AND can publish pair codes that mint UserID="" devices. The intended migration path keeps legacy for `/v2/sync/*` only.
- Fix sketch: in `handlePairPublish`, reject `dev.UserID == ""` explicitly with 403 ("legacy bearer cannot pair — register first"). Optionally gate the legacy fallback to `/v2/sync/*` only by introducing an `allowLegacy` flag on `deviceFromBearer` or by changing `requireDevice` to call a `deviceFromBearerStrict` variant. Document the migration overlap as "legacy can push/pull, cannot manage identity."
- Detection notes: an auth_handlers_test that publishes a pair invite with the legacy bearer should now 403.

**H4. KEK in daemon RAM has no clear path on lock; the auth.KEK byte array stays in memory until GC.**

- Where: `internal/auth/kek.go:14` (`type KEK [32]byte`), `internal/agent/seal_handlers.go:69-93` (`handleSealUnlock`), `internal/agent/sync.go:107-120` (`SetSealer`/`ClearSealer`).
- What: `handleSealUnlock` builds an `auth.AESGCMSealer` from the posted KEK bytes; the cipher.Block holds the expanded AES key schedule in memory and there's no zeroize on ClearSealer (`s.sealer = nil` just nils the interface — the underlying AES key schedule lingers until GC). The request body's `req.KEK []byte` likewise stays in memory until the JSON decoder's buffer is reused. `KEK [32]byte` returned from `Unwrap` is copied around by value (e.g. into `kek[:]` slices in `cli/auth.go:221`, `cli/migrate_key.go:106`).
- Why it matters: An attacker who reads daemon process memory (core dump, ptrace, /proc/self/mem) after a `resleeve logout` can still recover the KEK. The brief explicitly notes "not lockable in pure-Go without CGO" so this is partly accepted, but the round-5 spec implies that lock genuinely revokes access.
- Fix sketch: explicit zeroize on ClearSealer — wrap the AEAD in a struct that holds the raw key bytes and a `Wipe()` that zeros them; call Wipe in ClearSealer. Pure-Go won't survive swap-out but it shortens the window. Document it as "best-effort RAM clear; OS swap and core dumps remain a risk on this platform."
- Detection notes: a test that calls ClearSealer and then asserts a freshly built sealer with the same key still works (proves the original was forgotten) is hard in pure Go; document as a code-review invariant instead.

### Medium

**M1. login-challenge synthetic-salt HMAC key rotates on every restart — gives a "did the server reboot?" oracle to a probe.**

- Where: `internal/serve/server.go:86-89` (`New` generates `loginChallengeKey`), `internal/serve/auth_handlers.go:265-271` (`syntheticChallengeSalt`).
- What: The HMAC key is fresh-random per process. The comment correctly notes that "since the synthetic salts are decoys (the subsequent /v2/auth/login always 401s), rotation has no client-visible effect" — for any single email. But a passive observer who challenges a known-unknown email both before and after a restart sees two different decoy salts; a real account's salt is stable (persisted in `password_verifier_salt`). That's a restart-detection sidechannel, not an account-enumeration one — but it does mean an attacker can probabilistically classify a salt as "decoy vs. real" by replaying after a restart.
- Why it matters: Pretty minor — the attacker who can observe two challenges across a restart can already see whether the server restarted. The constant-shape guarantee for any single observation still holds. Documenting it because the followup-C honest-concern was "verify it actually is constant-shape under the test set" and the test set doesn't currently cover the cross-restart oracle.
- Fix sketch: persist the HMAC key (e.g. as a row in `server_users` table or a `serve_meta` table), generate on first boot, never rotate. Or derive the key from a long-lived secret in `Config`.
- Detection notes: an integration test that probes a known-unknown email, restarts the server, probes again, and asserts the salt is identical.

**M2. `handlePush` accepts any well-formed key, not only `sessions/`, `events/`, `memory/` prefixes.**

- Where: `internal/serve/server.go:160-190` (`handlePush`), `internal/sync/backend.go:58-74` (`ValidateKey`).
- What: `handlePush` calls `backend.Put(ctx, row.Key, row.Blob)` for any well-formed key. `ValidateKey` only rejects empty/leading-slash/dot-segment keys. A misbehaving client (or an attacker with a stolen device token) could write blobs under arbitrary prefixes, which the SSE fast-tier ignores (the `memory/` prefix check on line 185 gates fan-out) but which the local-disk backend cheerfully stores forever. Disk fill / blob smuggling.
- Why it matters: Self-DoS by a compromised device. Not a confidentiality issue.
- Fix sketch: validate `row.Key` starts with one of `sessions/`, `events/`, `memory/` and call out the allowed shapes explicitly. Drop and 400 anything else.
- Detection notes: a push with `{Key: "junk/foo", Blob: ...}` should now 400.

**M3. SSE fan-out: all subscribers see every memory row, regardless of UserID.**

- Where: `internal/serve/server.go:185-202` (`fanoutMemory`), `internal/serve/server.go:251-327` (`handleSSE`).
- What: `requireAuth` accepts any device bearer (or legacy single bearer); the SSE handler subscribes and receives every memory push the server gets, with no per-user filter. Since the blobs are AEAD-sealed and a wrong-KEK Open errors out at the client (logged but discarded — see `sync.go:752-757`), the server-side leak is only the key shape (which already includes scope path segments — those aren't ciphertext).
- Why it matters: Scope path is operational metadata, but `memory/<encoded-scope>/learnings/<id>` key shapes leak between users on a shared-upstream deployment (`team`, `multi_user: true` per round-2/10) once that ships. Today's single-user deployment is unaffected.
- Fix sketch: when per-user partitioning ships, filter SSE subscriptions by `dev.UserID` and the rows' user_id prefix (which means moving to user-scoped key prefixes, e.g. `u/<uid>/memory/...`).
- Detection notes: future test — two users on the same upstream should not see each other's memory rows.

**M4. Sealer is sampled at the top of `EnqueueEvents` and used through the loop body — a concurrent ClearSealer mid-loop is not observed.**

- Where: `internal/agent/sync.go:194-214` (`EnqueueEvents`).
- What: `sl := s.getSealer()` is read once; subsequent events in the same batch all seal with the same sealer reference even if `ClearSealer` is called concurrently. Direction of risk is fine (events still get sealed, not leaked) but it means a `logout` that races with an in-flight `IngestBatch` will commit sealed events to the outbox under the old KEK, even though the user expected the lock to take effect immediately.
- Why it matters: Low. The outbox row was going to be sealed under the same KEK that just got cleared; the drain loop will park on the nil sealer and never push it until the next unlock with the matching KEK. So the row sits sealed in the local outbox under the prior KEK, drained later when the user logs back in with the same password. If the user changes password (recovery flow), drain will continue pushing old-KEK blobs that the server stores opaquely — no decrypt happens server-side anyway, so still fine.
- Fix sketch: re-sample sealer per event inside the loop; or accept this as designed and document. Note that the design comment in `seal_handlers.go` already acknowledges enqueue happens regardless of sealer state to avoid losing capture.
- Detection notes: a test that races SetSealer → IngestBatch → ClearSealer and asserts no plaintext leaks would catch any regression.

**M5. Pair-invite ephemeral token revocation is best-effort with a 5-second timeout.**

- Where: `internal/cli/pair.go:103-119` (the deferred revoke block).
- What: `runPairInvite` logs in with a "pair-invite-ephemeral" device name to fetch the KEK, then `defer`s a POST to /v2/auth/logout to revoke that token. If the network call fails (transient upstream, the user hit ^C during publish, etc.) the token survives until the device is manually purged. The 5-sec timeout is short — common timeout failures will leave the token live for the full 1-year lifetime.
- Why it matters: Ephemeral tokens that survive accumulate. Each one is a long-lived bearer credential for the user's account. Followup-B specifically called this out and the current implementation revokes, but the failure mode (best-effort) means the followup goal isn't fully met under realistic networks.
- Fix sketch: mint the ephemeral token with a TTL field (currently devices have `created_at`/`last_seen_at` but no `expires_at`); the server enforces. Or batch-revoke older "pair-invite-ephemeral" devices on the next successful login. Or attach a one-shot "use_count" so the token dies after the publish call.
- Detection notes: add a test that simulates a publish + canceled context and asserts the ephemeral device was deleted (currently it'd survive).

**M6. Login error responses are timing-attack adjacent — unknown-email path skips Argon2id verifier compare, known-email path runs it.**

- Where: `internal/serve/auth_handlers.go:275-298` (`handleLogin`).
- What: For an unknown email, `GetByEmail` returns `ErrNotFound` and the handler returns 401 immediately. For a known email with a wrong verifier, the handler runs `subtle.ConstantTimeCompare` over the (already-hashed-by-the-client) verifier hash. The compare itself is cheap (constant-time, but a 32-byte memcmp), so the timing gap between "no such email" and "wrong password" is dominated by the DB query and is in the milliseconds. The client-side Argon2id work is the same in both cases (the client computed it before sending). So the server-observable timing difference is small but not zero.
- Why it matters: This is the documented login-challenge constant-shape mitigation (info-leak hardening on /login-challenge), but /login itself returns 401 without going through the verifier work on the unknown-email branch. A careful attacker can probe /login (skipping /login-challenge) and time-distinguish unknown-email vs. known-email-wrong-password.
- Fix sketch: on unknown email, do a dummy `subtle.ConstantTimeCompare(req.VerifierHash, decoy)` against a per-process random decoy. The wall-clock cost is negligible vs the DB query, but it removes the cleanly-distinct early-return.
- Detection notes: not really testable without precise timing — document as a hardening pass.

### Low

**L1. Endpoint file at 0600 includes the daemon bearer secret in cleartext.**

- Where: `internal/agent/endpoint.go:40-57` (`WriteEndpoint`).
- What: `~/.resleeve/endpoint` contains `URL\nSECRET\n` at mode 0600. The secret is hex(32-byte random) so it's safe in cleartext from a brute-force standpoint, but any process running as the same user can read it and impersonate the local daemon to the CLI. Matches the documented threat model.
- Why it matters: Already accepted. Documenting.
- Fix sketch: Out of scope for this review.

**L2. 4xx error envelope is `{"error": "string"}` and the client matches on body fragments (e.g. ErrNoUpstream's "no upstream configured").**

- Where: `internal/serve/server.go:369-371` (`writeError`), and the F-followup honest-concern noted in learnings.
- What: All 4xx responses ship `{"error": <freeform-text>}`. Clients that want to distinguish error codes have to either match on HTTP status or on the body substring. The cross-boundary sentinel-error case (ErrNoUpstream) does the latter; the daemon's 409 body text is the actual contract.
- Why it matters: Brittle. Already on the punch list as "track as future hardening" per the learning.
- Fix sketch: structured `{"error": {"code": "no_upstream", "message": "..."}}` envelope from both serve and daemon 4xx paths; clients match on `.code`.
- Detection notes: an integration test that asserts the daemon's 409 has a stable `code` field.

**L3. PID file at 0600 — `runMigrateKey` and `runDoctorMigrateKey` refuse if daemon alive via `syscall.Signal(0)` probe.**

- Where: `internal/cli/lifecycle.go:635-657` (`daemonAlive`).
- What: PID-file based liveness with signal-0 probe. If the PID has been recycled to a different process owned by the same user, `daemonAlive` returns true (false positive — refuses migrate-key for a non-daemon process). Cosmetic UX issue, not a security one — failure-open would be much worse here.
- Why it matters: None really; documenting as a known caveat for `migrate-key` UX.
- Fix sketch: stat /proc/PID/cmdline (linux) or `ps -p PID -o comm=` for fingerprint match. Not worth it.

**L4. `resleeve serve` defaults to `127.0.0.1:7860` but readily accepts any addr — Host header isn't checked, no CORS.**

- Where: `internal/cli/serve.go:31` (default addr), `internal/serve/server.go:107-141` (route registration; no Host or Origin check).
- What: Default bind is loopback, so a casual deployment is fine. Operator can override to a public addr without warning. Once on a public addr, there's no Host header validation and no CORS — a malicious page can POST cross-origin to a known upstream URL using credentials, but the bearer token sits in app-specific storage (OS keychain), so browser-side CSRF isn't trivially exploitable. Still, no `Access-Control-Allow-Origin` means browser fetches are rejected by the browser, which is the right default.
- Why it matters: Browser-side attack vector is minimal today. Operator-facing nudge: print a warning when `--addr` isn't loopback.
- Fix sketch: warn at startup when addr doesn't bind loopback ("operator: confirm you've fronted this with TLS + auth").

**L5. `pair accept` lowercases + uppercases pair code via `strings.ToUpper(strings.TrimSpace(...))` but doesn't canonicalize the dashes.**

- Where: `internal/cli/pair.go:220-228` (`runPairAccept`).
- What: `formatPairCode` inserts `-` every 4 chars. `runPairAccept` accepts whatever the user types — if the user pastes the code without dashes, the verifier hash won't match (since the wrap was over the dashed form). Bad UX, not a security issue.
- Fix sketch: strip `-` before computing verifier and wrap canonicalize to no-dash form throughout.

### Informational

**I1. Argon2id defaults (64 MiB / 3 iters / 1) are below OWASP 2024 recommendation but still adequate.**

- Where: `internal/auth/argon2.go:21-27` (`DefaultArgon2idParams`).
- What: The comment says "OWASP-leaning"; current OWASP cheat-sheet recommends 19 MiB minimum and lists 64 MiB + 3 iters as a common interactive default. Pretty defensible. The decision lands in design as "deployment-configurable" — at the binary level it's a constant.
- Note: round-2/10 §"Encryption model" §"Argon2id parameters" calls for "OWASP recommendations + deployment-configurable" — the second half isn't wired (no flag, no config-file knob, no env var). Document or add `RESLEEVE_ARGON2_MEMORY_KIB` env override.

**I2. AES-GCM nonce is random per Seal call. With 2^32 messages per key the collision probability is ~2^-32.**

- Where: `internal/auth/envelope.go:57-68` (`Seal`).
- What: 12-byte random nonce. NIST SP 800-38D §8.3 says "random nonces are OK up to ~2^32 messages per key" before collision probability exceeds 2^-32. The KEK is per-user and rotated on password reset; outbox volumes are O(events per user lifetime). Comfortable margin. Worth noting that envelope version byte is fixed at 0x01 and nonces aren't tied to the version in the AAD — a future v2 envelope changing nonce size would need a careful migration.

**I3. PRNG audit clean.**

- Where: every `rand.Read` call I traced (`internal/auth/argon2.go:37`, `internal/auth/envelope.go:59`, `internal/auth/kek.go:19,77`, `internal/auth/user.go:184`, `internal/auth/recovery.go:26`, `internal/serve/server.go:87`, `internal/serve/auth_handlers.go:537,544`, `internal/agent/endpoint.go:111`, `internal/cli/pair.go:126,353`, `internal/cli/serve.go:142`) uses `crypto/rand`. No `math/rand` in any non-test path.

**I4. Constant-time compares used for both the daemon bearer and the verifier hash.**

- Where: `internal/agent/handlers.go:219` (daemon bearer), `internal/serve/auth_handlers.go:295` (login verifier), `internal/serve/auth_handlers.go:442` (pair-claim verifier), `internal/serve/auth_handlers.go:506` (legacy single-bearer fallback). All use `subtle.ConstantTimeCompare`. Device-token lookup in `deviceFromBearer` is a DB query keyed by token — that's a `SELECT ... WHERE device_token = ?` which isn't constant-time at the DB layer, but the post-DB compare path is. Acceptable.

**I5. HMAC for synthetic salt uses a sensible domain-separated label.**

- Where: `internal/serve/auth_handlers.go:265-271`. `HMAC(key, "login-challenge-salt-v1\x00"||email)` — `\x00` separator + version label, so future label drift won't collide. Truncated to 16 bytes (matches `auth.NewSalt()` shape).

**I6. Sealer set/clear is mutex-guarded; the daemon loops re-check on every tick.**

- Where: `internal/agent/sync.go:67-128`, `internal/agent/sync.go:309-324` (`parked`). The race-condition fan-out in M4 above is the one nuance.

**I7. Pair-code format chunks 12 base32 chars (60 bits of entropy) — matches the design comment and is enough for the 5-min TTL plus Argon2id cost.**

- Where: `internal/cli/pair.go:122-130, 368-382`. Note: the comment on line 124 says "Brief says '60s one-time code' — TTL is server-controlled; we ship 5min default." Tightening to 60s would meaningfully reduce H2's brute-force window.

**I8. Hook stdin parser is JSON-only with field whitelist via the `hookEnvelope` struct.**

- Where: `internal/adapter/claude/fromnative.go:25-43, 60-67`. Malformed JSON → decode error returned, no panic. Missing `session_id` → typed error. Untrusted hook payload contents flow into event content `mustJSON` which itself just re-encodes — no shell-out, no unbounded slice growth, no `[]byte(string)` of the raw payload into anything sensitive. The raw payload IS preserved in `vendor.native_payload` to be ingested into the local DB, but that's local-only and the daemon trusts its hook input.

**I9. MCP tool args are decoded into `decodeArgs` typed helper — no shell-out, no path joining of user input outside the daemon.**

- Where: `internal/mcp/tools.go` (every handler uses `a.string()` / `a.bool()`). The `path` argument flows into `c.PutScope` etc. — those are validated server-side by the memory store. The MCP server itself is a stdio transport; no remote attack surface.

**I10. `syscall.Exec` in `resume` execs a CLI looked up via `exec.LookPath` from a known list (`claude`, `opencode`).**

- Where: `internal/cli/resume.go:199-210`. The cmd name comes from the adapter selection, not user input. Argv flows from the adapter's `NativeResumeCmd` builder. Acceptable — same trust model as any CLI wrapper.

**I11. Local backend Put writes 0600 atomically via tempfile + rename.**

- Where: `internal/sync/local/local.go:50-68`. Good practice. Endpoint file also uses atomic write (`WriteEndpoint`). seal.key (in legacy `runAgent` path) is read-only here — never written by the daemon today.

**I12. Loopback enforcement on `/v1/seal/unlock` is belt-and-suspenders.**

- Where: `internal/agent/seal_handlers.go:30-53`. Even on a daemon mis-bound to 0.0.0.0 (the operator would have to override `--addr` explicitly), the KEK upload route rejects non-loopback peers. `/v1/seal/lock` and `/v1/seal/status` also wear the loopback wrapper. Good defense-in-depth.

**I13. Outbox dequeue uses fixed-width lex-comparable timestamps.**

- Where: `internal/storage/sql/sqlite/sync.go:20-37`. Closed off the followup-G flake. Not a security finding; it's a correctness one that the team already landed.

**I14. SSE handler doesn't authenticate the subscriber's `since=<cursor>` value beyond passing it to `backend.List`, which validates the prefix.**

- Where: `internal/serve/server.go:251-327`. A malicious bearer can probe history with any cursor — but they already authenticated, so they have full read access anyway. Fine.

**I15. login-challenge ships `params` and `verifier_salt` for ANY email (real or unknown). The synthetic-salt path uses `auth.DefaultArgon2idParams()` for unknown emails, which means a server with a non-default per-account param set would emit telltale differences for unknown vs. real accounts.**

- Where: `internal/serve/auth_handlers.go:241-256`. Today every register uses defaults, so this is masked. If H1's mitigation lets operators raise the floor account-by-account (or a future feature lets users pick stronger params), unknown-email emits weaker params than the real ones — re-opens the enumeration oracle. Track alongside H1.

---

## Accepted-risk inventory

These are KNOWN limitations on the punch list. Documenting why each is accepted today.

1. **Placeholder `~/.resleeve/seal.key` (round 4 legacy mode).** 32 random bytes at mode 0600, NOT auto-generated by the agent in round 5 (`internal/cli/agent.go:22-67` — only loaded if user explicitly passes `--seal-key=PATH`). The round-5 model derives the KEK from the master password at login time. The file path lingers as a back-compat shim for users migrating from round 4. Threat model summary: any same-user process can read it (and the OS keychain entry, and the endpoint file, and daemon RAM) — all comparable. Mitigation = `resleeve migrate-key` + `resleeve doctor --migrate-key`.

2. **File-keychain fallback (`internal/cli/keychain.go:139-186`).** On platforms without a real OS keychain (or on Linux without a running Secret Service), tokens land at `~/.resleeve/devices/<host>/<account>.json` at mode 0600. Atomic write via tempfile + rename. Same threat model as the seal.key item.

3. **ErrNoUpstream string-body coupling** (followup F honest-concern, my L2 above). The daemon emits 409 + plain text "no upstream configured"; client uses `strings.Contains` before wrapping. Body-fragment match. Won't break unless the daemon text changes. Tracked.

4. **opencode adapter is a stub** (`internal/adapter/opencode/adapter.go`). Capture isn't real — no event surface to attack. Not a risk surface today.

5. **No rate limiting** on any HTTP endpoint (daemon or serve). The daemon is loopback so it doesn't matter. The serve side relies on the operator's reverse proxy. Document, don't fix in the binary today.

6. **KEK in daemon RAM** (H4 above, but listed here too because it's partly accepted). Pure-Go can't lock pages against swap without CGO. Best the daemon can do without dependencies is overwrite-on-lock; doing nothing today is documented.

7. **Login-challenge HMAC key per-process rotation** (M1 above). Documented as "no client-visible effect"; the cross-restart oracle is the small caveat.

8. **Sealer racing with batch enqueue** (M4 above). Designed: capture continues regardless of sealer state to avoid losing events on lock/unlock transitions. Confidentiality holds; clear-during-batch races commit under the prior KEK.

9. **No per-user backend partitioning yet** (M3 above). Round-2/10 anticipates this for v2+ team mode. Today's single-user-per-deployment shape is unaffected.

10. **Pair-code 60-bit entropy with 5-min default TTL** (H2 above). Accepted as a UX tradeoff; the published intent was 60s, the shipped default is 5 min.

---

## Out-of-scope (not reviewed)

- Browser/web UI — none exists yet.
- v1/serve TLS termination — operator's job via reverse proxy.
- Third-party deps' internals (`golang.org/x/crypto/argon2`, `zalando/go-keyring`, `modernc.org/sqlite`) — only their wire/API surface was considered.
- Multi-orchestrator federated identity / SCION mode — not implemented in this checkout.
- Backup/restore flows for the upstream's storage backend — local-disk only, no offsite plans.
- Storage-side audit logging — none today; out of scope per the design.

---

## Cross-cutting observations

### Crypto envelope layout

`internal/auth/envelope.go:14-27`. Wire format: `<0x01:1B><nonce:12B><ciphertext+tag:N+16B>`. The version byte is a forward-compat hook for cipher rotation; today only 0x01 is accepted. Notes:

- Nonce is generated fresh per Seal (`crypto/rand.Read`). 12-byte (96-bit) random nonces are the AES-GCM standard and safe up to ~2^32 messages per key (NIST SP 800-38D). The KEK is per-user and rotates on `ResetPassword`; in practice a single KEK will see far fewer than 2^32 Seal calls.
- AAD is unused (`s.aead.Seal(out, nonce, plaintext, nil)`). The version byte and nonce ride OUTSIDE the AEAD authenticator. Tampering with the version byte will cause `Open` to fail the explicit version-byte check (line 78-80), not the AEAD tag. Tampering with the nonce will be detected by the tag (since AES-GCM authenticates the nonce → ciphertext binding). Tampering with the ciphertext or tag is caught by the tag. Net: tamper-evident across the whole envelope, but version-byte tampering surfaces as a different error than ciphertext tampering — minor information leak in error messages, not exploitable.
- One possible improvement: include the version byte as AAD on Seal/Open so all error paths funnel through the same AEAD tag failure. Cost: breaks compatibility with existing 0x01-prefixed envelopes; needs a 0x02 rollout. Don't pull that thread now.

### Per-domain salt independence

`internal/auth/argon2.go:52-72` (Verifier), `internal/auth/kek.go:28-65` (WrappedKEK). Verifier hash and KEK-wrap key derive from **independent** salts. This means stealing the server-stored `password_verifier_*` doesn't let the attacker pre-compute the KEK-wrap key — they'd still have to grind Argon2id over the wrap salt separately. Matches round-2/10 design verbatim ("Salts are independent so the server-stored verifier cannot help an attacker unwrap the KEK"). Implementation: each `NewVerifier` and `Wrap` calls `NewSalt()` fresh.

### Dual-wrap recovery

`internal/auth/user.go:42-96` (Signup), `internal/auth/user.go:109-160` (Recover/ResetPassword). The KEK is wrapped twice — once under the password, once under the recovery key. Each wrap has its own salt + nonce. On password reset, ResetPassword re-wraps the KEK under the new password AND rotates the recovery key + recovery wrap. The new recovery key is returned for one-time display. The OLD recovery key is no longer accepted (Verifier hash changes). Implementation matches the spec.

### Constant-time guarantees vs DB-level lookups

For verifier compares the implementation is correct (`subtle.ConstantTimeCompare` everywhere). For device-token lookups (`/v2/auth/*` middleware), the path is `SELECT ... WHERE device_token = ?` which the DB compares non-constant-time. An attacker probing the bearer path can in principle time-distinguish prefix matches in the device_token column. Mitigations: tokens are 64 hex chars = 256 bits of entropy, so the brute-force search space is unrealistically large for any practical timing attack; the index lookup is constant-cost dominated by the index seek, not the column compare. Acceptable for v1.

### Endpoint discovery and the daemon's effective trust boundary

`internal/agent/endpoint.go:17-35` (`LoadEndpoint`). The CLI finds the daemon via `$RESLEEVE_AGENT_ENDPOINT` (env override) or `~/.resleeve/endpoint` (file at 0600 with URL + secret). Any process with the same UID can read either path and impersonate the CLI to the daemon. The daemon's `/v1/seal/unlock` route is the high-value target — a hostile local process that obtains the endpoint secret could POST a KEK and unlock-then-export. The `requireLoopback` middleware doesn't help here (the attacker IS on loopback). Practically: same-user code execution = game over for confidentiality. Matches the documented threat model; raising the local boundary requires OS-level isolation (Unix socket with peer credentials, fs-level enclaves, macOS Keychain access prompts). Document as accepted.

### Sealer install / drain race (followup G adjacent)

The brief flagged "sealer install / clear racing with drain/pull (followup G adjacent — they fixed timestamp lex, but check the sealer mutex too)." Walked the path:

- `SetSealer` / `ClearSealer` / `getSealer` are mutex-guarded (`sync.RWMutex`).
- `drainLoop` / `pullLoop` / `sseLoop` each call `parked()` on every tick. `parked()` re-reads `getSealer()`.
- `drainOnce` calls `getSealer()` once at the top via `s.getSealer()` inside `EnqueueEvents` — but `drainOnce` itself uses an unsealed local outbox row (the SEAL happened at enqueue time, not drain time). So drain operates on already-sealed bytes; no race window where drain ships plaintext.
- `pullKind` / `ingestPulled` / `ingestMemoryRow` call `getSealer()` at use time (not cached). If sealer is cleared mid-pull, the next row's ingest sees nil sealer and treats the blob as plaintext — which then fails to unmarshal as a Session/Event JSON. Failure-closed: error logged, cursor not advanced for that row, next pull retries. No plaintext leak; minor pull-loop noise.
- `sseLoop`'s `runSSE` also calls `getSealer()` per row. Same failure-closed semantics.

Net: sealer races are well-contained. No bug found.

### Cross-process state and TOCTOU on register/login

`/v2/auth/register`: `GetByEmail` → if not found, then `Create`. Two near-simultaneous register calls for the same new email race between the `GetByEmail` and `Create`. The `server_users` table has a UNIQUE constraint on email (migration 0005), so the second `Create` fails with constraint error → caller sees `"create user: "+err.Error()`. Acceptable; the loser sees a confusing error message but doesn't get a duplicate row. Minor UX nit: the error envelope reveals "constraint" which is a small impl detail leak. Low.

### outbox dequeue + ack racing with concurrent enqueue

`internal/storage/sql/sqlite/sync.go:51-101`. DequeueOutbox + AckOutbox is a SELECT-then-DELETE pattern. If a concurrent EnqueueOutbox INSERTs while DequeueOutbox is mid-SELECT, the new row may or may not be in the returned batch depending on SQLite's snapshot semantics under journal_mode=WAL — but `AckOutbox` deletes by explicit seq list (not by `MAX(seq) <= ?`), so it can't accidentally consume the new row. Idempotency is preserved by `INSERT OR IGNORE` on (kind, key). Net: behaves correctly under concurrent enqueue/drain.

### Subprocess invocation summary

All `os/exec` and `syscall.Exec` calls I found:

- `internal/adapter/claude/adapter.go:26-31` — `exec.LookPath("claude")` + `claude --version` for detection. Constant argv.
- `internal/adapter/opencode/adapter.go:37` — same, stub.
- `internal/cli/resume.go:199-210` — `exec.LookPath(cmd)` + `syscall.Exec` with argv from the adapter's NativeResumeCmd. Adapter-controlled, not user-controlled.
- `internal/cli/lifecycle.go:297, 605` — `exec.LookPath("claude")` (doctor); `exec.Command(self, cmdArgs...)` for daemon spawn (argv is constructed from known flags, not user strings).
- `internal/cli/plan.go:147-156` — `exec.Command(editor, tmpPath)` where `editor = $EDITOR || "vi"`. The user's own env var. Acceptable (this is the user's editor running over their own data).

No path where untrusted JSON or pulled-blob content becomes an argv element.

### Pairing wire shape vs. round-2/10 §"Pairing flow"

Round-2/10 doesn't have a pair flow section per se; round-4/02 does (referenced in `pair.go:18-43`). The implemented flow ships the verifier salt as `deterministicSalt(codeID, "pair-verifier")` — public-but-bound to the code_id. The comment correctly notes this is NOT security-critical since the pair code is the actual secret. The wrap salt is random and ships in the PairClaimResp after the verifier check. The split is sound.

### What I tried to break and couldn't

- **Replay /v2/auth/login with the same verifier_hash**: succeeds (mints a new device token each time). Expected; matches round-2/10 "tokens carry scope claims" and TTL semantics. The mitigation is server-side device-token issuance audit, not a per-login challenge nonce.
- **Force `requireDevice` to accept an expired pair code as a device**: no — `handlePairClaim` checks `pc.ExpiresAt.Before(now)` and returns 410 Gone.
- **Submit a malformed envelope to `Open` that elides the AEAD tag**: returns `"envelope: ciphertext too short"` (length check at line 75) — no panic, no leak.
- **Push a non-prefixed key like `../etc/passwd`**: `ValidateKey` rejects `..` segments (line 69-71). Leading-slash rejected (line 62). No filesystem escape.
- **`scope_path` in memory keys** — encoded via `strings.ReplaceAll(p, "/", ":")` (`internal/agent/sync.go:594-596`). The wire-side key segment can't include `/`, so backend can't be tricked into traversal via the scope path.

## Notes on methodology and confidence

I walked the security-relevant packages end-to-end (`internal/auth`, `internal/serve`, `internal/agent/seal_handlers.go`, `internal/agent/sync.go`, `internal/agent/ingest.go`, `internal/agent/handlers.go`, `internal/agent/daemon.go`, `internal/agent/endpoint.go`, `internal/cli/auth.go`, `internal/cli/pair.go`, `internal/cli/migrate_key.go`, `internal/cli/keychain.go`, `internal/cli/serve.go`, `internal/cli/agent.go`, `internal/cli/lifecycle.go`, `internal/cli/resume.go`, `internal/mcp/server.go`, `internal/mcp/tools.go`, `internal/sync/backend.go`, `internal/sync/local/local.go`, `internal/storage/sql/sqlite/sync.go`, `internal/storage/sql/sqlite/identity.go`, `internal/adapter/claude/fromnative.go`, `internal/adapter/claude/adapter.go`). Cross-checked against `docs/design/round-2/10-auth-subsystem.md`.

Confidence: high on the crypto primitives (the auth package is small and tightly scoped). High on the wire shape (no plaintext leaks observed). Medium on the identity flows (the H1/H2/H3 wrinkles are real but limited blast radius given the single-user-per-deployment posture). Lower on M4/M5 — those are subtle racer-y bits where a focused property test or fuzzer would shake out more.

`go test ./...` passes clean at HEAD.
