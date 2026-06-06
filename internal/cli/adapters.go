package cli

// Per-adapter CLI wiring. Each supported adapter contributes two things:
//
//   - registry self-registration — performed by the adapter package's own
//     init() (see internal/adapter/<name>/adapter.go), triggered simply by
//     importing the package (here for claude, or in a dedicated
//     cli/adapter_<name>.go file for each other adapter);
//   - an optional startup reconcile sweep — registered via
//     registerReconciler.
//
// Keeping each adapter's wiring in its OWN file (cli/adapter_codex.go,
// cli/adapter_opencode.go, …) means a new adapter lands without editing any
// shared list — no cross-adapter merge conflicts.

import (
	"context"

	"github.com/mattkorwel/resleeve/internal/adapter/claude"
	"github.com/mattkorwel/resleeve/internal/agent"
)

// startupReconcilers is the set of adapter reconcile sweeps the daemon runs
// once at startup. It is populated from init()s via registerReconciler so
// the daemon stays CLI-neutral (Q12) and adapters wire in per-file.
var startupReconcilers []agent.Reconciler

// registerReconciler adds a reconcile sweep to the daemon's startup set.
// Called from adapter wiring init()s.
func registerReconciler(r agent.Reconciler) {
	startupReconcilers = append(startupReconcilers, r)
}

func init() {
	// claude: full reconcile sweep over ~/.claude/projects (always on).
	registerReconciler(func(ctx context.Context, d *agent.Daemon) error {
		return claude.New().ReconcileOnce(ctx, d)
	})
}
