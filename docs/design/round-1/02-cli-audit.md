# CLI session-format audit

Goal: characterize each target CLI's on-disk session state to decide between opaque-blob sync vs. normalized schema. Local-disk evidence used where available (Claude Code and Gemini CLI installed locally); Codex / Aider / Open Interpreter from docs + repo.

## 1. Claude Code (`~/.claude/`)

### Storage location (verified locally)

- `~/.claude/projects/<sanitized-cwd>/<session-uuid>.jsonl` — primary conversation log. Cwd sanitized by replacing `/` with `-` (e.g. `/Users/mattkorwel/sessions` → `-Users-mattkorwel-sessions`). Per-project.
- `~/.claude/projects/<sanitized-cwd>/<session-uuid>/subagents/` — subagent transcripts.
- `~/.claude/sessions/<pid>.json` — live runtime state per process (`pid`, `sessionId`, `cwd`, `status`, `waitingFor`). Global, transient.
- `~/.claude/session-env/<session-uuid>/` — per-session env snapshot dir.
- `~/.claude/shell-snapshots/snapshot-zsh-*.sh` — shell snapshot at launch.
- `~/.claude/history.jsonl` — flat user-prompt history across all sessions.
- `~/.claude/backups/.claude.json.backup.<ts>` — rolling backups of `~/.claude.json`.
- `~/.claude/plans/`, `~/.claude/cache/`, `~/.claude/plugins/`.

### Format

JSONL, one event per line. Heterogeneous record types keyed by `type`:

- `permission-mode`, `file-history-snapshot`, `user`, `assistant`, `attachment`, `ai-title`, `system`.
- Every event carries `parentUuid`, `uuid`, `sessionId`, `timestamp`, `cwd`, `gitBranch`, `version`, `entrypoint`.
- Assistant messages embed the full Anthropic API message: `model`, `id`, `content[]` (with `thinking` / `text` / `tool_use` parts including encrypted `signature` blobs for extended thinking), `stop_reason`, `usage` (cache breakdowns, `iterations`), `requestId`.
- Tool results appear as `user` records with `message.content[].tool_result` referencing `tool_use_id`.
- Side-channel: `attachment` events carry `deferred_tools_delta`, `agent_listing_delta`, `skill_listing` — available tools/agents/skills are persisted *into* the transcript, not in a sidecar.

### Captured

Full message tree (parent-pointer DAG via `parentUuid`), tool calls/results, extended-thinking blocks with signatures, model name, cwd, git branch, CLI version, permission mode, file backups in `trackedFileBackups`, AI-generated session title, token usage and cache metrics per turn, deferred tool/agent/skill listings.

### NOT captured

MCP server config (lives in `~/.claude.json` / settings), env vars beyond the snapshot, secrets, the actual filesystem state at resume time (only "tracked file backups" — files Claude touched), worktree identity beyond `cwd` string, user's shell aliases (shell-snapshots stored separately).

### Native resume

`claude --resume [sessionId]` (picker or specified session); `claude --continue` resumes the most recent session in the current cwd. Resume rehydrates the JSONL into model context — including thinking signatures, which is what enables interleaved-thinking continuation. `cwd` field anchors the project bucket. https://code.claude.com/docs/en/cli-reference

### Schema stability

Implementation detail, not a documented spec. Field set has grown over minor versions (recent: `deferred_tools_delta`, `iterations` in usage, `gitBranch`). Records embed `version` (e.g. `2.1.140`), giving a per-line version stamp — useful for adapters.

### Hook points

- File watcher on `~/.claude/projects/**/*.jsonl` — cleanest (append-only, atomic line flushes).
- Hooks system in `settings.json` (`SessionStart`, `Stop`, `PostToolUse`) is the supported extension API.
- No public plugin API for swapping the transcript writer.

---

## 2. Codex CLI (`~/.codex/`)

Not installed locally. Findings from OpenAI docs and repo issues.

### Storage location

`~/.codex/sessions/YYYY/MM/DD/<id>.jsonl` (recent builds use `.jsonl.zst` zstd-compressed rollouts). Date-sharded, global, indexed via a separate `ThreadStore` for the resume picker.

### Format

JSONL "rollout files." Each line is a `RolloutItem` wrapping a `ResponseItem`: agent messages, reasoning, command executions, file changes, MCP tool calls, web searches, plan updates, approval decisions. Same stream is exposed live via `codex exec --json`.

### Captured

