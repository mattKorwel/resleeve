# Code quality review — resleeve @ cc04113

Date: 2026-06-04. Scope: `internal/*`. Read-only.

Baseline: `go test ./...` is green on every package. Coverage by
package: `event` 100%, `auth` 80%, `serve` 75%, `sync/local` 74%,
`adapter/claude` 70%, `mcp` 60%, `sqlite` 51%, `agent` 36%, `memory`
28%, `cli` 6%, `adapter/common` 0%, `sync` 0%, `agent/agenttest` 0%.

## Summary

- **A pre-pivot `auth.User` / client-side `users` table is fully dead
  code.** The whole client `userStore` (storage), `UpdateAuth`,
  `auth.Login`, `auth.Recover`, `auth.ResetPassword`, and migration
  0002 are unreachable in production. ~150 lines + one SQL migration
  could be retired without behavioural impact.
- **Three CLI files keep imports alive with placeholder identifiers**
  (`var _ = time.Now`, `var _ = errors.New`, `_ rsql.SessionStatusActive`)
  and one storage file does the same for `auth.Sealer`. `urlWithQuery`
  in `cli/auth.go` is itself unused. Each is a low-cost local cleanup.
- **Three CLI flag-parsing dialects coexist** (`flag.FlagSet`,
  hand-rolled switch-prefix, hand-rolled with mixed positional). New
  verbs pick at random; help text inconsistency follows. Worth a
  decision before the next CLI surface drops.
- **Big god-functions in `cli/resume.go` (186 lines), `lifecycle.go::runDoctor`
  (128), `cli/pair.go::runPairInvite` (128), `cli/migrate_key.go::runMigrateKey`
  (116).** Each mixes parsing, network I/O, persistence, and stdout
  formatting in one body; not blocking but a refactor target.
- **CLI surface ships at 5.9% coverage.** Several recently-shipped
  flows (`runRegister`, `runLogin`, `runLogout`, `runPair*`,
  `runMigrateKey`, every `scope`/`plan`/`learning` verb, the entire
  `runResume` body) have no unit tests outside the helper they call.
  `internal/adapter/common.SynthesizePrime` — the cross-CLI prime
  synthesizer that ships in two adapters — is uncovered.

---

## Findings (priority order)

### High — likely to bite

#### Q1 — client-side `auth.User` plus `users` SQL table are dead code
- **What.** Production callers never go through `auth.User` /
  `auth.Login` / `auth.Recover` / `auth.ResetPassword` or the
  `UserStore` / `userStore` storage abstraction. The whole client-side
  user persistence layer is shadowed by the round-4 split into
  server-side `server_users` + `devices` + `pairings`. The remaining
  in-production usage of `auth.Signup` is just to produce a
  `SignupResult` so the CLI can serialise the verifier/wrap material
  over the wire to `serve` (see `cli/auth.go:70`).
- **Where.**
  - `internal/auth/user.go:42-160` — `Signup` reachable, `Login`,
    `Recover`, `ResetPassword`, `ErrInvalidCredentials` unused outside
    `auth/auth_test.go`.
  - `internal/auth/recovery.go:24-59` — `NewRecoveryKey` / `RecoveryKey.Bytes`
    only reached via `Signup`/`ResetPassword`.
  - `internal/storage/sql/store.go:103-110` — `UserStore` interface
    + `Store.Users()` accessor (line 270).
  - `internal/storage/sql/sqlite/users.go` (entire file, 93 lines).
  - `internal/storage/sql/sqlite/sqlite.go:29,56,79` — `userStore` field,
    init, accessor.
  - `internal/storage/sql/sqlite/migrations/0002_users.up.sql` — the
    table itself.
  - `grep -rn "Users()"` finds zero non-self callers.
- **Why it matters.** New contributors have to reason about it
  ("am I supposed to use this for client-side login?"), and parallel
  agents are likely to use it as scaffolding for the next polish item.
  The duplicated server / client crypto-wrap struct definitions also
  blur the boundary between local-only state and server-side identity.
