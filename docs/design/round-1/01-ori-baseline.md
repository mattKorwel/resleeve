# Ori baseline review

Repo: `/Users/mattkorwel/dev/ori` (github.com/mattkorwel/ori). v3 live on fleet since 2026-05-09.

## 1. Stack

- **Language:** Go (1.26.2)
- **HTTP server:** net/http (stdlib)
- **Storage:** git repo (bare filesystem under `keys/` directory)
- **Data format:** JSON (pointer/session records) + Markdown (plan/learning content) + opaque bytes
- **Build:** single binary (`go build ./cmd/ori`), statically compiled
- **Deps:** minimal — stdlib + `cloud.google.com/go` (GCS/auth), `oklog/ulid`, `pelletier/go-toml`

## 2. Purpose / scope (what it actually does today)

From README:
> You work with AI coding agents across multiple machines and across many sessions per day. Ori is the small piece of infrastructure that remembers **what you're doing, where it's running, and how to pick it back up** — independent of any one harness or machine.

Wired end-to-end:

- Fleet-wide `ori status` showing live/idle/stale pointers
- Scope-based content storage (plan, learning, arbitrary typed items)
- Pointer lifecycle (where is work happening) with heartbeat tracking
- Session log per scope (pointer-only — does **not** store transcript content)
- CLI verbs: `ori status`, `ori start`, `ori plan`, `ori learning`, `ori scope`, `ori read|write|list|delete`
- HTTP API server (`ori server serve`)
- Bridge plugins for cloudcode and gemini CLI (embedded in binary)
- MCP server (stub; forwards to HTTP API)

## 3. Data model (`internal/brain/types.go`)

- **Scope** — hierarchical, slash-separated path (`amplify`, `amplify/ori`, `byo-agents/managed-claw/containers`). Untyped, arbitrary depth. Top-level scopes require explicit creation; sub-scopes can be created implicitly.
- **Item** — tuple `(scope, type, name)`. One value (bytes). Type is a label (`plan`, `learning`, `pointer`, `session`, user-defined). Name defaults to `_default` (unnamed slot).
- **Pointer** — singleton per scope. Fields: `worker` (broker), `started_at`, `cwd`, `session_id` (cloudcode), `ended_at`.
- **Session** — per-scope log entry for one cloudcode run. Fields: `session_id`, `scope`, `broker`, `started_at`, `ended_at`. **Note:** this is a pointer/record of the session, not the session's transcript content.
- **ScopeMeta** — title, kind (portfolio/program/project/etc.), description, default cwd, created/updated timestamps.

Liveness classification (`types.go:100-104`):

- `live`: heartbeat <2 min
- `idle`: 2–30 min
- `stale`: >30 min, `ended_at` null
- `ended`: `ended_at` set

## 4. Storage layout (`docs/v3-keystore.md:55-72`)

```
keys/
  <scope-segment>/
    scope.meta.json           # ScopeMeta record
    .donotinherit             # optional: halt inherit walks
    <type>/
      .d/
        _default              # unnamed item
        <name>                # named items
    <sub-scope>/
      scope.meta.json
      ...
```

- Plain files in git repo (committed/pushed on every write).
- Markdown/JSON at operator's discretion (CLI special-cases `pointer` → `.json`, `plan`/`learning` → any).
- `.d/` directory is a convention hiding items.
- Reserved names: `_default` and dot-prefixed (internal).

**DB:** none. Pure git-backed filesystem store (`internal/store/git/v3.go`).

## 5. Sync / transport (`internal/server/routes.go`)

HTTP API:

- `GET/PUT /v1/meta/{scope...}` — scope metadata
- `GET/PUT/DELETE /v1/item/{type}/{name}/{scope...}` — item CRUD
- `GET /v1/items-by-type/{type}/{scope...}` — list items of a type
- `GET /v1/inherit/{type}/{name}/{scope...}` — ancestor walk (concatenated)
- `POST /v1/heartbeat/{scope...}` — in-memory only (no git write)
- `GET /v1/status` — fleet status

**Sync model:** CLI or harness → HTTP → `ori-server` (on cloudtop) → git repo (commit + push on mutation). Heartbeats ephemeral (in-memory).

**Auth:** `ori auth set | show | ping` configures server URL + token; passed as header.

## 6. CLI integrations

Two harness plugins embedded in binary, installed to `~/.config/<harness>/plugins/`:

1. **Cloudcode** (`internal/cli/harnesses/cloudcode/plugins/ori-bridge.js`):
   - On boot: read `$ORI_DEFAULT_SCOPE`, write pointer + session record.
   - Every 30s: POST heartbeat.
   - On exit: set `ended_at` on pointer + session.
   - On context-window compaction: inject inherited plan + learnings.
2. **Gemini CLI** (`internal/cli/harnesses/gemini/hooks/`):
   - Shell hooks: `ori-session-start.sh`, `ori-heartbeat.sh`, `ori-context.sh`, `ori-session-end.sh`. Same lifecycle.

