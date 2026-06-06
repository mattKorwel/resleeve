# Round 11 — Security "quick win" hardening (server/auth side)

Source of truth: `docs/REVIEW_SECURITY.md` (review @ cc04113). This round
lands three server/auth-side fixes that are independent of the CLI
adapters. Scope was constrained to `internal/serve`, `internal/auth`,
`internal/agent` (+ their tests).

Note: the prior commit `87a8427` already landed a first pass at several
review findings (M1/M2/M4/M5/M6, plus H1 and H3, and an in-memory variant
of H2). This round closes the remaining gap — **H4 (KEK clearing)** — and
hardens / locks in the H2 and H1 behaviors with explicit tests and the
parity invariant called out in I15.

---

## 1. Pair-claim brute-force counter (REVIEW_SECURITY → H2)

**Threat closed.** A 60-bit base32 pair code is brute-forceable inside its
5-minute TTL if the `code_id` leaks (logged, screen-shared, pasted into a
compromised channel). Before this fix the only throttle was the TTL plus
the per-attempt Argon2id cost — an attacker with a beefy box could grind
many verifier guesses per code. Successful guess ⇒ device token + KEK
unwrap from the published envelope.

**What it does.** `/v2/auth/pair/claim` now keeps a per-`code_id` failed-
verifier counter (`Server.pairAttempts`, guarded by `pairAttemptsMu`).
After `pairClaimMaxAttempts` (= 5) consecutive verifier failures the code
row is hard-deleted (`pairClaimLockout`) and the counter cleared. The
verifier compare stays `subtle.ConstantTimeCompare`. Counter is dropped on
successful claim and on the lockout-delete path.

