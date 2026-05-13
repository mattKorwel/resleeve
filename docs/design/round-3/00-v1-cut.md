# v1 cut: single-machine, Claude Code only

The thinnest vertical slice through the fully-designed system (rounds 1–2) that delivers end-to-end value. Per [[design-then-mvp-methodology]]: this is a *slice* of the full design, not a reduced design.

Three slices total — **v1 → v2 → v3.** Each independently shippable and useful on its own.

## v1 scope

### In

- **Persona:** single-machine solo dev ([`../round-2/06-journey-single-machine.md`](../round-2/06-journey-single-machine.md)).
- **Adapter:** Claude Code only.
- **Mode:** standalone daemon (no SCION, no remote server).
- **Storage:** SQLite + local filesystem blobs. Postgres support is in v2; the abstraction is in place from v1 per [`../round-2/11-storage-backends.md`](../round-2/11-storage-backends.md).
- **Auth:** local-only, single-user. Master password required on `resleeve up`; recovery key generated and shown once.
- **Features:** capture, search, show, tail (SSE).
- **UX surface:** `resleeve up`, `resleeve down`, `resleeve purge`, `resleeve session list/search/show/tail`, `resleeve usage`, `resleeve gc`, `resleeve doctor`.

### Out — deferred to later cuts

- Remote `resleeve serve` + cross-machine resume → **v2**
- Other adapters (opencode, Codex, Gemini) → **v3** for opencode/Codex; later for Gemini
- Aider, Open Interpreter → post-v3
- SCION integration → **v3**
- Eventing system / subscriptions / webhooks → **v3** (arrives with SCION needs)
- Shares → **v3**
- Memory module → **v3+**
- Postgres deployment → **v2** (when remote server ships)
- Cloud blob storage (S3 / GCS) → **v2+**
- Document adapter family (Firestore / MongoDB) → **v3+** (demand-driven)

## Acceptance criteria

A user can:

1. `brew install resleeve` (or curl-pipe-sh).
2. `resleeve up` — daemon starts, bridge plugin installed for Claude Code, master password set, recovery key shown once.
3. `claude` — session captured invisibly to local SQLite + blob store.
4. After the session, `resleeve session search "auth bug"` finds it.
5. `resleeve session show <id>` shows the transcript.
6. `resleeve session tail <id>` (while a session is running) shows events live.
7. `resleeve down` cleanly tears down.
8. `resleeve purge` wipes data with explicit confirmation.

That's v1. Everything else is later.

## Build order

Each stage independently shippable; cumulatively v1.

### Stage 1 — Foundation

- `go mod init github.com/mattkorwel/resleeve`.
- **License:** Apache-2.0 (lean — friendlier to corporate adoption than MIT for infrastructure software).
- Directory layout:
  ```
  cmd/resleeve/main.go
  internal/storage/sql/         # SQL abstractions
  internal/storage/sql/sqlite/  # SQLite driver
  internal/storage/blob/        # blob store (filesystem default)
  internal/event/               # event types per 04-event-schema.md
  internal/adapter/             # adapter interface per 09-adapter-interface.md
  internal/adapter/claude/      # Claude Code adapter
  internal/agent/               # local daemon (resleeve agent)
  internal/cli/                 # CLI verbs
  internal/auth/                # local single-user auth + KeyStore abstraction
  ```
- **Pure-Go SQLite driver** (`modernc.org/sqlite`) so the binary is CGO-free → simpler cross-platform builds.
- CI: GitHub Actions, lint + test on darwin/linux/windows.
- **Schema-portability lint** (catches backend-specific SQL even though only SQLite ships in v1 — discipline from day one per [`../round-2/11-storage-backends.md`](../round-2/11-storage-backends.md)).

### Stage 2 — Storage + auth data plane

- SQL schema: `users`, `sessions`, `events`, `slots`, `blobs_index`, `audit`.
- **Argon2id** password hashing (OWASP-recommended params).
- **KEK two-wrap** (password key + recovery key) per [`../round-2/10-auth-subsystem.md`](../round-2/10-auth-subsystem.md).
- **`KeyStore` abstraction:** macOS Keychain / Linux Secret Service / Windows Credential Manager / encrypted-file fallback.
- Migrations via `golang-migrate`.
- Storage interfaces (`SessionStore`, `EventStore`, `SlotStore`, `UserStore`) — concrete SQLite impl; Postgres impl deferred to v2 but interfaces are sized for it.

### Stage 3 — Event capture (the core loop)

