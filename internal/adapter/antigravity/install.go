package antigravity

import (
	"context"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// InstallBridge is a no-op for Antigravity. Unlike Claude Code (which exposes
// a settings.json hook surface resleeve writes into), Antigravity has no
// plugin/hook mechanism: capture is performed entirely by ReconcileOnce
// watching the per-conversation SQLite databases. There is nothing to
// install, so this returns nil — installation is implicitly "always on" via
// the startup reconcile sweep wired in internal/cli/adapter_antigravity.go.
func (a *Adapter) InstallBridge(ctx context.Context, opts adapter.InstallOpts) error {
	// No bridge to install; capture is file/db-watch based. Intentionally a
	// successful no-op so `resleeve install --all` doesn't error on hosts
	// that have agy installed.
	return nil
}

// UninstallBridge is the symmetric no-op. Since InstallBridge writes nothing,
// there is nothing to remove.
func (a *Adapter) UninstallBridge(ctx context.Context) error {
	return nil
}
