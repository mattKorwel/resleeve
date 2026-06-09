# Changelog

All notable changes to resleeve are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and resleeve adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Pre-1.0 versions (`0.x.y`) may include breaking changes in minor bumps. See
[docs/RELEASING.md](docs/RELEASING.md) for the release cadence and procedure.

## [Unreleased]

### Added

### Changed

### Fixed

### Security

## [0.3.0] - 2026-06-09

### Added

- **More coding CLIs.** Capture and resume sessions from **Codex** and
  **opencode** at full fidelity (native resume back into the same tool), plus
  **Google Antigravity** (experimental). Choose the tool with
  `resleeve install-bridge --adapter <name>`; Claude Code stays the default.
- **In-session memory tools (MCP).** `resleeve install-bridge --mcp` registers
  resleeve's memory server into each CLI so an agent can read and curate your
  plans and learnings while it works. `--mcp-only` installs just the server;
  `install-bridge --uninstall` removes it.
- **Team mode.** `resleeve serve` can host a group: each person gets their own
  private memory, and you can create **shared memory** a team reads and writes.
  New `resleeve brain` commands — `create`, `list`, `member add|rm|list`,
  `use` — create and join shared spaces and choose which one a machine writes
  to. Use `serve --single-tenant` for a solo server.
- **Plan history + safe concurrent edits.** Plans now keep a full version
  history; concurrent edits are conflict-checked — a stale edit is rejected
  with the current version so you can reconcile instead of silently
  overwriting. Learnings now record who added them.

### Changed

- **Encryption is layered and configurable.** A team server
  (`serve --master-key`) encrypts data at rest with a separate key per memory
  space; a solo `--single-tenant` server keeps full end-to-end encryption and
  only ever stores ciphertext. On a team server you can switch a personal space
  back to end-to-end with `resleeve brain encrypt <id> e2e`. Team servers
  require a master key (`--master-key` / `$RESLEEVE_SERVER_MASTER_KEY`).
- Refreshed the README and user/architecture docs; added a team-shared-memory
  walkthrough.

### Security

- Multi-user servers enforce per-user isolation: each token can read and write
  only its own (and explicitly shared) memory — never another user's.
- The encryption key is now wiped from daemon memory on logout instead of
  lingering until garbage collection.

## [0.2.0] - 2026-06-04

### Added

- `resleeve up --memory-only` (and `install-bridge --memory-only`) installs
  only the SessionStart hook and bakes `--memory-only` into the hook command:
  memory injection still fires, but no transcripts land in the events table.
  Useful for privacy, disk, latency, or rolled-up-context-only workflows.
- `docs/QUICKSTART.md`, `docs/USER_GUIDE.md`, `docs/ARCHITECTURE.md`,
  `docs/SMOKE.md` — public documentation set for the round-5 / round-6 ship.
- `docs/REVIEW_QUALITY.md` and `docs/REVIEW_SECURITY.md` — internal audit
  reports at commit `cc04113` covering code quality (20 findings) and security
  posture (30 findings) respectively.
- Real Argon2id KEK derivation from a user-supplied master password, replacing
  the daemon-local placeholder seal key. `resleeve register`, `login`,
  `logout`, `pair invite`, `pair accept`, and `migrate-key` are now wired
  end-to-end.
- OS-native keychain integration (macOS Keychain and libsecret on Linux)
  replaces the file-based keychain stub.

### Changed

- Rewrote `README.md` to reflect the round-5 ship.

### Fixed

- **Security (H1)** — `serve` now enforces an Argon2id parameter floor and
  ceiling on `/v2/auth/register` (memory 64 MiB ↔ 4 GiB, time iters 2 ↔ 32) to
  prevent both under- and over-resourced registrations.
- **Security (H2)** — `serve` now locks a pair-claim code after 5 failed
  attempts; TTL expiry is indistinguishable from lockout to avoid an oracle.
- **Security (H3)** — `serve` rejects the legacy bearer token on user-scoped
  endpoints (`/v2/auth/pair/publish`, `/v2/auth/logout`) when the bearer's
  user-id field is empty.
- `serve` login-challenge returns a constant-shape response for unknown
  emails (info-leak hardening).
- `migrate-key` refuses to run when the daemon is alive, closing a race
  window where in-flight requests could observe a half-rotated key.
- Outbox `next_attempt_at` lex-sort flake: switched from RFC3339Nano (which
  strips trailing zeros) to a fixed-width nanosecond format so the dequeue
  probe's lex comparison is stable.

## [0.1.0] - 2026-06-04

### Added

- Initial public cut. Fleet-operator MVP: single Go binary + SQLite, daemon
  on loopback, hook-driven session capture for Claude Code (and an opencode
  adapter stub), scope tree with plans + append-only learnings, hierarchical
  scope marker (`.resleeve-scope`) for context inheritance.
- `resleeve serve` HTTP API (Push, Pull, SSE, Health) + envelope encryption at
  the sync boundary; cross-machine sync validated end-to-end with zero
  plaintext leak across 7 markers.
- Two-tier sync: slow tier (sessions + events) via 30 s polling +
  push-on-commit + outbox; fast tier (memory) via SSE.
- SessionStart hook three-layer output: top-level `systemMessage`
  (user-visible notice), `hookSpecificOutput.additionalContext`
  (model-visible context), and `~/.resleeve/last-injected.md` (audit trail).

[Unreleased]: https://github.com/mattkorwel/resleeve/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/mattkorwel/resleeve/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/mattkorwel/resleeve/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/mattkorwel/resleeve/releases/tag/v0.1.0
