# SMOKE.md — Pre-release smoke-test checklist

This is the non-destructive smoke-test checklist developers run before tagging a
release or after a large merge to main. It walks the daemon, hook, capture,
memory, MCP, and sync surfaces in a single sitting so regressions across the
seams (the places parallel-agent work tends to break) get caught before users
see them.

Nothing in the **non-destructive** section mutates production state in a way
you can't undo by re-running `resleeve up`. It assumes the smoke is executed
against your everyday `~/.resleeve/` data directory. Anything that requires a
clean DB or a multi-host setup is in the **deferred** section at the bottom.

See [USER_GUIDE.md](../README.md) for the day-to-day verbs and
[docs/design/round-4/](design/round-4/) for the architecture this checklist
exercises.

## Prerequisites

1. A clean git tree on the branch under test.
2. A built binary at the repo root:

   ```bash
   go build -o resleeve ./cmd/resleeve
   ```
3. A daemon that's either currently running or cleanly down. The checklist
   starts with a `down + up` cycle, so either state is fine — but if a previous
   abnormal shutdown left a stale `~/.resleeve/daemon.pid`, clear it first:

   ```bash
   pgrep -f "resleeve agent" >/dev/null || rm -f ~/.resleeve/daemon.pid
   ```
4. `claude` CLI installed if you want the bridge cards in `doctor` to read
   `bridge OK`. Not required for the daemon/MCP/sync subset.

> **Capture the whole run.** Pipe every step (or the whole script) through
> `tee` so the artifact is reviewable after the fact:
>
> ```bash
> ./run-smoke.sh 2>&1 | tee /tmp/resleeve-smoke-$(date +%Y%m%d-%H%M%S).log
> ```

## Non-destructive checklist

Seven steps. Run them in order — later steps assume the daemon state earlier
steps leave behind.

### 1. Daemon cycle

Confirms `down` is clean and `up` re-installs the bridge idempotently.

```bash
./resleeve down
./resleeve up
```

Expected (abbreviated):

```
daemon: stopped (pid NNNN)
bridge: removed from ~/.claude/settings.json
---
daemon: started (pid MMMM, http://127.0.0.1:PORT)
bridge: installed in ~/.claude/settings.json
```

**Pass:** `up` reports a new pid; the bridge entry is present in
`~/.claude/settings.json` exactly once (re-runs of `up` don't duplicate it).
**Fail:** stale-pid error, port-bind error, or a second `_resleeve: true`
entry appearing under any hook event.

### 2. `doctor` cards

Confirms every health card the daemon exposes is reachable from the CLI.

```bash
./resleeve doctor
```

Expected cards (all OK in the happy path):

```
data dir         OK   ~/.resleeve
daemon           OK   pid MMMM
endpoint         OK   http://127.0.0.1:PORT
/v1/health       OK   200 OK
sealer           OK   unlocked   (or:  ✗ locked — run `resleeve login`)
bridge           OK   installed in ~/.claude/settings.json
upstream         OK   standalone (no upstream configured)   (or:  URL)
sync (slow)      OK   outbox depth=0
sync (fast/sse)  OK   subscribed
hook env         OK   RESLEEVE_HTTP_ADDR=...
```

**Pass:** exit code 0; every card OK or a known-acceptable state (`sealer ✗
locked` is OK if you haven't run `login`; `upstream standalone` is OK in
single-host mode).
**Fail:** any unexpected `✗`, or a card that doesn't render at all (means a
new card was added without smoke coverage — update this list).

### 3. F14 hook envelope smoke

Confirms the SessionStart hook emits the **correct** envelope shape — the one
Claude Code recognizes, not the flat-top-level one v1 emitted by mistake.
This is the F13/F14 regression guard.

```bash
echo '{"hook_event_name":"SessionStart","session_id":"smoke-test","cwd":"'"$PWD"'","source":"startup"}' \
  | ./resleeve hook --adapter claude
```

Expected (pretty-printed):

```json
{
  "systemMessage": "resleeve: loaded scope <scope> (<N> bytes)",
  "hookSpecificOutput": {
    "hookEventName": "SessionStart",
    "additionalContext": "# Plan...\n..."
  }
}
```

