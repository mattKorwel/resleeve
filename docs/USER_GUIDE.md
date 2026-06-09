# resleeve — User Guide

The comprehensive how-to for the `resleeve` CLI. Covers daemon lifecycle, the
CLI adapters (Claude Code, Codex, opencode, Antigravity), the memory module
(scopes / plans / learnings + inheritance + plan versioning), the MCP curation
server, the two capture modes, cross-machine sync (standalone vs connected,
identity, pairing, KEK migration), team mode and shared brains, the encryption
model, the `resume` verb, manual-sync escape hatches, pre-existing data
backfill, and troubleshooting.

For a 5-minute setup, read [`QUICKSTART.md`](../QUICKSTART.md) first. For the
internal model, see [`ARCHITECTURE.md`](../ARCHITECTURE.md). For a
non-destructive validation checklist, see [`SMOKE.md`](./SMOKE.md). For the
full design history, see [`docs/design/`](./design/).

---

## 1. Daemon lifecycle

resleeve is a single Go binary that runs a small local HTTP daemon on
loopback. The daemon owns the SQLite store, the SessionStart hook endpoint,
the MCP-backing memory routes, and (if configured) a sync client against an
upstream `resleeve serve`. All CLI verbs hit the daemon over HTTP.

### 1.1 Data directory layout

Everything lives under `~/.resleeve/`:

```
~/.resleeve/
  data.db              SQLite store (sessions, events, scopes, plans, learnings, outbox)
  daemon.pid           PID of the running daemon (used by `down`, `doctor`)
  daemon.log           stdout/stderr of the background daemon
  endpoint             "127.0.0.1:PORT BEARER" pair — discovered by every CLI verb
  last-injected.md     audit trail: the most recent SessionStart additionalContext
  seal.key             (legacy) placeholder KEK — see §5.5
```

Override with `RESLEEVE_DATA_DIR` if you need to. Mode bits are `0700` for the
directory and `0600` for every file inside.

### 1.2 `resleeve up`

Boots the daemon as a session-leader background process and installs the
Claude Code hooks into `~/.claude/settings.json`.

```bash
resleeve up
# [1/2] daemon started at http://127.0.0.1:53421
# [2/2] installed bridge (claude)
# resleeve is up.
```

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--no-bridge` | false | Don't touch `~/.claude/settings.json` |
| `--memory-only` | false | Install only the SessionStart hook (see §4) |
| `--upstream URL` | `$RESLEEVE_UPSTREAM` | v2 sync upstream URL |
| `--upstream-token TOK` | `$RESLEEVE_UPSTREAM_TOKEN` | Legacy bearer for that upstream |

`up` is idempotent: if a daemon is already running it'll say so and exit 0.

### 1.3 `resleeve down`

Sends `SIGTERM` to the daemon and removes the bridge hooks. Add
`--keep-bridge` to leave the hooks in place (rare — only useful if you want to
flip back to `up` quickly without re-writing `settings.json`).

```bash
resleeve down
# [1/2] daemon stopped (pid 84217)
# [2/2] removed bridge (claude)
```

### 1.4 `resleeve doctor`

The unified status report. Hits every reachable surface and renders cards:

```
resleeve doctor
===============
  data dir         /Users/you/.resleeve
  daemon           ✓ running (pid 84217)
  endpoint         http://127.0.0.1:53421
  /v1/health       ✓ 200 OK
  sealer           ✓ unlocked
  bridge (claude)  ✓ installed in /Users/you/.claude/settings.json
  upstream         (standalone — no upstream configured)
  hook env         ✓ bridge + daemon both up
