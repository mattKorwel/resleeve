# Validation pass — test matrix + plan

A full real-instance validation of everything shipped in rounds 9–13, plus
code/security/docs review. Status column: ⬜ pending · ✅ pass · ❌ fail · ⏭️
skipped (why). "How" = the real commands; expected outcome in the same cell.

> Environment: macOS, real binaries — claude 2.1.170, codex 0.137.0, opencode
> 1.16.2, agy 1.0.4. resleeve built from source (`go build ./cmd/resleeve`).
> Each phase uses an **isolated `$HOME`/data dir + a random daemon port** so
> we never touch the operator's real `~/.resleeve` / `~/.claude` etc.

## A. Local single-machine (daemon + memory + bridge)

| # | What | How / expected | Status |
|---|---|---|---|
| A1 | Daemon lifecycle | `resleeve up` → `doctor` (all green) → `down`; pidfile/endpoint created+removed | ⬜ |
| A2 | Claude bridge install | `install-bridge --adapter claude` writes `_resleeve` hooks to settings.json idempotently; `--uninstall` removes, preserves user hooks | ⬜ |
| A3 | Session capture | feed a real Claude JSONL transcript through the hook / reconcile; events land in SQLite, idempotent re-run (INSERT OR IGNORE) | ⬜ |
| A4 | Scope memory CRUD | `scope set` / `plan write` / `plan read` / `learning append` / `learning list` round-trip; inheritance walk up `.resleeve-scope` | ⬜ |
| A5 | Auto-inject | SessionStart hook emits `additionalContext` for the resolved scope; `~/.resleeve/last-injected.md` written | ⬜ |
| A6 | `--memory-only` | `install-bridge --memory-only`: injection fires, no transcript rows persisted | ⬜ |

## B. MCP memory server

| # | What | How / expected | Status |
|---|---|---|---|
| B1 | stdio server | `resleeve mcp` speaks tools/list + tools/call; catalog has scope/plan/learning/context tools | ⬜ |
| B2 | Auto-register per CLI | `install-bridge --mcp` writes the `resleeve` MCP entry into each CLI config (claude `~/.claude.json`, codex `config.toml`, opencode `opencode.json`, antigravity `~/.gemini/config/mcp_config.json`); `--uninstall` removes, preserves user entries | ⬜ |
| B3 | Tool round-trip | call `resleeve_plan_write` → `resleeve_plan_read` returns it with version; `resleeve_learning_append` records author | ⬜ |
| B4 | doctor mcp cards | `doctor` shows `mcp (<cli>)` registered ✓ after B2, ✗ after uninstall | ⬜ |

## C. Multi-CLI capture + resume (per adapter)

| # | Adapter | How / expected | Status |
|---|---|---|---|
| C1 | codex | Detect; `hooks.json` install; reconcile real `~/.codex/sessions` rollouts → events; **replay**: hydrate → `codex exec resume <id>` continues it | ⬜ |
| C2 | opencode | Detect (SQLite era); reconcile real `opencode.db` → events; **replay**: ToNative import JSON → `opencode import` → `opencode run --session` | ⬜ |
| C3 | antigravity | Detect; reconcile real `~/.gemini/antigravity-cli/conversations/*.db` (protobuf) → events with recovered text; **prime** resume only | ⬜ |
| C4 | cross-CLI prime | resume a claude session `--cli codex` → synthesized prime markdown | ⬜ |

> Note: C1/C2/C3 capture + the codex/opencode native resume were already
> proven in the adapter smoke tests (`RESLEEVE_SMOKE=1`); this re-confirms on
> current `main`. Interactive TUI launches are eyeball-only.

## D. Plan versioning + optimistic concurrency

| # | What | How / expected | Status |
|---|---|---|---|
| D1 | Version history | two `plan write`s → `plan` has v1,v2; `GET /v1/plan/versions` lists both | ⬜ |
| D2 | Conflict | write with a stale `base_version` → **409** carrying current HEAD; `--force` overrides | ⬜ |
| D3 | Author stamp | learning append records `author_user_id` | ⬜ |

## E. Sync (single-tenant, zero-knowledge)

| # | What | How / expected | Status |
|---|---|---|---|
| E1 | serve up | `resleeve serve --single-tenant` binds; health OK | ⬜ |
| E2 | push/pull | daemon A pushes a session+memory; pull on a fresh daemon B reconstructs it | ⬜ |
| E3 | **Zero-knowledge** | inspect the serve backend blob — it is **ciphertext** (client-sealed); no plaintext markers (session id, scope, plan text) | ⬜ |

## F. Team mode (multi-tenant) + isolation

| # | What | How / expected | Status |
|---|---|---|---|
| F1 | serve multi-tenant | `serve` (default MT) **fails without** `--master-key`; succeeds with one | ⬜ |
| F2 | Two users | register/login/pair user A and user B (distinct per-device tokens) | ⬜ |
| F3 | **Isolation** | A pushes to their personal brain; B's pull returns **nothing of A's** (different brain prefix) | ⬜ |
| F4 | Legacy bearer rejected | a no-user/legacy bearer → 401/403 on `/v2/sync/*` in MT mode | ⬜ |
| F5 | Shared brain | A `brain create` → `brain member add B`; B `brain use <id>`; A writes a plan, B reads it; B writes a learning, A sees it (author=B) | ⬜ |
| F6 | Owner-gating | B (non-owner) `brain member add` → 403; remove-owner → 400 | ⬜ |

## G. Encryption matrix (the cells)