Full event stream sufficient for replay — docs explicitly call rollouts "the source of truth for replay." `ContextManager` rebuilds model state from the rollout on resume.

### NOT captured (or not portably)

MCP server config (lives in `~/.codex/config.toml`), sandbox policy state, approval-mode preferences (resume-time flags, not transcript-resident).

### Native resume

`codex resume` (picker, cwd-scoped), `codex resume --last`, `codex resume --all`, `codex resume <SESSION_ID>`. Sister surfaces (VS Code ext, web/desktop) share the rollouts via ThreadStore — already a cross-surface example to study. Known bugs: local index goes stale and recent sessions sometimes don't appear in `--all` (openai/codex#19822, #20131, #20165). https://developers.openai.com/codex/cli/reference

### Schema stability

Actively evolving in 2026 (thread summaries, rename, fork). The `--json` event format is the public-facing spec; rollout internals are not formally promised stable. `.jsonl.zst` compression is a recent wrinkle for any sync layer.

### Hook points

- File watcher on `~/.codex/sessions/` + zstd decoder.
- `codex exec --json` is the cleanest programmatic interception point — wrap headless invocations and tee the JSONL stream.
- No first-party plugin API.

---

## 3. Aider (per-repo)

Not installed locally. Characterized from official docs.

### Storage location

Per repo, in cwd:

- `.aider.chat.history.md` — full transcript (Markdown).
- `.aider.input.history` — readline-style input history.
- `.aider.tags.cache.v3/` — repo-map symbol cache (not conversation state).
- `.aider.llm.history` — raw LLM request/response log (when enabled).

Override via `--chat-history-file`, `--input-history-file`, `AIDER_*` env vars, or `.aider.conf.yml`.

### Format

**Markdown**, not JSON. Conversation rendered as human-readable prose with file-edit blocks. Tool/edit operations appear as embedded diff fences — not as discrete structured records.

### Captured

User prompts, assistant prose, edit blocks, command output shown in chat. `/save` writes a *commands file* that reconstructs which files are in chat — not a session replay.

### NOT captured (structurally)

Tool-call argument JSON (only rendered form), token usage per turn, model identity per turn, system prompt. Parsing the markdown back into structured turns is lossy.

### Native resume

`--restore-chat-history` (or `AIDER_RESTORE_CHAT_HISTORY=true`) replays the markdown back into context on launch. Coarse-grained: whole file, all-or-nothing. `/clear` and `/reset` discard.

### Schema stability

Stable because it's Markdown — but that's also why it's the lowest-fidelity source.

### Hook points

- File watcher on `.aider.chat.history.md` in any repo aider runs in (no central registry — watch known repos or point history file at a known location via env).
- No plugin API; Python API (`aider.coders.Coder`) is the wrapper path.

---

## 4. Gemini CLI (`~/.gemini/`)

Verified locally — empty so far (no saved chats, just `tipsShown`).

### Storage location (verified)

- `~/.gemini/tmp/<user-or-projecthash>/chats/session-YYYY-MM-DDTHH-MM-<shortid>.jsonl` — automatic per-session log.
- `~/.gemini/tmp/<hash>/chats/checkpoint-<tag>.json` — manual checkpoints from `/chat save <tag>`.
- `~/.gemini/tmp/<hash>/logs.json` — array log.
- `~/.gemini/tmp/<hash>/.project_root` — anchors a workspace identity.
- `~/.gemini/projects.json` — global registry mapping cwd → hash dir.
- `~/.gemini/history/<hash>/` — separate history tree (mirrors tmp).

### Format

JSONL for auto-sessions, single JSON object for manual `checkpoint-<tag>.json` (a serialized `GeminiChat` history written by the Logger class). First line of auto-session is a header; subsequent lines are turns.

### Captured

Chat history with tool calls (per checkpointing docs), project hash, timestamps.

### NOT captured

File-checkpointing of edits is a separate feature, gated by `--checkpointing` flag, lives outside the chat file. MCP server config in `~/.gemini/settings.json`.

### Native resume