```

Exit code: **1** when bridge is installed but the daemon is down — the
silent-no-op state. Scripted callers (CI, smoke tests, `doctor && up` chains)
will catch this. On a TTY the warning is bright red.

Maintenance flags (see §8 for details):

- `--backfill-counts` — recompute `sessions.event_count` (one-shot).
- `--backfill-cwd` — repair `sessions.cwd` for reconcile-only legacy rows
  (daemon must be down).
- `--migrate-key` — interactive helper to remove the legacy `seal.key`.

### 1.5 `resleeve usage`

Storage stats:

```
data dir: /Users/you/.resleeve
total:    412.3 MiB
sessions: 87
events:   54213
```

### 1.6 `resleeve purge`

Wipes the entire data dir after explicit confirmation. Refuses to run while
the daemon is alive — run `down` first. Skip the prompt with `--yes` (for
scripted recovery flows).

```bash
resleeve purge
# This will delete /Users/you/.resleeve permanently.
# Type 'purge' to confirm: purge
# purged.
```

### 1.7 CLI adapters and `install-bridge`

resleeve supports four CLIs. `resleeve up` installs the Claude Code bridge by
default; to wire another CLI use `resleeve install-bridge --adapter <name>`.
Each adapter self-registers, so the rest of resleeve (scopes, plans, learnings,
sync) behaves identically regardless of which CLI you drive.

| Adapter (`--adapter`) | Session capture | Native resume (same CLI) | Prime resume (cross-CLI) | MCP memory tools |
| --- | --- | --- | --- | --- |
| `claude` | ✓ hooks + reconcile | ✓ JSONL replay | ✓ | ✓ |
| `codex` | ✓ hooks + rollout reconcile | ✓ `codex resume` | ✓ | ✓ |
| `opencode` | ✓ DB + SSE (no bridge needed) | ✓ `opencode import` | ✓ | ✓ |
| `antigravity` | ⚠ experimental (file-watch) | ✗ | ✓ | ✓ |

Legend: ✓ shipped · ⚠ experimental · ✗ not supported. Antigravity reads an
undocumented on-disk format and only resumes via the synthesized prime path.

```bash
resleeve install-bridge --adapter codex          # install the Codex bridge
resleeve install-bridge --adapter opencode       # opencode
resleeve install-bridge --adapter antigravity    # experimental
```

`install-bridge` flags:

| Flag | Meaning |
| --- | --- |
| `--adapter NAME` | Which CLI to wire (`claude` \| `codex` \| `opencode` \| `antigravity`; default `claude`) |
| `--memory-only` | Install only the SessionStart injection hook — no capture (see §4) |
| `--mcp` | **Also** register the `resleeve mcp` memory server in the CLI's MCP config (in addition to the capture hook) |
| `--mcp-only` | Register **only** the MCP memory server; skip the capture hook entirely |
| `--uninstall` | Remove resleeve's entries instead of installing (honors `--mcp` / `--mcp-only` to scope what's removed) |
| `--dry-run` | Print the resulting settings; don't write |

`--mcp` is how you let the in-session agent read and curate memory directly
(see §3). `--mcp-only` is useful for CLIs that pull memory rather than being
push-injected via a hook. To tear a bridge down:

```bash
resleeve install-bridge --adapter codex --uninstall            # remove hook + (if added) mcp
resleeve install-bridge --adapter opencode --mcp-only --uninstall  # remove only the mcp entry
```

---

## 2. The memory module

resleeve's memory model is three concepts: **scopes** (the hierarchy),
**plans** (overwritable named slots of markdown), and **learnings**
(append-only punctuated insights). Plans and learnings auto-inject into new
Claude Code sessions via the SessionStart hook, scoped by where you opened
the CLI.

### 2.1 Scopes

A scope is a slash-separated path: `resleeve`, `resleeve/mcp-server`,
`acme/billing/migrations`. The hierarchy is conceptual — there's no
filesystem constraint — but `cwd` walks discover them via marker files.

Each scope carries:

- `path` (the id),
- `kind` — one of `portfolio | program | project | dispatch | agent | other`
  (an empty string is also accepted),
- `title`, `description`, `cwd`,
- `do_not_inherit` — boundary reset flag (see §2.4).

Create / update a scope:

```bash
resleeve scope set resleeve \
  --kind project \
  --title "Resleeve" \
  --description "Self-hosted multi-CLI agent session+memory service"
```

List, inspect, delete:

```bash
resleeve scope list
resleeve scope get resleeve
resleeve scope delete resleeve   # refuses if children exist; delete them first
```

#### 2.1.1 The `.resleeve-scope` marker

Drop a file named `.resleeve-scope` in any directory to pin the working scope
for sessions started under that tree. The contents of the file (a single
line) is the marker scope; the resolved scope adds the relative subpath:

```
~/dev/acme/.resleeve-scope          # contents: "acme"
~/dev/acme/billing/                 # → scope "acme/billing"
~/dev/acme/billing/migrations/      # → scope "acme/billing/migrations"
```

Lookup order (matches both the SessionStart hook and the MCP server so they
agree):

1. `$RESLEEVE_SCOPE` env var — verbatim override.
2. Walk `cwd` ancestors for the nearest `.resleeve-scope` marker.
3. Fall back to `filepath.Base(cwd)`.

If the marker file is empty, the resolved scope is the relative path from the
marker's directory. If you set the marker to a deep path, you can pin a
working subtree wherever you want.

#### 2.1.2 Scope kinds

Kinds are advisory — they're a discrete enum for filtering and rendering, not
a permissions model:

| Kind | Use it for |
| --- | --- |
| `portfolio` | Topmost grouping (e.g. an org or persona) |
| `program` | A long-running initiative spanning many projects |
| `project` | A bounded codebase or service |
| `dispatch` | A short-lived task / ticket under a project |
| `agent` | A subagent's scratch scope |
| `other` | Escape hatch |

#### 2.1.3 `--do-not-inherit`

Set this on any scope to reset inheritance at that node. Children walk up to
this scope but stop — they don't see ancestors' plans or learnings.

```bash
resleeve scope set acme/billing/spike --kind dispatch --do-not-inherit \
  --title "Throw-away spike — fresh context"
```

The list output marks these with `⛔`.

### 2.2 Plans

Plans are **named slots** of markdown on a scope. The slot named `_default`
is the convention everything else assumes — most operators only ever touch
that one. Named slots are useful for sibling drafts (e.g. `next-quarter` vs
`_default`).

Plan writes are **overwriting** — the slot's content becomes whatever you
pass. Four input modes:

| Mode | Command |
| --- | --- |
| stdin (agent-driven default) | `echo '...' \| resleeve plan write <scope>` |
| `--content` | `resleeve plan write <scope> --content 'inline markdown'` |
| `--file` | `resleeve plan write <scope> --file plan.md` |
| `--edit` | `resleeve plan write <scope> --edit` (spawns `$EDITOR`) |

Read and list:

```bash
resleeve plan read resleeve                # _default slot
resleeve plan read resleeve --slot next    # named slot
resleeve plan read resleeve --inherit      # walks parents, prints stacked sections
resleeve plan list resleeve
```

`plan read --inherit` returns one `## Plan (scope)` block per ancestor, in
shallow→deep order — the same format the SessionStart hook bakes into
`additionalContext`.

#### 2.2.1 Plan version history and safe concurrent edits

