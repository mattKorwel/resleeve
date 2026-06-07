# resleeve

**Memory and identity that survive when the body doesn't.**

The metaphor: your consciousness lives in a cortical "stack" and downloads into any compatible "sleeve." Bodies are interchangeable; the stack is what's you.

`resleeve` does the same for AI coding agents. Your **stack** — transcripts, plans, scope-keyed memory, learnings — is portable. Keep it on this laptop, host it on a server you control, or stand up `resleeve serve` for a team. Your **sleeves** — `claude`, `opencode`, whatever ships next — connect to wherever the stack lives. End-to-end encrypted: even your own server stores ciphertext only. Pick up where you left off, on any machine, in any CLI vendor.

Self-hosted. Apache-2.0. Single Go binary. SQLite under the hood.

---

## Try it

Download the binary for your platform from [the latest release](https://github.com/mattKorwel/resleeve/releases/latest), then:

```bash
# Start the daemon + install the Claude Code hook
resleeve up

# Confirm everything is healthy
resleeve doctor

# Open Claude Code in any directory
cd ~/dev/some-project
claude
# → SessionStart shows: resleeve: loaded scope "some-project" (N bytes)
```

That's it. The hook auto-captures the session; any plan or learning you've written for this scope auto-injects on session start; the next `claude` you open here picks up the same context.

Building from source? `go install github.com/mattkorwel/resleeve/cmd/resleeve@latest`.

## What it does

- **Captures** sessions from Claude Code, Codex, and opencode into local SQLite (Antigravity capture is experimental). Lossless event log, idempotent on replay.
- **Scope-keyed memory**: a hierarchical scope tree (`.resleeve-scope` markers walk the file tree), with named plans and append-only learnings per scope.
- **Auto-injects** the rolled-up scope context into fresh sessions via the SessionStart hook — your agent picks up your standing prompt without you typing it.
- **`--memory-only` mode** if you want the memory layer without persisting transcripts.
- **Sync** (optional) across machines: outbox-based push, 30 s pull, SSE fast tier for memory; all blobs AEAD-sealed under a KEK derived from your master password.
- **`resleeve resume`** replays a captured session into a fresh CLI process — same vendor (full fidelity) or cross-vendor (synthesized prime).
- **MCP server** so an agent in-session can curate its own memory (`resleeve_scope_set`, `resleeve_plan_write`, `resleeve_learning_append`, …).

## CLI support

| Capability | Claude Code | Codex | opencode | Antigravity |
|---|:---:|:---:|:---:|:---:|
| Session capture | ✓ hooks + reconcile | ✓ hooks + rollout reconcile | ✓ DB + SSE (bridge-free) | ⚠ file-watch |
| Native resume (same vendor) | ✓ JSONL replay | ✓ `codex resume` | ✓ `opencode import` | ✗ |
| Prime resume (cross vendor) | ✓ | ✓ | ✓ | ✓ |
| MCP memory server | ✓ | ✓ | ✓ | ✓ |

**Legend.** ✓ shipped · ⚠ experimental · ✗ not supported

All four adapters self-register through `internal/adapter/registry`; the `Adapter` contract lives in `internal/adapter/adapter.go`. Each is verified end-to-end against the real vendor binary. Antigravity is experimental — it reads an undocumented on-disk format and resumes via prime synthesis only.

Platform support: macOS, Linux, and Windows are CI-tested every PR. Platform-specific process control (terminate, liveness, detached spawn) and the native resume hand-off live in `//go:build`-tagged siblings; see the [round-8 Windows port notes](docs/design/round-8/windows-port.md).

## Docs

- [Quickstart](docs/QUICKSTART.md) — install → capture → plan → auto-inject, in 5 minutes
- [User guide](docs/USER_GUIDE.md) — every verb, flag, and mode
- [Use cases](docs/use-cases/) — task walkthroughs (memory-only, sync, MCP curation, cross-CLI handoff, self-hosting)
- [Architecture](docs/ARCHITECTURE.md) — package layout, data flow, daemon/serve split
- [Design docs](docs/design/) — round-by-round decision log (why things are the way they are)
- [Security review](docs/REVIEW_SECURITY.md) · [Code quality review](docs/REVIEW_QUALITY.md) — read-only audits
- [Releasing](docs/RELEASING.md) · [Smoke tests](docs/SMOKE.md) · [Changelog](docs/CHANGELOG.md)

## Project status

Pre-1.0, self-hosted, single Go binary. Four CLI adapters ship today: **Claude Code**, **Codex**, and **opencode** with full capture + native resume, and **Antigravity** (experimental) with capture + prime resume. Multi-tenant team mode — per-user "brains", shared brains, and key-based auth — is in active development (design: [docs/design/round-11](docs/design/round-11/)).

See the [security review](docs/REVIEW_SECURITY.md) and [code quality review](docs/REVIEW_QUALITY.md) for the audit floor.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Shortest path: read [AGENTS.md](AGENTS.md) and the [architecture doc](docs/ARCHITECTURE.md), pick an open item from the [quality](docs/REVIEW_QUALITY.md) or [security](docs/REVIEW_SECURITY.md) review, and open a PR. Security issues: [SECURITY.md](SECURITY.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).
