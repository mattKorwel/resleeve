package adapter

import (
	"context"
	"errors"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
)

// ErrNotImplemented is returned by Adapter methods that are part of
// the interface contract but not yet implemented in the current stage.
var ErrNotImplemented = errors.New("adapter: not implemented")

// Adapter is the contract every CLI adapter implements. v1 ships only
// the Claude Code adapter; opencode / Codex / Gemini implementations
// live in v3. See docs/design/round-2/09-adapter-interface.md.
type Adapter interface {
	// Identity
	Name() string
	Detect(ctx context.Context) (Detection, error)

	// Bridge plugin lifecycle
	InstallBridge(ctx context.Context, opts InstallOpts) error
	UninstallBridge(ctx context.Context) error

	// Capture (translate native input to normalized events)
	FromNative(ctx context.Context, raw []byte, src Source) ([]event.Event, error)

	// Re-sleeving paths (Stage 5+)
	ToNative(ctx context.Context, events []event.Event, target NativeFormat) ([]byte, error)
	Hydrate(ctx context.Context, session SessionView, target Workspace) error
	NativeResumeCmd(session SessionView) (cmd string, args []string)
}

// Detection describes the state of a CLI's installation on the host.
type Detection struct {
	Installed bool
	Path      string            // absolute path to the CLI binary, if found
	Version   string            // CLI version (vendor-flavored)
	Quirks    map[string]string // adapter-specific notes
}

// InstallOpts configures bridge-plugin installation.
type InstallOpts struct {
	ResleeveBinPath string // absolute path to the resleeve binary (preferred over $PATH lookup)
	Endpoint        string // optional override; daemon endpoint URL
	DryRun          bool   // compute changes, do not write
}

// Source describes where a FromNative input came from.
type Source struct {
	Kind        SourceKind
	SessionHint string // optional session_id when the source knows it
}

// SourceKind discriminates the shape of a FromNative input.
type SourceKind int

const (
	SourceUnknown SourceKind = iota
	SourceHook              // one Claude-Code hook JSON envelope on stdin
	SourceJSONL             // one JSONL line from a session file
)

// SessionView is a read-only projection of a stored session, used by
// Hydrate (not yet implemented in v1).
type SessionView struct {
	SessionID   string
	CLI         string
	CLIVersion  string
	Cwd         string
	GitBranch   string
	Model       string
	StartedAt   time.Time
	EndedAt     *time.Time
	EventStream func() ([]event.Event, error)
}

// Workspace is the target host's environment for Hydrate.
type Workspace struct {
	Cwd     string
	HomeDir string
	Env     map[string]string
}

// NativeFormat selects the on-disk format Adapter.ToNative produces.
type NativeFormat struct {
	Vendor  string
	Version string // empty = latest known
}