Plans keep **version history**: every write creates a new version rather than
discarding the old content. Each write also records **who** made it (the
author), which matters once a brain is shared across a team (§10).

Writes use **optimistic concurrency**. `plan write` reads the current version
first and writes against it. If someone else advanced the plan since you last
read it, your write is **rejected** — resleeve prints the current version and
tells you to reconcile, instead of silently overwriting their change:

```bash
echo "..." | resleeve plan write acme-backend
# plan write: conflict — acme-backend [_default] was advanced to version 7
# since you last read it.
# Re-read with `resleeve plan read acme-backend`, merge, and retry
# (or pass --force to overwrite).
```

The fix is to re-read, merge in your change, and write again — or pass
`--force` to overwrite the current version deliberately. `plan read` prints the
version it returned (to stderr, so stdout stays pure content) so you can see
exactly what you're writing against.

```bash
resleeve plan read acme-backend          # stderr: "# plan acme-backend [_default] version 7"
# ...edit...
echo "merged content" | resleeve plan write acme-backend          # writes version 8
echo "merged content" | resleeve plan write acme-backend --force  # overwrite, skip the check
```

On a single-user machine you'll rarely see a conflict; it's the safety net for
shared brains where two people (or two of your own devices) edit the same plan.

### 2.3 Learnings

Learnings are **append-only**. Use them for durable, punctuated conclusions
worth carrying forward — bugs you don't want to re-debug, design decisions
you don't want to relitigate, gotchas the next session needs. Don't use them
as a chat log.

```bash
resleeve learning append resleeve "Event seq = ts.UnixNano() — same-ns ties broken by event_uuid asc."
resleeve learning append resleeve - <<EOF        # multiline via stdin
Memory-only mode: ./resleeve up --memory-only installs only the SessionStart hook.
SessionStart still injects context; no events are persisted.
EOF
```

To correct an old entry, **soft-supersede** it. The original stays in
storage for the audit trail but is hidden from default reads:

```bash
resleeve learning append resleeve "Corrected: ..." --supersedes lng_abc123
```

Read:

```bash
resleeve learning list resleeve                       # default
resleeve learning list resleeve --inherit             # walks ancestors
resleeve learning list resleeve --include-superseded  # show retired entries too
```

The `--include-superseded` flag is the only way to see soft-superseded
entries; `--inherit` collapses parent learnings into the same stream.

### 2.4 Inheritance — the walk

Plans and learnings inherit down the scope tree. When a new Claude Code
session opens at scope `acme/billing/migrations`, the SessionStart hook walks
up the chain — `acme/billing/migrations` → `acme/billing` → `acme` — and
concatenates each ancestor's `_default` plan + learnings.

`.donotinherit` boundary reset: if any node in the chain has
`do_not_inherit=true`, the walk drops everything above it. The boundary node
itself is included; only its ancestors are skipped.

```
chain (deep→shallow):  team-foo/project-alpha/auth  →  team-foo/project-alpha (donotinherit)  →  team-foo
walk result:           [team-foo/project-alpha, team-foo/project-alpha/auth]
                       (team-foo dropped)
```

### 2.5 `resleeve context <scope>`

Prints the rolled-up markdown the bridge would inject into Claude Code on
SessionStart. Same body that lands in `~/.resleeve/last-injected.md` after
every session opens (see §9).

```bash
resleeve context resleeve
# Resleeve memory for scope resleeve
#
# ## Plan (resleeve)
# ...
# ## Plan (resleeve/docs-user-guide)
# ...
# ## Learnings (resleeve)
# - 2026-06-04: ...
```

The body composes parent plans + child plan + learnings, shallow→deep,
truncating at any `.donotinherit` boundary. The daemon may cap very large
bodies; in practice you'll get the full chain unless a single ancestor's
plan is huge.

---

## 3. The MCP server (in-agent memory curation)

resleeve ships an MCP (Model Context Protocol) server so agents can curate
memory in-band — no `Bash` tool shell-outs, no copy/paste. Every MCP tool
maps 1:1 to a daemon HTTP endpoint, so calling the tool is identical to
running the equivalent CLI verb.

### 3.1 Why it exists

Before MCP, the curation pattern was: agent suggests, user types
`resleeve plan write …` into the chat, Claude Code runs it via Bash. That
worked but it cost a Bash invocation and a confirmation prompt per
operation. The MCP server collapses that to one in-process tool call per
curation, with an `instructions` field the host loads on connect.

### 3.2 Registering the MCP server

The simplest path is to let `install-bridge` write the MCP entry into the CLI's
own MCP config for you. Add `--mcp` to register the memory server alongside the
capture hook, or `--mcp-only` to register just the memory server:

```bash
resleeve install-bridge --adapter claude --mcp        # capture hook + MCP server
resleeve install-bridge --adapter codex  --mcp        # same for Codex
resleeve install-bridge --adapter opencode --mcp-only # MCP server only
```

This auto-registers `resleeve mcp` into each CLI's MCP config, so the in-session
agent gets the `resleeve_*` tools without you editing config by hand. Remove it
with `--uninstall` (see §1.7).

You can also register manually — the binary speaks JSON-RPC 2.0 over
stdin/stdout, so any MCP host can launch it:

```bash
claude mcp add resleeve resleeve mcp --scope $RESLEEVE_SCOPE   # manual, Claude Code
resleeve mcp                                                   # the raw stdio server
```

`$RESLEEVE_SCOPE` is optional. If unset, the server resolves a default scope
at boot using the same walk the SessionStart hook does (env var → marker file
→ `filepath.Base(cwd)`). That default is the auto-loaded context the
`instructions` field carries.

