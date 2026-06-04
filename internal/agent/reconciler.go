package agent

import "context"

// Reconciler is a callback the daemon fires once at startup, after
// Serve binds the listener and (if upstream is configured) sync starts.
// Each registered Reconciler runs in its own goroutine; non-context
// errors are logged but non-fatal. Q12 introduces this so the daemon
// stops importing concrete CLI adapter packages (e.g.
// internal/adapter/claude): the CLI layer hands the daemon a list of
// reconcile callbacks and the daemon stays adapter-neutral.
//
// Typical wiring (in internal/cli/agent.go):
//
//	cfg.Reconcilers = []agent.Reconciler{
//	    func(ctx context.Context, d *agent.Daemon) error {
//	        return claude.New().ReconcileOnce(ctx, d)
//	    },
//	}
//
// The reconcile callback is free to call any *Daemon method (notably
// IngestBatch) — adapter Ingester contracts are satisfied at the
// call site, not the daemon side.
type Reconciler func(ctx context.Context, d *Daemon) error
