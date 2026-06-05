package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/registry"
)

// Name is the adapter identifier registered with the daemon.
const Name = "codex"

func init() {
	registry.Register(Name, func() adapter.Adapter { return New() })
}

// Adapter implements adapter.Adapter for the OpenAI Codex CLI.
type Adapter struct{}

// New returns a fresh Codex adapter.
func New() *Adapter { return &Adapter{} }

// Name returns the adapter identifier.
func (a *Adapter) Name() string { return Name }

// Detect probes for the `codex` binary on $PATH and queries its version.
// Returns Detection{Installed: false} when not found — never an error.
// The detected codex home (CODEX_HOME override or ~/.codex) is reported
// under Quirks["codex_home"] so callers can derive rollout/hook paths.
func (a *Adapter) Detect(ctx context.Context) (adapter.Detection, error) {
	home := codexHome()
	path, err := exec.LookPath("codex")
	if err != nil {
		return adapter.Detection{
			Installed: false,
			Quirks:    map[string]string{"codex_home": home},
		}, nil
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	out, _ := cmd.Output()
	d := adapter.Detection{
		Installed: true,
		Path:      path,
		Version:   parseVersion(string(out)),
		Quirks:    map[string]string{"codex_home": home},
	}
	return d, nil
}

// parseVersion extracts the trailing semver from `codex --version`
// output, tolerating the `codex-cli ` prefix (e.g. "codex-cli 0.121.0").
// Falls back to the trimmed raw string when no recognizable form is found.
func parseVersion(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Take the last whitespace-separated token — that's the semver for
	// both "codex-cli 0.121.0" and a bare "0.121.0".
	fields := strings.Fields(s)
	last := fields[len(fields)-1]
	return strings.TrimPrefix(last, "codex-cli")
}

// codexHome returns the Codex home directory, honoring the CODEX_HOME
// override and falling back to ~/.codex. Used for path derivation
// everywhere (rollout sessions tree, hooks.json).
func codexHome() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

// InstallBridge / UninstallBridge live in install.go.
// FromNative lives in fromnative.go.
// ToNative / Hydrate / NativeResumeCmd live in tonative.go / hydrate.go / native_resume.go.
