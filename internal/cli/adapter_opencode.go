package cli

import (
	"context"

	"github.com/mattkorwel/resleeve/internal/adapter/opencode"
	"github.com/mattkorwel/resleeve/internal/agent"
)

// Wires the opencode adapter into the CLI. The named import triggers the
// opencode package's init() (registry self-registration); the reconciler
// gives the daemon a read-only backfill sweep over opencode.db, gated on
// Detect — skipped when opencode isn't installed or its store predates the
// SQLite era (capture/replay target dev/SQLite only).
func init() {
	registerReconciler(func(ctx context.Context, d *agent.Daemon) error {
		a := opencode.New()
		det, _ := a.Detect(ctx)
		if !det.Installed || det.Quirks["unsupported"] == "pre-sqlite" {
			return nil
		}
		return a.ReconcileOnce(ctx, d)
	})
}