- **Fix sketch.** Either (a) delete `auth/user.go`'s `Login` /
  `Recover` / `ResetPassword` and the entire client `userStore` +
  migration 0002 + `UserStore` interface, or (b) re-wire local
  daemon registration to use the client-side store (current `register`
  only mutates the server). (a) is straightforward — the
  `SignupResult` carrier is what the CLI actually needs and that's a
  package-local concern. (b) is much more design work.

#### Q2 — daemon emits text/plain errors; serve emits JSON
- **What.** `internal/agent` returns errors via `http.Error()` (text/plain
  with trailing newline). `internal/serve` returns errors via
  `writeError()` (`{"error": "..."}`). Same wire (the daemon's
  loopback HTTP), two contracts. The agent's `client.go::doJSON`
  string-matches the body to detect the no-upstream sentinel —
  documented in the learnings as one-sided sentinel-error wrapping.
- **Where.**
  - `internal/agent/handlers.go:201` — `writeJSON` exists, no `writeError`.
  - `internal/agent/sync_handlers.go:26,49` — raw `http.Error(w, "no
    upstream configured…", 409)`.
  - `internal/agent/client.go:194` — string-match
    `strings.Contains(bodyStr, "no upstream configured")` to gate the
    `ErrNoUpstream` wrap.
  - `internal/serve/server.go:363-371` — `writeJSON` / `writeError`.
- **Why it matters.** If a future tweak to the daemon's error body
  drops or rewords "no upstream configured", standalone-mode detection
  silently degrades to a plain `daemon 409 Conflict` (also captured
  verbatim in a learning). Cross-package inconsistency also taxes
  every new daemon route author.
- **Fix sketch.** Add an `agent/handlers.go::writeError(w, status,
  code, msg)` that emits `{"error":{"code":...,"message":...}}`, port
  the 4xx call-sites to it, and switch `client.go::doJSON` to match on
  `code`. Daemon-internal change only; no external consumers.

#### Q3 — three CLI flag-parsing dialects
- **What.** Three different parsing patterns live in `internal/cli`:
  (1) `flag.NewFlagSet` with `fs.Parse(args)` (used by 17 verbs);
  (2) hand-rolled `for i := 0; i < len(args); i++` with
  `case "--flag" / case strings.HasPrefix(a, "--flag=")` (used by
  `scope`, `plan`, `learning`, `sync` push/pull, others); (3) ad-hoc
  reject-on-flag loops (`sync push/pull`). Two verbs (`scope set`,
  `plan write`) explicitly comment that the hand-roll is intentional
  ("flag.FlagSet stops at the first positional"), but the other
  hand-rolled verbs don't actually rely on that property — they could
  be `flag.FlagSet`-based. Help text and error wording then drift
  per file.
- **Where.**
  - `flag.NewFlagSet` sites: `auth.go:33,136,237; serve.go:24;
    session.go:64,108; hook.go:24; agent.go:15; migrate_key.go:55;
    lifecycle.go:27,94,137,185; install_bridge.go:16; mcp.go:32;
    pair.go:65,198`.
  - Hand-rolled: `scope.go:34-114, 175-195; plan.go:32-95, …;
    learning.go:28-83; sync.go:48-79, 83-119`.
  - `cli/auth.go:429-437` — `urlWithQuery` helper imported only by a
    stale comment; never called.
- **Why it matters.** Inconsistent UX (`--foo bar` vs `--foo=bar` works
  in some verbs, errors in others), inconsistent error spellings, and
  every new verb is a coin-flip on which style to follow.
- **Fix sketch.** Pick one. The `flag.NewFlagSet` lane covers all
  verbs the hand-roll covers IF positional args go AFTER all flags;
  the few verbs that need flexible ordering can wrap a thin
  reorder-then-FlagSet helper. The hand-rolled
  `parsePlanWriteArgs`-style is ~30 lines per verb of duplicable
  switch/case — a real cost.

#### Q4 — `runResume` is 186 lines mixing every concern
- **What.** `internal/cli/resume.go::runResume` is 186 lines:
  flag parsing, optional pull, adapter dispatch, scope-derived plan
  fetch, hydrate dispatch, native-cmd resolution, stdout formatting,
  exec hand-off. Test coverage is zero on the function (the
  `cli` package has 5.9% coverage and `resume.go` has no unit test).
