# Round 9 — Antigravity (`agy`) adapter

> **Status: implemented (Tier C — capture + prime).** This document was
> rewritten after reading the **real** on-disk format. The earlier draft
> guessed JSONL transcripts; that was **wrong**. Antigravity stores each
> conversation as a **SQLite database** whose step rows carry an
> **undocumented protobuf** payload. Everything below is ground-truth from
> the three real conversation databases under
> `~/.gemini/antigravity-cli/conversations` (`agy` 1.0.4) plus the shipped
> adapter in `internal/adapter/antigravity/`.

Antigravity is the Google Antigravity CLI (`agy`), derived from
Windsurf/Cascade. It is the round-9 Tier-C CLI: **capture is best-effort
file/db reconcile**, **resume is prime + native `agy --conversation`**. There
is no hook/plugin surface and no transcript import, so it never reaches Tier A.

## The CLI

- Binary: `agy` (1.0.4 verified), typically at `~/.local/bin/agy`.
- Version: `agy --version` → `1.0.4`.
- Resume (verified): `agy --conversation <id>` (resume by id),
  `agy --continue`/`-c` (most recent), `agy --print`/`-p "<prompt>"` (single
  non-interactive prompt), `agy --prompt-interactive`/`-i`.

## On-disk format (verified)

```
~/.gemini/antigravity-cli/
├── conversations/
│   └── <conversation-uuid>.db        ← ONE SQLite db per conversation
└── cache/
    └── last_conversations.json       ← { "<cwd>": "<conversation-id>" }
```

Each conversation `.db` has a `steps` table:

```sql
CREATE TABLE steps (
  idx integer, step_type integer, status integer,
  has_subtrajectory numeric, metadata blob, error_details blob,
  permissions blob, task_details blob, render_info blob,
  step_payload blob, step_format integer, PRIMARY KEY(idx)
);
```

(plus `trajectory_meta`, `gen_metadata`, `executor_metadata`,
`parent_references`, `trajectory_metadata_blob`, `battle_mode_infos` — not
read by the adapter.)

`step_payload` is a **protobuf wire-format blob with no published `.proto`**.
`idx` is the conversation's native total order; `step_type` (integer)
discriminates the role.

## Protobuf decoding — hand-rolled, no new dep

We deliberately do **not** add `google.golang.org/protobuf`. Instead
`protowire.go` is a ~100-line self-contained wire reader
(`DecodeStrings([]byte) []stringLeaf`):

- reads field tag → wire type (varint / fixed64 / length-delimited / fixed32);
- recurses into length-delimited fields that parse cleanly as sub-messages;
- collects every valid-UTF8, mostly-printable length-delimited **leaf** as a
  string, tagged with its **proto path** (chain of field numbers);
- never errors — truncated/garbage input simply yields fewer leaves;
- bounded recursion (`maxDepth`) so a malformed blob can't blow the stack.

Because the wire format does not distinguish `string`, `bytes`, and nested
message (all wire type 2), classification is heuristic by design. The raw
blob is always retained in `event.Vendor.NativePayload`.

## step_type → event kind (reverse-engineered)

Mapping derived by decoding the real databases and correlating recovered text
with role. Observed in the three dbs: `7,8,9,14,15,21,23,33,98,132`.

| step_type | Kind | How determined |
|---|---|---|
| **14** | `user_message` | Prompt text at proto path `[19,2]` / `[19,3,1]`; e.g. *"tell me about parallel and background agents…"* |
| **15** | `assistant_message` | Model answer at `[20,1]` / `[20,8]`; planning at `[20,3]` |
| **7,8,9,21,33,132** | `tool_call` | Tool name at `[5,4,2]`, JSON args (keys `toolAction`/`toolSummary`) at `[5,4,3]` — observed `view_file`, `list_dir`, `run_command`, `search_web`, `grep_search`, `list_permissions` |
| **98** | `system` | `# Conversation History` context-injection block |
| **23** | `system` | Generated trajectory title (e.g. *"Understanding Parallel Background Agents"*) |
| anything else | `system` (`antigravity:unclassified`) | **Not guessed** — payload preserved verbatim |

The conversation id is also embedded in payloads (path `[*,20,4]`); the
adapter primarily uses the **db filename** as the conversation/session id.

## Capture: `ReconcileOnce`

`ReconcileOnce(ctx, Ingester)` is the **only** capture path (there is no live
bridge):

