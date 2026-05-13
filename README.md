# resleeve

> **STATUS:** pre-release / work in progress. Functional v1 captures Claude Code sessions to local SQLite end-to-end. Distribution (Homebrew formula, curl-installer, signed releases) not yet wired up.

Open-source self-hosted persistent infrastructure for AI coding-agent sessions. **Sleeves are fungible; the stack persists.**

Resleeve captures session content from AI coding-agent CLIs (Claude Code first; opencode and Codex on the v3 roadmap), stores it with zero-knowledge encryption [planned for v2+], and lets you find, replay, share, and re-sleeve any session across machines and harnesses.

## What works today (v0.0.0-dev)

- **Live capture** from Claude Code via hooks (`SessionStart`, `PostToolUse`, `Stop`, `UserPromptSubmit`).
- **Reconcile sweep** over `~/.claude/projects/**/*.jsonl` on daemon startup; deterministic UUIDs dedup against the live path.
- **Local daemon** on random TCP port + XDG endpoint file; bearer auth (secret rotates per daemon start).
- **CLI:** `up / down / doctor / usage / purge / session list / session show / session search / session tail / install-bridge / agent / hook`.
- **Storage:** pure-Go SQLite (`modernc.org/sqlite`), auto-migrations, embedded schema, portable ANSI SQL ready for the Postgres v2 backend.
- **Auth crypto (Stage 2):** Argon2id + KEK two-wrap + recovery key (`internal/auth/`). Wired into `resleeve up` in v2 when zero-knowledge sync ships.

## Quickstart (build from source)

```sh
# Requires Go 1.26+
git clone https://github.com/mattkorwel/resleeve.git
cd resleeve
go build -o resleeve ./cmd/resleeve

# Wire up capture
./resleeve up           # spawns the daemon, installs bridge into ~/.claude/settings.json

# Use Claude Code normally; events stream live.
claude

# When you're done, browse what was captured.
./resleeve session list
./resleeve session show <id>
./resleeve session search "auth bug"
./resleeve session tail <id>             # poll-stream of new events
./resleeve doctor                        # check daemon/bridge state
./resleeve usage                         # data-dir size + session count

# Stop later.
./resleeve down                          # stops daemon, removes bridge
./resleeve purge --yes                   # wipe ~/.resleeve/
```

Data lives at `~/.resleeve/data.db` (SQLite) and `~/.resleeve/endpoint` (URL + bearer secret for the daemon).

## The metaphor

From Altered Carbon — the *stack* is the persistent thing; the *sleeve* (a body, in the books; ephemeral compute, in our world) is fungible. AI sessions today are coupled to one CLI on one host. Resleeve makes them portable.

## Why this exists

The intersection of *multi-CLI session capture* + *zero-knowledge self-hosted sync* + *open, documented schema* is empty. SpecStory captures multiple CLIs but the backend is proprietary SaaS. Atuin sync is well-built but shell-only. CCManager is local-only. Anthropic's "Remote Control" is single-vendor, single-live-session.

Resleeve fills that gap.

## Status & roadmap

- ✅ **v1 (in progress):** single-machine, Claude Code only, local SQLite.
- ⬜ **v2:** remote `resleeve serve`, cross-machine resume, Postgres backend (works with Cloud SQL, AlloyDB, Spanner-Postgres, RDS, Supabase, Neon, on-prem), S3-compatible blob store, full Argon2id login + KEK + recovery flow.
- ⬜ **v3:** fleet operator under SCION orchestration, opencode + Codex adapters, eventing system + webhook subscriptions + shares.

[Full design docs](docs/design/) span three rounds:

- [Round 1](docs/design/round-1/) — research + initial decisions.
- [Round 2](docs/design/round-2/) — fully-featured design: journeys, eventing, schema, API, adapters, auth, storage backends.
- [Round 3](docs/design/round-3/) — MVP slicing into v1 → v2 → v3 cuts.

## Architecture (one sentence)

`claude` runs → Anthropic hooks fire `resleeve hook` for each event → the hook reads `$XDG_RUNTIME_DIR/resleeve.endpoint` (or `~/.resleeve/endpoint`) → POSTs through `internal/adapter/claude.FromNative` into the daemon's `/v1/sessions/{id}/events` → `internal/agent.IngestBatch` calls `EventStore.Append` on SQLite (idempotent on `(session_id, event_uuid)`).

## License

[Apache-2.0](LICENSE).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Code of conduct: [Contributor Covenant 2.1](CODE_OF_CONDUCT.md).

## Security

See [SECURITY.md](SECURITY.md) for vulnerability disclosure.