**Pass:** both `systemMessage` (user-visible notice) AND
`hookSpecificOutput.additionalContext` (model-visible context) are present at
the documented JSON paths; `~/.resleeve/last-injected.md` is updated with the
same body.
**Fail:** a flat top-level `additionalContext` (regressed envelope shape — CC
will silently drop it), missing `systemMessage` (F14 regression), or empty
`additionalContext` when a plan demonstrably exists at the resolved scope.

### 4. F13 dangerous-state smoke

Confirms `doctor` warns LOUDLY when the bridge is installed but the daemon is
down — the silent-no-op UX hazard from round 4.

> **Gotcha.** `./resleeve down` removes BOTH the daemon AND the bridge, so it
> tests the safe state, not the dangerous one. To reach the
> bridge-installed-but-daemon-dead state you must hard-kill the daemon:

```bash
kill -9 "$(cat ~/.resleeve/daemon.pid)"
rm ~/.resleeve/daemon.pid
./resleeve doctor; echo "exit=$?"
```

Expected:

```
daemon           ✗    NOT RUNNING
bridge           OK   installed in ~/.claude/settings.json
*** HOOKS ARE NO-OPS — run `resleeve up` ***
exit=1
```

**Pass:** exit code 1, the loud warning line is present (the exact wording
may vary; the key invariant is that the dangerous combination is called out,
not merely listed).
**Fail:** exit code 0, or the warning line missing — means a fresh CC session
launched in this state would silently get no memory injection with no signal
that anything is wrong.

Cleanup:

```bash
./resleeve up
```

### 5. `ErrNoUpstream` typed sentinel

Confirms standalone-mode sync commands return the typed 409 — not a generic
`daemon 409 Conflict` that the CLI used to string-match against.

Skip this step if your daemon was started with `--upstream`. Otherwise:

```bash
./resleeve sync push; echo "exit=$?"
```

Expected (stderr):

```
sync push: daemon 409 Conflict: no upstream configured ...: agent: no upstream configured
exit=<non-zero>
```

**Pass:** the trailing `: agent: no upstream configured` is present — that's
the wrapped typed sentinel (`agent.ErrNoUpstream`) visible end-to-end.
**Fail:** plain `daemon 409 Conflict: ...` with no sentinel suffix (regression
to body-string matching) or any other status code.

### 6. MCP stdio smoke

Confirms the MCP server lists the 10 `resleeve_*` tools and surfaces its
`instructions` field over the stdio transport.

> **Gotcha.** `timeout(1)` is **not** shipped with macOS by default. Do not
> reach for `timeout 2 ./resleeve mcp < req.json` — it'll fail with `command
> not found` on a stock developer Mac. Use the tempfile-stdin + background +
> sleep + `kill -TERM` pattern below, which is portable across macOS and
> Linux.

```bash
REQ=$(mktemp)
cat > "$REQ" <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
EOF

./resleeve mcp < "$REQ" > /tmp/mcp-out.json 2>/tmp/mcp-err.log &
MCP_PID=$!
sleep 1
kill -TERM $MCP_PID 2>/dev/null
wait $MCP_PID 2>/dev/null

# Inspect output
grep -c '"name":"resleeve_' /tmp/mcp-out.json
grep -o '"instructions":"[^"]\{1,80\}' /tmp/mcp-out.json | head -1
rm "$REQ"
```

Expected:

```
10
"instructions":"Plan + recent learnings for the scope..."
```

**Pass:** exactly 10 `resleeve_*` tool entries; non-empty `instructions`
string in the initialize response.
**Fail:** any other count (a tool was added or removed without updating this
smoke), empty `instructions` (regression on the rolled-up-context priming).

### 7. `doctor --backfill-counts`

Confirms the maintenance pass is idempotent on a live DB.

```bash
./resleeve doctor --backfill-counts
./resleeve doctor --backfill-counts   # second run: should print 0 updated
```

Expected (first run):

```
backfill-counts: recomputed event_count for N sessions
```

Second run:

```
backfill-counts: recomputed event_count for 0 sessions
```

**Pass:** the second run reports 0 — recompute is a true fixpoint.
**Fail:** non-zero on the second run means the recompute isn't deterministic
or the underlying counter drifted between runs.

## MCP write smoke

Adjunct to step 6 — confirms the write-path tools (`scope_set`,
`plan_write`, `learning_append`) actually mutate state visible to the CLI.
Run this AFTER step 6 succeeds.

```bash
REQ=$(mktemp)
cat > "$REQ" <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"resleeve_scope_set","arguments":{"path":"smoke-test","kind":"project","title":"smoke-test scope"}}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"resleeve_plan_write","arguments":{"scope":"smoke-test","content":"# smoke plan\nhello from mcp"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"resleeve_learning_append","arguments":{"scope":"smoke-test","text":"smoke learning entry"}}}
EOF