- **Where.** `internal/cli/resume.go:21-207` (the body), plus the
  `cli/resume_test.go` file does not exist.
- **Why it matters.** This is the user-facing critical path — the
  rehydrate UX. The function's lack of seams means any regression
  has to be caught by manual end-to-end smoke.
- **Fix sketch.** Split into (a) `resolveAdapterAndMode(cliName,
  mode)`, (b) `resolveSessionView(c, sessionID)`, (c)
  `resolvePlanContent(c, ses, mode)` (the BuildContext branch), (d)
  `materializeAndExec(a, view, opts, exec bool)`. Each is unit-testable.

#### Q5 — `cli/migrate_key.go::loginAndUnwrapKEK` and
       `cli/pair.go::unwrapKEKViaLogin` are near-duplicates
- **What.** Both helpers run the same login-challenge → derive verifier →
  POST /v2/auth/login → unwrap KEK sequence. The only meaningful
  differences are (a) the cosmetic "pair-invite-ephemeral" device
  name in the pair version and (b) the error-wrap prefix. Same crypto,
  same wire shape, two copies.
- **Where.** `internal/cli/migrate_key.go:271-302` and
  `internal/cli/pair.go:321-348`.
- **Why it matters.** The shared codepath is exactly the spot
  follow-ups (rotation, second-factor, key rolls) will keep touching.
  Two implementations drift.
- **Fix sketch.** Extract to a sibling file as
  `loginAndUnwrapKEK(ctx, upstream, email, password, deviceName string)`
  and have both callers pass their own device name. ~30 lines deleted.

### Medium — worth scheduling

#### Q6 — keep-import-alive idioms in production files
- **What.** `var _ = time.Now` (`cli/auth.go:440`), `var _ =
  errors.New` (`cli/pair.go:399`), `_ auth.Sealer =
  (*auth.AESGCMSealer)(nil) // keep auth import live`
  (`storage/sql/sqlite/identity.go:246`). The first two parking
  placeholders are leftovers from partial-implementation phases; the
  comment on the storage one (`// keep auth import live`) is the most
  honest — but the assertion lives in a file (`identity.go`) that has
  no other reason to know `auth.AESGCMSealer` exists, so it's really
  parked in the wrong place.
- **Where.** see above.
- **Why it matters.** These prevent the compiler from helping us
  notice an unused import — i.e. they hide the dead-code signal the
  team relies on elsewhere.
