# Round 4 — User experience

## New CLI verbs

```
resleeve register      [--upstream URL]
resleeve login
resleeve logout
resleeve pair invite
resleeve pair accept   <code>
resleeve resume        <session-id> [--cli NAME] [--mode replay|prime]
                                    [--print] [--cwd DIR]
resleeve session list  [--remote] [--since DUR] [--scope GLOB]
resleeve session show  <session-id>   (already exists; gains on-demand pull)
resleeve sync          [push|pull]    (manual escape hatch)
resleeve serve         [--addr ADDR] [--backend KIND] [--config FILE]
```

Existing v1 verbs (`up`, `down`, `doctor`, `usage`, `purge`, `agent`,
`hook`, `install-bridge`, `session search`, `session tail`, `scope`,
`plan`, `learning`, `context`) are unchanged.

## Lifecycle changes

### `resleeve up`

Gains optional `--upstream <url>`. With it:

1. Validates the URL is reachable (`GET /v2/sync/health`).
2. Persists `upstream` to `~/.resleeve/config.toml`.
3. If no device token in keychain: prints "Run `resleeve login` to
   unlock cross-machine sync" and stays in capture-only mode.
4. If device token present: hands off to daemon, which starts the
   outbox drain + pull loops.

Without `--upstream`: identical to v1 behavior. Standalone islands stay
supported as a first-class mode.

### `resleeve down`

Unchanged.

### `resleeve doctor`

Gains rows:

```
  upstream          https://sync.you.dev (✓ reachable)
  identity          mattkorwel@example.com (device-token expires in 28d)
  sync (slow)       last pull 12s ago • outbox depth 0
  sync (fast/SSE)   ✓ connected (47m uptime)
```

## The MVP demo flow (locked Q8)

End-to-end on two machines:

**Machine A** (laptop, where you've been working):

```
$ resleeve up --upstream https://sync.you.dev
$ resleeve login
Master password: ********
✓ Unlocked. Sync active.

$ claude
> work on the auth refactor
> ... lots of CC activity ...
> Ctrl-D
```

Behind the scenes: events flow into local SQLite, get pushed to
upstream via the outbox.

**Machine B** (a fresh pod / new laptop / devcontainer rebuild):

```
$ resleeve up --upstream https://sync.you.dev
$ resleeve login
Master password: ********
✓ Unlocked. Pulling state...
✓ 47 sessions, 4 scopes synced

$ resleeve session list --remote --since 1h
SCOPE/AGENT          CLI     STATUS  EVENTS  STARTED
auth-refactor/default claude  active  142     12m ago    ← from machine A

$ resleeve resume <sid>
✓ Hydrated to ~/.claude/projects/-Users-x-auth-refactor/<sid>.jsonl
$ claude --resume <sid>
> resuming where I left off on the laptop ...
```

**The handoff is seamless from CC's perspective** — it sees a normal
`--resume`. From resleeve's perspective: a single user-visible verb
exercised every layer (decrypt, ingest, hydrate, exec).

## Cross-CLI demo (v1 stub)

```
$ resleeve resume <claude-sid> --cli opencode
ℹ️  Source CLI (claude) differs from target (opencode) — prime mode
ℹ️  3 tool_calls flattened to text
✓ Hydrated to ~/.resleeve/hydrate/<new-uuid>.md
$ opencode --prompt-file ~/.resleeve/hydrate/<new-uuid>.md
> ...continues as a new opencode session primed with the context
```

opencode adapter is a stub in v1 (just enough to prove the seam).

## Failure modes

| Failure | UX |
|---|---|
| Daemon locked (no master password unlocked yet) | Explicit verbs (`resume`, `session list --remote`) fail with `error: resleeve login required`. Local-only verbs (`session show <local-sid>`, `scope list`) still work. |
| Upstream unreachable | Reads fall back to local view with stderr `(upstream unreachable; showing local state)`. Writes queue in outbox. No data loss. |
| Outbox at capacity (>10MB or >1000 rows) | Warn in `doctor`. Capture continues — outbox keeps growing — but `doctor` exit code goes non-zero. |
| Device token expired | Daemon stops pushing/pulling; logs to `daemon.log`. User runs `resleeve login`. |
| Pairing code expired | Error on `pair accept`. User re-runs `pair invite` on machine A. |
| KEK mismatch on `pair accept` (wrong master password) | Error before any state is changed. Re-try with correct password. |
| SSE drops (memory tier) | Background reconnect with exponential backoff. Local memory reads continue from local DB. On reconnect, replay missed events using cursor. |
| Outbox push 409 (key collision) | Logged as caller bug (keys include seq; collisions shouldn't happen). Row dropped from outbox; daemon continues. Surfaces in `doctor` as `sync (slow) — 1 dropped due to key conflict`. |

## Configuration

New file: `~/.resleeve/config.toml`. Example:

```toml
[upstream]
url = "https://sync.you.dev"

[sync]
slow_pull_interval = "30s"
sse_reconnect_backoff_max = "5m"
outbox_warn_depth = 1000

[identity]
email = "mattkorwel@example.com"
# device_token lives in OS keychain, NOT this file
# recovery_key is shown once at registration, NOT stored

[ui]
session_list_default_limit = 20
```

Override precedence: env var > config file > built-in default.

Env vars: `RESLEEVE_UPSTREAM`, `RESLEEVE_CONFIG`, `RESLEEVE_SCOPE`
(already exists from F11).

## Backwards compatibility

A v1 install (no `--upstream`, no `register`/`login`) keeps working
identically. Sync is opt-in. The daemon detects "no upstream
configured" and skips all sync code paths.

A `resleeve up` on an existing v1 data dir runs the new schema
migration (adds `outbox`, `sync_state`); existing tables untouched.
Restart-safe.

## Out of scope for round 4

- **`session list --remote --user <other>`** — multi-user. Defer.
- **`session merge`** — combining sessions captured on different
  machines (if you ran CC in parallel by accident). Defer; the
  fleet-operator persona doesn't usually do this.
- **Web UI / dashboard**. Defer indefinitely; CLI is the product.
- **Live "tail across all machines"**. SSE makes this possible but
  the verb (`resleeve session tail --remote --all`) is a v3 polish.

## Implementation slicing

Once round 4 is locked, MVP is **4 commits** (per `00-fleet-operator-cut.md`
+ Stage 5 commits):

1. **Stage 5 commit 1**: claude→claude replay (`Adapter.ToNative`,
   `Hydrate`, `NativeResumeCmd` + `resleeve resume`)
2. **v2 commit 1**: `Backend` interface + local-disk impl + `resleeve serve`
   stub
3. **v2 commit 2**: client outbox + pull + on-demand pull, slow tier only
4. **v2 commit 3**: SSE fast tier + memory sync

Stage 5 cross-CLI prime stub (`claude→opencode`) lands as a fifth commit
after MVP demos cleanly.
