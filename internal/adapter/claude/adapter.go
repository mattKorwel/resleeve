package claude

import (
	"context"
	"os/exec"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// Name is the adapter identifier registered with the daemon.
const Name = "claude"

// Adapter implements adapter.Adapter for Claude Code.
type Adapter struct{}

// New returns a fresh Claude Code adapter.
func New() *Adapter { return &Adapter{} }

// Name returns the adapter identifier.
func (a *Adapter) Name() string { return Name }

// Detect probes for the `claude` binary on $PATH and queries its version.
// Returns Detection{Installed: false} when not found — never an error.
func (a *Adapter) Detect(ctx context.Context) (adapter.Detection, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return adapter.Detection{Installed: false}, nil
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	out, err := cmd.Output()
	version := strings.TrimSpace(string(out))
	d := adapter.Detection{Installed: true, Path: path, Version: version}
	if err != nil {
		// Version probe failed but binary exists — that's still installed.
		return d, nil
	}
	return d, nil
}

// InstallBridge / UninstallBridge live in install.go.
// FromNative lives in fromnative.go.
// ToNative / Hydrate / NativeResumeCmd live in tonative.go / hydrate.go / native_resume.go.
