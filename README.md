# resleeve

> **STATUS:** pre-release / work in progress. Not yet usable.

Open-source self-hosted persistent infrastructure for AI coding-agent sessions. **Sleeves are fungible; the stack persists.**

Resleeve captures session content from AI coding-agent CLIs (Claude Code first; opencode and Codex on the v3 roadmap), stores it with zero-knowledge encryption, and lets you find, replay, share, and re-sleeve any session across machines and harnesses.

## The metaphor

From Altered Carbon — the *stack* is the persistent thing; the *sleeve* (a body, in the books; ephemeral compute, in our world) is fungible. AI sessions today are coupled to one CLI on one host. Resleeve makes them portable.

## Why this exists

The intersection of *multi-CLI session capture* + *zero-knowledge self-hosted sync* + *open, documented schema* is empty. SpecStory captures multiple CLIs but the backend is proprietary SaaS. Atuin sync is well-built but shell-only. CCManager is local-only. Anthropic's "Remote Control" is single-vendor, single-live-session.

Resleeve fills that gap.

## Status

Not yet usable. Designing in the open; implementation underway.

- [Design docs](docs/design/) — full architectural design across three rounds (research, fully-featured design, MVP slicing).
- [v1 cut plan](docs/design/round-3/00-v1-cut.md) — the first vertical slice: single-machine, Claude Code only.

## License

[Apache-2.0](LICENSE).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Code of conduct: [Contributor Covenant 2.1](CODE_OF_CONDUCT.md).

## Security

See [SECURITY.md](SECURITY.md) for vulnerability disclosure.