- **Fix sketch.** Delete the three `var _ =` lines and let the
  unused-import error surface. Move the `auth.Sealer` assertion to
  `internal/auth/envelope.go` (next to `AESGCMSealer`'s definition)
  where it's load-bearing for a different package.

#### Q7 — `internal/adapter/common.SynthesizePrime` has 0% coverage
- **What.** The cross-CLI prime synthesizer (399 lines) is consumed by
  both the claude and opencode adapters and is the universal
  re-sleeve path. It has no tests; `claude/prime_test.go` exists but
  is testing a different helper. The kind-by-kind rendering, the
  byte-cap truncation logic, the tool_call/tool_result pairing — none
  of it is exercised in isolation.
- **Where.** `internal/adapter/common/prime.go:58-399`.
- **Why it matters.** Both adapters render prime through this; a
  regression in truncation (e.g. dropping the wrong end) silently
  ships in both at once.
- **Fix sketch.** Add `common/prime_test.go` with table cases for:
  empty events; events-only-thinking; tool-call/result pairing;
  byte-cap with N events; truncation note presence.

#### Q8 — `runDoctor` is 128 lines doing all of: status, backfill,
       migrate-key
- **What.** `cli/lifecycle.go::runDoctor` has three full sub-commands
  hidden behind boolean flags (`--backfill-counts`, `--backfill-cwd`,
  `--migrate-key`) that short-circuit the "report cards" body. Plus
  it inlines all the card rendering. The structure works but it's
  exactly the function future card additions will keep growing.
- **Where.** `internal/cli/lifecycle.go:184-312`.
- **Why it matters.** Each `--*` flag is really a separate verb
  pretending to be a doctor mode. Once round 6 lands more cards (per
  the polish punch list), this will go over 200 lines easily.
- **Fix sketch.** Promote `--backfill-counts` / `--backfill-cwd` /
  `--migrate-key` to real `doctor` subcommands (`resleeve doctor
  backfill counts`), or top-level (`resleeve backfill counts`). Then
  `runDoctor` is purely about cards.

#### Q9 — memory `BuildContext` / `CollectPlanChain` /
       `CollectLearningsChain` have zero tests
- **What.** `internal/memory/context.go`'s three core functions —
  the ones that produce the SessionStart-injected markdown — are
  uncovered. `walk_test.go` covers `Ancestors` and `ApplyBoundary`
  thoroughly but stops there.
- **Where.** `internal/memory/context.go:21-110`.
- **Why it matters.** The "F14 visible-indicator" lesson called out
  that a silent feature that "works" looks broken — the inverse
  is also true: a regression in BuildContext that returns "" when it
  shouldn't goes unnoticed because there's no test for the
  positive-case happy path.
- **Fix sketch.** A `memory/context_test.go` with a fake
  `StoreReader` (3 scopes, 2 plans, 4 learnings, one `DoNotInherit`)
  and table cases for: empty chain, full chain, boundary-trimmed
  chain, plans-only, learnings-only, both.

#### Q10 — duplicated `deterministicSalt` derivation
- **What.** The pair-code verifier salt derivation
  `auth.DeriveKey([]byte("pair-verifier"), []byte(codeID), …)` is
  spelled out in `cli/pair.go::deterministicSalt` and re-derived
  byte-identically in `serve/pair_test.go::deterministicSaltForTest`.
  A test-only mirror is justified to dodge a circular import, but the
  Argon2id parameters are baked in by hand in both places — change
  one without the other and the test silently lies.
- **Where.** `internal/cli/pair.go:384-396` and
  `internal/serve/pair_test.go:216-225`.
- **Why it matters.** Future Argon2 hardening will hit both — and the
  test mirror will silently let the production code "pass" by
  matching only itself.
- **Fix sketch.** Move the derivation into a `pairing` helper that
  both packages can import (e.g. `internal/auth/pairing.go`), or at
  minimum a const for the params shared by both.

#### Q11 — `agent.SyncClient.PullNow` shadowed by `PullNowCounts`
- **What.** `PullNow(ctx) error` calls `PullNowCounts(ctx) (counts,
  error)` and discards counts. `PullNow` has no callers in
  production code (only `sync_test.go` smoke). It's a thin wrapper
  that survives only as a legacy alias.
- **Where.** `internal/agent/sync.go:219-222`.
- **Fix sketch.** Inline the two callers (the test) to
  `PullNowCounts` and delete the wrapper.

#### Q12 — agent imports concrete `claude` adapter inside `daemon.go`
- **What.** `internal/agent/daemon.go:14,138` imports the claude
  adapter package and constructs `claude.New()` in `Serve` for the
  reconcile sweep. The agent package is supposed to be CLI-neutral;
  adapter selection is supposed to live in `cli/` and be handed to
  the daemon. Once an opencode reconcile lands (round 6 polish), the
  pattern doesn't generalise.
- **Where.** `internal/agent/daemon.go:14,138-142`.
- **Fix sketch.** Add `Config.Reconcilers []func(ctx, d) error`;
  CLI registers `claude.Adapter.ReconcileOnce` at startup.

#### Q13 — `cli/scope.go::runScopeDelete` string-matches the error body
- **What.** `runScopeDelete` checks both `strings.Contains(err.Error(),
  "has children")` AND `errors.Is(err, memory.ErrScopeHasChildren)`.
  The OR is double-belted: the typed sentinel is the right check; the
  string-contains branch is dead unless the daemon strips
  the wrapping somewhere. Pattern matches the documented
  daemon-side error-shape drift risk.
- **Where.** `internal/cli/scope.go:186`.
- **Fix sketch.** Drop the `strings.Contains` half; if the typed
  sentinel doesn't survive the HTTP boundary, fix the propagation
  rather than papering over with a string check.

#### Q14 — `agent/memory_handlers.go::putScope` does stringly-typed EOF check
- **What.** Body decode wraps a check `if err.Error() != "EOF" { …
  return 400 }`. Comparing error messages by literal string is the
  same anti-pattern called out in the round-5 lessons for
  `ErrNoUpstream`; here it's used inline.
- **Where.** `internal/agent/memory_handlers.go:53-58`.
- **Fix sketch.** Use `errors.Is(err, io.EOF)`.

### Low — polish

#### Q15 — `_test.go`-adjacent helpers leaking into production
- **What.** The `agent.TestHandler` boundary was fixed by moving it to
  `internal/agent/agenttest/handler.go` (follow-up E). One residual
  leak remains: `daemon.SetSecret` exists in production purely to let
  `agenttest.TestHandler` install a known bearer secret on a daemon
  that hasn't called `Serve()`. The doc comment is honest about it
  ("tests that front the daemon via Handler()") but the public method
  has no other consumers.
- **Where.** `internal/agent/daemon.go:159-165`.
- **Fix sketch.** Either accept the seam as-is (it's small) or expose
  the constructor option (`Config.Secret`) so production paths
  could in principle use it instead of generating their own.

#### Q16 — `internal/storage/blob` is empty
- **What.** `internal/storage/blob/doc.go` is one file with package
  doc. No implementation. Either a placeholder for a planned subsystem
  or stale.
- **Where.** `internal/storage/blob/doc.go`.
- **Fix sketch.** If unplanned, delete the package; if a future drop
  is expected, link the design doc reference inline.

#### Q17 — RFC3339Nano lex-sort hazard everywhere except outbox
- **What.** Round-5 follow-up G fixed `outbox.next_attempt_at` to use
  a fixed-width nanosecond format (the learning captures the rationale
  in detail). Every other time column in sqlite stores still uses
  `time.RFC3339Nano` (which strips trailing zeros) — see
  `users.go:26-27`, `identity.go:31-32, 88-89, 174-175, 218`, etc.
  Most of these compare by equality / IN, not lexicographic — but
  they exist as latent traps for future code that adds an ORDER BY.
- **Where.** Every `Format(time.RFC3339Nano)` call in
  `internal/storage/sql/sqlite/*.go` that isn't `outbox.next_attempt_at`.
- **Fix sketch.** Either standardise on the fixed-width format
  everywhere now (one-line change, idempotent) or add an inline
  comment "// safe: not lex-compared" next to each existing call so
  reviewers reading the file know the constraint.

#### Q18 — `internal/adapter/opencode/adapter.go:135` TODO with no
       owner / date
- **What.** Lone `TODO(v3)` for the opencode CLI flag surface. Tagged
  by milestone but no owner / date. The only TODO in the codebase.
- **Where.** `internal/adapter/opencode/adapter.go:135-137`.
- **Fix sketch.** Either link the punch-list scope
  (`resleeve/<slug>`) or convert to a real follow-up entry in the
  scope's plan.

#### Q19 — outdated comment in `cli/auth.go::pair.go`
- **What.** Pair.go's flow comment (`runPair` doc block, lines 18-43)
  references "/v1/seal/export endpoint (added below)" — but
  `unwrapKEKViaLogin` actually pulls the KEK by re-running login (it
  cannot ask the daemon to export the live KEK; that's intentional).
  The seal/export route doesn't exist in the codebase.
- **Where.** `internal/cli/pair.go:18-43`.
- **Fix sketch.** Rewrite the doc block to reflect the actual flow
  (login-as-helper-to-re-derive-KEK).

#### Q20 — `runMigrateKey` step 5 prints "delete the file" instructions
       even after a successful re-run with no rows to migrate
- **What.** The end-of-migrate-key user-facing block (`migrate_key.go:156-161`)
  hard-codes the "Next: delete the legacy placeholder" prompt. After
  a dry-run, after a re-run that finds nothing to migrate, the same
  hint prints. Mildly confusing UX.
- **Where.** `internal/cli/migrate_key.go:130-162`.
- **Fix sketch.** Gate the hint behind `totals.Migrated > 0 ||
  totals.Skipped > 0`; otherwise print "no rows migrated; legacy key
  may already be retired."

---

## Coverage observations

| Package | Coverage | Notable gaps |
| --- | --- | --- |
| `event` | 100% | — |
| `auth` | 80% | `Login`/`Recover`/`ResetPassword` covered by test; never reached in prod. |
| `serve` | 75% | SSE backlog-replay path is exercised but pull-then-SSE-then-reconnect with cursor isn't. |
| `sync/local` | 74% | `Delete` happy path covered; idempotent delete with missing parent dirs less so. |
| `adapter/claude` | 70% | `prime.go` (claude wrapper) covered; the underlying `common.SynthesizePrime` is not. |
| `mcp` | 60% | `tools.go` actions covered; per-tool argument-validation error paths thin. |
| `sqlite` | 51% | `identity.go` (devices / pairings) covered; `users.go` covered only because of dead client-side path. |
| `agent` | 36% | `SyncClient` core covered (~6 tests); doctor handlers, seal handlers, memory handlers thin or untested. |
| `memory` | 28% | `Ancestors` / `ApplyBoundary` covered; **`BuildContext` / `CollectPlanChain` / `CollectLearningsChain` are uncovered**. |
| `cli` | **6%** | Only `pair_test.go` / `on_demand_pull_test.go` / `keychain_test.go` / `migrate_key_test.go` exist. **`runResume`, `runRegister`, `runLogin`, `runPair*`, `runDoctor`, `runMigrateKey`, every `scope`/`plan`/`learning` verb has no unit test**. |
| `adapter/common` | **0%** | Cross-CLI `SynthesizePrime` — the universal prime path — has no tests of its own (claude's `prime_test.go` tests a wrapper). |
| `sync` | 0% | Only `ValidateKey`; not exercised by a test, only via `local`. |
| `agent/agenttest` | 0% | Test helper, used via downstream tests. |
| `storage/sql` | 0% | Interfaces only, no logic to cover. |

High-risk uncovered branches that warrant tests (in priority order):

1. `runResume` happy path (claude replay) and prime fallback (opencode).
2. `common.SynthesizePrime` truncation + tool_call/result pairing.
3. `memory.BuildContext` boundary application.
4. Seal handlers' `requireLoopback` rejection for non-loopback peers.
5. `agent.SyncClient.runSSE` parsing of `data:` framing with multi-line
   buffer flush.

---

## Cleanup wins

A short list of trivial deletes / renames each worth <30 min:

1. **Delete `cli/auth.go:429-437` `urlWithQuery`** — uncalled helper.
2. **Delete the three `var _ = …` keep-import lines** (`cli/auth.go:440`,
   `cli/pair.go:399`, `sqlite/sync_test.go:150`) and clean up the
   resulting unused-import errors.
3. **Move `_ auth.Sealer = (*auth.AESGCMSealer)(nil)`** from
   `sqlite/identity.go:246` to `auth/envelope.go` next to the type.
4. **Inline `agent.SyncClient.PullNow`** into its one test caller and
   delete the wrapper.
5. **Drop `strings.Contains(err.Error(), "has children")` branch in
   `cli/scope.go:186`** — typed sentinel is sufficient.
6. **Replace `err.Error() != "EOF"`** in `agent/memory_handlers.go:53`
   with `errors.Is(err, io.EOF)`.
7. **Promote `cli/sync.go::pluralize`** (currently single-caller, single
   file) to a `cli/util.go` helper — or inline.
8. **Delete `internal/storage/blob`** if no concrete impl is planned
   for round 6; otherwise add the design-doc reference.
9. **Rewrite `cli/pair.go:18-43` doc block** to drop the stale
   "/v1/seal/export endpoint (added below)" claim.
10. **Owner-tag the lone TODO** in `opencode/adapter.go:135` (link to
    `resleeve/opencode-bridge` punch-list scope).
