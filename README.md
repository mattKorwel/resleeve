# resleeve

Self-hosted, multi-CLI session and memory service for AI coding agents.
**Stack persists; sleeves rotate.**

Resleeve captures sessions from coding-agent CLIs (Claude Code today; opencode and
Codex on the adapter roadmap), persists them to a local SQLite stack, and — via
an optional `resleeve serve` upstream — rotates them across whatever sleeve
(machine, harness, fresh container) you happen to land in next. Memory is a
first-class substrate: scopes, plans, and append-only learnings inherit down a
tree and auto-inject into new sessions through hooks. The fleet operator
persona: one stack of context that follows you; ephemeral compute attached and
detached at will.

---

## Status & license

- **Round 5 shipped (2026-06-04).** Real Argon2id KEK + master-password login,
  OS-native keychain (macOS Keychain / libsecret), `pair invite/accept`,
  `--memory-only` bridge mode, typed daemon error sentinels, sealed-memory
  upstream invariant.
- **Round 4 shipped (earlier 2026-06-04).** Fleet-operator MVP: cross-machine
  capture → encrypted `resleeve serve` → pull + `resleeve resume <sid>` →
  `claude --resume <sid>`. Zero plaintext leak across 7 markers (session id,
  cwd, prompt, scope path, title, plan, learning).
- License: [Apache-2.0](LICENSE). Public from day one.
- Commit log: `git log --oneline` (recent rounds tagged `e08c6fd` v0.1.0 and
  forward).

