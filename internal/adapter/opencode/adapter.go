package opencode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/common"
	"github.com/mattkorwel/resleeve/internal/event"
)

// Name is the adapter identifier registered with the daemon and used
// by `resleeve resume --cli opencode`.
const Name = "opencode"

// Adapter implements adapter.Adapter for opencode. Stage-5 stub: only
// re-sleeve via prime mode is wired; capture verbs return
// adapter.ErrNotImplemented.
type Adapter struct{}

// New returns a fresh opencode adapter.
func New() *Adapter { return &Adapter{} }

// Name returns the adapter identifier.
func (a *Adapter) Name() string { return Name }

// Detect probes for the `opencode` binary on $PATH. Same shape as the
// claude adapter; never returns a non-nil error — if the binary's
// missing, Installed: false reflects that.
func (a *Adapter) Detect(ctx context.Context) (adapter.Detection, error) {
	path, err := exec.LookPath("opencode")
	if err != nil {
		return adapter.Detection{Installed: false}, nil
	}
	// We don't probe --version yet because the opencode flag surface
	// is TBD; the binary's mere presence is enough for v1.
	return adapter.Detection{Installed: true, Path: path}, nil
}

// InstallBridge is not implemented for opencode in v1.
func (a *Adapter) InstallBridge(ctx context.Context, opts adapter.InstallOpts) error {
	return fmt.Errorf("%w: opencode.InstallBridge (v3 scope)", adapter.ErrNotImplemented)
}

// UninstallBridge is not implemented for opencode in v1.
func (a *Adapter) UninstallBridge(ctx context.Context) error {
	return fmt.Errorf("%w: opencode.UninstallBridge (v3 scope)", adapter.ErrNotImplemented)
}

// FromNative is not implemented for opencode in v1 (capture is v3
// scope; resleeve has no opencode hook surface yet).
func (a *Adapter) FromNative(ctx context.Context, raw []byte, src adapter.Source) ([]event.Event, error) {
	return nil, fmt.Errorf("%w: opencode.FromNative (v3 scope)", adapter.ErrNotImplemented)
}

// ToNative renders the event stream into opencode's native input. The
// only mode opencode supports in v1 is prime: a synthesized markdown
// opening prompt. Replay mode is permanently unsupported (opencode
// can't ingest claude JSONL). Auto silently picks prime.
func (a *Adapter) ToNative(ctx context.Context, events []event.Event, mode adapter.RenderMode) ([]byte, error) {
	switch mode {
	case adapter.RenderModeAuto, adapter.RenderModePrime:
		view := sessionViewFromEvents(events)
		return common.SynthesizePrime(view, events, common.PrimeOpts{})
	case adapter.RenderModeReplay:
		return nil, fmt.Errorf("opencode.ToNative: replay mode is not supported (opencode can't ingest claude JSONL); use prime")
	default:
		return nil, fmt.Errorf("opencode.ToNative: unknown mode %q", mode)
	}
}

// Hydrate forces prime mode for opencode and writes the synthesized
// prompt to ~/.resleeve/hydrate/<new-uuid>.md. A fresh session id is
// minted because, conceptually, the target opencode run is a new
// session — the prior history is folded into a single opening prompt.
func (a *Adapter) Hydrate(ctx context.Context, session adapter.SessionView, opts adapter.HydrateOpts) (adapter.HydrateResult, error) {
	if opts.Mode == adapter.RenderModeReplay {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: replay mode is not supported; opencode forces prime")
	}
	if session.EventStream == nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: session view has no EventStream")
	}
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

	home, err := os.UserHomeDir()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: homedir: %w", err)
	}
	dir := filepath.Join(home, ".resleeve", "hydrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: mkdir %s: %w", dir, err)
	}

	newID := uuid.NewString()
	out := filepath.Join(dir, newID+".md")
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: write tempfile: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("opencode.Hydrate: rename: %w", err)
	}

	return adapter.HydrateResult{
		Mode:      adapter.RenderModePrime,
		Path:      out,
		SessionID: newID,
		Notes: []string{
			"prime mode: synthesized opening prompt; cross-CLI translation is lossy by design",
		},
	}, nil
}

// NativeResumeCmd returns the command + args that hand the prime
// prompt to opencode. opencode's actual prompt-file flag is TBD;
// this returns `opencode --prompt-file <path>` as a stub the operator
// can override (or run via --print and pipe themselves).
//
// TODO(v3): once opencode's CLI surface is pinned, swap to whatever
// the canonical flag turns out to be — likely `opencode run --prompt
// <file>` or stdin via `-`.
func (a *Adapter) NativeResumeCmd(ctx context.Context, session adapter.SessionView, result adapter.HydrateResult) (string, []string, error) {
	if result.Mode != adapter.RenderModePrime {
		return "", nil, fmt.Errorf("opencode.NativeResumeCmd: unsupported mode %q (only prime)", result.Mode)
	}
	if strings.TrimSpace(result.Path) == "" {
		return "", nil, fmt.Errorf("opencode.NativeResumeCmd: prime result missing Path")
	}
	return "opencode", []string{"--prompt-file", result.Path}, nil
}

// sessionViewFromEvents constructs a minimal SessionView from the
// events themselves, for ToNative-direct callers without one.
func sessionViewFromEvents(events []event.Event) adapter.SessionView {
	var view adapter.SessionView
	for _, e := range events {
		if e.SessionID != "" {
			view.SessionID = e.SessionID
			break
		}
	}
	view.CLI = Name
	return view
}
