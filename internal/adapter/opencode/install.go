package opencode

import (
	"context"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// InstallBridge is a no-op for opencode. opencode capture is bridge-free:
// resleeve reads opencode.db (read-only) and optionally subscribes to the
// server's GET /event SSE stream. There is no plugin to install and no
// config to edit, so this returns nil (not adapter.ErrNotImplemented).
func (a *Adapter) InstallBridge(ctx context.Context, opts adapter.InstallOpts) error {
	// opencode capture is bridge-free: SSE + db read. Nothing to install.
	return nil
}

// UninstallBridge is a no-op for opencode, mirroring InstallBridge.
func (a *Adapter) UninstallBridge(ctx context.Context) error {
	// opencode capture is bridge-free: SSE + db read. Nothing to remove.
	return nil
}
