package antigravity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/common"
)

// Hydrate materializes target-CLI state for a session.
//
// Antigravity supports PRIME mode only (the auto default here): it writes a
// synthesized markdown opening prompt to ~/.resleeve/hydrate/<new-uuid>.md
// and mints a fresh session id, conceptually starting a new agy conversation
// seeded with the prior context. REPLAY (writing a native conversation .db)
// is unsupported and returns a clear error — see ToNative for why.
//
// For same-CLI resume of an antigravity-origin session, callers should skip
// Hydrate and use NativeResumeCmd directly with the original conversation id;
// `agy --conversation <id>` reopens the native store with full fidelity.
func (a *Adapter) Hydrate(ctx context.Context, session adapter.SessionView, opts adapter.HydrateOpts) (adapter.HydrateResult, error) {
	mode := opts.Mode
	if mode == adapter.RenderModeAuto {
		mode = adapter.RenderModePrime
	}
	if mode == adapter.RenderModeReplay {
		return adapter.HydrateResult{}, fmt.Errorf("antigravity.Hydrate: replay mode is unsupported; resume an antigravity-origin session natively via `agy --conversation %s`, or pass --mode prime", session.SessionID)
	}
	if mode != adapter.RenderModePrime {
		return adapter.HydrateResult{}, fmt.Errorf("antigravity.Hydrate: unknown mode %q", mode)
	}
	if session.EventStream == nil {
		return adapter.HydrateResult{}, fmt.Errorf("antigravity.Hydrate: session view has no EventStream")
	}
	events, err := session.EventStream()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("antigravity.Hydrate: load events: %w", err)
	}

	body, err := common.SynthesizePrime(session, events, common.PrimeOpts{
		PlanContent: opts.PlanContent,
	})
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("antigravity.Hydrate: synthesize prime: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("antigravity.Hydrate: homedir: %w", err)
	}
	dir := filepath.Join(home, ".resleeve", "hydrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("antigravity.Hydrate: mkdir %s: %w", dir, err)
	}

	newID := uuid.NewString()
	out := filepath.Join(dir, newID+".md")
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("antigravity.Hydrate: write tempfile: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("antigravity.Hydrate: rename: %w", err)
	}

	return adapter.HydrateResult{
		Mode:      adapter.RenderModePrime,
		Path:      out,
		SessionID: newID,
		Notes: []string{
			"prime mode: synthesized opening prompt; antigravity has no native transcript import, so tool calls are flattened to text",
		},
	}, nil
}
