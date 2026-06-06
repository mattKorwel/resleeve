package antigravity

import (
	"context"
	"os/exec"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/registry"
)

// Name is the adapter identifier registered with the daemon.
const Name = "antigravity"

// binary is the Antigravity CLI executable name probed on $PATH.
const binary = "agy"

func init() {
	registry.Register(Name, func() adapter.Adapter { return New() })
}

// Adapter implements adapter.Adapter for the Google Antigravity CLI.
type Adapter struct{}

// New returns a fresh Antigravity adapter.
func New() *Adapter { return &Adapter{} }

// Name returns the adapter identifier.
func (a *Adapter) Name() string { return Name }

// Detect probes for the `agy` binary on $PATH and queries its version.
// Returns Detection{Installed: false} when not found — never an error.
//
// The Quirks map records the two facts that make this adapter unusual, so
// operator-facing diagnostics (`resleeve doctor`) can surface them:
//   - capture mode is "db-watch" (file/SQLite reconcile), not a live bridge;
//   - re-sleeve supports prime only (no native replay/import).
func (a *Adapter) Detect(ctx context.Context) (adapter.Detection, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return adapter.Detection{Installed: false}, nil
	}
	quirks := map[string]string{
		"capture":   "db-watch", // SQLite-per-conversation reconcile; no hook bridge
		"resleeve":  "prime-only",
		"transport": "sqlite+protobuf",
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	out, err := cmd.Output()
	version := strings.TrimSpace(string(out))
	d := adapter.Detection{Installed: true, Path: path, Version: version, Quirks: quirks}
	if err != nil {
		// Version probe failed but binary exists — still installed.
		return d, nil
	}
	return d, nil
}

// InstallBridge / UninstallBridge live in install.go.
// FromNative lives in fromnative.go.
// ReconcileOnce + Ingester live in watcher.go.
// Steps → events decoding lives in steps.go.
// ToNative / Hydrate / NativeResumeCmd live in tonative.go / hydrate.go / native_resume.go.