1. Load `last_conversations.json`, invert to `{conversation-id → cwd}`.
2. Walk `conversations/*.db`.
3. Open each db **strictly read-only**: DSN `file:<path>?mode=ro&_pragma=query_only(1)`
   (`agy` is the sole writer; we never touch their store).
4. `SELECT idx, step_type, status, step_payload FROM steps ORDER BY idx`.
5. Synthesize a `session_start` from `(conversation-id, cwd)`; map each step
   to one event via `DecodeStrings` + `classify`.
6. `IngestBatch` per conversation.

**Leniency is contractual**: a malformed db, an unreadable row, an
undecodable payload, or a **panic** inside decoding is logged and skipped —
never fatal. `reconcileDB` and `mapStep` both `recover()`; a step that panics
degrades to a `system` event carrying the raw payload.

**Scope** uses the shared rule (copied from claude into `scope.go`):
`$RESLEEVE_SCOPE` → `.resleeve-scope` marker walk → `filepath.Base(cwd)`.

**Idempotency**: event UUIDs are deterministic UUIDv5 over
`(conversationID, "step:"+idx)` (and `"session_start"`), under a namespace
distinct from claude's. Re-running the sweep mints identical UUIDs, so the
storage layer's `(session_id, event_uuid)` PK + `INSERT OR IGNORE` dedups.

CLI wiring: `internal/cli/adapter_antigravity.go` registers a startup
reconcile sweep gated on `Detect().Installed` (mirrors `cli/adapters.go`).

## Re-sleeve: prime + native resume only

- **Replay is NOT supported.** Forging the undocumented protobuf-in-SQLite
  store would be unsafe; `ToNative(replay)` and `Hydrate(replay)` return a
  clear error pointing at the native resume path.
- **Prime mode** (the auto default here): `Hydrate` synthesizes a markdown
  opening prompt via `common.SynthesizePrime`, writes it atomically to
  `~/.resleeve/hydrate/<new-uuid>.md`, and mints a fresh session id.
- **Same-CLI resume** of an antigravity-origin session is native:
  `NativeResumeCmd` returns `("agy", ["--conversation", <id>])` for a
  replay-shaped result (or via `NativeResumeByConversationID`).
- **Prime resume** feeds the synthesized file to `agy --print` through the
  platform shell (`native_resume_unix.go` / `native_resume_windows.go`),
  matching claude's split-file pattern.

`InstallBridge` / `UninstallBridge` are successful **no-ops** (nothing to
install — capture is db-watch). `FromNative` returns a clear "unsupported"
error for all source kinds, pointing callers at `ReconcileOnce`.

## Honest limitations / fragility

- **Undocumented protobuf.** Field numbers (`[19,2]`, `[20,1]`, `[5,4,2]`,
  `[*,20,4]`) and `step_type` codes are reverse-engineered from `agy` 1.0.4.
  A future `agy` release can change them silently; the adapter degrades to
  `system` events (payload preserved) rather than mis-attributing roles.
- **Best-effort text extraction.** The string/bytes/message ambiguity means
  occasional noise leaves (UUIDs, file paths) or a missed leaf. Known-path
  extraction with a longest-non-noise-leaf fallback mitigates but doesn't
  eliminate this.
- **Tool results are not separately modeled.** Tool *calls* are captured
  (name + args); the compressed tool *result* bytes that follow are left in
  the raw payload, not emitted as a distinct `tool_result` event.
- **No live capture.** A conversation is only captured once its `.db` exists
  and the reconcile sweep runs; there is no streaming/live hook.
- **`step_type` set is incomplete.** Only codes seen across three dbs are
  mapped. New codes → `system` (`antigravity:unclassified`).

## Test coverage

- `protowire_test.go` — table/fixture-driven wire-decoder tests
  (top-level string, nested, mixed wire types, short/binary rejection,
  truncation leniency, varint edge cases, deep-nesting no-blowup).
- `mapstep_test.go` — step→event mapping (user/assistant/tool/unknown/
  context/title), `classify`, deterministic-UUID stability.
- `adapter_test.go` — Detect/Name, FromNative-unsupported, ToNative
  replay-error + prime-works, NativeResumeCmd (conversation + prime),
  install no-ops.
- `smoke_test.go` — **guarded by `RESLEEVE_SMOKE=1`**, runs `ReconcileOnce`
  against the real `~/.gemini/antigravity-cli/conversations`, asserts ≥1
  conversation with recoverable user text (incl. the known parallel-agents
  prompt) and verifies sweep idempotency. Last run: **3 conversations**,
  start=3 user=10 assistant=43 tool=34 system=6.
