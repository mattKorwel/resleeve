// Package codex is the OpenAI Codex CLI adapter — Tier A (full
// bidirectional). It mirrors the claude adapter almost method-for-method:
// a stdin-JSON hook bridge via $CODEX_HOME/hooks.json plus a file-watcher
// safety net over the rollout JSONL written under
// $CODEX_HOME/sessions/<YYYY>/<MM>/<DD>/rollout-<ts>-<uuid>.jsonl.
//
// Replay = write a rollout file then `codex resume <uuid>`. Prime =
// synthesize an opening prompt then `codex exec -` reading it from stdin.
//
// Two install-time gotchas (verified against codex source):
//   - Hooks are untrusted by default and inert until the user trusts them
//     once via the TUI review. InstallBridge surfaces this; doctor should
//     warn "bridge installed but untrusted → hooks are no-ops".
//   - There is no durable per-hook id, so the installer OWNS the
//     $CODEX_HOME/hooks.json file wholesale (read → merge → write).
//
// See docs/design/round-9/01-codex-adapter.md.
package codex