### 3.3 The 10 tools

| Tool | Args | Maps to |
| --- | --- | --- |
| `resleeve_scope_set` | `path`, `kind?`, `title?`, `description?`, `cwd?`, `do_not_inherit?` | `scope set` |
| `resleeve_scope_get` | `path?` (defaults to resolved scope) | `scope get` |
| `resleeve_scope_list` | (none) | `scope list` |
| `resleeve_scope_delete` | `path` | `scope delete` |
| `resleeve_plan_write` | `scope?`, `slot?`, `content` | `plan write` |
| `resleeve_plan_read` | `scope?`, `slot?`, `inherit?` | `plan read` |
| `resleeve_plan_list` | `scope?` | `plan list` |
| `resleeve_learning_append` | `scope?`, `content`, `supersedes_id?` | `learning append` |
| `resleeve_learning_list` | `scope?`, `inherit?`, `include_superseded?` | `learning list` |
| `resleeve_context` | `scope?` | `context` |

Every `scope?` / `path?` argument falls back to the resolved default scope.
Agents that drop the argument get sensible behavior tied to where the host
was launched.

### 3.4 The `instructions` field — two layers

When a host calls `initialize`, the server responds with an `instructions`
string. Two sections, separated by a header:

1. **Standing INSTRUCTIONS** — never auto-curate; plans are slots and
   overwrite; learnings are append-only with soft-supersede; scope hygiene
   rules; the scope-kind enum. Full text in
   [`internal/mcp/INSTRUCTIONS.md`](../internal/mcp/INSTRUCTIONS.md).
2. **Auto-loaded scope context** — the rolled-up markdown for the default
   scope (the same body `resleeve context <default>` would emit). This
   means the agent boots already knowing the inherited plan + learnings,
   without having to call `resleeve_context` first.

This is the entire reason agents don't need to ask "catch me up" — the MCP
init handshake puts them inside the operator's context already.

### 3.5 Transport: stdio vs HTTP

Only **stdio** ships today. JSON-RPC 2.0 over `stdin`/`stdout`; stderr is the
verb's own log channel (`mcp.StderrLogger`). The HTTP transport is reserved
for a future debug surface but the in-process MCP servers every host runs
use stdio, so there's no rush.

---

## 4. Capture modes

resleeve installs a small set of Claude Code hooks. There are two modes:
**full** (the default — captures everything) and **memory-only** (only injects
context on SessionStart, never persists an event).

### 4.1 Full mode (default)

Four hooks land in `settings.json`:

- `SessionStart` — fetches `additionalContext` from the daemon, emits the
  envelope, writes `~/.resleeve/last-injected.md`.
- `UserPromptSubmit` — captures the human turn.
- `PostToolUse` — captures every tool call's args + result.
- `Stop` — captures session-end metadata.

Every captured event lands in the local SQLite store. The events table is
the highest-volume table in `data.db`.

```bash
resleeve up                          # full mode (default)
```

### 4.2 `--memory-only` mode

Installs **only** the SessionStart hook. The hook command itself is baked
with `--memory-only`, so even though it still runs on every session start,
it skips the `AppendEvents` path entirely. Zero rows land in `sessions` or
`events` — but the `additionalContext` injection still fires normally.

```bash
resleeve up --memory-only
# [2/2] installed bridge (claude, memory-only)
```

Switching is fully toggleable:

```bash
resleeve up                       # full mode — re-installs the other 3 hooks
resleeve up --memory-only         # memory-only mode — strips PostToolUse/Stop/UserPromptSubmit
```

Memory CRUD (scopes/plans/learnings via MCP **and** CLI) keeps working in
either mode — those go through `/v1/scope`, `/v1/plan`, `/v1/learnings`, not
the events path.

### 4.3 When to pick which

| Concern | Pick |
| --- | --- |
| Privacy — don't persist prompts or tool args | `--memory-only` |
| Disk — `events` is the biggest table | `--memory-only` |
| Latency — no per-tool-call daemon round-trip | `--memory-only` |
| Mental model — "rolled-up context only, no archive" | `--memory-only` |
| You want `resleeve resume` to replay sessions | full mode |
| You want to grep tool args across history | full mode |
| You want `session show <id>` to render anything | full mode |

Memory-only is the "I want the inheritance + injection feature, but not the
session archive" cut. Full mode is the everything-on default.

---

## 5. Cross-machine sync

### 5.1 Standalone mode

The default. No upstream, nothing leaves the box. Everything ships locally
in `~/.resleeve/data.db`. `resleeve sync push/pull` will return a typed
`ErrNoUpstream` (HTTP 409 with body `no upstream configured`) — that's
expected, not an error.

```bash
resleeve up                           # no --upstream → standalone
resleeve sync push
# daemon 409 Conflict: no upstream configured ...: agent: no upstream configured
```

### 5.2 Connected mode

Point a daemon at a `resleeve serve` instance and it starts a sync client.

```bash
resleeve up --upstream https://serve.example.com --upstream-token <bearer>
# or set $RESLEEVE_UPSTREAM and $RESLEEVE_UPSTREAM_TOKEN
```

What syncs, and at what tier:

| Kind | Tier | Cadence |
| --- | --- | --- |
| `sessions` | slow | push-on-commit + 30s pull poll |
| `events` | slow | push-on-commit + 30s pull poll |
| `memory` (scopes / plans / learnings) | fast | SSE near-real-time |

