# Use case: `--memory-only` mode

## The problem this solves

You want the memory layer — scopes, plans, learnings, auto-injection on
SessionStart — but you do **not** want transcripts persisted to local disk.

- **Privacy.** No prompts, tool args, or tool results touch `~/.resleeve/data.db`.
- **Disk.** The `events` table is the largest in the store; skipping it
  bounds the data dir to your scope/plan/learning footprint.
- **Latency.** No per-tool-call daemon round-trip on `PostToolUse`.
- **Mental model.** "Rolled-up context only, no archive." `resleeve context
  <scope>` is the whole story.

The trade: `resleeve resume`, `session show`, and any
historical-grep-over-tool-args workflow have nothing to show.

## Trade-off vs. full mode

| Capability                                        | Full | `--memory-only` |
| ------------------------------------------------- | ---- | --------------- |
| SessionStart auto-injection (plans + learnings)   | yes  | yes             |
| Scope / plan / learning CRUD (CLI + MCP)          | yes  | yes             |
| Inheritance walk + `.donotinherit` boundaries     | yes  | yes             |
| `~/.resleeve/last-injected.md` audit trail        | yes  | yes             |
| Cross-machine memory sync (fast tier, SSE)        | yes  | yes             |
| `PostToolUse` / `UserPromptSubmit` / `Stop` hooks | yes  | **no**          |
| `resleeve resume <id>` / `session show <id>`      | yes  | **no rows**     |

## Walkthrough

Boot the daemon and install the bridge in memory-only mode:

```bash
resleeve up --memory-only
# [1/2] daemon started at http://127.0.0.1:53421
# [2/2] installed bridge (claude, memory-only)
# resleeve is up.
```

If a daemon is already running (e.g. previously up'd in full mode), flip just
the bridge without bouncing the daemon:

```bash
resleeve install-bridge --memory-only
# Installed claude bridge.
```

Confirm. `doctor` shows `bridge ✓`; cross-check the hooks file to verify only
`SessionStart` was installed:

```bash
resleeve doctor
jq '.hooks | keys' ~/.claude/settings.json
# [
#   "SessionStart"
# ]
```

Pin a scope, seed a plan, append a learning:

```bash
cd ~/dev/acme
resleeve scope set acme --kind project --title "Acme — privacy-sensitive client work"

cat <<'EOF' | resleeve plan write acme
## Current focus
Migrate billing service to the new schema. Do not touch the auth path.

## Constraints
Client NDA — never paste customer data into prompts or scratch files.
EOF

resleeve learning append acme \
  "Memory-only mode active for this client — no transcripts archived locally."
```

Open Claude Code in that directory. The SessionStart hook fetches the
rolled-up context and emits the wrapped envelope:

```text
resleeve: loaded scope acme (412 bytes)
```

Verify exactly what was injected:

```bash
cat ~/.resleeve/last-injected.md
# Resleeve memory for scope acme
#
# ## Plan (acme)
# ...
# ## Learnings (acme)
# - 2026-06-04: Memory-only mode active ...
```

## What does NOT happen

`PostToolUse`, `Stop`, and `UserPromptSubmit` are never registered. Every tool
call, every prompt, every session-end is invisible to the daemon. After a long
session, both tables are still empty:

```bash
sqlite3 ~/.resleeve/data.db "SELECT COUNT(*) FROM events;"
# 0
sqlite3 ~/.resleeve/data.db "SELECT COUNT(*) FROM sessions;"
# 0
```

The hook binary also self-enforces: the `--memory-only` flag is baked into the
`SessionStart` command itself, so even that one hook short-circuits its
`AppendEvents` path. Only `additionalContext` injection runs.

## When to use this mode

- NDA / data-handling rule that says "no transcripts."
- Disk-constrained host (thin VM, sandboxed container, ephemeral worktree).
- You only ever read `resleeve context` and `last-injected.md`; you've never
  used `resleeve resume`.
- You want MCP memory tools + inheritance walk without paying for an event
  archive you won't read.

## Switching back

The two modes are bidirectional and lossless for memory state — scopes /
plans / learnings live in their own tables and are untouched by either
install:

```bash
resleeve up
# [2/2] installed bridge (claude)
```

Full-mode `up` re-adds `UserPromptSubmit`, `PostToolUse`, `Stop` and strips
the `--memory-only` flag from `SessionStart`. Going the other direction
(`resleeve up --memory-only` on top of a full install) strips those three back
out — see `internal/adapter/claude/install.go` for the migration.

## See also

- [`../USER_GUIDE.md#capture-modes`](../USER_GUIDE.md) — full reference.
- [`../ARCHITECTURE.md`](../ARCHITECTURE.md) — daemon, bridge, and the
  event-vs-memory split.