- **Claude Code adapter:**
  - Bridge plugin install: writes `SessionStart` / `PostToolUse` / `Stop` hooks to `~/.claude/settings.json`. Idempotent; merges non-destructively if hooks already exist.
  - File-watcher safety net on `~/.claude/projects/**/*.jsonl`.
  - **Event-UUID generation:** deterministic from canonical content + offset, so live + file-watcher paths dedup converge.
  - `FromNative`: parse JSONL records into normalized events + `vendor.native_payload`.
- **Local daemon (`resleeve agent`):**
  - Random TCP port + `$XDG_RUNTIME_DIR/resleeve.endpoint` (per [`../round-2/02-journey-01-decisions.md`](../round-2/02-journey-01-decisions.md) Q4).
  - Event buffering: pod-lifetime path under `$XDG_STATE_HOME/resleeve/buffer/`.
  - **Idempotent upsert** on `(session_id, event_uuid)`.
  - Bridge-plugin auth: shared secret in `~/.config/resleeve/agent.json` (mode 0600).
- **Bridge plugin (JS):**
  - Minimal: reads daemon endpoint from `$RESLEEVE_AGENT_ENDPOINT` or discovery file, POSTs events.
  - No business logic; all logic in the daemon.

### Stage 4 — Query surface

- `session list [--scope=glob] [--since=…] [--recent]`.
- `session search "<query>"` — full-text over `content.text` fields using SQLite FTS5 (Postgres `tsvector` path stubbed but unused in v1).
- `session show <id>` — pretty-printed transcript (tool calls collapsible, thinking blocks foldable).
- `session tail <id>` — SSE stream from session module API (per `04-event-schema.md`).

### Stage 5 — Lifecycle commands

- `resleeve up` — generate / prompt master password; generate recovery key (display once, persist hash only); install bridges; start daemon.
- `resleeve down` — stop daemon; remove bridges from settings files.
- `resleeve purge` — explicit data wipe (interactive `--yes` required).
- `resleeve usage` — per-scope and per-session storage stats.
- `resleeve gc --older-than=90d` — pruning. Defaults conservative; no autoprune.
- `resleeve doctor` — diagnostic command for "why isn't capture working" (checks daemon, endpoint file, bridge plugin presence, `settings.json` validity).

### Stage 6 — Polish

- **Test fixtures:** pinned Claude Code session files for at least 3 recent versions.
- **Round-trip tests:** `FromNative(ToNative(events)) == events`. Tracked in CI.
- **Integration tests:** spin up real Claude Code in CI (where available), exercise a minimal session, verify capture end-to-end.
- **Docs:** README, getting-started, troubleshooting, FAQ.
- **Distribution:** Homebrew formula, GitHub Releases binaries (darwin-arm64 / darwin-amd64 / linux-amd64 / linux-arm64 / windows-amd64), curl-pipe-sh installer.

## Risk register

| Risk | Mitigation |
|---|---|
| Claude Code's hook API shifts | Pin behavior to released Anthropic docs; capture `vendor.version` per event for forward compat. |
| SQLite write contention under heavy capture | WAL mode by default; serialized writer goroutine; benchmark before v1 ships. |
| Bridge plugin overwriting user-customized hooks | Detect existing hooks; merge non-destructively; warn on conflicts; never auto-overwrite. |
| KeyStore unavailability on headless Linux | Fallback to encrypted-file with password prompt at daemon start; document clearly. |
| Cross-platform binary distribution complexity | CI builds for the five major (os, arch) tuples; signed releases (cosign). |
| Recovery key UX | Display ONCE with visual emphasis; recommend 1Password / Bitwarden / paper backup. `resleeve auth rotate-recovery` for refresh. |
| Schema-portability discipline drift | Lint rule + reviewer checklist; catches backend-specific SQL before merge — even though only SQLite ships. |

## v2 cut: cross-machine, Claude Code only

What v2 adds on top of v1:

- **`resleeve serve`** as a separate process (the daemon-stays-local, server-runs-anywhere split).
- **TLS** for daemon ↔ server.
- **Full auth subsystem:** signup, login flow, JWT-style access tokens, daemon tokens.
- **PostgreSQL backend** as a first-class alternative to SQLite — same connection-string interface unlocks Cloud SQL, AlloyDB, Spanner-Postgres-interface, RDS, Supabase, Neon, on-prem.
- **`resleeve session resume`** from a second host. Hydrate flow on a fresh machine.
- **`resleeve install-bridge --auto`** with login on a fresh host (per [`../round-2/05-journey-cross-machine.md`](../round-2/05-journey-cross-machine.md)).
- **Cwd rewrite** on hydrate when worktree paths differ.
- **S3-compatible blob store** as the cloud-VM upgrade path (covers GCS via HMAC API).

Acceptance: user starts a session on laptop, walks to cloudtop, `resleeve session resume <id>` continues it without context loss.

