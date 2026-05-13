package claude

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
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

// InstallBridge / UninstallBridge are implemented in install.go.

// ToNative renders normalized events back into Claude Code's native
// JSONL format. Deferred to Stage 5 (re-sleeving).
func (a *Adapter) ToNative(ctx context.Context, events []event.Event, target adapter.NativeFormat) ([]byte, error) {
	return nil, fmt.Errorf("%w: claude.ToNative (Stage 5)", adapter.ErrNotImplemented)
}

// Hydrate materializes the harness's native session file in a target
// workspace. Deferred to Stage 5.
func (a *Adapter) Hydrate(ctx context.Context, session adapter.SessionView, target adapter.Workspace) error {
	return fmt.Errorf("%w: claude.Hydrate (Stage 5)", adapter.ErrNotImplemented)
}

// NativeResumeCmd returns the command + args to launch Claude Code in
// resume mode for a given session.
func (a *Adapter) NativeResumeCmd(session adapter.SessionView) (string, []string) {
	return "claude", []string{"--resume", session.SessionID}
}