Slow tier batches via an on-disk outbox; failures retry with exponential
backoff. Fast tier streams via SSE so a learning written on machine A is
visible on machine B in seconds.

Critical invariant: the pull path bypasses `IngestBatch`, so re-pulling a
row never loops it back to upstream
(`TestSyncClient_PullThenPushDoesNotLoop` guards this).

### 5.3 Identity — `register` / `login` / `logout`

The legacy model used a single shared bearer token plus a shared
`seal.key` file. Newer versions replace that with per-device identity tokens and a
master-password-derived KEK.

```bash
resleeve register --upstream https://serve.example.com
# email: you@example.com
# master password: ...
# confirm password: ...
# registered ✓
#   user id     usr_abc123
#   device id   dev_xyz789
#   email       you@example.com
#
# RECOVERY KEY — save this somewhere safe. It is shown ONCE.
# If you lose your password AND this key, encrypted content is unrecoverable.
#
#   1234-5678-9012-3456-7890-1234
#
# Next: `resleeve login` to unlock the local daemon sealer.
```

**The recovery key is shown once.** Server-side blobs are encrypted with a KEK
that's derived from your master password via Argon2id. Lose both the
password and the recovery key and the data is gone.

`login` runs the verifier-challenge handshake against the server, unwraps
your KEK locally (server never sees plaintext), persists the device token in
the OS keychain, and installs the KEK into the local daemon via
`/v1/seal/unlock`. Subsequent captures encrypt under that KEK.

```bash
resleeve login --upstream https://serve.example.com
# email: you@example.com
# master password: ...
# login: daemon sealer installed ✓
# login ✓
```

`logout` revokes the device token on the server, removes it from the
keychain, and locks the local daemon's sealer (sync push/pull will park
until a fresh `login`).

```bash
resleeve logout --upstream https://serve.example.com
# logout ✓
```

#### 5.3.1 The OS keychain

Device tokens live in the OS keychain, not on disk:

| OS | Backend |
| --- | --- |
| macOS | native Keychain via `zalando/go-keyring` |
| Linux | `libsecret` (GNOME Keyring / KWallet / etc.) |
| Windows | Credential Manager |

The legacy file backend is automatically migrated on first
`login` for a given `(upstream, email)` pair — upgraders don't lose tokens.

### 5.4 Pairing — `pair invite` / `pair accept`

Pairing onboards a second device without re-typing the master password.
Device A (already logged in) publishes a one-time envelope wrapped under a
short pair code. Device B claims it.

On device A:

```bash
resleeve pair invite --upstream https://serve.example.com
# email: you@example.com
# master password: ...
# pair code published ✓
# share these with the device being onboarded — they expire in ~5 minutes:
#
#   code id     2f1c9a8b...
#   pair code   K7Q4-9XYZ-MTPB
#   expires     2026-06-04T17:30:00Z
#
# on the new device: resleeve pair accept --code-id=2f1c9a8b... --upstream=https://serve.example.com
```

On device B:

```bash
resleeve pair accept --code-id=2f1c9a8b... --upstream=https://serve.example.com
# pair code: K7Q4-9XYZ-MTPB
# email (for local label): you@example.com
# pair accept: daemon sealer installed ✓
# pair accept ✓
#   user id     usr_abc123
#   device id   dev_def456
#
# Initial sync will start automatically.
```

The pair code is 60 bits of entropy formatted as three base32 quads. Default
TTL is 5 minutes (server cap is 10 minutes). The briefing said "60s" — the
implementation ships a longer window because passing a code to another
device over an out-of-band channel within 60s is unrealistic. Adjust with
`--ttl 60s` if you want the tight bound.

Once accepted: device A's KEK is now installed on device B. Both machines
sync against the same upstream identity. Pair invite revokes its own
ephemeral login token on the way out (see
`docs/design/round-4/`).

### 5.5 `migrate-key` — placeholder → real KEK

An earlier version shipped envelope encryption with a placeholder: a random 32-byte AES
key at `~/.resleeve/seal.key`. Newer versions retire that file in favor of a KEK
derived from your master password. If you have existing upstream-encrypted
data captured under the old `seal.key`, run `migrate-key` ONCE to re-encrypt
all server-side blobs under the new KEK:

```bash
resleeve down                                # daemon-alive guard refuses if up
resleeve migrate-key --upstream https://serve.example.com
# email: you@example.com
# master password: ...
# About to re-encrypt all upstream rows for you@example.com under the new master-password KEK.
# This is idempotent and safe to rerun, but cannot be undone without the old key file.
# Type 'migrate' to confirm: migrate
#   sessions  migrated=42  skipped=0
#   events    migrated=18372  skipped=0
#   memory    migrated=11  skipped=0
# migrate-key: re-encrypted 18425 row(s); 0 already on new KEK or unreadable.
# migrate-key: daemon sealer rotated ✓
#
# Next: verify a `resleeve session list` / `resleeve resume` works, then
# delete the legacy placeholder with:
#   rm /Users/you/.resleeve/seal.key
```

Why the daemon-alive guard: a live daemon under the OLD sealer could push
mid-migration, producing mixed-key blobs upstream. Operator must `resleeve
down` first. The verb refuses to start if it detects a live PID.

`migrate-key` is **idempotent**: rerunning it after a partial completion is
safe. Rows already re-encrypted under the new KEK fail `Open` with the old
sealer and are silently skipped.

After you've verified the migration worked, clean up the local file:

