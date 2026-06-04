# QUICKSTART — first 5 minutes with resleeve

Zero to a live Claude Code session that auto-loads a scoped plan +
learnings as `additionalContext`. About 5 minutes. Deeper coverage of
each verb: [USER_GUIDE.md](USER_GUIDE.md). Why the pieces fit:
[ARCHITECTURE.md](ARCHITECTURE.md). End-to-end smoke checklist:
[SMOKE.md](SMOKE.md).

## 1. Prerequisite check

```bash
go version          # need >= 1.22 (go.mod currently pins 1.26)
claude --version    # Anthropic Claude Code CLI must be on $PATH
```

resleeve is a single Go binary + on-disk SQLite at `~/.resleeve/data.db`.

## 2. Install resleeve

No published release yet — build from source:

```bash
git clone https://github.com/mattkorwel/resleeve.git ~/dev/resleeve
cd ~/dev/resleeve
go build -o resleeve ./cmd/resleeve
```

The walkthrough invokes `./resleeve` from `~/dev/resleeve`; move it
onto `$PATH` once you're past the first run.

## 3. `resleeve up`

Spawns the daemon (loopback HTTP on a random port) and wires the
SessionStart / PostToolUse / Stop / UserPromptSubmit hooks into
`~/.claude/settings.json`:

```bash
./resleeve up
```

Expected output:

```text
[1/2] daemon started at http://127.0.0.1:53219
[2/2] installed bridge (claude)
resleeve is up.
```

Bridge entries use Claude Code's **matcher-wrapped** hook format (F12)
— `{matcher: "", hooks: [{type, command, _resleeve: true}]}`. The
`_resleeve: true` tag is how `install-bridge` re-finds its own entries
without disturbing hand-authored hooks. For injection-only (no event
capture) use `./resleeve up --memory-only`.

## 4. `resleeve doctor`

Run this whenever something feels off:

```bash
./resleeve doctor
```

Expected output (daemon up, bridge installed, no upstream):

```text
resleeve doctor
===============
  data dir         /Users/you/.resleeve
  daemon           ✓ running (pid 41122)
  endpoint         http://127.0.0.1:53219
  /v1/health       ✓ 200 OK
  sealer           ✓ unlocked
  bridge (claude)  ✓ installed in /Users/you/.claude/settings.json
  upstream         (standalone — no upstream configured)
  sync (slow)      last drain never • last pull never • outbox depth 0
  claude binary    /usr/local/bin/claude
  resleeve binary  /Users/you/dev/resleeve/resleeve
  hook env         ✓ bridge + daemon both up
```


Card guide:

- **daemon / endpoint / /v1/health** — lifecycle.
- **sealer** — KEK state. `✓ unlocked` allows sync push/pull.
- **bridge (claude)** — `~/.claude/settings.json` carries
  `_resleeve: true` entries.
- **upstream / sync (slow|fast)** — meaningful only with a
  `resleeve serve` upstream. See USER_GUIDE.md → "Sync".
- **hook env** — the F13 silent-failure guard. `bridge ✓` + `daemon ✗`
  produces a loud red line and a non-zero exit code:

  ```text
    hook env         bridge ✓ — but daemon ✗ ! HOOKS ARE NO-OPS — run `resleeve up`
  ```

  Hooks fire but the daemon refuses connections, so Claude sees no
  `additionalContext` — looks broken, actually just unplugged.

## 5. Run `claude`

```bash
mkdir -p ~/dev/some-project && cd ~/dev/some-project
claude
```

The SessionStart hook fires here. First run in a fresh dir has no
scope content yet — silent no-op. Wire one up next.

## 6. Pick a scope

A *scope* is the addressable slot resleeve injects from. Two ways to
name one:

- **Marker file** (recommended for project roots):
  ```bash
  echo "my-project" > ~/dev/some-project/.resleeve-scope
  ```
  Every cwd at or below the marker resolves to `my-project/<rel-path>`.
- **Auto-derived** — no marker → hook falls back to `filepath.Base(cwd)`,
  so `~/dev/some-project` → scope `some-project`.