`/chat save <tag>` → `/chat resume <tag>` (interactive, tag-based). `/chat list`, `/chat delete`. No documented `--resume` flag equivalent to Claude/Codex — resume is in-session. Recent versions added auto-session logging (issue #5101).

### Schema stability

Younger than Claude/Codex; auto-logging is a recent addition. Treat as unstable.

### Hook points

- File watcher on `~/.gemini/tmp/*/chats/`.
- Project hash mapping in `projects.json` is the index.
- Gemini CLI is open-source (Apache-2.0), so direct integration via extension is plausible.

---

## 5. Open Interpreter

Not installed locally. Characterized from docs.

### Storage location

`<platformdirs user_data_dir>/Open Interpreter/conversations/` (macOS: `~/Library/Application Support/Open Interpreter/conversations/`). Global, not per-project.

### Format

JSON files containing the `messages` list (LMC — Language Model Conversation — format used internally). `%save_message [path]` writes `messages.json`; `%load_message [path]` restores.

### Captured

`messages[]` (role, content, plus code/tool blocks in OI's LMC schema).

### NOT captured

Working directory, environment, computer-tool state (filesystem changes OI made), per-turn model identity unless re-included.

### Native resume

`interpreter --conversations` lists; `%load_message` inside chat. Programmatic: `interpreter.messages = [...]`. Resume reliability has historically been buggy (issue #962). No `--resume <id>` flag as polished as Claude/Codex.

### Schema stability

LMC format documented but project has been quieter recently; moderately stable, verify per release.

### Hook points

- File watcher on conversations dir, or wrap the Python API (`interpreter.messages` is the supported surface).

---

## Cross-CLI comparison

| CLI | Format | Per-project? | Native resume | Tool-call format | Portability (1–5) |
|---|---|---|---|---|---|
| Claude Code | JSONL, parent-uuid DAG | Yes (`projects/<sanitized-cwd>/`) | `--resume`, `--continue` | Anthropic API `tool_use` / `tool_result` blocks, structured | **5** |
| Codex CLI | JSONL (often `.zst`), date-sharded | No (global, cwd-tagged) | `codex resume [--last/--all/<id>]` | `ResponseItem` events, structured | **4** (decode + schema churn) |
| Aider | Markdown + plain-text history | Yes (repo root) | `--restore-chat-history` | Rendered diff fences in prose | **2** (lossy back-parse) |
| Gemini CLI | JSONL auto-session + JSON checkpoints | Indirectly (project-hash dir) | `/chat save`/`resume <tag>` (in-session) | Structured tool calls in JSONL | **4** |
| Open Interpreter | JSON (LMC messages array) | No (global) | `%load_message`, `interpreter.messages` | LMC code/tool blocks, structured | **3** |

---

## Recommendations for the session service

- **Sync the opaque blobs first.** Watch `~/.claude/projects/`, `~/.codex/sessions/`, `~/.gemini/tmp/*/chats/`, OI's conversations dir, and `.aider.chat.history.md` in registered repos. Ship event-sourced append-only sync — every one of these (except Aider) is append-mostly.
- **Normalize only the four structured formats** (Claude, Codex, Gemini, OI). Aider is the outlier: treat it as write-only export-target, not import-source.
- **Cross-resume is realistic only between Claude ↔ Codex ↔ Gemini ↔ OI** at the message-text level. True tool-call cross-resume is blocked by per-vendor encrypted artifacts (Claude's `thinking.signature`, Anthropic `cache_creation_input_tokens`, Codex's `ResponseItem` reasoning items). On cross-CLI resume, drop those and accept a clean break in extended thinking / reasoning continuation.
- **Index, don't transform, at write time.** Maintain a sidecar SQLite index (session_id, cli, cwd, model, msg_count, last_ts) over the original blobs. Adapters run on read.
- **What still needs running to fully verify** — Codex rollout file internal field set in current builds (docs describe the model but not exact line schema), Open Interpreter's current on-disk JSON shape, Aider's exact rendering of multi-edit tool calls. Install each in a sandbox before finalizing adapters.

## Source paths and URLs

Local files inspected:

- `/Users/mattkorwel/.claude/projects/-Users-mattkorwel-sessions/d5c6de06-f378-4172-9f63-4eeae6574876.jsonl`
- `/Users/mattkorwel/.claude/sessions/8073.json`
- `/Users/mattkorwel/.claude/history.jsonl`
- `/Users/mattkorwel/.gemini/projects.json`, `state.json`, `tmp/mattkorwel/chats/session-2026-05-12T21-28-0eec4026.jsonl`, `tmp/mattkorwel/logs.json`

Docs:

- https://code.claude.com/docs/en/cli-reference
- https://developers.openai.com/codex/cli/reference
- https://aider.chat/docs/config/options.html
- https://aider.chat/docs/usage/commands.html
- https://geminicli.com/docs/cli/checkpointing/
- https://deepwiki.com/google-gemini/gemini-cli/3.9-session-management
- https://docs.openinterpreter.com/settings/all-settings
