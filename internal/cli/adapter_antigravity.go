package cli

// Per-adapter CLI wiring for Antigravity (`agy`). Importing the adapter
// package (below, for its init()) self-registers it in the adapter registry;
// the init() here adds its startup reconcile sweep. Keeping this in its own
// file means the adapter lands without editing any shared list — no
// cross-adapter merge conflicts (see cli/adapters.go for the pattern).

import (
	"context"

	"github.com/mattkorwel/resleeve/internal/adapter/antigravity"
	"github.com/mattkorwel/resleeve/internal/agent"
)

func init() {
	// antigravity: reconcile sweep over ~/.gemini/antigravity-cli/conversations.
	// Gated on Detect so hosts without `agy` installed skip it cleanly (the
	// sweep would otherwise just no-op on a missing dir, but probing first
	// keeps daemon logs quiet and matches the intended "only run for
	// installed CLIs" contract).
	registerReconciler(func(ctx context.Context, d *agent.Daemon) error {
		a := antigravity.New()
		if det, _ := a.Detect(ctx); !det.Installed {
			return nil
		}
		return a.ReconcileOnce(ctx, d)
	})
}