Full F11 resolution: `$RESLEEVE_SCOPE` override → `.resleeve-scope`
marker walked up cwd ancestors (scope =
`<marker>/<rel-from-marker-dir>`) → `filepath.Base(cwd)` → `unknown`.

## 7. Create the scope record

The marker only names which scope to load; the scope tree itself is a
separate daemon-stored object. Create one:

```bash
./resleeve scope set my-project \
  --kind project \
  --title "My side project — Go HTTP service"
```

Expected output:

```text
scope set: my-project
```

## 8. Write a plan

Plans are markdown payloads keyed by scope + slot. Default slot is
`_default` — what the SessionStart hook injects. Default input is
**stdin** (so agents can pipe content directly):

```bash
echo "Current task: ship the /healthz endpoint with a 200 OK and a JSON body." \
  | ./resleeve plan write my-project
```

Expected output:

```text
plan write: my-project [_default] (66 bytes)
```

Alternative inputs: `--content "..."`, `--file path.md`, `--edit` (opens
`$EDITOR`).

## 9. Append a learning

Append-only one-liners — punctuated insights worth carrying across
sessions:

```bash
./resleeve learning append my-project \
  "Keep /healthz dependency-free — no DB ping; readiness is a separate probe."
```

Expected output:

```text
learning appended: my-project (id=lrn_01HXYZ...)
```

Soft-supersede with `--supersedes <id>`.

## 10. Verify the rollup

`resleeve context` prints the exact markdown the SessionStart hook
will hand to Claude Code as `additionalContext`:

```bash
./resleeve context my-project
```

Expected output:

```markdown
# Resleeve memory for scope my-project

## Plan (my-project)

Current task: ship the /healthz endpoint with a 200 OK and a JSON body.

## Learnings (my-project)

- 2026-06-04: Keep /healthz dependency-free — no DB ping; readiness is a separate probe.

```

`(no context — no plans or learnings on the inherited chain)` means
the scope is empty or you typoed its path.

## 11. Open a fresh `claude` session

```bash
cd ~/dev/some-project
claude
```

F14 renders a top-level `systemMessage` notice at the top of the chat
UI on every SessionStart injection:

```text
resleeve: loaded scope "my-project" (193 bytes)
```

That notice is the visible indicator the hook fired AND the daemon
answered AND Claude Code accepted the envelope shape (F13). No notice
= run `resleeve doctor`.

## 12. Ask a question that uses the loaded context

Inside the `claude` session:

```text
Look at the current plan and learnings for this project. Sketch the handler.
```

Claude will reach for the rolled-up plan + learning — quoting the
"no DB ping" learning back to you, and scaffolding a handler that
returns the documented 200 JSON.

## 13. Audit what was injected

Every successful injection lands at `~/.resleeve/last-injected.md`:

```bash
cat ~/.resleeve/last-injected.md
```

Same body as step 10, prefixed with a header comment that carries the
resolved scope + RFC3339 injection timestamp:

```markdown
<!-- scope: my-project
     injected at: 2026-06-04T18:42:10Z -->

# Resleeve memory for scope my-project
...
```

Useful for "did the right scope load?" debugging without re-running
`claude`.

## 14. Bonus paths

- **In-agent memory curation via MCP** — `resleeve mcp` exposes
  `resleeve_*` tools so Claude can set/read scopes/plans/learnings
  without shelling out. USER_GUIDE.md → "MCP in-agent curation".
- **Cross-machine sync** — `resleeve up --upstream URL --upstream-token
  TOK`. USER_GUIDE.md → "Sync (two tiers)".
- **Identity** — `resleeve register / login / logout` + `pair invite /
  pair accept` replace the round-4 placeholder `seal.key`.
  USER_GUIDE.md → "Identity".
- **Memory-only mode** — `resleeve up --memory-only` skips event
  capture; injection still fires. USER_GUIDE.md → "Memory-only mode".
- **Resume a past session** — `resleeve resume <session-id>` rebuilds
  state for a fresh `claude --resume`. USER_GUIDE.md → "Resume".

End-to-end smoke checklist: [SMOKE.md](SMOKE.md).
