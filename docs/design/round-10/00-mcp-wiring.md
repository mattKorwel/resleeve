# Round 10 — Cross-CLI MCP server wiring

## Problem

resleeve already ships a full MCP memory server: `resleeve mcp`
(`internal/mcp/`) exposing `resleeve_scope_*` / `plan_*` / `learning_*` /
`context` tools over stdio JSON-RPC. Every supported CLI is an MCP *client*,
so the server is callable from all of them — but nothing registered it into
each CLI's MCP config automatically. `resleeve install-bridge` only wrote the
capture **hook**.

For push-injectable CLIs (claude, codex) the hook can inject memory at
`SessionStart`. For opencode and antigravity there is no hook surface at all
(capture is db/SSE based), so the **only** way those CLIs can read resleeve
memory is by calling the MCP tools — which requires the server to be
registered in their MCP config. This round adds that registration as an
adapter-owned capability, with uninstall, dry-run, and doctor visibility.

## Interface addition

`internal/adapter/adapter.go` — two methods added to the `Adapter` interface,
implemented by all four adapters:

```go
InstallMCP(ctx context.Context, opts InstallOpts) error
UninstallMCP(ctx context.Context) error
```

These reuse the existing `InstallOpts`:

- `ResleeveBinPath` → the server command. The registered command is
  `ResleeveBinPath` plus the single argument `["mcp"]`. When empty, each
  adapter falls back to `os.Executable()` (symlink-resolved), mirroring
  `resolveBinPath` in the hook installers.
- `DryRun` → compute the resulting config and print it to stdout; write
  nothing.

Each adapter owns its CLI's config format. The entry is keyed by the server
name `resleeve` (own-block), so re-install is idempotent and uninstall
removes only resleeve's entry while preserving user-authored MCP servers. The
implementations live in a new `install_mcp.go` per adapter, mirroring the
style of the existing `install.go` hook installers (idempotent own-block
writes, marker-based re-find, atomic temp+rename, preserve user entries).

## Per-CLI config targets and formats

All locations were **verified against the real installs on this host** before
finalizing (claude / codex / opencode / antigravity are all installed).

### claude — `~/.claude.json` → top-level `mcpServers`

Verified: `~/.claude.json` carries the `mcpServers` key (it was `null` on this
host, i.e. no servers yet); `~/.claude/settings.json` holds
hooks/permissions/theme but **not** `mcpServers`. So MCP registration targets
`~/.claude.json`, a different file from the hook bridge
(`~/.claude/settings.json`).

```json
"mcpServers": {
  "resleeve": { "command": "<bin>", "args": ["mcp"] }
}
```

We own only the `resleeve` key; all 50 other top-level keys on the real file
are read and rewritten verbatim. We deliberately edit the file rather than
shell `claude mcp add`.

### codex — `$CODEX_HOME/config.toml` → `[mcp_servers.resleeve]`

Verified: `~/.codex/config.toml` carries `[mcp_servers.node_repl]` with
`command` / `args` (+ a `[mcp_servers.node_repl.env]` sub-table). `$CODEX_HOME`
is honored (default `~/.codex`).

```toml
# >>> resleeve mcp (managed) >>>
[mcp_servers.resleeve]
command = "<bin>"
args = ["mcp"]
# <<< resleeve mcp (managed) <<<
```

`config.toml` is **user-owned** (unlike `hooks.json`, which resleeve owns
wholesale). There is **no TOML library in the dependency set** and we are not
allowed to touch `go.mod`/`go.sum`, so we do **not** round-trip the whole file
through a parser (that would reformat and drop comments). Instead we manage a
**marker-delimited block** spliced in/out textually by sentinel comment lines.
Everything outside the block — including the user's other `[mcp_servers.*]`
tables and their env sub-tables — is preserved byte-for-byte. Re-install
strips the prior block and re-appends a fresh one. Uninstall removes the block;
if resleeve was the file's sole content the file is removed (matching
`UninstallBridge`'s "owned empty file shouldn't linger" behavior).

### opencode — `~/.config/opencode/opencode.json[c]` → `mcp.resleeve`

Verified: `~/.config/opencode/opencode.jsonc` exists and contains only
`{"$schema": "https://opencode.ai/config.json"}`. `$XDG_CONFIG_HOME` is
honored (falling back to `~/.config`). We prefer an existing `opencode.jsonc`,
then `opencode.json`, defaulting to `opencode.json` when neither exists.

```json
"mcp": {
  "resleeve": {
    "type": "local",
    "command": ["<bin>", "mcp"],
    "enabled": true
  }
}
```