## v3 cut: fleet operator under SCION, Claude Code + opencode + Codex

What v3 adds on top of v2:

- **opencode adapter** built fresh, using `mattkorwel/ori`'s `harness-serve-cloudcode` branch as architectural reference (per [`../round-1/05-decisions.md`](../round-1/05-decisions.md)).
- **OpenAI Codex adapter** (closes the v3 adapter set per priority order).
- **Sidecar container image** (`ghcr.io/mattkorwel/resleeve-sidecar`).
- **SCION orchestrator registration** + federated token validation (per [`../round-2/10-auth-subsystem.md`](../round-2/10-auth-subsystem.md)).
- **Eventing system:** domain bus, persistent event log, webhook subscriptions, DLQ, auto-disable (per [`../round-2/03-eventing-system.md`](../round-2/03-eventing-system.md)).
- **Shares:** redaction engine, public links, recipient import (per [`../round-2/07-journey-oss-share.md`](../round-2/07-journey-oss-share.md)).

Acceptance: user runs `scion run --parallel=5` with opencode harnesses; all 5 sessions captured to resleeve; SCION Hub receives lifecycle webhooks via subscription.

## Post-v3

- **Memory module** — fold ori or build natively, depending on ori's state at the time.
- **Gemini CLI adapter.**
- **Aider (write-only export) + Open Interpreter** adapters.
- **Document adapter family** (Firestore for GCP-native; MongoDB for vendor-agnostic) — demand-driven, separate implementation per [`../round-2/11-storage-backends.md`](../round-2/11-storage-backends.md).
- **Team / multi-user features.**
- **v2 follow-ups** from journey docs: MFA, hardware keys, SSE on the domain bus, replayable shares, etc.

## Meta-decisions (locked)

The v1-slicing-itself open questions, resolved in conversation.

### Core repo decisions

- **License: Apache-2.0.** Patent grant + no-trademark clause. Corporate-friendly. Matches Kubernetes, Anthropic SDKs, Terraform (pre-BSL), the Apache foundation. The right call for infrastructure that companies may deploy.
- **Repo visibility:** **public from day one** with a clear `STATUS: pre-release / work in progress` notice in the README. `v0.x.y` versioning signals not-yet-stable. Building in the open enables early stargazers and transparent design evolution.
- **GitHub namespace:** `mattkorwel/resleeve`. Create an org later if it makes sense.
- **Default branch:** `main`.
- **Versioning:** `v0.x.y` until v1 acceptance criteria fully met; `v1.0.0` = first release meeting the v1 cut. SemVer thereafter.
- **Commit convention:** Conventional Commits — drives auto-changelog generation.
- **Release cadence:** tag-driven for `v0.x.y`, no fixed schedule until v1.0.0.

### Distribution channels (v1)

- **Homebrew** (macOS): `brew install resleeve`
- **Scoop** (Windows): `scoop install resleeve`
- **curl-pipe-sh** (Linux + macOS fallback): `curl -fsSL get.resleeve.dev | sh`
- **Go install** (free with the source): `go install github.com/mattkorwel/resleeve/cmd/resleeve@latest`
- **Docker image** for `resleeve serve`: deferred to v2 (no remote server in v1).

### Repo content

- **Design docs ship at first commit** under `/docs/design/{round-1,round-2,round-3}/`. They're the public artifact of how the project was reasoned through.
- **Stub `/v1/events` endpoint** in v1: returns `501 Not Implemented` with a body explaining "the eventing system lands in v3 with SCION integration." Cleaner than 404 for future consumers.

### Community & ops

- **CI:** GitHub Actions.
- **Communication:** GitHub Discussions for community Q&A; GitHub Issues for bugs/features. No Discord/Slack until v3+, if ever.
- **Code of conduct:** Contributor Covenant 2.1, standard text.
- **Security policy:** `SECURITY.md` with vulnerability contact (`security@resleeve.dev` once domain registered; personal email until then).
- **Telemetry / analytics:** **none in v1.** Privacy-respecting OSS from day one. Opt-in usage metrics may be considered v2+ if there's a real reason.

### Domain

- **Register `resleeve.dev`** early (~$12/yr — cheap insurance against squatting).
- Used for: docs site (`resleeve.dev`), installer endpoint (`get.resleeve.dev`), security contact (`security@resleeve.dev`).

## Definition of "done" for v1

Acceptance criteria above, plus:

- All Stage 1–6 work merged.
- Test fixtures covering at least 3 Claude Code versions.
- README with a 30-second quickstart.
- Homebrew formula merged or curl-installer working.
- Public Apache-2.0 repo with passing CI.
- At least one external user has installed and successfully captured a session (informal "did it actually work" check).
