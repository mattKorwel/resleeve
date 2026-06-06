package antigravity

import (
	"context"
	"fmt"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// FromNative is not a live hook surface for Antigravity. The Adapter
// interface keeps this method for parity with hook-driven adapters (claude),
// but Antigravity capture goes exclusively through ReconcileOnce, which
// decodes the per-conversation SQLite databases directly. There is no single
// "native input" (hook envelope / JSONL line) to translate here, so every
// source kind returns a clear, actionable error rather than silently
// dropping input.
func (a *Adapter) FromNative(ctx context.Context, raw []byte, src adapter.Source) ([]event.Event, error) {
	return nil, fmt.Errorf(
		"antigravity: FromNative is unsupported (source kind %v); capture runs through ReconcileOnce over ~/.gemini/antigravity-cli/conversations/*.db, not a live hook",
		src.Kind,
	)
}
