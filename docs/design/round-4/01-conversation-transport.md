# Stage 5 — Conversation-layer transport

The Adapter interface already declares `Hydrate`, `ToNative`, and
`NativeResumeCmd` as `ErrNotImplemented` stubs (`internal/adapter/adapter.go`
per `round-2/09-adapter-interface.md`). Round 4 fills them in.

## Two paths through the same verbs

| Path | When | Adapter behavior |
|---|---|---|
| **Replay** | Source CLI == target CLI **and** adapter supports it | `ToNative` emits the target CLI's exact native local format (for claude: a JSONL); `Hydrate` writes it; `NativeResumeCmd` returns the CLI's own `--resume <id>` command. Full fidelity. |
| **Prime** | Source CLI != target CLI **or** adapter doesn't support replay | `ToNative` emits a synthesized opening prompt (markdown sections summarizing prior activity); `Hydrate` writes a scratch file with that prompt; `NativeResumeCmd` returns a one-shot command that pipes the prompt into the target CLI. Lossy by design. |

`Hydrate` picks the path internally based on `(source CLI, target CLI,
adapter capabilities)`; callers don't choose. `resleeve resume`
exposes `--mode replay|prime` as an override for explicit control.

## Adapter interface (verbatim signatures)

```go
type Adapter interface {
    Name() string
    Detect(ctx) (Detection, error)
    InstallBridge(ctx, opts InstallOpts) error
    FromNative(ctx, raw []byte, src Source) ([]event.Event, error)

    // Stage 5:
    Hydrate(ctx, sessionID string, opts HydrateOpts) (HydrateResult, error)
    ToNative(ctx, events []event.Event, mode RenderMode) ([]byte, error)
    NativeResumeCmd(ctx, sessionID string, result HydrateResult) (cmd string, args []string, error)
}

type HydrateOpts struct {
    TargetCLI string     // empty = source CLI
    Mode      RenderMode // empty = auto
    Cwd       string     // override cwd for the resumed session; empty = source cwd
}

type RenderMode string
const (
    RenderModeAuto   RenderMode = ""        // adapter picks replay if possible, prime otherwise
    RenderModeReplay RenderMode = "replay"
    RenderModePrime  RenderMode = "prime"
)

type HydrateResult struct {
    Mode      RenderMode // what the adapter actually chose
    Path      string     // file path of the materialized state (JSONL for replay, prompt scratch for prime)
    SessionID string     // may differ from input (e.g., prime mode may mint a new id)
    Notes     []string   // human-readable warnings ("3 tool_calls flattened to text")
}
```

## Replay path — claude → claude (v1's only fully fidelity case)

1. `ToNative(events, RenderModeReplay)`:
   - For each `event.Event`, emit one JSONL line.
   - Prefer `event.Vendor.NativePayload` if non-empty (verbatim CC record bytes — preserves exact shape).
   - Synthesize from normalized `content` when `NativePayload` is absent (e.g., reconcile-synthesized `session_start`).
   - Order by `(seq, event_uuid)` ascending (per amended `round-2/04-event-schema.md`).

2. `Hydrate(sessionID, opts)`:
   - Derive target dir: `~/.claude/projects/<cwd-encoded>/`. cwd is taken from the session row (or `opts.Cwd` override).
   - Write `<sessionID>.jsonl` atomically (tempfile + rename).
   - Return `HydrateResult{Mode: replay, Path: ..., SessionID: <same>}`.

3. `NativeResumeCmd(sessionID, result)`:
   - Return `("claude", []string{"--resume", sessionID})`.
   - Caller exec's it.

**Fidelity invariant**: the resumed CC session is bit-equivalent to
the original up to the resume point. CC's own `--resume` machinery
takes over from there.

## Prime path — claude → opencode (v1 stub)

