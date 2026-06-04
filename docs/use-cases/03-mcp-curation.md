# Use case: MCP curation — the agent writes its own memory

## The problem

Pre-written context is half the loop. The other half is the agent
discovering something mid-session that future sessions should know —
"the test runner needs `CGO_ENABLED=0`," "this dir ships a vendored
fork of foo." Without an in-band write path, those facts evaporate at
session end, or live on as `Bash` shell-outs to `resleeve learning
append …` that cost a confirmation prompt apiece.

The MCP server closes the loop: the human still triggers writes ("save
that"), but the agent executes them as first-class tool calls. Memory
becomes co-authored — human asks, agent writes, both read next session.

## How the MCP server fits

`resleeve mcp` is a stdio JSON-RPC 2.0 server. The host (Claude Code,
opencode, codex, etc.) spawns it once per session over `stdin`/`stdout`;
stderr is the verb's own log channel. Every tool call maps 1:1 to a
daemon HTTP endpoint, so MCP and CLI are interchangeable curation paths.

Two things ship over the wire on `initialize`:

1. **10 tools** — `resleeve_scope_set/get/list/delete`,
   `resleeve_plan_write/read/list`,
   `resleeve_learning_append/list`, `resleeve_context`. See the table
   below.
2. **An `instructions` field** — a standing prompt that primes the
   model on *when* to curate and *how* the scope/plan/learning model
   works. Plus the rolled-up context for the default scope, appended
   underneath. The model sees this on connect; the user never does.

## Walkthrough — Claude Code

Register the server (Claude's local config, scoped to one project):

```bash
claude mcp add resleeve resleeve mcp --scope $RESLEEVE_SCOPE
```

`$RESLEEVE_SCOPE` is optional. If unset, the server walks the same
lookup chain as the SessionStart hook (env var → `.resleeve-scope`
marker → `filepath.Base(cwd)`) and uses that as the default for any
tool argument the agent omits.

Restart Claude Code, or reconnect via `/mcp`. Confirm the server
loaded:

```text
/mcp
> resleeve  connected  10 tools
```

Now ask Claude to do work and explicitly invite a write:

```text
> Look at how this repo wires the SQLite store. If anything about
  the migration strategy surprises you, save it as a learning on
  scope resleeve/memory.
```

Claude reads the code, finds the surprising bit, and calls
`resleeve_learning_append` with `scope="resleeve/memory"` and a
one-sentence content body. Verify out-of-band:

```bash
resleeve learning list resleeve/memory
```

The new entry is there, with the id the tool returned. Next session
on any sleeve pointed at the same stack picks it up via the
SessionStart injection.

## Walkthrough — the standing prompt

The `instructions` payload is sourced from
[`internal/mcp/INSTRUCTIONS.md`](../../internal/mcp/INSTRUCTIONS.md).
The headline rule is **operator-driven curation**:

> Never auto-curate. Memory is a high-signal substrate, not a chat
> log. Only call the write tools when the user explicitly asks —
> phrases like "save this as a learning", "update the plan",
> "remember that…" are the trigger.

Other invariants: plans are named slots that overwrite (default
`_default`, preserve existing `## Now / ## Next` shape); learnings
are append-only with `supersedes_id` for corrections (never for
tidying); prefer existing scopes (use `resleeve_scope_list` to
discover, don't fabricate); underscore-prefixed scopes are reserved.

The principle: the agent has the tools at all times but only fires
them when there's a concrete, durable takeaway and the operator has
asked for it.

## Tool reference

| Tool | Required args | Optional args | Purpose |
| --- | --- | --- | --- |
| `resleeve_scope_set` | `path` | `kind`, `title`, `description`, `cwd`, `do_not_inherit` | Create/update a scope. |
| `resleeve_scope_get` | — | `path` (defaults to resolved scope) | Fetch one scope's metadata. |
| `resleeve_scope_list` | — | — | List the full scope tree. |
| `resleeve_scope_delete` | `path` | — | Delete a scope (409 if it has children). |
| `resleeve_plan_write` | `content` | `scope`, `slot` (default `_default`) | Overwrite a plan slot. |
| `resleeve_plan_read` | — | `scope`, `slot`, `inherit` | Read a slot; `inherit` walks ancestors. |
| `resleeve_plan_list` | — | `scope` | List all slots at a scope. |
| `resleeve_learning_append` | `content` | `scope`, `supersedes_id` | Append a learning (append-only). |
| `resleeve_learning_list` | — | `scope`, `inherit`, `include_superseded` | List learnings, optionally walking ancestors. |
| `resleeve_context` | — | `scope` | Rolled-up markdown (the SessionStart payload). |

Every `scope?` / `path?` falls back to the resolved default. Agents
that omit it get behavior tied to where the host was launched.

## Why this matters

The memory layer is shared across sleeves. A learning written via MCP
on the laptop shows up in the next SessionStart injection on the
workstation, the server, or any other host whose daemon syncs with
the same hub. The MCP write is not a per-machine note — it lands in
the same SQLite store the CLI reads, and replication carries it.
Every sleeve picks up where the last one left off without anyone
having to remember which machine the insight came from.

## Caveats

- **Subagents.** A Claude Code `Task`-tool spawn does **not** fire a
  fresh SessionStart hook, so a freshly spawned subagent has no
  injected context. If the subagent needs the standing state, pre-load
  it in the spawn prompt — e.g. paste the output of
  `resleeve context <scope>` into the task brief, or have the parent
  agent call `resleeve_context` and forward the result.
- **Auto-curation is off by design.** If you expect the agent to
  proactively save every interesting fact, you'll be disappointed —
  the standing prompt explicitly forbids that. Ask, and it writes.

## See also

- [`../USER_GUIDE.md`](../USER_GUIDE.md) — the MCP server section
  (§3) covers transport and registration in more depth.
- [`../ARCHITECTURE.md`](../ARCHITECTURE.md) — how the daemon, MCP
  server, and SessionStart hook share the same memory routes.
- [`01-memory-only.md`](01-memory-only.md) — running the memory layer
  without the capture hooks, for hosts that only want injection +
  curation.