Scope resolution (`cli/resolver.go`): `--scope` flag > `$ORI_DEFAULT_SCOPE` > `~/.config/ori/path-defaults.toml` (longest-prefix match on cwd) > error.

## 7. Wired vs. stubbed

**Wired (tested, used daily):**

- Keystore (CRUD, inherit walks, `.donotinherit` truncation)
- All CLI verbs in phases A–C
- HTTP server + client
- Scope resolver
- Pointer + session lifecycle (write, heartbeat, classification)
- Bridge plugin for cloudcode
- Capsule broker SSH + reverse-tunnel logic
- `ori start` with full resolution (broker, harness, cwd, collision check)

**Stubbed / phase-deferred:**

- MCP server (routes exist; shell-tools not wired)
- Dispatch (`ori dispatch <scope>`) — skeleton, summarize rollup TBD
- Gemini CLI bridge (hooks exist; integration not fully tested)
- Multi-tenant brains (code allows it; not exercised)
- v2 → v3 session migration is manual script

## 8. Notable design choices

- **Untyped scope tree** — broke from v2's rigid `Vault → Project → Branch → Dispatch → Agent`. No validation in brain; conventions live in CLI.
- **Git as primary durability** — every write commits + pushes. Heartbeats deliberately ephemeral.
- **Scope resolution order** — flag > env > longest-prefix file match. Operator-friendly.
- **Inherit walks** — plain file concatenation with section headers; `.donotinherit` boundary.
- **Bridge as plugin** — embedded in ori binary, installed to harness plugin dir; harness calls bridge, not vice-versa.
- **Item names reserved** — `_default`, dot-prefixed. Operator can't collide.
- **Dispatch as CLI tooling** — `d.<date>` scopes + conventions, not typed in brain.

## 9. Implications for the session service

- **What ori has that we keep:** scope tree, pointer/heartbeat, HTTP server, auth, bridge-plugin pattern, single-binary deploy.
- **What ori is missing:** session **content** capture. Today `Session` records only the *fact* that a session ran, not its transcript.
- **Where git breaks down:** session events are high-frequency (hundreds/min per session). Git's commit-and-push model is wrong for this. Need a second storage backend (SQLite index + content-addressed blob store) inside the same binary.
- **Bridge pattern extends naturally** to Codex / Aider / OI — same plugin/hook approach, different host CLIs.

## 10. File map

| Path | Purpose |
|---|---|
| `/Users/mattkorwel/dev/ori/README.md` | Architecture, quick-start, CLI surface |
| `/Users/mattkorwel/dev/ori/docs/v3-keystore.md` | Full design (storage, API contract, phasing) |
| `/Users/mattkorwel/dev/ori/cmd/ori/main.go` | Entry point; dispatches to CLI/server/MCP |
| `/Users/mattkorwel/dev/ori/internal/brain/types.go` | Data model (Scope, Pointer, Session, ScopeMeta, liveness) |
| `/Users/mattkorwel/dev/ori/internal/brain/store.go` | Store interface (abstract CRUD) |
| `/Users/mattkorwel/dev/ori/internal/store/git/v3.go` | Git-backed implementation |
| `/Users/mattkorwel/dev/ori/internal/server/server.go` | HTTP server startup, middleware |
| `/Users/mattkorwel/dev/ori/internal/server/routes.go` | HTTP route registration |
| `/Users/mattkorwel/dev/ori/internal/server/handlers.go` | Handler logic (scope CRUD, inherit, heartbeat, status) |
| `/Users/mattkorwel/dev/ori/internal/cli/cli.go` | Verb dispatcher + `Run()` entry point |
| `/Users/mattkorwel/dev/ori/internal/cli/verbs_status.go` | `ori status` (fleet + scope card) |
| `/Users/mattkorwel/dev/ori/internal/cli/verbs_start.go` | `ori start` (broker resolve, collision, SSH) |
| `/Users/mattkorwel/dev/ori/internal/cli/verbs_content.go` | `ori plan`, `ori learning`, `ori read/write/list` |
| `/Users/mattkorwel/dev/ori/internal/cli/verbs_scope.go` | Scope CRUD + pointer lifecycle |
| `/Users/mattkorwel/dev/ori/internal/cli/resolver.go` | Scope resolution |
| `/Users/mattkorwel/dev/ori/internal/cli/harnesses/cloudcode/plugins/ori-bridge.js` | Cloudcode plugin |
| `/Users/mattkorwel/dev/ori/internal/cli/harnesses/gemini/hooks/` | Gemini CLI shell hooks |
| `/Users/mattkorwel/dev/ori/internal/api/client.go` | HTTP client used by CLI |
| `/Users/mattkorwel/dev/ori/internal/mcp/server.go` | MCP stdio server |
| `/Users/mattkorwel/dev/ori/go.mod` | Dependencies |