| # | Cell | How / expected | Status |
|---|---|---|---|
| G1 | single-tenant → e2e | client seals; serve blob is ciphertext the server can't read (= E3) | ⬜ |
| G2 | multi-tenant server-side (default) | client sends plaintext; serve stores **at-rest ciphertext** (backend blob ≠ what client sent) but the server **can** decrypt with the brain DEK; pull round-trips | ⬜ |
| G3 | e2e opt-in on team server | `brain encrypt <personal-id> e2e`; after re-handshake the daemon **seals** for that brain (serve at-rest layer sees only client ciphertext) | ⬜ |
| G4 | e2e rejected on shared | `brain encrypt <shared-id> e2e` → 400 | ⬜ |
| G5 | master-key custody | rotate/wrong master key → DEK unwrap fails (500), never silent plaintext | ⬜ |

## H. Proactive fan-out (live shared brains)

| # | What | How / expected | Status |
|---|---|---|---|
| H1 | Cross-member live | B holds an SSE subscription to shared brain X; A pushes a memory row to X; **B receives it live** (not just on the 30s pull) | ⬜ |
| H2 | Isolation | a subscriber to a different brain receives nothing of X | ⬜ |
| H3 | Heartbeat | idle SSE connection gets `: ping` keepalives | ⬜ |

---

## Review passes (parallel to the smoke run)

- **Code review** — focus the new surface (rounds 9–13): the four adapters,
  the registry/wiring, the multi-tenant keyspace + brains/membership, the
  encryption (atrest envelope, default-flip seal decision), plan versioning,
  the fan-out hub. Looking for correctness bugs, race conditions, resource
  leaks, error-handling gaps, and dead/duplicated code.
- **Security review** — the trust boundaries: tenant isolation (can a token
  ever read another brain?), the seal-decision (any window that ships
  plaintext when it shouldn't?), DEK/master-key handling, credential/token
  auth, the `whoami` handshake, SSRF/abuse surface, the legacy-bearer gates,
  quota/DoS. Map to the threat model in `REVIEW_SECURITY.md`.
- **Docs review** — verify every command/flag in QUICKSTART/USER_GUIDE/
  ARCHITECTURE/README/use-cases matches the real CLI and behavior observed in
  the smoke run; flag anything stale or overclaimed.

## Results (2026-06-09)

### Smoke — real isolated instances (PASS)
- **A1 ✅** daemon `up`/`doctor`/`down`; bridge installed; new `mcp (<cli>)` doctor cards present.
- **A4 ✅** `scope set` + `plan write/read` + `learning append/list` + `context` round-trip; rolled-up context injects the HEAD plan + learnings.
- **D1 ✅** plan versioning: `plan write` reports v1→v2; `plan read` materializes HEAD with its version.
- **B2/B4 ✅** `install-bridge --mcp-only` writes the `resleeve` MCP entry into `~/.claude.json`; doctor card flips ✓; `--uninstall` removes it and flips back.
- **F1 ✅** multi-tenant `serve` **refuses to boot without `--master-key`** ("requires --master-key"); boots with a 32-byte key; single-tenant boots keyless.
- C (multi-CLI capture+resume), E/F3/G/H integration cells: boundaries **code-verified by the security + code review** (below); the codex/opencode native-resume + the four-adapter captures were already proven by the guarded `RESLEEVE_SMOKE` tests this cycle. A full live two-user integration smoke is the remaining hands-on gap.

### Minor smoke finding
- `serve` logs the **requested** `--addr` (`:0`) rather than the resolved port; the daemon (`up`) prints the real port. UX gap.

### Code review — surface is healthy, no blockers/majors
Fail-closed crypto (decrypt/unwrap errors never fall through to plaintext; `shouldSeal` defaults true), correct locking (DEK cache, fan-out hub, seal cache), no resource leaks, drop-on-full fan-out backstopped by the periodic pull. Minor: (1) **non-atomic provisioning** can orphan a brain without a DEK → plaintext passthrough (= M-A below); (2) stale "but continue" comment on `handlePull`'s Get-error branch (code returns 500).

### Security review — isolation + encryption boundaries HOLD
Could **not** break: forged `?brain=`, legacy bearer on multi-tenant sync, member privilege escalation, e2e/server-side downgrade, DEK→plaintext fallthrough, cross-brain key leak. Findings:
- **M-A (medium, convergent with code-review #1) — silent plaintext-at-rest for a DEK-less brain.** When at-rest is enabled but a brain has no DEK (partial-failure provisioning, or a brain created pre-master-key), `brainDEK` returns passthrough and the server stores **plaintext** with no error/log. The one real confidentiality gap. Fix: fail closed (or lazily provision a DEK) when at-rest is on and a brain key is missing; make provisioning atomic.
- **M-B / M-C (medium) — no DoS limits, widened by multi-tenancy**: no push body-size cap (`http.MaxBytesReader`), no per-user storage quota, unbounded SSE subscriptions + brain creation per token. Was the documented "no rate limiting" accepted risk for single-user; now any registered user can exhaust the shared host.
- **L-A** push doesn't call `sync.ValidateKey` (safe for the local backend, risk for a future backend); **L-B** any member can enumerate a shared brain's member ids; **L-C** SSE reconnect re-decrypts the whole brain prefix (amplification).

## Execution notes

- Phases A–D run on one isolated daemon. E–H need a `serve` upstream + 1–2
  daemons; team cells (F/G) need the multi-tenant `serve` + two registered
  users. All in throwaway temp dirs.
- Anything that needs a model API call (interactive resume into a live CLI)
  is eyeball-only and called out; the resleeve-internal behavior (capture,
  memory, sync, isolation, encryption-at-rest, fan-out) is fully scriptable.
- Findings flow back here (status column) + into the code/security/docs review
  outputs.
