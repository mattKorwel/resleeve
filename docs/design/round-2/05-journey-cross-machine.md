# Journey #2: cross-machine solo dev

One user, two hosts (laptop + cloudtop, or laptop + desktop). No orchestrator. Wants their work to follow them when they switch machines.

This is **"fleet of size 1 distributed across 2 hosts, no orchestrator."** Mostly tests:

1. Identity propagation: same user, different hosts.
2. The `resleeve login` / bootstrap flow on a fresh machine.
3. Human-facing resume verbs (not orchestrator-driven).
4. Bridge-plugin auto-detection in standalone mode (per [`02-journey-01-decisions.md`](./02-journey-01-decisions.md) Q4 + Q10).

## Setup

### First host (laptop, already running)

User has already:

- Installed resleeve.
- Run `resleeve serve` somewhere (laptop itself, a small VPS, a homelab box).
- Run `resleeve login` to derive an Argon2id key from a master password.
- Bridge plugin installed in `~/.claude/settings.json`; daemon auto-starts on first invocation.

### New host (cloudtop, fresh)

User SSHs into cloudtop for the first time:

```
$ brew install resleeve   # or curl-pipe-sh, or cargo install, etc.
$ resleeve login
  Server URL: https://resleeve.example.com
  Email:      matt@example.com
  Master password: ********
  ✓ Logged in. Token saved to ~/.config/resleeve/auth.json.
  ✓ Key derived locally; server never sees it.
$ resleeve install-bridge --auto
  ✓ Detected Claude Code at /usr/local/bin/claude
  ✓ Installed bridge to ~/.claude/settings.json
  ✓ Daemon configured to start on first claude invocation.
$ claude
  # Claude Code boots, bridge plugin starts daemon, daemon authenticates,
  # session captures stream to server.
```

After this, cloudtop is just like laptop — fresh sessions captured, server queryable.

## Golden path: switch machines mid-task

User is fixing an auth bug on the laptop. An hour in, they want to continue on the cloudtop.

1. **Laptop:** user stops `claude` (Ctrl-D or terminal close). Session ends gracefully. Bridge plugin flushes events. Server records `session_end`.

2. **Cloudtop:** user SSHs in. They want to find and resume the same session.

   ```
   $ resleeve session list
     ID        SCOPE                AGENT    STARTED  LAST    STATUS
     01HZ001   auth-rewrite        default  1h ago   2m ago  ended
     01HZF12   docs-fixes          default  3h ago   3h ago  ended
   ```

3. Pick the latest auth-rewrite session and resume:

   ```
   $ resleeve session resume 01HZ001
     ✓ Hydrated session 01HZ001 to ~/.claude/projects/<sanitized>/<uuid>.jsonl
     ✓ Launching: claude --resume 01HZ001 (cwd: ~/projects/auth-rewrite)
     # Claude Code starts in resumed mode; full context restored.
   ```

   Or — the more slot-y version — start a new session under the same slot:

   ```
   $ resleeve session new --scope=auth-rewrite --resume-latest
   ```

   The slot model (`02-journey-01-decisions.md` Q3) means even if the cloudtop session has a *new* `session_id`, the slot `(scope=auth-rewrite, agent=default)` is the same; relationship preserved across pods/machines.

4. User keeps working. New events stream to server. When they next look on laptop, the new session is visible there too.

## Sad paths

### S1. Cloudtop has no Claude Code installed

`resleeve session resume <id>` knows which CLI produced the session (`vendor.name = "claude_code"`). On hydrate, checks for the CLI binary on `$PATH`. If missing:

> Error: this session was created by claude_code; no binary on $PATH.  
> Install Claude Code, or use `--cross-cli=opencode` to re-sleeve into opencode (extended-thinking continuity will be dropped).

### S2. Worktree mismatch

Laptop session ran in `~/projects/auth-rewrite`. Cloudtop has the repo at `~/work/auth-rewrite`. Resume into a non-existent cwd confuses the harness.

- `resleeve session resume` detects the recorded cwd doesn't exist on this host.
- Prompts: *"Original cwd was `~/projects/auth-rewrite`. Provide `--cwd` or skip."*
- User passes `--cwd=~/work/auth-rewrite`. Resleeve rewrites the hydrated session file's `cwd` field so the harness resumes in the right directory.

### S3. Different harness version

Laptop ran Claude Code 2.1.140; cloudtop has 2.2.x with new event types. Per `04-event-schema.md`, the bridge captures `vendor.version` per event. Resleeve's adapter handles old-version `native_payload` on read. New-version events on cloudtop carry the newer version stamp. Round-trip stays clean.

### S4. Lost master password on new host

Atuin-style recovery:

- User must have saved their recovery key at signup (single-use, displayed once, never recoverable from server).
- `resleeve login --recovery-key=RESL-XXXX-YYYY-ZZZZ` validates the key, lets them reset the password, re-derives the encryption key.
- No recovery key = no recovery. Documented hard at signup.

### S5. Server unreachable from new host

- Login fails with a clear error.
- Bridge plugin auto-detect (Q10) returns "no resleeve" → silent no-op. Session captures to the harness's native file only.
- Manual re-sync once connectivity returns: `resleeve sync --since=<ts>` ingests local Claude Code session files into the server.

### S6. Concurrent sessions on the same slot

User accidentally has `claude` running on the laptop AND cloudtop, both under the same scope. Two sessions, same slot.

- Both run independently — separate `session_id`s, both tagged with the same `(scope, agent_name)`.
- `resleeve fleet status --scope=auth-rewrite` shows two `slot.live` entries.
- This is fine: sessions are per-pod, no conflict in resleeve. The user should pick one and `Ctrl-C` the other if they don't want parallel work.
- No automatic preemption in v1.

## What's actually NEW vs. fleet-operator journey

- `resleeve login` and bootstrap UX (orchestrator-less identity flow).
- Recovery-key handling.
- Worktree-mismatch resolution (cwd rewrite on hydrate).
- `resleeve install-bridge --auto` for bridge plugin installation.
- "Concurrent sessions same slot" being a no-op (not a conflict).

## Open questions

- **Multi-device auth UX beyond passwords:** MFA? Hardware keys (FIDO2)? Lean: v2+. v1 is password + recovery key.
- **Local-first mode:** if a user wants resleeve server + CLI on the same laptop (no remote), is there a simpler `resleeve up --local-only` shortcut that skips the URL prompt? Lean: yes.
- **Tab completion / TUI picker for resume:** users won't memorize ULIDs. Default to fzf-style picker if no ID given, mirroring Claude Code's own `--resume` picker.
- **Auto-detect of CLIs by `install-bridge --auto`:** how aggressive? Detect Claude/opencode/Codex/Gemini binaries on `$PATH` and prompt-per-CLI? Lean: yes, prompt-per-CLI, with `--yes` for auto-accept.
- **Memory layer cross-machine (v2+):** when memory ships, plans/learnings also need to follow. Same authn presumably; designed alongside memory.