This is pre-1.0 software. The capture, memory, and sync paths are validated
end-to-end, but several edges are explicitly placeholder; see
[Known limitations](#known-limitations).

---

## Install

### Go install (current canonical path)

```bash
go install github.com/mattkorwel/resleeve/cmd/resleeve@latest
```

Requires Go 1.26+. Installs the `resleeve` binary into `$GOPATH/bin` (or
`$HOME/go/bin`). Make sure that directory is on your `PATH`.

### Homebrew (planned)

```bash
# Not yet published. Tap will live at:
brew tap mattkorwel/resleeve
brew install resleeve
```

Tracked under round-5 packaging follow-ups.

### Build from source

```bash
git clone https://github.com/mattkorwel/resleeve.git
cd resleeve
go build -o resleeve ./cmd/resleeve
./resleeve --help
```

The resulting binary is fully static apart from the host's libc; SQLite is
pure-Go (`modernc.org/sqlite`), no cgo.

---

## Quick demo

```bash
$ resleeve up                                  # daemon + install Claude Code hooks
daemon: 127.0.0.1:53219  pid 41213
bridge installed: ~/.claude/settings.json
sealer: locked (run `resleeve login` to unlock for sync)

$ cd ~/dev/my-project && echo "my-project" > .resleeve-scope
$ claude                                       # SessionStart hook fires
> resleeve: loaded scope my-project (412 bytes)

claude> what was the auth bug we fixed last week?
# additionalContext (plan + parent learnings) is already in the model's prompt;
# CC answers from project memory without a "catch me up" preamble.

$ resleeve session list | head -3
$ resleeve learning append my-project 'JWT clock skew was 30s; bumped to 120s.'
$ resleeve doctor                              # cards: daemon, bridge, sealer, sync
```

Longer walk-through, including the cross-machine flow, lives in
[`docs/QUICKSTART.md`](docs/QUICKSTART.md).

---

## What's in the box

| Feature | What it does | Surface |
|---|---|---|
| Session capture | Hooks `SessionStart` / `PostToolUse` / `Stop` / `UserPromptSubmit` stream events into the daemon | `resleeve up`, Claude Code adapter |
| Memory module | Scope tree + named plan slots + append-only learnings; rolled-up `BuildContext` inherits down the tree | `resleeve scope / plan / learning / context` |
| Auto-inject | `SessionStart` hook fetches scope rollup and injects as `additionalContext`; user-visible `systemMessage` notice; audit at `~/.resleeve/last-injected.md` | F13/F14 envelope |
| MCP server | Stdio MCP exposing 10 `resleeve_*` tools so the agent can curate memory itself | `resleeve mcp` |
| Cross-machine sync | Two-tier: slow tier (sessions + events) push-on-commit + 30 s pull + outbox; fast tier (memory) SSE near-real-time | `resleeve serve`, `resleeve up --upstream ...` |
| Envelope encryption | AES-GCM at the sync boundary; zero plaintext on the upstream blob store | `internal/auth.Sealer` |
| Identity & pairing | Argon2id-derived KEK from master password, OS keychain (Keychain / libsecret), `pair invite/accept` for second device | `resleeve register / login / logout / pair` |
| `resleeve resume` | Writes a `~/.claude/projects/.../<sid>.jsonl` and emits `claude --resume <sid>` to continue a session on a new sleeve | `resleeve resume <sid>` |
| Capture modes | Full capture (events + memory) or `--memory-only` (memory injection only; no transcripts hit disk) | `resleeve up --memory-only` |
| Reconcile sweep | On daemon startup, walks `~/.claude/projects/**/*.jsonl` and dedups against live capture via deterministic UUIDs | automatic |

---

## Architecture at a glance

```
  Claude Code (or other CLI)
        |
        |  SessionStart / PostToolUse / Stop / UserPromptSubmit
        v
  hook (`resleeve hook --adapter claude`)
        |
        |  POST /v1/sessions/{id}/events   (loopback + bearer)
        v
  resleeve daemon  ──────────────┐
   - IngestBatch                  │   memory CRUD
   - SyncClient (drain/pull/SSE)  │   /v1/scope, /v1/plan, /v1/learnings
   - Sealer (AES-GCM envelope)    │
        |                         │
        v                         v
   SQLite  (~/.resleeve/data.db)  + MCP stdio server (`resleeve mcp`)
        |
        |  (optional)  push-on-commit · 30s pull · SSE
        v
  resleeve serve   <─ pull ─  resleeve daemon on machine B
   - encrypted-at-rest blob backend (local-disk today)
   - HTTP API: /v2/sync/{push,pull,sse,health}
```

Package map: `internal/agent` (daemon), `internal/serve` (upstream),
`internal/sync` (`Backend` interface + `local/`), `internal/auth` (KEK,
recovery key, Sealer), `internal/adapter/{claude,opencode,common}`,
`internal/memory`, `internal/storage/sql/sqlite`, `internal/cli`,
`internal/mcp`. Full picture in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

---

## Modes

Resleeve has three operational modes; pick the one that matches your trust
model and disk budget. Detail in [`docs/USER_GUIDE.md`](docs/USER_GUIDE.md).

### Standalone

```bash
resleeve up
```

Single machine, no upstream. Capture and memory live in `~/.resleeve/data.db`.
`resleeve sync push/pull` returns a typed `ErrNoUpstream`. The daemon-down
silent-no-op trap is mitigated by `resleeve doctor`, which loudly warns when
the bridge is installed but the daemon isn't running.

### Connected (cross-machine)

```bash
# on host with upstream
resleeve serve --addr 0.0.0.0:7860 --root /var/lib/resleeve --auth-token "$TOK"

# on each client
resleeve login                                   # unlocks KEK from master password
resleeve up --upstream https://host:7860 --upstream-token "$TOK"
resleeve pair invite                             # second-device flow
```

Sessions and memory replicate between machines through the encrypted upstream.
`resleeve resume <sid>` lets you continue a session that started elsewhere.

### Memory-only

```bash
resleeve up --memory-only
```

Installs only the `SessionStart` hook; `PostToolUse` / `Stop` /
`UserPromptSubmit` are stripped. No transcript events hit `sessions` or
`events` tables. Memory CRUD (scope / plan / learnings) goes through
`/v1/scope`, `/v1/plan`, `/v1/learnings` and keeps working in full.

Use this mode for:
- **Privacy** — no on-disk transcripts.
- **Disk** — `events` is the biggest table by far.
- **Latency** — no per-tool-call daemon round-trip.
- **Mental model** — you want the rolled-up context, not the archive.

---

## Documentation

| Doc | Audience |
|---|---|
| [`docs/QUICKSTART.md`](docs/QUICKSTART.md) | First-run: install, `up`, scope marker, single-machine capture |
| [`docs/USER_GUIDE.md`](docs/USER_GUIDE.md) | All three modes, memory workflows, MCP curation, cross-machine pairing |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Daemon, serve, sync, sealer; package map; data model |
| [`docs/SMOKE.md`](docs/SMOKE.md) | Non-destructive smoke checklist (lifecycle, hooks, `ErrNoUpstream`, MCP, cross-machine) |
| [`docs/design/round-1/`](docs/design/round-1/) | Research, prior art, persona, locked decisions |
| [`docs/design/round-2/`](docs/design/round-2/) | Fully-featured design: journeys, eventing, schema, API, adapters, auth, storage backends |
| [`docs/design/round-3/`](docs/design/round-3/) | MVP slicing; memory module design |
| [`docs/design/round-4/`](docs/design/round-4/) | Fleet-operator cut: conversation transport, cross-machine sync, rehydrate UX |
| [`RESUME.md`](RESUME.md) | Hand-off doc for picking up where the last build session left off |
| [`SECURITY.md`](SECURITY.md) | Vulnerability disclosure |

---

## Known limitations

Honest list — these are real and called out so you don't get surprised.

- **Daemon HTTP error envelope is one-sided.** Client-side `agent.ErrNoUpstream`
  is typed via `errors.Is`, but the daemon emits a plain text/plain 409. The
  client recognizes that 409 by substring match. Real fix is a structured
  JSON error envelope from the daemon; tracked as future hardening.
- **OS keychain on Linux/Windows is untested in CI.** macOS Keychain and
  libsecret-on-Linux paths exist in `internal/cli/keychain.go` but only the
  macOS path is exercised against a real keychain today. Linux works on
  developer machines with libsecret installed; Windows DPAPI is unimplemented.
- **Real cross-machine requires `resleeve serve` separately.** There is no
  managed/hosted upstream. You run `resleeve serve` somewhere reachable, mint
  an auth token, and pass `--upstream URL --upstream-token TOK` to each
  client's `resleeve up`. The pair flow handles seal-key sharing once both
  daemons are running.
- **Memory curation auto-policy is conservative.** The MCP server gives the
  agent write access to memory, but you should explicitly ask CC to "save the
  following as a learning under scope X" — memory is a high-signal substrate,
  not a chat log. Don't enable auto-curate without thinking about it.
- **Daemon-down silent no-op.** If the daemon is down, the
  `SessionStart` hook still fires but emits nothing. `resleeve doctor` warns
  loudly, but the hook itself doesn't auto-start the daemon. See
  [`docs/USER_GUIDE.md`](docs/USER_GUIDE.md) for the operational pattern.

---

## Contributing

Start with [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the package map,
then the round-of-interest under [`docs/design/`](docs/design/) for the design
rationale. The four design rounds are sequential — each round's `00-*.md` is
the index. Round 4 is the most current cut; round 5 ships as commits on `main`
ahead of its design doc.

Practical pointers:
- Code conventions and tests: [`CONTRIBUTING.md`](CONTRIBUTING.md).
- Smoke checklist before you push: [`docs/SMOKE.md`](docs/SMOKE.md).
- Code of conduct: [Contributor Covenant 2.1](CODE_OF_CONDUCT.md).
- Security: [`SECURITY.md`](SECURITY.md).

The repo dogfoods itself — drop a `.resleeve-scope` marker (one already exists
at the repo root) and your local CC sessions inside the checkout get the
project plan + learnings auto-injected via the memory module. That's the
intended workflow for anyone working on resleeve itself.
