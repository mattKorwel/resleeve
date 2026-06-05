package opencode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/common"
)

// execCommand is the seam tests use to stub out the `opencode import`
// invocation. Production points it at exec.CommandContext.
var execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// Hydrate materializes opencode's native state for a session so that
// exec'ing the returned NativeResumeCmd resumes it.
//
// Replay mode (auto default, opencode → opencode): write the import
// transcript JSON to ~/.resleeve/hydrate/<sessionID>.json, then run
// `opencode import <path>` (a state-materialization step — exactly what
// Hydrate is for). opencode import preserves the provided session id, so
// the result's SessionID is the captured one and NativeResumeCmd returns
// `opencode run --session <id>`. If import fails at runtime, fall back to
// prime with an explanatory Note.
//
// Prime mode (cross-CLI, or forced): write a synthesized markdown opening
// prompt to ~/.resleeve/hydrate/<new-uuid>.md and mint a fresh session id.
func (a *Adapter) Hydrate(ctx context.Context, session adapter.SessionView, opts adapter.HydrateOpts) (adapter.HydrateResult, error) {
	mode := opts.Mode
	if mode == adapter.RenderModeAuto {
		mode = adapter.RenderModeReplay
	}
	if session.EventStream == nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: session view has no EventStream")
	}
	events, err := session.EventStream()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: load events: %w", err)
	}

	switch mode {
	case adapter.RenderModePrime:
		return a.hydratePrime(ctx, session, opts)
	case adapter.RenderModeReplay:
		// fallthrough below
	default:
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: unknown mode %q", mode)
	}

	body, err := a.ToNative(ctx, events, adapter.RenderModeReplay)
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: ToNative: %w", err)
	}

	dir, err := hydrateDir()
	if err != nil {
		return adapter.HydrateResult{}, err
	}

	sessionID := session.SessionID
	if id := importSessionID(body); id != "" {
		sessionID = id
	}
	out := filepath.Join(dir, fallbackStr(sessionID, uuid.NewString())+".json")
	if err := atomicWrite(out, body); err != nil {
		return adapter.HydrateResult{}, err
	}

	// `opencode import` binds the session's directory to the cwd it runs in
	// (verified live: it overrides info.directory). Run it in the captured
	// cwd so the resumed session points at the right project — but only if
	// that dir exists locally (cross-machine resume may not have it), else
	// let import default to the process cwd.
	importCwd := opts.Cwd
	if importCwd == "" {
		importCwd = session.Cwd
	}
	notes := []string{"replay: imported transcript via `opencode import`; opencode preserves the session id"}
	if importCwd != "" {
		if fi, statErr := os.Stat(importCwd); statErr == nil && fi.IsDir() {
			notes = append(notes, fmt.Sprintf("imported in cwd %q so the session binds to it", importCwd))
		} else {
			notes = append(notes, fmt.Sprintf("captured cwd %q not present locally; session binds to the import process cwd instead", importCwd))
			importCwd = ""
		}
	}

	// Run `opencode import <path>` to materialize the native session.
	if err := runImport(ctx, out, importCwd); err != nil {
		// Defensive: import failed at runtime — fall back to prime so the
		// resume still works, just lossily.
		res, perr := a.hydratePrime(ctx, session, opts)
		if perr != nil {
			return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: import failed (%v) and prime fallback failed: %w", err, perr)
		}
		res.Notes = append([]string{
			fmt.Sprintf("opencode import failed (%v); fell back to prime mode (lossy)", err),
		}, res.Notes...)
		return res, nil
	}

	return adapter.HydrateResult{
		Mode:      adapter.RenderModeReplay,
		Path:      out,
		SessionID: sessionID,
		Notes:     notes,
	}, nil
}

// hydratePrime renders the prime markdown and writes it to a fresh
// scratch file, minting a new session id.
func (a *Adapter) hydratePrime(ctx context.Context, session adapter.SessionView, opts adapter.HydrateOpts) (adapter.HydrateResult, error) {
	events, err := session.EventStream()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: load events: %w", err)
	}
	body, err := common.SynthesizePrime(session, events, common.PrimeOpts{
		PlanContent: opts.PlanContent,
	})
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: synthesize prime: %w", err)
	}

	dir, err := hydrateDir()
	if err != nil {
		return adapter.HydrateResult{}, err
	}
	newID := uuid.NewString()
	out := filepath.Join(dir, newID+".md")
	if err := atomicWrite(out, body); err != nil {
		return adapter.HydrateResult{}, err
	}

	return adapter.HydrateResult{
		Mode:      adapter.RenderModePrime,
		Path:      out,
		SessionID: newID,
		Notes:     []string{"prime mode: synthesized opening prompt; cross-CLI translation is lossy by design"},
	}, nil
}

// runImport runs `opencode import <path>`. When cwd is non-empty the import
// runs there so opencode binds the session's directory to it (import
// overrides info.directory with its working dir).
func runImport(ctx context.Context, path, cwd string) error {
	cmd := execCommand(ctx, "opencode", "import", path)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("opencode import %s: %w: %s", path, err, string(out))
	}
	return nil
}

// hydrateDir returns ~/.resleeve/hydrate, creating it if needed.
func hydrateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("opencode.Hydrate: homedir: %w", err)
	}
	dir := filepath.Join(home, ".resleeve", "hydrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("opencode.Hydrate: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// atomicWrite writes body to path via a temp file + rename.
func atomicWrite(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("opencode.Hydrate: write tempfile: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("opencode.Hydrate: rename: %w", err)
	}
	return nil
}