```bash
resleeve doctor --migrate-key
# Found legacy placeholder at:
#   /Users/you/.resleeve/seal.key  (size=32, mode=600)
# ...
# Delete this file now? Type 'yes' to confirm: yes
# removed ✓
```

`doctor --migrate-key` also refuses while the daemon is alive (same
footgun).

---

## 6. `resleeve resume`

`resume` re-sleeves a captured session into a fresh CLI instance. Two render
modes (with a third "auto" default).

### 6.1 Replay mode (`--mode replay`)

Writes the captured session's events back into the target CLI's native
on-disk format — for Claude Code that's the `.jsonl` transcript under
`~/.claude/` — then hands off to the CLI's own resume (`claude --resume`,
`codex resume`, `opencode import`) so you land inside that CLI as if you'd run
it yourself. Replay is full-fidelity but only works when the captured session
and the target CLI are the **same** vendor. Claude Code, Codex, and opencode
each support native replay; Antigravity does not (it's prime-only).

### 6.2 Prime mode (`--mode prime`)

Synthesizes a "## Plan" preamble from the captured session's scope's
rolled-up memory context (the same body `resleeve context <scope>` emits)
plus a brief recap, and pipes that as a first message into a fresh native
session. Works **across CLIs** — a Claude session can be primed into
opencode, codex, etc. — because we don't try to reproduce native state.

Prime mode pulls scope context via `GET /v1/context`. Empty scopes or the
sentinel `"unknown"` (used when the source CLI never reported one) are
skipped — prime renders with an empty plan in that case.

### 6.3 Auto (default)

`auto` picks replay when the source and target CLIs match and the target
adapter supports native replay, and prime otherwise (cross-vendor, or a target
like Antigravity that has no native replay path).

### 6.4 Flags

| Flag | Meaning |
| --- | --- |
| `--cli NAME` | Override the target CLI (`claude`, `codex`, `opencode`, `antigravity`) |
| `--mode replay\|prime\|auto` | Force a render mode |
| `--cwd DIR` | Override the working directory the resumed CLI lands in |
| `--print` | Don't `exec` — print the native resume command instead |
| `--no-pull` | Skip the on-demand sync pull (see §6.5) |

### 6.5 The on-demand pull

Before resolving the session id locally, `resume` does a best-effort
`POST /v1/sync/pull-now` so the freshest upstream rows land first. A session
captured on machine A 10 seconds ago is resumable on machine B without
waiting for the 30s polling tick.

If the pull times out or upstream is unreachable, `resume` falls back to
local state and prints a warning to stderr. See
[`docs/design/round-4/03-rehydrate-ux.md`](./design/round-4/03-rehydrate-ux.md) §"Failure modes".

### 6.6 Examples

```bash
resleeve session list                                # find a session
resleeve resume ses_abc123                           # auto-mode, exec into claude
resleeve resume ses_abc123 --mode prime              # force prime
resleeve resume ses_abc123 --cli opencode            # cross-CLI dispatch
resleeve resume ses_abc123 --cwd ~/dev/elsewhere     # land in a different dir
resleeve resume ses_abc123 --print                   # show the cmd, don't exec
# claude --resume ses_abc123
```

---

## 7. Manual sync escape hatch

`resleeve sync push` and `resleeve sync pull` hit
`POST /v1/sync/push-now` / `POST /v1/sync/pull-now`. Useful when you don't
want to wait for the 1s drain ticker or the 30s pull tick.

```bash
resleeve sync push
# pushed 3 rows

resleeve sync pull
# pulled 0 sessions, 7 events, 2 memory
```

Both require the daemon to be running with `--upstream` configured. In
standalone mode they return a 409 with body `no upstream configured` — the
client wraps that as a typed `agent.ErrNoUpstream` so scripts can match on
it via `errors.Is`.

When to reach for these:

- You want to validate a cross-machine smoke test without waiting 30s.
- You captured a session on the road, opened a laptop on home wifi, and
  want machine B to see it *right now*.
- You're debugging an outbox stall and want to force a drain.

The background loops keep running regardless; manual push/pull is purely an
"impatience" tool.

---

## 8. Pre-existing data backfill

Two one-shot maintenance passes for pre-existing rows captured before a
specific fix landed:

### 8.1 `doctor --backfill-counts`

Recomputes `sessions.event_count` for every row. Cleanup for sessions
captured before the fix (`fde1c40`) wired `SyncEventCount` into
`IngestBatch` — older rows stayed at `event_count=0` until their next batch
arrived. Safe to re-run; `SyncEventCount` is `COUNT(*)` over events, not a
delta.

```bash
resleeve doctor --backfill-counts
#   scanned 100/240 sessions...
#   scanned 200/240 sessions...
# recomputed 240 session event_counts.
```

Can run with the daemon up — the SyncEventCount path is concurrency-safe.

### 8.2 `doctor --backfill-cwd`

Repairs `sessions.cwd` for pre-existing reconcile-only sessions (polish punch
list #6). Refuses to run while the daemon is up — the migration performs
raw `UPDATE`s that bypass the ingest pipeline, and a live daemon could
re-overwrite rows from a concurrent reconcile sweep.

```bash
resleeve down
resleeve doctor --backfill-cwd
# ...
# scanned: repaired 17, skipped 4
resleeve up
```

---

## 9. Troubleshooting

### 9.1 Silent injection failure

**Symptom:** You opened a fresh Claude Code session and saw no
`Resleeve memory for scope ...` notice. `doctor` shows `bridge ✓` and
`daemon ✗`.

**Cause:** The bridge hook fires on every SessionStart, but with the daemon
down it gets connection-refused and emits nothing. Claude Code sees no
`additionalContext` and proceeds as if the feature didn't exist.

**Fix:** `resleeve up`. The `hook env` card in `doctor` will yell at you
loudly (bright red on a TTY, non-zero exit code) when it detects this state.

### 9.2 Auditing what got injected

`~/.resleeve/last-injected.md` is the audit trail. After every successful
SessionStart injection, the hook persists the exact body it emitted as
`hookSpecificOutput.additionalContext`:

```bash
cat ~/.resleeve/last-injected.md
# Resleeve memory for scope resleeve/docs-user-guide
#
# ## Plan (resleeve)
# ...
```

If a session opened with no notice, check this file before chasing a daemon
issue — the file's `mtime` tells you whether the hook ran at all.

### 9.3 Sealer locked

**Symptom:** `doctor` shows `sealer ✗ locked (run resleeve login)`. Sync
push/pull stalls; new captures land in the outbox but never drain.

**Cause:** Daemon started without a KEK installed. In current versions, the
daemon does NOT auto-load `seal.key` — you must `login` to install the KEK.

**Fix:**

```bash
resleeve login --upstream https://serve.example.com
# ...
# login: daemon sealer installed ✓
```

If you're still on the legacy `seal.key` model and don't have an upstream,
this card will surface — but sync is a no-op anyway, so capture keeps
working.

### 9.4 Standalone mode 409 on `sync push/pull`

**Expected.** No upstream configured → daemon returns HTTP 409 with body
`no upstream configured`. The CLI wraps this as `agent.ErrNoUpstream`. If
you actually want sync, run `resleeve up --upstream URL --upstream-token
TOK` (or set `$RESLEEVE_UPSTREAM` / `$RESLEEVE_UPSTREAM_TOKEN` and re-up).

### 9.5 Hook fires but nothing happens

Three layers to check, in order:

1. `~/.resleeve/last-injected.md` — did the hook run at all? (file mtime)
2. `doctor` — `hook env` card.
3. `~/.resleeve/daemon.log` — any errors during the daemon's SessionStart
   handler?

If the file exists with recent mtime but Claude Code shows no notice,
suspect the host-side envelope shape. resleeve established that Claude
Code expects:

```json
{
  "systemMessage": "resleeve: loaded scope X (N bytes)",
  "hookSpecificOutput": {
    "hookEventName": "SessionStart",
    "additionalContext": "..."
  }
}
```

Anything else gets silently dropped.

### 9.6 "Daemon down" after `down`

That's how it works — `resleeve down` removes both the daemon and the
bridge. Running `doctor` afterward shows the post-down state, not the
silent-no-op state. If you want to validate the silent-no-op guard specifically, kill the PID
without removing the bridge:

```bash
kill -9 $(cat ~/.resleeve/daemon.pid) && rm ~/.resleeve/daemon.pid
resleeve doctor   # → loud silent-no-op warning, exit 1
```

(That stanza appears in `docs/SMOKE.md` as well — it's the standard
recovery validation.)

---

## 10. Team mode and shared brains

`resleeve serve` is **multi-user by default**. Each account gets its own
private memory namespace — a **brain** — and a group can share one. This is how
several people drive their AI agents off the same plans and learnings while
keeping unrelated work private.

For an end-to-end walkthrough (operator stands up the server, members join,
someone creates and populates a shared brain) see
[`use-cases/06-team-shared-memory.md`](./use-cases/06-team-shared-memory.md).
This section is the verb/flag reference.

### 10.1 What a brain is

A brain is a memory namespace: its own scopes, plans, and learnings. Every
account has a personal brain. A **shared brain** is created by a member, who
becomes its **owner**; the owner adds other members, and any member can point a
machine at it. When a machine's active brain is a shared brain, that machine's
sessions read and write that shared namespace — so a plan one member writes
auto-injects into every member's sessions, and learnings record who added them.

### 10.2 Standing up a team server

```bash
resleeve serve \
  --addr 127.0.0.1:7860 \
  --root /var/lib/resleeve/serve \
  --master-key /var/lib/resleeve/master.key
```

| Flag | Meaning |
| --- | --- |
| `--master-key FILE` | File holding the 32-byte server master key (hex or base64). **Required in team mode.** Also reads `$RESLEEVE_SERVER_MASTER_KEY`. |
| `--single-tenant` | Solo self-host: no brains, flat behavior, client end-to-end encryption (master key optional). The old single-user model. |
| `--addr` | Listen address (default `127.0.0.1:7860`; non-loopback prints a TLS warning) |
| `--root` | Blob storage root |
| `--auth-token` | Legacy single bearer (per-device tokens are the norm; see §5.3) |
| `--dsn` | SQLite DSN for the identity database |

The master key wraps each brain's at-rest encryption key (§11). **A team server
refuses to boot without one** — clients send plaintext under the default policy,
so the server's at-rest key is the only encryption layer and resleeve won't
persist plaintext silently. Generate one with `openssl rand -hex 32`, store it
mode `0600`, and back it up out of band: lose it and every brain's at-rest data
is unrecoverable.

Pass `--single-tenant` for a solo box: no brain partitioning, the client
encrypts end-to-end (zero-knowledge — the server only sees ciphertext), and the
master key is optional. That's the behavior covered in
[`use-cases/05-self-hosted-serve.md`](./use-cases/05-self-hosted-serve.md).

### 10.3 Joining and identity

Members join exactly as in §5.3: `register` once per account, `login` to unlock
the local sealer, `up --upstream` to bring the daemon online. A second device
for the same person uses pairing (§5.4) instead of a second `register`.

Identity is **per-device** — each device carries its own token, so a lost laptop
is revoked with `resleeve logout` without disturbing the account's other devices
or the rest of the team.

### 10.4 `resleeve brain` — managing brains

The management verbs talk to the upstream with your device token; each takes
`--upstream URL` (or `$RESLEEVE_UPSTREAM`) and an optional `--email` for the
keychain lookup.

```bash
resleeve brain create <name>                 # create a shared brain (you own it) → prints the brain id
resleeve brain list                          # brains you belong to (active one starred)
resleeve brain member list <brain-id>        # list a brain's members
resleeve brain member add  <brain-id> <user-id>   # owner only
resleeve brain member rm   <brain-id> <user-id>   # owner only
```

`brain create` prints the new brain id (e.g. `brn_…`) to stdout. `brain list`
shows id, name, kind, your role, and which brain is active. User ids (`usr_…`)
are printed by `register` / `login`; the owner needs them to add members.

### 10.5 `resleeve brain use` — selecting the active brain

`brain use` is **local** — it sets which brain this machine's daemon syncs
against. It does not touch the server.

```bash
resleeve brain use <brain-id>       # make this machine read/write the shared brain
resleeve brain use --clear          # revert to your personal brain
```

The running daemon samples the active brain at startup, so **restart it**
(`resleeve down && resleeve up`) after switching for the change to take effect.
The command prints this reminder.

### 10.6 Shared plans and learnings

Once a shared brain is active, the ordinary memory verbs (§2) operate on it.
Plans inject into every member's sessions on SessionStart; learnings the team
appends carry forward and record their author. Concurrent plan edits are guarded
by optimistic concurrency (§2.2.1): a stale write is rejected with the current
version so two members never silently clobber each other.

---

## 11. Encryption model

resleeve encrypts data both in transit and at rest. Which layer protects your
data depends on whether you run a team server, a solo `--single-tenant` server,
or no upstream at all.

| Layer | When it applies | What it protects | Who can read plaintext |
| --- | --- | --- | --- |
| **In transit (TLS)** | Any upstream | The connection to `resleeve serve` (operator fronts it with a TLS reverse proxy) | Transport only |
| **Server-side at rest** (default on a team server) | Multi-user `serve` | Each brain's data, encrypted at rest under a per-brain key wrapped by the operator's master key | The server (to fan a shared brain out to its members); the operator holds the master key |
| **End-to-end** (a personal brain opted in on a team server) | `brain encrypt <id> e2e` | That brain's data; the server stores ciphertext only | Only that member |
| **End-to-end** (solo) | `serve --single-tenant` | All data; the client seals before it leaves the box | Only the account owner |

### 11.1 In transit

`resleeve serve` speaks plaintext HTTP and is meant to sit on loopback behind a
TLS-terminating reverse proxy (Caddy, nginx). Binding a non-loopback `--addr`
prints a startup warning for exactly this reason. See
[`use-cases/05-self-hosted-serve.md`](./use-cases/05-self-hosted-serve.md).

### 11.2 Server-side at rest (team default)

On a team server, the default policy is **server-side**: clients send plaintext
over TLS, and the server encrypts each brain's data at rest under a per-brain
key that is itself wrapped by the operator's master key (`--master-key`). This
is what lets the server hand a shared brain's contents to every member of its
group. A member only ever sees the brains they belong to; the bytes on disk are
ciphertext. Because clients send plaintext, the master key is **mandatory** — a
team server won't boot without one.

### 11.3 End-to-end (solo `--single-tenant`)

A solo server runs zero-knowledge: the **client** seals every blob (AES-256-GCM
under a KEK derived from your master password via Argon2id) before it crosses
the wire, and the server stores ciphertext only. The master password and the
unwrapped KEK never leave the client — a compromised server can reorder blobs
and see operational metadata but cannot decrypt anything. This is the model
detailed in §5 and `use-cases/05`.

### 11.4 Opting a personal brain back to end-to-end

On a team server, a member can make a **personal** brain zero-knowledge — so
even the operator can't read it — by switching its policy:

```bash
resleeve brain encrypt <brain-id> e2e          # zero-knowledge; server stores ciphertext only
resleeve brain encrypt <brain-id> server-side  # switch back to server-readable
```

End-to-end is **only valid on a personal brain** — the server rejects `e2e` on a
shared brain, because a shared brain must be server-readable to fan out to its
members. The new policy applies to **new** writes after the daemon re-samples it
(restart with `resleeve down && resleeve up`, or run `resleeve brain use`);
existing rows are not re-encrypted. `brain encrypt` is owner-only.

---

## Forward references

- [`QUICKSTART.md`](../QUICKSTART.md) — minimal "first 5 minutes" path.
- [`ARCHITECTURE.md`](../ARCHITECTURE.md) — internal layering and the
  daemon/serve split.
- [`SMOKE.md`](./SMOKE.md) — non-destructive validation checklist.
- [`docs/design/round-1/`](./design/round-1/) — original design + decisions.
- [`docs/design/round-2/`](./design/round-2/) — eventing, schema, adapter
  interface, auth subsystem, storage backends.
- [`docs/design/round-3/`](./design/round-3/) — memory module.
- [`docs/design/round-4/`](./design/round-4/) — conversation transport,
  cross-machine sync, rehydrate UX.
