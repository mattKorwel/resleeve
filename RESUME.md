# Resleeve — session resume document

A self-contained handoff for picking up where the last session left off.
Last updated: 2026-06-04.

## Three ways to load context (pick one)

**1. The dogfood option (recommended)** — `cd ~/dev/resleeve && claude`.
The `.resleeve-scope` marker file in this repo root binds scope `resleeve`
to any CC session started here. On `SessionStart`, the resleeve hook fetches
the rolled-up plan + learnings from the local daemon's memory module and
injects them as `additionalContext`. New CC sees the current state of the
project — no "catch me up" prompt needed.

Prereq: `resleeve doctor` shows the daemon running and the bridge installed.
If not: `resleeve up` (optionally with `--upstream URL --upstream-token TOK`).

**2. Hand-to-fresh-CC** — `claude -p "read RESUME.md and continue from where the previous session left off"` from this directory. No infra dependency.

**3. Auto-memory** — if Claude is running with auto-memory enabled at
`~/.claude/projects/<this-cwd-encoded>/memory/`, the file
`project_resleeve.md` is loaded automatically into every new conversation.

## What this project is

Open-source self-hosted multi-CLI AI agent session+memory service.

- Repo: https://github.com/mattKorwel/resleeve (public, Apache-2.0)
- Local: `~/dev/resleeve` (moved from `~/resleeve` on 2026-06-03)
- Single Go binary + SQLite, optional `resleeve serve` upstream for sync
- Persona: **fleet operator** — stack persists, sleeve rotates
  (`docs/design/round-1/05-decisions.md`)

## Current status (2026-06-04)

**Round-4 SHIPPED.** Fleet-operator MVP wire is end-to-end and validated:

- Capture on machine A → `resleeve serve` (encrypted blobs) → machine B
  pulls slow tier on startup, gets memory via SSE near-real-time → `resleeve
  resume <sid>` on B writes `~/.claude/projects/.../<sid>.jsonl` and emits
  `claude --resume <sid>` to seamlessly continue
- AES-GCM envelope encryption with a placeholder daemon-local seal key
  (`~/.resleeve/seal.key`); real KEK derivation from master password is
  round 5
- Live cross-machine smoke confirmed: **0 of 7 plaintext markers** (session
  id, cwd, prompt, scope path, title, plan body, learning body) leak in any
  on-disk blob

## Round-4 commits on `main`

In chronological order, after the round-3 v0.1.0 push at `e08c6fd`:

| Commit | What |
|---|---|
| `3d1fa84` | Stage 5 slice 1 — claude→claude replay (Hydrate + `resleeve resume`) |
| `5bdb65d` | v2 slice 1 — Backend interface + local-disk + `resleeve serve` stub |
| `af8d762` | v2 slice 2 — outbox + push-on-commit + 30s pull (slow tier) |
| `797fd60` | Stage 5 slice 2 — prime mode + opencode adapter stub |
| `60b4e69` | v2 slice 3 — SSE fast tier + memory sync |
| `58f1fdf` | v2 slice 2.5 — envelope encryption at sync boundary |
| `f402da9` | fix — seal memory blobs too (slice 3+2.5 integration gap) |

Plus earlier in the day (Phase 1 dogfood bugs):
`fde1c40` F7 event_count · `7f6bfac` F8 tail --limit · `8a42c22` F1+F2+F3 reconcile · `e81cb49` F11 marker · `c7042ff` F4+F11 docs · `5a80c43` F12 hooks format.

Round-4 design docs: `docs/design/round-4/{00-fleet-operator-cut, 01-conversation-transport, 02-cross-machine-sync, 03-rehydrate-ux}.md`.

## Architecture cheat sheet

| Package | Role |
|---|---|
| `internal/agent` | local daemon: HTTP on loopback, IngestBatch, SyncClient (drain + pull + SSE) |
| `internal/serve` | `resleeve serve` HTTP API: `POST /v2/sync/push`, `GET /v2/sync/pull`, `GET /v2/sync/sse`, `GET /v2/sync/health` |
| `internal/sync` | `Backend` interface; `local` subpackage = filesystem impl |
| `internal/auth` | Argon2id+KEK two-wrap, recovery key, `Sealer` AES-GCM envelope, placeholder seal-key file |
| `internal/adapter` | Adapter interface; `claude/` (replay + prime), `opencode/` (stub), `common/prime.go` (shared synthesizer) |
| `internal/cli` | verb dispatch (agent, hook, install-bridge, session, up/down/doctor/usage/purge, scope/plan/learning/context, resume, serve) |
| `internal/storage/sql/sqlite` | store, embedded auto-migrations 0001–0004 |
| `internal/memory` | scope tree, plans, learnings, inheritance walks, `BuildContext` rollup |

