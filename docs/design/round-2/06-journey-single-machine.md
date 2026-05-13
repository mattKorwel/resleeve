# Journey #3: single-machine solo dev

One user. One machine. No orchestrator, no second host. Resleeve as an invisible session-capture + search layer over normal CLI usage.

This is the **MVP user.** Resleeve has to be invisible during normal use ("set it up once, forget it exists") and useful when needed ("find that thing"). Mostly tests:

1. The absolute simplest happy path — install and forget.
2. First-time setup UX.
3. The human-facing surface for browsing / searching captured sessions.

## Setup

```
$ brew install resleeve
$ resleeve up
  ✓ Created data dir: ~/.local/share/resleeve
  ✓ Master password set (derived key cached in system keychain).
  ✓ Recovery key: RESL-XXXX-YYYY-ZZZZ
    SAVE THIS — required to recover if you lose your password.
  ✓ Daemon listening at $XDG_RUNTIME_DIR/resleeve.endpoint
  ✓ Detected Claude Code; installed bridge to ~/.claude/settings.json
$
```

`resleeve up` is one command. It:

- Creates the data dir.
- Prompts for master password (or generates one and writes to system keychain).
- Generates a recovery key, displays once, never persisted server-side.
- Starts the daemon (random TCP port, writes endpoint file per `02-journey-01-decisions.md` Q4).
- Detects installed CLIs and installs the bridge plugin for each.

After `up`, the user does nothing different. They run `claude` like always; resleeve captures in the background.

## Golden path: just use Claude Code

```
$ cd ~/projects/auth-rewrite
$ claude
  # Bridge plugin auto-detects daemon via $XDG_RUNTIME_DIR/resleeve.endpoint.
  # Sessions captured as a side effect.
  # ... user works ...
$ exit
  # Session captured. Domain bus emits session.ended.
```

Later, the user wants to find something:

```
$ resleeve session search "JWT validation bug"
  01HZ001  auth-rewrite/default  2h ago  "fix the auth bug" → JWT validation...
  01HZD45  legacy-tokens/default 3d ago  "audit JWT usage" → ...

$ resleeve session show 01HZ001
  # Pretty-printed transcript
```

Or browses the recent list:

```
$ resleeve session list --recent
  01HZ001  auth-rewrite       2h ago  ended  47 events
  01HZD45  legacy-tokens      3d ago  ended  812 events
  01HZK00  side-quest         5d ago  ended  23 events
```

Or watches a still-running session live (the firehose SSE endpoint from `04-event-schema.md`):

```
$ resleeve session tail 01HZ001
  [22:13:04] thinking: "The auth bug likely lives in the JWT validation path..."
  [22:13:08] assistant: "I'll start by reading the auth middleware."
  [22:13:09] tool_call: Read(file_path="/src/auth.go")
  [22:13:09] tool_result: package auth\n\nimport (...
  ...
```

## Sad paths

### S1. Daemon not running

Bridge plugin checks for `$XDG_RUNTIME_DIR/resleeve.endpoint`. Not there?

- **Default:** auto-start the daemon on demand if the `resleeve` binary is on `$PATH`. The bridge plugin spawns `resleeve agent --background` and waits for the endpoint file to appear (timeout 2s).
- **Override:** `RESLEEVE_NO_AUTOSTART=1` disables this; bridge plugin no-ops instead.

The user who ran `resleeve up` once expects capture to work next time they open a terminal. Auto-start makes the magic work.

### S2. Disk space concerns

User's data dir has been growing. Sessions accumulate.

- `resleeve gc --older-than=90d [--dry-run]` to prune.
- `resleeve usage` shows storage per scope + per session.
- Default: no autoprune. Honest about not deleting user data without asking.

### S3. Privacy: secrets in transcripts

A tool call returned a config file containing an API key. It's now in the captured session.

- Default redaction rules apply *on share*, not on capture (see [`07-journey-oss-share.md`](./07-journey-oss-share.md)).
- v2+: opt-in capture-time redaction rules in config.
- For users worried about local-disk plaintext: data-at-rest encryption is on by default (key in keychain, ciphertext on disk).

### S4. Multiple CLIs on the same machine

User runs `claude` for one project, `opencode` for another, `gemini` for a third. All three should capture.

- `resleeve up` detects all installed CLIs and prompts for each. `--yes` accepts all defaults.
- Each adapter's bridge plugin lives in the harness-specific settings file (`~/.claude/settings.json`, `~/.opencode/...`, `~/.gemini/settings.json`).
- Sessions are tagged with `vendor.name` so filtering / search by CLI works.

### S5. User uninstalls

```
$ resleeve down
  ✓ Stopped daemon.
  ✓ Removed bridge from ~/.claude/settings.json
  ✓ Data preserved at ~/.local/share/resleeve (use `resleeve purge` to delete).
```

Clean teardown is a v1 requirement. Data preservation is the default; explicit purge required.

## What's actually NEW vs. fleet-operator journey

- `resleeve up` one-command setup (no orchestrator, no remote server).
- Auto-start of daemon by bridge plugin.
- System keychain integration for derived-key storage (`KeyStore` abstraction: macOS Keychain, Linux Secret Service, Windows Credential Manager).
- `resleeve session tail` for live viewing (uses the SSE firehose).
- `resleeve gc` / `resleeve usage` for housekeeping.
- `resleeve down` / `resleeve purge` for clean uninstall.

## Open questions

- **`resleeve up` defaults:** prompt vs. silent generation of master password? Lean: prompt by default, `--generate-password` to auto-create + show.
- **First-run UX format:** plain CLI prompts (v1) vs. opinionated TUI (v2). Lean: plain CLI.
- **Daemon logs location:** `~/.local/state/resleeve/daemon.log`, rotated by size.
- **System keychain abstraction:** `KeyStore` interface with three impls (macOS / Linux / Windows). Fallback: encrypted-on-disk key file with a password prompt every daemon start. Worth designing now.
- **`resleeve up` re-runs:** idempotent? Re-detect CLIs and install missing bridges? Lean: yes, with `--reinstall` to force.
- **`resleeve session tail` for ended sessions:** what does it do? Lean: play back the recorded events at original timing (or fast). Like asciinema for AI sessions.
