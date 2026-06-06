package cli

import (
	"context"

	"github.com/mattkorwel/resleeve/internal/adapter/codex"
	"github.com/mattkorwel/resleeve/internal/agent"
)

// Wires the codex adapter into the CLI. The named import triggers the codex
// package's init() (registry self-registration); the reconciler registration
// gives the daemon a startup sweep over $CODEX_HOME/sessions, gated on Detect
// so it no-ops when codex isn't installed.
func init() {
	registerReconciler(func(ctx context.Context, d *agent.Daemon) error {
		a := codex.New()
		if det, _ := a.Detect(ctx); !det.Installed {
			return nil
		}
		return a.ReconcileOnce(ctx, d)
	})
}
