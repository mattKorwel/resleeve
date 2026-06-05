# resleeve

**Memory and identity that survive when the body doesn't.**

In Richard K. Morgan's *Altered Carbon*, your consciousness lives in a cortical "stack" and downloads into any compatible "sleeve." Bodies are interchangeable; the stack is what's you.

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

- **Captures** Claude Code (and, in development, opencode) sessions into local SQLite. Lossless event log, idempotent on replay.
- **Scope-keyed memory**: a hierarchical scope tree (`.resleeve-scope` markers walk the file tree), with named plans and append-only learnings per scope.
- **Auto-injects** the rolled-up scope context into fresh sessions via the SessionStart hook — your agent picks up your standing prompt without you typing it.
- **`--memory-only` mode** if you want the memory layer without persisting transcripts.
- **Sync** (optional) across machines: outbox-based push, 30 s pull, SSE fast tier for memory; all blobs AEAD-sealed under a KEK derived from your master password.
- **`resleeve resume`** replays a captured session into a fresh CLI process — same vendor (full fidelity) or cross-vendor (synthesized prime).
- **MCP server** so an agent in-session can curate its own memory (`resleeve_scope_set`, `resleeve_plan_write`, `resleeve_learning_append`, …).

## CLI support matrix

| | Claude Code | opencode | Codex | Gemini CLI |
|---|---|---|---|---|
| Hook-based capture | ✓ | ✗ (stub) | ✗ (planned) | ✗ (planned) |
| JSONL reconcile sweep | ✓ | ✗ | ✗ | ✗ |
| `resume` — same-vendor replay (full fidelity) | ✓ | ✗ | ✗ | ✗ |
| `resume` — prime synthesis (target side) | ✓ | ✓ | n/a | n/a |
| MCP client picks up `resleeve mcp` | ✓ | ⚠ untested | ✗ | ✗ |
| `--memory-only` mode | ✓ | n/a (no capture) | n/a | n/a |

**Legend.** ✓ shipped · ⚠ should work but unvalidated · ✗ not implemented · n/a not applicable

The Claude Code adapter is the reference implementation. The opencode adapter compiles + implements `Hydrate` for prime mode; capture and replay are stubs pending vendor stability. Codex / Gemini / others are interface-ready (the `Adapter` contract is in `internal/adapter/adapter.go`) but unimplemented.

Platform support: macOS, Linux, and Windows are CI-tested every PR. The platform-specific daemon process control (terminate, liveness, detached spawn) and native resume hand-off live in `//go:build`-tagged siblings; see the [round-8 Windows port notes](docs/design/round-8/windows-port.md).

## Docs

| | |
|---|---|
| [Quickstart](docs/QUICKSTART.md) | First 5 minutes: install, capture, write a plan, see it auto-inject. |
| [User guide](docs/USER_GUIDE.md) | Comprehensive how-to. Every verb, every flag, every mode. |
| [Use cases](docs/use-cases/) | Walkthroughs: memory-only, cross-machine sync, MCP curation, cross-CLI handoff, self-hosting. |
| [Architecture](docs/ARCHITECTURE.md) | Package layout, data flow, daemon/serve split. For contributors. |
| [Releasing](docs/RELEASING.md) | Cadence and how to cut a release. |
| [Smoke tests](docs/SMOKE.md) | Pre-release validation checklist. |
| [Changelog](docs/CHANGELOG.md) | Per-version notes (keep-a-changelog). |
| [Security review](docs/REVIEW_SECURITY.md) | Read-only audit (round 7); threat model + accepted risks. |
| [Code quality review](docs/REVIEW_QUALITY.md) | Read-only audit (round 7); findings + remediations. |
| [Design docs](docs/design/) | Round-by-round decisions log (rounds 1–4). Reading these is the fastest way to understand why something is the way it is. |

## Project status

Pre-1.0. Built and dogfooded by one author. The Claude Code adapter is live; the opencode adapter is a stub pending vendor stability. Single-user-per-`serve` deployment today; multi-user / team mode is on the round-8 roadmap.

See [docs/REVIEW_SECURITY.md](docs/REVIEW_SECURITY.md) for the security floor (zero critical findings; H1–H3 fixed; M3 deferred to multi-user). See [docs/REVIEW_QUALITY.md](docs/REVIEW_QUALITY.md) for the code-quality floor.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The shortest path: read [AGENTS.md](AGENTS.md) and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), pick an item from [docs/REVIEW_QUALITY.md](docs/REVIEW_QUALITY.md) or [docs/REVIEW_SECURITY.md](docs/REVIEW_SECURITY.md)'s open list, open a PR.

Security issues: see [SECURITY.md](SECURITY.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