Hooks installed at `~/.claude/settings.json` use the **matcher-wrapped**
format (F12): `{matcher: "", hooks: [{type, command, _resleeve: true}]}`.
Flat-format `_resleeve` entries from older installs are migrated on next
`resleeve up`.

Cwd → scope resolution (F11): `$RESLEEVE_SCOPE` env override > `.resleeve-scope`
marker walking cwd ancestors > `filepath.Base(cwd)` > `"unknown"`.

Event `seq` (F4) is `ts.UnixNano()`, not a per-session counter. Same-nanosecond
ties broken by `event_uuid` ascending.

## Polish punch list (rough priority order)

1. `resleeve sync push/pull` — manual escape hatch; simplest unblocker. `PullNow` already exists as an internal method.
2. On-demand pull integration in `resume` / `session show` — round-4 slice 2c.
3. `resleeve register / login / logout` + `pair invite / accept` — round-4/02 identity flow; replaces manual seal.key file copy.
4. `resleeve doctor` cards: `upstream`, `outbox depth`, `sse status`.
5. **Round 5**: real Argon2id KEK derivation from master password — replaces the daemon-local placeholder seal key at `~/.resleeve/seal.key`.
6. Backfill `sessions.cwd` for pre-existing reconcile-only sessions (F1+F2 historical).
7. `doctor --backfill` to recompute `event_count` on legacy sessions (F7 historical).
8. Wire `HydrateOpts.PlanContent` through `resleeve resume` so prime mode pulls from `memory.BuildContext`.

## Running daemon (this machine)

- Data dir: `~/.resleeve/` — `data.db`, `endpoint`, `daemon.pid`, `daemon.log`, `seal.key` (placeholder KEK material)
- Bridge in `~/.claude/settings.json` (idempotent re-install via `_resleeve: true` tag)
- Doctor: `resleeve doctor` from anywhere
- Up with sync: `resleeve up --upstream URL --upstream-token TOK`
- Multi-device pairing (placeholder): copy `~/.resleeve/seal.key` to the second machine + share `--auth-token`. Replaced by `resleeve pair` in round 5+.

## Cross-machine smoke recipe (for re-validation)

```bash
SERVE_DIR=$(mktemp -d); A_HOME=$(mktemp -d); B_HOME=$(mktemp -d)
TOKEN=$(openssl rand -hex 16); BIN=~/dev/resleeve/resleeve
mkdir -p "$A_HOME/.resleeve" "$B_HOME/.resleeve" "$A_HOME/.claude" "$B_HOME/.claude"

"$BIN" serve --addr 127.0.0.1:7860 --root "$SERVE_DIR" --auth-token "$TOKEN" &
HOME="$A_HOME" "$BIN" agent --addr 127.0.0.1:7861 \
  --upstream http://127.0.0.1:7860 --upstream-token "$TOKEN" &
sleep 1
cp "$A_HOME/.resleeve/seal.key" "$B_HOME/.resleeve/seal.key"
chmod 600 "$B_HOME/.resleeve/seal.key"

# capture + memory writes on A
echo '{"hook_event_name":"SessionStart","session_id":"S-1","cwd":"/tmp/x"}' \
  | HOME="$A_HOME" "$BIN" hook --adapter claude
HOME="$A_HOME" "$BIN" scope set demo --kind project --title "demo"
echo "hello B" | HOME="$A_HOME" "$BIN" plan write demo
sleep 3

# start B (initial pull catches everything A pushed)
HOME="$B_HOME" "$BIN" agent --addr 127.0.0.1:7862 \
  --upstream http://127.0.0.1:7860 --upstream-token "$TOKEN" &
sleep 3
sqlite3 "$B_HOME/.resleeve/data.db" "SELECT id, cwd FROM sessions WHERE id='S-1';"
HOME="$B_HOME" "$BIN" context demo
HOME="$B_HOME" "$BIN" resume S-1 --cwd /tmp/x-resumed --print
```

## Where to look next

For the design rationale behind any decision: `docs/design/round-{1,2,3,4}/`.
For working notes on the open punch-list: scope `resleeve` plan + learnings in
the local resleeve daemon. For "what changed since I last looked": `git log
--oneline e08c6fd..` shows everything after the v0.1.0 push.

## License

Apache-2.0. Public from day one.