./resleeve mcp < "$REQ" > /tmp/mcp-write-out.json 2>/tmp/mcp-write-err.log &
MCP_PID=$!
sleep 1
kill -TERM $MCP_PID 2>/dev/null
wait $MCP_PID 2>/dev/null

# Cross-check via CLI
./resleeve scope get smoke-test
./resleeve plan read smoke-test
./resleeve learning list smoke-test

# Cleanup
./resleeve scope delete smoke-test
rm "$REQ"
```

Expected: each cross-check command renders the value written via MCP; the
`scope delete` at the end succeeds (refuses with 409 only if children exist —
this smoke creates none).

**Pass:** all three writes visible via CLI; `scope delete smoke-test` exits 0.
**Fail:** any write missing (MCP→store gap), or `scope delete` refusing for
any reason other than child scopes (means the smoke leaked test state — clean
up by hand via `resleeve scope delete smoke-test --force`).

## Deferred (destructive) smokes

These need either a clean DB, a multi-host setup, or both, and so don't fit
in the pre-release checklist a developer can run from their everyday machine.
Track them separately when cutting an identity/sync release.

- **`register` / `login` / `logout`.** Needs a clean DB (`~/.resleeve/` moved
  aside) plus a `resleeve serve` spinup to hit. Validates Argon2id KEK
  derivation (round-5) end-to-end. Cleanup: restore the moved-aside dir.
- **`pair invite` / `pair accept`.** Needs two hosts (or two data dirs on one
  host with different ports), one in invite mode, one accepting. Validates
  the ephemeral device token round-trip — and is the only way to exercise the
  revoke-on-accept path tracked in follow-up B.
- **`migrate-key`.** Destructive on the sealer state; needs a daemon that is
  fully down (follow-up D guards against running it while alive, but the
  smoke is to verify the guard fires). Run against a throwaway data dir.
- **Real cross-machine sync.** Two hosts, one `resleeve serve`, both pushing
  and pulling; verify the seven plaintext markers (session id, cwd, prompt,
  scope path, title, plan, learning) never appear unencrypted in the serve's
  on-disk artifacts. This is the live smoke that caught the slice 3 + 2.5
  integration gap on f402da9.
- **OS keychain end-to-end.** Follow-up A (macOS Keychain / libsecret) —
  needs the host's native keychain, can't be faked in CI.

## Bonus: stress run

If the non-destructive checklist passes, run the unit tests under `-race`
with a small count to catch flake regressions. G's `next_attempt_at`
lex-order fix (commit 3a039dd, see learning in scope `resleeve`) is the
canonical example of a production bug that manifested as ~0.3% flake —
worth re-verifying it didn't regress.

```bash
go test ./... -count=10 -race
```

Expected: all `ok`, no `FAIL`, no `DATA RACE` blocks. A single flake in
`internal/agent` is the bug to look for specifically — it's the suite the
lex-order fix stabilized.

## Output format for pass/fail

Pipe the whole script through `tee` (suggested at the top) so the run is
reviewable after the fact. A reasonable wrapper:

```bash
#!/usr/bin/env bash
set -u   # NOT -e: smoke step 5 is expected to exit non-zero in standalone
LOG=/tmp/resleeve-smoke-$(date +%Y%m%d-%H%M%S).log
{
  echo "=== step 1: daemon cycle ==="; ./resleeve down; ./resleeve up
  echo "=== step 2: doctor cards ==="; ./resleeve doctor
  # ... etc
} 2>&1 | tee "$LOG"
echo "log: $LOG"
```

When reporting smoke results in a release PR, attach the log file or paste
the section headers + their pass/fail line. The default review bar is "all
seven non-destructive steps pass; deferred set is acknowledged but not
required for this release."