**Indistinguishability.** Lockout, natural TTL expiry, and never-existed
all return the SAME wire response (`writePairClaimGone` → 404 "code not
found or expired"), so a brute-forcer cannot tell whether their guessing
tripped the lockout or the inviter's TTL simply elapsed.

**Where.** `internal/serve/auth_handlers.go` (`handlePairClaim`,
`pairClaimRecordFailure`, `pairClaimLockout`, `pairClaimClearAttempts`,
`writePairClaimGone`); `internal/serve/server.go`
(`pairAttempts`/`pairAttemptsMu` fields, `pairClaimMaxAttempts` const).

**Persistence caveat (deliberate deviation).** The brief suggested
persisting the counter on the `pairing_codes` row. That would require
touching `internal/storage`, which is outside this round's allowed file
set. The counter is therefore in-memory: it is lost on `serve` restart,
but pair codes are 5-minute-TTL anyway and the `Get`/`SweepExpired` paths
reconcile any drift, so a restart at worst resets an attacker's budget on
a code that is about to expire. Persisting it on the row is tracked as a
follow-up for whoever owns the storage layer.

**Tests** (`internal/serve/pair_test.go`):
- `TestPairClaim_LocksAfterFiveFailedAttempts` — locks at N; post-lockout
  even the correct hash returns 404.
- `TestPairClaim_CorrectClaimBeforeLockoutSucceeds` — a correct claim
  after a couple of typos (below the threshold) still succeeds.
- `TestPairClaim_LockoutIsIndistinguishableFromExpiry` — status + body
  byte-identical between lockout and TTL-expiry.
- `TestPairClaim_SuccessfulClaimDoesNotPersistAttemptCount` — counter and
  map are empty after a successful claim.

---

## 2. Argon2 param floor + unknown-email param parity (REVIEW_SECURITY → H1, I15)

**Threat closed.** (a) A malicious client could register with cracking-
cheap Argon2id params (e.g. 8 KiB / 1 iter); a later verifier-hash leak
would then be offline-crackable in seconds. (b) Returning different
Argon2id params for unknown vs. real emails on `/v2/auth/login-challenge`
would turn the endpoint into an account-existence oracle (I15).

**What it does.**
- (a) Floor + ceiling: `validateArgon2Params` (called from
  `validateRegister`) rejects params below the OWASP-leaning floor
  (`argon2MinMemoryKiB` = 64 MiB == `DefaultArgon2idParams()`,
  `argon2MinTimeIters` = 2, parallelism ≥ 1) and above a DoS ceiling.
- (b) Parity: the unknown-email branch of `handleLoginChallenge` returns
  `auth.DefaultArgon2idParams()` — byte-identical to what a real,
  default-registered account emits. Because register floors every account
  at exactly the default, a real default account and the decoy are
  indistinguishable. The salt derivation stays
  `HMAC(loginChallengeKey, "login-challenge-salt-v1\x00" || email)`
  truncated to 16 bytes (verified unchanged).

**Residual.** The memory ceiling permits a non-CLI client to register
ABOVE the floor. If that ever happens, such an account would emit
params different from the default decoy (the I15 edge). The shipped CLI
always registers at the default, so parity holds in practice; closing the
above-floor edge would mean either pinning register to exactly the default
or making the decoy mirror per-account params (which itself leaks). Left
as documented residual per I15 ("track alongside H1").

**Where.** `internal/serve/auth_handlers.go` (`validateArgon2Params`,
`validateRegister`, `handleLoginChallenge` parity comment).

**Tests** (`internal/serve/auth_handlers_test.go`):
- `TestRegister_RejectsWeakParams` — below-floor params ⇒ 400.
- `TestRegister_RejectsDoSParams` — above-ceiling params ⇒ 400.
- `TestRegister_AcceptsRoundTwoTenBaseline` — default baseline accepted.
- `TestLoginChallenge_UnknownEmailParamParity` (new) — unknown-email
  params == known-account params == default.
- `TestLoginChallenge_ConstantShapeForUnknownEmail` — overall constant
  shape, deterministic per-email synthetic salt, no 404 oracle.

---

## 3. KEK clearing from RAM (REVIEW_SECURITY → H4, accepted-risk #6)

**Threat closed.** The derived KEK / AES key schedule lingered in daemon
memory after `resleeve logout`: `ClearSealer` only did `s.sealer = nil`,
which drops the interface reference but leaves the key bytes on the heap
until GC reuses the page. An attacker reading process memory (core dump,
ptrace, `/proc/self/mem`) after a logout could still recover the KEK — so
"lock" did not actually revoke access.

**What it does.** Adds a best-effort zeroize path:
- `internal/auth/kek.go`: package-level `Wipe([]byte)` (zeros a slice) and
  `(*KEK).Wipe()` (zeros the array in place).
- `internal/auth/envelope.go`: `AESGCMSealer` now retains an owned copy of
  the raw 32-byte key; `Wipe()` zeros it, drops the AEAD, and makes
  subsequent `Seal`/`Open` fail closed instead of operating on stale
  material or panicking. A new `Wipeable` interface advertises this.
  `NewAESGCMSealer` copies the caller's key so the caller can scrub its
  own buffer immediately.
- `internal/agent/sync.go`: `ClearSealer` (logout / future idle-lock) and
  `SetSealer` (re-unlock / rotation) type-assert to `auth.Wipeable` and
  `Wipe()` the old sealer while holding `sealerMu`.
- `internal/agent/seal_handlers.go`: `handleSealUnlock` calls
  `auth.Wipe(req.KEK)` right after constructing the sealer to scrub the
  request-body copy.

**Honest limits (documented in code).** Pure Go cannot reach the expanded
AES key schedule inside `crypto/aes`, cannot `mlock` pages against swap,
and cannot scrub copies the runtime/compiler may have made by value. This
shortens the recovery window; it does not eliminate swap- or core-dump-
based recovery. Matches the brief's "best-effort RAM clear" framing and
accepted-risk #6.

**Tests:**
- `internal/auth/envelope_test.go`:
  `TestAESGCMSealer_WipeZerosKeyAndDisablesSeal` (key zeroed; Seal/Open
  fail closed; Wipe idempotent), `TestAESGCMSealer_CopiesKeyOnConstruct`
  (caller may scrub its KEK), `TestWipe_ZerosSlice`, `TestKEK_Wipe`.
- `internal/agent/seal_handlers_test.go`:
  `TestClearSealer_WipesKEKAndBlocksSealUntilReUnlock` (after logout the
  held sealer is scrubbed and seals fail; re-unlock restores function),
  `TestSetSealer_WipesReplacedSealer`.

---

## Build/test status

`gofmt -l` clean on touched files, `go vet ./...` clean, `go build ./...`
and `go test ./...` green. Only `internal/serve`, `internal/auth`,
`internal/agent` (+ tests) and this doc were modified.