1. `ToNative(events, RenderModePrime)`:
   - Synthesize a markdown opening prompt:
     - `## Context` — cwd, git branch, model, source-CLI version.
     - `## Plan` — pulled from memory module via `context.BuildContext(scope)`.
     - `## Recent activity` — last N (default 20) events summarized in 1–2 lines each. tool_calls become `"called Read(/x.go) → received contents"` style references. `assistant_message` events get their text. `thinking` is dropped.
     - `## Next` — last unanswered user prompt or open tool result, framed as "here's where we left off".
   - Target length: ~1500–3000 tokens. Configurable later.

2. `Hydrate(sessionID, opts)`:
   - Write the prompt to a scratch file under `~/.resleeve/hydrate/<new-uuid>.md`.
   - Mint a fresh `SessionID` for the resumed conversation (it's a new session in the target CLI).
   - Return `HydrateResult{Mode: prime, Path: ..., SessionID: <new>, Notes: ["3 tool_calls flattened to text"]}`.

3. `NativeResumeCmd(sessionID, result)`:
   - Return whatever the target CLI accepts. For opencode (stub):
     `("opencode", []string{"--prompt-file", result.Path})`.
   - If the target CLI doesn't have a file-based prompt input, fall back
     to stdin: caller pipes the file contents in.

**Fidelity invariant**: none. Prime is "here's what you were doing,
now continue" — the new CLI sees a single opening prompt, not a
multi-turn history.

## `resleeve resume` wrapper

```
resleeve resume <session-id> [--cli <name>] [--mode replay|prime]
                             [--print] [--cwd <dir>]
```

Behavior:

1. **On-demand sync pull first** (per `02-cross-machine-sync.md`).
   Timeout 2s; fall back to stale local data if upstream is slow.
2. Load events for `<session-id>` from local SQLite.
3. Resolve target CLI: `--cli` if given, else the session's source CLI.
4. Resolve adapter for the target CLI; bail if none.
5. Call `Hydrate`.
6. Get `NativeResumeCmd`.
7. If `--print`, output the command to stdout and exit.
   Otherwise, `syscall.Exec` it (replaces resleeve's process so the
   CLI inherits the user's tty cleanly).

## Cross-CLI translation table (v1)

| Source kind | Target = claude (replay) | Target = opencode (prime stub) |
|---|---|---|
| `user_message` | JSONL `type:"user"` with `content` | "User: <text>" |
| `assistant_message` | JSONL `type:"assistant"` with content + gen_ai fields | "Assistant: <text>" |
| `tool_call` | JSONL `type:"tool_call"` with full args | "Called Tool(<name>): <args summary>" |
| `tool_result` | JSONL `type:"tool_result"` linking to call | "Tool returned: <result summary>" |
| `thinking` | JSONL `type:"thinking"` | (dropped) |
| `system` | JSONL `type:"system"` | (dropped unless `permission_mode` or similar) |
| `attachment` | JSONL `type:"attachment"` | "Attached: <ref>" |
| `session_start` | JSONL `type:"session_start"` | (dropped; used as Context preamble) |
| `session_end` | JSONL `type:"session_end"` | (dropped) |

## Out of scope for round 4

- **Subagent fan-out across machines.** v1 captures subagents as
  nested sub_sessions; rehydrating them cross-machine needs a
  separate design pass.
- **Forks / branching.** "Start from a mid-session checkpoint" is
  conceptually clean but UX-loud; v3+.
- **Mixed-mode partial replay** (e.g., replay messages 1–50, prime
  messages 51–100). Possible but no clear demand.

## Implementation slicing (preview)

Three commits, each behind the same `Adapter` interface:

1. `feat(adapter/claude): replay-mode ToNative + Hydrate for claude→claude`
2. `feat(cli): resleeve resume wrapper (calls Hydrate, exec's NativeResumeCmd)`
3. `feat(adapter): prime-mode ToNative stub + opencode adapter scaffold`

Each commit has a unit test plus a small integration test (build CC
JSONL → reconcile back into resleeve → ToNative → diff against the
original). For commit 1, fidelity is byte-exact when
`vendor.native_payload` is preserved.
