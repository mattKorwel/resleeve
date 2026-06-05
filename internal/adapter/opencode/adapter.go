package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/registry"
)

// Name is the adapter identifier registered with the daemon and used by
// `resleeve resume --cli opencode`.
const Name = "opencode"

func init() {
	registry.Register(Name, func() adapter.Adapter { return New() })
}

// Adapter implements adapter.Adapter for opencode (dev / SQLite era).
type Adapter struct{}

// New returns a fresh opencode adapter.
func New() *Adapter { return &Adapter{} }

// Name returns the adapter identifier.
func (a *Adapter) Name() string { return Name }

// Detect probes for the `opencode` binary on $PATH and verifies the
// SQLite era is in play.
//
// Returns Detection{Installed: false} when the binary is missing — never
// an error. When the binary exists but the SQLite-era artifacts don't
// (opencode.db / $OPENCODE_DB), Detect returns Installed:true with
// Quirks["unsupported"]="pre-sqlite"; capture/replay then refuse with a
// clear upgrade message rather than silently using a legacy reader.
func (a *Adapter) Detect(ctx context.Context) (adapter.Detection, error) {
	path, err := exec.LookPath("opencode")
	if err != nil {
		return adapter.Detection{Installed: false}, nil
	}

	version := ""
	if out, verr := exec.CommandContext(ctx, path, "--version").Output(); verr == nil {
		version = strings.TrimSpace(string(out))
	}

	dataDir, dbPath, derr := resolveDBPath()
	quirks := map[string]string{
		"data_dir": dataDir,
		"db_path":  dbPath,
	}
	d := adapter.Detection{Installed: true, Path: path, Version: version, Quirks: quirks}

	if derr != nil {
		// Couldn't even resolve a home dir; report installed but unsupported.
		quirks["unsupported"] = "pre-sqlite"
		return d, nil
	}

	if !sqliteEraPresent(dbPath) {
		quirks["unsupported"] = "pre-sqlite"
	}
	return d, nil
}

// sqliteEraPresent reports whether the opencode.db file exists. Era
// detection keys off the on-disk artifact, not the version string (more
// robust per the design).
func sqliteEraPresent(dbPath string) bool {
	if dbPath == "" {
		return false
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// resolveDBPath returns opencode's data dir and the resolved db path,
// honoring $OPENCODE_DB (verified database.ts:43-52):
//   - $OPENCODE_DB == ":memory:" or an absolute path → used verbatim.
//   - $OPENCODE_DB relative → joined under the data dir.
//   - unset → <data_dir>/opencode.db.
//
// The data dir is $XDG_DATA_HOME/opencode (falling back to
// ~/.local/share/opencode), matching Global.Path.data.
func resolveDBPath() (dataDir, dbPath string, err error) {
	dataDir = openCodeDataDir()

	if env := strings.TrimSpace(os.Getenv("OPENCODE_DB")); env != "" {
		if env == ":memory:" || filepath.IsAbs(env) {
			return dataDir, env, nil
		}
		return dataDir, filepath.Join(dataDir, env), nil
	}

	if dataDir == "" {
		return "", "", os.ErrNotExist
	}
	return dataDir, filepath.Join(dataDir, "opencode.db"), nil
}

// openCodeDataDir resolves $XDG_DATA_HOME/opencode, falling back to
// ~/.local/share/opencode. Returns "" only if no home dir can be found.
func openCodeDataDir() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "opencode")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

// InstallBridge / UninstallBridge live in install.go (no-ops).
// FromNative lives in fromnative.go.
// ToNative / Hydrate / NativeResumeCmd live in tonative.go / hydrate.go / native_resume.go.
// ReconcileOnce lives in watcher.go.
