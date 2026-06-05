package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/common"
	"github.com/mattkorwel/resleeve/internal/event"
)

// toNativePrime renders the captured events as a synthesized markdown
// opening prompt (delegated to the shared synthesizer).
func (a *Adapter) toNativePrime(session adapter.SessionView, events []event.Event) ([]byte, error) {
	return common.SynthesizePrime(session, events, common.PrimeOpts{})
}

// hydratePrime writes a prime-mode opening prompt to a scratch file under
// ~/.resleeve/hydrate/<new-uuid>.md and returns the path. A fresh session
// id is minted because prime mode conceptually starts a new conversation
// in the target CLI. opts.PlanContent (if non-empty) is forwarded to the
// prime synthesizer to populate the "## Plan" body.
func (a *Adapter) hydratePrime(ctx context.Context, session adapter.SessionView, events []event.Event, opts adapter.HydrateOpts) (adapter.HydrateResult, error) {
	body, err := common.SynthesizePrime(session, events, common.PrimeOpts{
		PlanContent: opts.PlanContent,
	})
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: synthesize prime: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: homedir: %w", err)
	}
	dir := filepath.Join(home, ".resleeve", "hydrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: mkdir %s: %w", dir, err)
	}

	newID := NewSessionUUIDv7()
	out := filepath.Join(dir, newID+".md")
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: write tempfile: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: rename: %w", err)
	}

	notes := []string{
		"prime mode: synthesized opening prompt; tool calls flattened to text",
		"resume via `codex exec -` reading the prompt from stdin",
	}
	return adapter.HydrateResult{
		Mode:      adapter.RenderModePrime,
		Path:      out,
		SessionID: newID,
		Notes:     notes,
	}, nil
}