The file is JSONC (JSON-with-comments). The real install is plain JSON; we
parse with `encoding/json` after stripping whole-line `//` comments (the only
comment form the default config uses) and rewrite as plain JSON, preserving
all other keys (e.g. `$schema`). Block comments / trailing commas are not
supported — on such a file `InstallMCP` returns an error rather than corrupt
it. (Confirm if a user keeps inline/block comments — see uncertainties.)

### antigravity — `~/.gemini/config/mcp_config.json` → `mcpServers.resleeve`

Verified: the antigravity MCP server map lives at
`~/.gemini/config/mcp_config.json` (the **shared gemini config**), carrying a
top-level `mcpServers` object with `notebooks` / `visualization` entries, each
`{command, args}`. Note this is **not**
`~/.gemini/antigravity-cli/mcp_config.json` (which does not exist); the
`~/.gemini/antigravity-cli/mcp/` directory holds per-tool descriptor JSON, not
the server map.

```json
"mcpServers": {
  "resleeve": { "command": "<bin>", "args": ["mcp"] }
}
```

We own only the `resleeve` key; `notebooks` / `visualization` and any other
keys are preserved.

## install-bridge UX

`resleeve install-bridge` gains flags (existing hook behavior is unchanged
when none of the new flags are passed):

- `--mcp` — ALSO register the `resleeve mcp` server (in addition to the
  capture hook).
- `--mcp-only` — register ONLY the MCP server; skip the capture hook
  entirely. This is the memory-pull path for CLIs that cannot be push-injected
  via a hook (opencode / antigravity).
- `--uninstall` — remove resleeve's entries instead of installing. Honors
  `--mcp` / `--mcp-only` to scope what is removed (hook, mcp, or both).
- existing `--adapter`, `--memory-only`, `--dry-run` unchanged.

Examples:

```
resleeve install-bridge --adapter claude --mcp          # hook + mcp
resleeve install-bridge --adapter opencode --mcp-only    # mcp only
resleeve install-bridge --adapter codex --mcp-only --uninstall
resleeve install-bridge --adapter antigravity --mcp-only --dry-run
```

The confirmation line names what was touched ("Installed codex mcp.",
"Installed claude bridge + mcp.", "Removed opencode mcp entries.").

## doctor card

`resleeve doctor` gains one `mcp (<cli>)` card per supported CLI, printed
right after the existing `bridge (claude)` card. Each card reads the CLI's MCP
config file directly (mirroring `printBridge`'s file-inspection style rather
than going through the adapter) and reports `✓ registered` / `✗ not
registered` with the resolved path:

```
mcp (claude)       ✓ registered in /Users/.../.claude.json
mcp (codex)        ✗ not registered in /Users/.../.codex/config.toml
mcp (opencode)     ✗ not registered in /Users/.../.config/opencode/opencode.jsonc
mcp (antigravity)  ✗ not registered (no config at /Users/.../.gemini/config/mcp_config.json)
```

JSON CLIs are checked by parsing and looking for the `resleeve` key under the
right top-level object (`mcpServers` for claude/antigravity, `mcp` for
opencode); codex is checked by the `[mcp_servers.resleeve]` text marker.

## Tests

Per-adapter `install_mcp_test.go` (`t.TempDir` + HOME/`CODEX_HOME`/
`XDG_CONFIG_HOME` override, matching the existing `install_test.go` style):
writes the entry, idempotent re-install (exactly one resleeve entry), uninstall
removes only ours and preserves a user-authored MCP server, dry-run writes
nothing. opencode/claude/antigravity assert valid JSON with the right nesting;
codex asserts the TOML table + `command`/`args` lines and that other
`[mcp_servers.*]` tables survive.

## Uncertainties for an integrator to confirm

- **claude**: this host's `~/.claude.json` had `mcpServers: null`. We treat a
  null/missing value as "create the object." Confirm Claude Code reads
  user-scoped MCP servers from `~/.claude.json` `mcpServers` on the target
  Claude Code version (some versions also support project-scoped `.mcp.json`
  and `claude mcp add --scope`). We chose the user file form deliberately.
- **opencode**: we strip only whole-line `//` comments. A config with inline
  or block comments / trailing commas will fail to parse and `InstallMCP`
  errors out (safe, but the user must register manually or remove comments).
  Confirm the `mcp.<name>` local shape (`type: "local"`, `command: [...]`,
  `enabled: true`) matches the opencode version in use.
- **codex**: the managed marker-block approach is robust to comments and other
  tables, but assumes the user does not hand-edit *inside* the sentinel
  comments. Trust model unchanged: codex MCP servers, like hooks, may require
  the user to trust them once via the TUI.
- **antigravity**: confirm the gemini CLI / antigravity build in use reads
  `~/.gemini/config/mcp_config.json` (vs. an IDE-specific
  `~/.gemini/antigravity-ide/...` location). The `{command, args}` shape was
  taken directly from the existing `notebooks`/`visualization` entries on this
  host.
```
