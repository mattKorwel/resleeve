# Round 1 — research, thinking, design

**Project name: `resleeve`.** Open-source self-hosted infrastructure for AI agent sessions + memory across ephemeral compute.

Stack: single Go binary + SQLite + git (two storage backends for two data domains).

## Deliverables

1. [`01-ori-baseline.md`](./01-ori-baseline.md) — review of the predecessor at `mattkorwel/ori`. Ori folds into resleeve as the `memory/` module.
2. [`02-cli-audit.md`](./02-cli-audit.md) — on-disk session-format audit per target CLI.
3. [`03-prior-art.md`](./03-prior-art.md) — competitors, adjacent inspiration (Atuin, asciinema, MCP, Langfuse, OTel-GenAI), emerging standards.
4. [`04-design.md`](./04-design.md) — initial synthesis. §1 is superseded; the rest applies. See decisions doc.
5. [`05-decisions.md`](./05-decisions.md) — current locked decisions, with changelog.

Round 2 begins in [`../round-2/`](../round-2/).

## TL;DR (current state)

- **Persistent agent infrastructure**, not a "session manager." Primary persona: fleet operator running N agents across N ephemeral sleeves (k8s pods, VMs, containers) via an orchestrator. The stack persists, sleeves don't.
- **Primary orchestrator target: SCION** (`GoogleCloudPlatform/scion`). Resleeve = "what the agent was thinking"; SCION's git worktree = "what the agent did." Paired, not competing.
- **Ori is folded in as `memory/`.** Two storage backends in one binary: git for memory (low-frequency, vault-durable) and SQLite + blob store for sessions (high-frequency events).
- **Identity federates** via SCION Agent Token under orchestration; falls back to password-derived key (Atuin model) for standalone use.
- **Zero-knowledge data-at-rest** in both modes. Server stores ciphertext only.
- **v1 adapters:** Claude Code → sst/opencode → OpenAI Codex → Gemini CLI. Aider + Open Interpreter to v2.
- **Schema aligned with OTel-GenAI** so sessions round-trip through observability tools.

## Open questions

Round-1 questions all resolved in [`05-decisions.md`](./05-decisions.md). Round-2 design questions surface in each journey doc.
