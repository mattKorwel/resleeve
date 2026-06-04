package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// Hydrate materializes the target CLI's local state for a session so
// that exec'ing the returned NativeResumeCmd picks up the session
// natively. For claude, this means writing a JSONL into
// ~/.claude/projects/<cwd-encoded>/<sessionID>.jsonl.
//
// Only replay mode is implemented in this commit; prime mode follows
// in a later commit (see round-4/01-conversation-transport.md).
func (a *Adapter) Hydrate(ctx context.Context, session adapter.SessionView, opts adapter.HydrateOpts) (adapter.HydrateResult, error) {
	mode := opts.Mode
	if mode == adapter.RenderModeAuto {
		mode = adapter.RenderModeReplay
	}
	if mode != adapter.RenderModeReplay {
		return adapter.HydrateResult{}, fmt.Errorf("%w: claude.Hydrate %s mode", adapter.ErrNotImplemented, mode)
	}

	cwd := opts.Cwd
	if cwd == "" {
		cwd = session.Cwd
	}
	if cwd == "" {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: session has no cwd and none provided in opts; can't derive project dir")
	}

	if session.EventStream == nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: session view has no EventStream")
	}
	events, err := session.EventStream()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: load events: %w", err)
	}

	body, err := a.ToNative(ctx, events, mode)
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: ToNative: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: homedir: %w", err)
	}
	dir := filepath.Join(home, ".claude", "projects", encodeCwdForProjectDir(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, session.SessionID+".jsonl")

	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: write tempfile: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: rename: %w", err)
	}

	notes := []string{}
	if len(body) == 0 {
		notes = append(notes, "no events with native payload were written; session may not resume meaningfully")
	}
	withPayload := 0
	for _, e := range events {
		if len(e.Vendor.NativePayload) > 0 {
			withPayload++
		}
	}
	if skipped := len(events) - withPayload; skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d event(s) without native payload were skipped (e.g. synthesized session_start)", skipped))
	}

	return adapter.HydrateResult{
		Mode:      adapter.RenderModeReplay,
		Path:      out,
		SessionID: session.SessionID,
		Notes:     notes,
	}, nil
}

// encodeCwdForProjectDir converts a filesystem path to Claude Code's
// project-dir naming convention by replacing each '/' with '-'.
// e.g. "/Users/x/proj" -> "-Users-x-proj".
func encodeCwdForProjectDir(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}
