package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/common"
	"github.com/mattkorwel/resleeve/internal/event"
)

// toNativePrime renders the captured events as a synthesized markdown
// opening prompt. See docs/design/round-4/01-conversation-transport.md
// for the section layout. The session view passed here is the
// already-loaded one from Hydrate; for ToNative-direct callers without
// a session view we synthesize a placeholder so the renderer still has
// something to put in the Context block.
func (a *Adapter) toNativePrime(session adapter.SessionView, events []event.Event) ([]byte, error) {
	return common.SynthesizePrime(session, events, common.PrimeOpts{})
}

// hydratePrime writes a prime-mode opening prompt to a scratch file
// under ~/.resleeve/hydrate/<new-uuid>.md and returns the path. The
// session id in the result is freshly minted because, conceptually,
// prime mode starts a new conversation in the target CLI — even when
// the target is also claude. opts.PlanContent (if non-empty) is
// forwarded to the prime synthesizer to populate the "## Plan" body.
func (a *Adapter) hydratePrime(ctx context.Context, session adapter.SessionView, events []event.Event, opts adapter.HydrateOpts) (adapter.HydrateResult, error) {
	body, err := common.SynthesizePrime(session, events, common.PrimeOpts{
		PlanContent: opts.PlanContent,
	})
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: synthesize prime: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: homedir: %w", err)
	}
	dir := filepath.Join(home, ".resleeve", "hydrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: mkdir %s: %w", dir, err)
	}

	newID := uuid.NewString()
	out := filepath.Join(dir, newID+".md")
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: write tempfile: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("claude.Hydrate: rename: %w", err)
	}

	notes := []string{
		"prime mode: synthesized opening prompt; tool calls flattened to text",
	}
	return adapter.HydrateResult{
		Mode:      adapter.RenderModePrime,
		Path:      out,
		SessionID: newID,
		Notes:     notes,
	}, nil
}
