// Package opencode is a Stage-5 stub for the opencode CLI adapter.
//
// v1 scope is "prove the cross-CLI seam": opencode's adapter only
// implements the re-sleeve verbs (ToNative / Hydrate /
// NativeResumeCmd) via prime mode (synthesized opening prompt). The
// capture verbs (InstallBridge / UninstallBridge / FromNative) return
// adapter.ErrNotImplemented — opencode capture lands in v3 once we
// have a real hook surface to target.
//
// Replay mode is permanently unsupported here: opencode can't ingest
// claude's JSONL. Calls with mode=Replay return an explicit error
// rather than silently degrading. mode=Auto picks prime.
//
// See docs/design/round-4/01-conversation-transport.md for the full
// re-sleeve design.
package opencode
